<script setup lang="ts">
import { nextTick, onMounted, ref, watch } from 'vue'
import { useChat } from './chat'
import { mediaUrl, type MediaRef, type Usage } from './api'

const { bubbles, draft, attachments, pending, loadingOlder, atStart, send, reconcile, loadOlder, newSession } = useChat()

const listEl = ref<HTMLElement | null>(null)
const pinned = ref(true)
const fileEl = ref<HTMLInputElement | null>(null)

// 附件（doc/32 A5）：浏览器把文件读成 base64 随 POST 上行，服务端落盘换占位行——
// 页面上传的从来不是「贴进对话的字节」，是「要落盘的文件」。
const MAX_ATTACH = 4
const MAX_FILE_BYTES = 5 * 1024 * 1024

async function onPickFiles(ev: Event) {
  const input = ev.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  for (const f of files) {
    if (attachments.value.length >= MAX_ATTACH) break
    if (f.size > MAX_FILE_BYTES || !f.type) continue
    const dataBase64 = await new Promise<string>((resolve, reject) => {
      const r = new FileReader()
      r.onload = () => resolve(String(r.result).split(',')[1] ?? '')
      r.onerror = () => reject(r.error)
      r.readAsDataURL(f)
    })
    attachments.value.push({ name: f.name, img: { mime: f.type, dataBase64, name: f.name } })
  }
}

function removeAttachment(i: number) {
  attachments.value.splice(i, 1)
}

function isImage(m: MediaRef): boolean {
  return m.mime.startsWith('image/')
}

// 通信字段的显示面（doc/32 A6/A7）：有 name 显示 name，无名退化「图片 {seq}」，
// 存量行（无 name/seq）退化 mime+size——三层兜底，刷新 reconcile 后名字不丢。
function mediaLabel(m: MediaRef): string {
  if (m.name) return m.name
  if (m.seq != null) return `图片 ${m.seq}`
  return `${m.mime} ${fmtSize(m.size)}`
}

function fmtSize(n: number): string {
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)}MB`
  if (n >= 1024) return `${Math.round(n / 1024)}KB`
  return `${n}B`
}

// usage 小字（doc/27 §4a 消费侧）：↑输入 ↓输出 · 输入/窗 (占比%)；无窗只显示花销。
// 占比 >70% 橙、>90% 红（pi 阈值惯例）；1% 以下显示 <1 免得整轮 0%。
function fmtTok(n: number): string {
  if (n >= 1000) {
    const k = n / 1000
    return `${k >= 100 ? Math.round(k) : Number(k.toFixed(1))}k`
  }
  return String(n)
}
function usageText(u: Usage): string {
  const a = fmtTok(u.promptTokens)
  const line = `↑${a} ↓${fmtTok(u.completionTokens)}`
  if (!u.window) return line
  const pct = (u.promptTokens / u.window) * 100
  const p = pct > 0 && pct < 1 ? '<1' : String(Math.round(pct))
  return `${line} · ${a}/${fmtTok(u.window)} (${p}%)`
}
function usageClass(u: Usage): string {
  if (!u.window) return ''
  const pct = (u.promptTokens / u.window) * 100
  return pct > 90 ? 'hot' : pct > 70 ? 'warm' : ''
}

function scrollBottom() {
  const el = listEl.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(bubbles, () => {
  if (pinned.value) nextTick(scrollBottom)
}, { deep: true })

onMounted(async () => {
  await reconcile()
  await nextTick()
  scrollBottom()
})

async function onScroll() {
  const el = listEl.value
  if (!el) return
  pinned.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
  if (el.scrollTop < 40 && !atStart.value) {
    const before = el.scrollHeight
    await loadOlder()
    await nextTick()
    // prepend 后把视口推回原内容，避免跳顶。
    el.scrollTop += el.scrollHeight - before
  }
}
</script>

<template>
  <div class="shell">
    <main>
      <header>
        <div class="brand">
          <span class="brand-dot" aria-hidden="true"></span>
          <h1>tako</h1>
        </div>
        <button class="ghost" @click="newSession">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">
            <path d="M8 3v10M3 8h10" />
          </svg>
          新对话
        </button>
      </header>

      <section ref="listEl" class="list" @scroll="onScroll">
        <div v-if="!bubbles.length" class="empty">
          <div class="empty-logo" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
              <path d="M21 11.5a8.4 8.4 0 0 1-9 8.4 8.6 8.6 0 0 1-3.8-.9L3 21l1.9-5.2a8.4 8.4 0 1 1 16.1-4.3z" />
            </svg>
          </div>
          <p class="empty-title">开始一段对话</p>
          <p class="empty-hint">投递后看着回复逐字生成<br />刷新或新开标签页都能接上</p>
        </div>

        <div v-if="loadingOlder" class="older">
          <span class="spinner" aria-hidden="true"></span>加载更早…
        </div>
        <div v-else-if="!atStart" class="older muted">上滚查看更早历史</div>

        <div v-for="b in bubbles" :key="b.key" :class="['bubble', b.role, b.state]">
          <template v-if="b.media?.length">
            <template v-for="(m, i) in b.media" :key="`${b.key}:m${i}`">
              <img
                v-if="isImage(m)"
                class="bubble-media"
                :src="mediaUrl(m.path)"
                :alt="mediaLabel(m)"
                :title="mediaLabel(m)"
                loading="lazy"
              />
              <p v-else class="bubble-file">📎 {{ mediaLabel(m) }}（{{ m.mime }}，{{ fmtSize(m.size) }}）</p>
            </template>
          </template>
          <p v-if="b.text">{{ b.text }}</p>
          <span v-else-if="b.state === 'streaming' || b.state === 'offline'" class="typing" aria-label="正在生成">
            <i></i><i></i><i></i>
          </span>
          <p v-if="b.state === 'offline'" class="queue-note">
            <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
              <path d="M2.6 13.4 13.4 2.6M6.2 9.8a3.4 3.4 0 0 1 3.6-.8" /><path d="M3.4 6.8a6.2 6.2 0 0 1 8.4-1.4" />
            </svg>
            拉不到帧，改看会话状态
          </p>
          <p v-if="b.state === 'queued'" class="queue-note">
            <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
              <circle cx="8" cy="8" r="6.2" /><path d="M8 4.6V8l2.2 1.6" />
            </svg>
            会话忙，排队待发
          </p>
          <p v-if="b.state === 'stopped'" class="stopped-note">
            <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
              <rect x="4.4" y="4.4" width="7.2" height="7.2" rx="1.4" />
            </svg>
            已停止，未进历史
          </p>
          <p v-if="b.state === 'error'" class="err-note">
            <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
              <circle cx="8" cy="8" r="6.2" /><path d="M8 5v3.6M8 11h.01" />
            </svg>
            失败，未进历史
          </p>
          <span v-if="b.role === 'assistant' && b.usage" class="usage" :class="usageClass(b.usage)">{{ usageText(b.usage) }}</span>
          <span v-else-if="b.role === 'assistant' && b.usageUnknown" class="usage">用量 ?</span>
        </div>
      </section>

      <form class="composer" @submit.prevent="send">
        <input ref="fileEl" type="file" accept="image/*,application/pdf,audio/*,video/*" multiple hidden @change="onPickFiles" />
        <button
          type="button"
          class="attach"
          :disabled="attachments.length >= MAX_ATTACH"
          aria-label="添加图片或文件"
          @click="fileEl?.click()"
        >
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
            <path d="M13.2 7.6 8 12.8a3.4 3.4 0 0 1-4.8-4.8l5.4-5.4a2.3 2.3 0 0 1 3.2 3.2l-5.4 5.4a1.1 1.1 0 0 1-1.6-1.6L9.6 4.8" />
          </svg>
        </button>
        <div v-if="attachments.length" class="attach-chips">
          <span v-for="(a, i) in attachments" :key="i" class="chip">
            {{ a.name }}<button type="button" class="chip-x" aria-label="移除附件" @click="removeAttachment(i)">×</button>
          </span>
        </div>
        <input
          v-model="draft"
          :placeholder="pending.length ? `排队 ${pending.length} 条，当前回复收口后自动发出` : '说点什么…'"
          aria-label="消息输入框"
        />
        <button type="submit" :disabled="!draft.trim() && !attachments.length" aria-label="发送">
          <svg viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
            <path d="M3.1 2.2 18 10 3.1 17.8c-.7.4-1.6-.1-1.6-.9v-4.4l9-2.5-9-2.5V3.1c0-.8.9-1.3 1.6-.9z" />
          </svg>
          <span class="sr-only">发送</span>
        </button>
      </form>
    </main>
  </div>
</template>

<style>
html,
body {
  margin: 0;
  padding: 0;
  height: 100%;
  background: #f7f7f9;
}
</style>

<style scoped>
.shell {
  height: 100vh;
  background:
    radial-gradient(60rem 30rem at 50% -10rem, #eef2ff 0%, transparent 60%),
    #f7f7f9;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', system-ui, sans-serif;
  color: #1c1c22;
  -webkit-font-smoothing: antialiased;
}
main {
  max-width: 44rem;
  margin: 0 auto;
  padding: 0 1rem 1.25rem;
  display: flex;
  flex-direction: column;
  height: 100%;
  box-sizing: border-box;
}

/* ── 顶栏：毛玻璃吸顶 ── */
header {
  position: sticky;
  top: 0;
  z-index: 10;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.9rem 0.25rem;
  background: rgba(247, 247, 249, 0.82);
  backdrop-filter: blur(12px);
  -webkit-backdrop-filter: blur(12px);
}
.brand {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}
.brand-dot {
  width: 0.6rem;
  height: 0.6rem;
  border-radius: 50%;
  background: linear-gradient(135deg, #60a5fa, #2563eb);
  box-shadow: 0 0 0 3px rgba(37, 99, 235, 0.14);
}
header h1 {
  font-size: 1.05rem;
  font-weight: 700;
  letter-spacing: 0.06em;
  margin: 0;
}
.ghost {
  display: inline-flex;
  align-items: center;
  gap: 0.35rem;
  border: 1px solid #e2e2e8;
  background: #fff;
  color: #3f3f4a;
  font-size: 0.8rem;
  border-radius: 999px;
  padding: 0.4rem 0.9rem;
  cursor: pointer;
  transition: border-color 0.15s, color 0.15s, box-shadow 0.15s;
  box-shadow: 0 1px 2px rgba(16, 16, 24, 0.04);
}
.ghost svg {
  width: 0.85rem;
  height: 0.85rem;
}
.ghost:hover:not(:disabled) {
  border-color: #b9cdf7;
  color: #2563eb;
  box-shadow: 0 2px 8px rgba(37, 99, 235, 0.1);
}
.ghost:disabled {
  opacity: 0.5;
  cursor: default;
}

/* ── 消息列表 ── */
.list {
  flex: 1;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 0.65rem;
  padding: 0.75rem 0.25rem 1rem;
  scrollbar-width: thin;
  scrollbar-color: #d8d8e0 transparent;
}
.list::-webkit-scrollbar {
  width: 6px;
}
.list::-webkit-scrollbar-thumb {
  background: #d8d8e0;
  border-radius: 3px;
}

/* ── 空态 ── */
.empty {
  margin: auto;
  text-align: center;
  padding: 2rem 1rem;
}
.empty-logo {
  width: 4rem;
  height: 4rem;
  margin: 0 auto 1.1rem;
  border-radius: 1.4rem;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #2563eb;
  background: linear-gradient(160deg, #ffffff, #eaf0ff);
  border: 1px solid #e3e9f8;
  box-shadow: 0 8px 24px rgba(37, 99, 235, 0.12);
}
.empty-logo svg {
  width: 2rem;
  height: 2rem;
}
.empty-title {
  font-size: 1.15rem;
  font-weight: 650;
  margin: 0 0 0.4rem;
}
.empty-hint {
  font-size: 0.85rem;
  line-height: 1.7;
  color: #8b8b96;
  margin: 0;
}

/* ── 更早历史提示 ── */
.older {
  align-self: center;
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.72rem;
  color: #6e6e7a;
  background: #fff;
  border: 1px solid #e8e8ee;
  border-radius: 999px;
  padding: 0.25rem 0.8rem;
  box-shadow: 0 1px 2px rgba(16, 16, 24, 0.04);
}
.older.muted {
  color: #a5a5b0;
  background: transparent;
  border-color: transparent;
  box-shadow: none;
}
.spinner {
  width: 0.75rem;
  height: 0.75rem;
  border-radius: 50%;
  border: 2px solid #cfd8ea;
  border-top-color: #2563eb;
  animation: spin 0.8s linear infinite;
}
@keyframes spin {
  to { transform: rotate(360deg); }
}

/* ── 气泡 ── */
.bubble {
  max-width: 78%;
  padding: 0.6rem 0.9rem;
  font-size: 0.92rem;
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-word;
  animation: rise 0.18s ease-out;
}
@keyframes rise {
  from { opacity: 0; transform: translateY(4px); }
  to { opacity: 1; transform: none; }
}
.bubble p {
  margin: 0;
}
.bubble.user {
  align-self: flex-end;
  color: #fff;
  background: linear-gradient(135deg, #3b82f6, #2563eb);
  border-radius: 1.1rem 1.1rem 0.3rem 1.1rem;
  box-shadow: 0 2px 10px rgba(37, 99, 235, 0.22);
}
.bubble.assistant {
  align-self: flex-start;
  background: #fff;
  border: 1px solid #ececf1;
  border-radius: 1.1rem 1.1rem 1.1rem 0.3rem;
  box-shadow: 0 1px 3px rgba(16, 16, 24, 0.05);
}
.bubble.error {
  background: #fff5f5;
  border-color: #fbd5d5;
  color: #b03434;
  box-shadow: none;
}
/* 排队待发：还没投递成功，视觉上就该和「已在服务端」的气泡分开 */
.bubble.queued {
  opacity: 0.62;
  border-style: dashed;
  border-color: #dfe3ee;
  box-shadow: none;
}

/* 打字指示（streaming 且尚无文本） */
.typing {
  display: inline-flex;
  align-items: center;
  gap: 0.3rem;
  height: 1.2em;
}
.typing i {
  width: 0.38rem;
  height: 0.38rem;
  border-radius: 50%;
  background: #b3bfd6;
  animation: blink 1.2s ease-in-out infinite;
}
.typing i:nth-child(2) { animation-delay: 0.15s; }
.typing i:nth-child(3) { animation-delay: 0.3s; }
@keyframes blink {
  0%, 60%, 100% { opacity: 0.3; transform: translateY(0); }
  30% { opacity: 1; transform: translateY(-2px); }
}

.err-note {
  display: flex;
  align-items: center;
  gap: 0.3rem;
  font-size: 0.72rem;
  margin-top: 0.4rem !important;
  opacity: 0.85;
}
.err-note svg {
  width: 0.8rem;
  height: 0.8rem;
  flex: none;
}
.stopped-note {
  display: flex;
  align-items: center;
  gap: 0.3rem;
  font-size: 0.72rem;
  margin-top: 0.4rem !important;
  color: #6e6e7a;
}
.stopped-note svg {
  width: 0.8rem;
  height: 0.8rem;
  flex: none;
}
.queue-note {
  display: flex;
  align-items: center;
  gap: 0.3rem;
  font-size: 0.72rem;
  margin-top: 0.4rem !important;
  color: #6e6e7a;
}
.queue-note svg {
  width: 0.8rem;
  height: 0.8rem;
  flex: none;
}

/* 气泡内媒体（doc/32 A5）：占位行换成的 <img> / 文件片 */
.bubble-media {
  display: block;
  max-width: min(100%, 20rem);
  max-height: 18rem;
  border-radius: 0.6rem;
  margin-bottom: 0.4rem;
  background: rgba(255, 255, 255, 0.85);
}
.bubble.user .bubble-media {
  background: #fff;
}
.bubble-file {
  font-size: 0.8rem !important;
  opacity: 0.9;
}

/* usage 小字（doc/27）：气泡下方一行灰字；>70% 橙、>90% 红 */
.usage {
  display: block;
  margin-top: 0.35rem;
  font-size: 0.7rem;
  color: #8b8b96;
  font-variant-numeric: tabular-nums;
}
.usage.warm {
  color: #b45309;
}
.usage.hot {
  color: #b03434;
}

/* ── 输入区：悬浮卡片 ── */
.composer {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  background: #fff;
  border: 1px solid #e6e6ec;
  border-radius: 999px;
  padding: 0.4rem 0.4rem 0.4rem 0.5rem;
  box-shadow: 0 4px 20px rgba(16, 16, 24, 0.07);
  transition: border-color 0.15s, box-shadow 0.15s;
}
.attach {
  flex: none;
  width: 1.9rem;
  height: 1.9rem;
  display: flex;
  align-items: center;
  justify-content: center;
  border: none;
  border-radius: 50%;
  background: transparent;
  color: #6e6e7a;
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}
.attach svg {
  width: 1.05rem;
  height: 1.05rem;
}
.attach:hover:not(:disabled) {
  background: #eef2ff;
  color: #2563eb;
}
.attach:disabled {
  opacity: 0.4;
  cursor: default;
}
.attach-chips {
  display: flex;
  gap: 0.3rem;
  flex-wrap: wrap;
  max-width: 42%;
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  font-size: 0.72rem;
  color: #3f3f4a;
  background: #f1f3f9;
  border: 1px solid #e2e6f0;
  border-radius: 999px;
  padding: 0.15rem 0.6rem;
  max-width: 12rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.chip-x {
  border: none;
  background: none;
  color: #8b8b96;
  font-size: 0.85rem;
  line-height: 1;
  cursor: pointer;
  padding: 0;
}
.chip-x:hover {
  color: #b03434;
}
.composer:focus-within {
  border-color: #b9cdf7;
  box-shadow: 0 4px 24px rgba(37, 99, 235, 0.13);
}
.composer input {
  flex: 1;
  min-width: 0;
  border: none;
  outline: none;
  background: transparent;
  font: inherit;
  font-size: 0.92rem;
  color: inherit;
  padding: 0.45rem 0;
}
.composer input::placeholder {
  color: #a5a5b0;
}
.composer input:disabled {
  cursor: default;
}
.composer button {
  flex: none;
  width: 2.35rem;
  height: 2.35rem;
  display: flex;
  align-items: center;
  justify-content: center;
  border: none;
  border-radius: 50%;
  background: linear-gradient(135deg, #3b82f6, #2563eb);
  color: #fff;
  cursor: pointer;
  transition: transform 0.12s, box-shadow 0.15s, opacity 0.15s;
  box-shadow: 0 2px 8px rgba(37, 99, 235, 0.3);
}
.composer button svg {
  width: 1rem;
  height: 1rem;
  margin-left: 0.1rem;
}
.composer button:hover:not(:disabled) {
  transform: scale(1.06);
}
.composer button:active:not(:disabled) {
  transform: scale(0.96);
}
.composer button:disabled {
  background: #dde5f3;
  color: #fff;
  box-shadow: none;
  cursor: default;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
  white-space: nowrap;
}
</style>
