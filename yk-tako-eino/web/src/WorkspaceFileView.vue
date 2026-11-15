<script setup lang="ts">
// 文件查看（doc/78 §A4）：字节仍走 GET /api/media（带 Bearer，不能直接塞 href）；
// 只读——不提供编辑、不提供保存。
//
// md 文件按 markdown 渲染显示（doc/79）：复用 markdown.ts 那条管线（marked + DOMPurify），
// 排版走全局 `.md-body`。默认**预览态**，可切「原文」看原始字节。
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { authFetch } from './auth'
import { workspaceFileUrl } from './api'
import { MD_BODY_CLASS, renderSafe } from './markdown'

const props = defineProps<{ workspaceId: string; path: string }>()

const TEXT_EXT = new Set([
  'txt', 'md', 'py', 'js', 'ts', 'tsx', 'vue', 'go', 'json', 'yaml', 'yml',
  'html', 'css', 'sh', 'log', 'csv', 'ini', 'conf', 'toml', 'sql', 'xml',
])
// 按 markdown 渲染的扩展名（doc/79 §3.1）。只看扩展名、不看 Content-Type：
// /api/media 的 mime 表只覆盖图片/pdf/音视频，.md 落到 application/octet-stream。
// **只这两个**：把 json/yaml 也当 markdown 渲染是倒退（散文排版套在数据文件上）。
const MD_EXT = new Set(['md', 'markdown'])
const IMAGE_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'bmp'])
const PREVIEW_LIMIT = 256 * 1024

type ViewState =
  | { kind: 'loading' }
  | { kind: 'text'; text: string; size: number }
  | { kind: 'image'; url: string; size: number }
  | { kind: 'large'; size: number }
  | { kind: 'other'; size: number }
  | { kind: 'error'; text: string }

const state = ref<ViewState>({ kind: 'loading' })
const downloading = ref(false)
const downloadError = ref('')
// 原文态（默认 false = 预览态）。**切文件时复位**：在 A 文件点了「原文」，
// 打开 B 文件也还是原文态是 bug 不是特性。
const showRaw = ref(false)

let objectUrl = ''
let loadToken = 0

function release() {
  if (objectUrl) {
    URL.revokeObjectURL(objectUrl)
    objectUrl = ''
  }
}

function ext(path: string): string {
  const base = path.slice(path.lastIndexOf('/') + 1)
  const i = base.lastIndexOf('.')
  return i < 0 ? '' : base.slice(i + 1).toLowerCase()
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

async function errorText(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { code?: number; message?: string }
    if (res.status === 401) return '请先登录'
    if (body.code === 114001) return '找不到这个项目'
    if (body.code === 114101) return '找不到这个文件'
    if (body.code === 117116) return '这个项目还需要迁移目录后才能使用'
    return body.message || `取文件失败（HTTP ${res.status}）`
  } catch {
    return res.status === 401 ? '请先登录' : `取文件失败（HTTP ${res.status}）`
  }
}

async function load() {
  const token = ++loadToken
  release()
  downloadError.value = ''
  if (!props.workspaceId || !props.path) {
    state.value = { kind: 'error', text: '没有选中文件' }
    return
  }
  state.value = { kind: 'loading' }
  let res: Response
  try {
    res = await authFetch(workspaceFileUrl(props.workspaceId, props.path))
  } catch {
    if (token === loadToken) state.value = { kind: 'error', text: '连不上引擎' }
    return
  }
  if (token !== loadToken) return
  if (!res.ok) {
    const text = await errorText(res)
    if (token === loadToken) state.value = { kind: 'error', text }
    return
  }
  let blob: Blob
  try {
    blob = await res.blob()
  } catch {
    if (token === loadToken) state.value = { kind: 'error', text: '文件读不下来' }
    return
  }
  if (token !== loadToken) return

  const e = ext(props.path)
  if (IMAGE_EXT.has(e)) {
    objectUrl = URL.createObjectURL(blob)
    state.value = { kind: 'image', url: objectUrl, size: blob.size }
  } else if (TEXT_EXT.has(e)) {
    if (blob.size > PREVIEW_LIMIT) {
      state.value = { kind: 'large', size: blob.size }
    } else {
      state.value = { kind: 'text', text: await blob.text(), size: blob.size }
    }
  } else {
    state.value = { kind: 'other', size: blob.size }
  }
}

const sizeText = computed(() => {
  const s = state.value
  return 'size' in s ? fmtSize(s.size) : ''
})

// 这是不是一份「按 markdown 渲染」的文本文件。
function isMarkdown(path: string): boolean {
  return MD_EXT.has(ext(path))
}

// 只有 md 文本文件才出现「预览/原文」切换：其它文件只有一种看法，给 .txt 也加切换是噪音。
const showToggle = computed(
  () => state.value.kind === 'text' && isMarkdown(props.path),
)

// md 预览态的 HTML。**breaks: false**——文件里的换行是作者排版的结果，
// 标准 markdown 语义下单个换行不等于换行（聊天气泡用的是 breaks: true，两边分家）。
// 安全：走 renderSafe（DOMPurify），md 文件同样是外部内容，可能带 <script>/onerror。
const renderedHtml = computed(() => {
  const s = state.value
  if (s.kind !== 'text') return ''
  return renderSafe(s.text, { breaks: false })
})

async function download() {
  downloading.value = true
  downloadError.value = ''
  try {
    const res = await authFetch(workspaceFileUrl(props.workspaceId, props.path))
    if (!res.ok) {
      downloadError.value = await errorText(res)
      return
    }
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = props.path.slice(props.path.lastIndexOf('/') + 1)
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
  } catch {
    downloadError.value = '连不上引擎'
  } finally {
    downloading.value = false
  }
}

watch(() => [props.workspaceId, props.path], () => {
  // 换文件 = 换一份内容，默认回预览态（原文态不复用）。
  showRaw.value = false
  void load()
}, { immediate: true })
onBeforeUnmount(release)
</script>

<template>
  <div class="file-view">
    <div class="file-head">
      <span class="file-path" :title="path">{{ path }}</span>
      <span v-if="sizeText" class="file-size">{{ sizeText }}</span>
      <!-- md 才有两种看法：预览（渲染）与原文（原始字节）。默认预览。 -->
      <div v-if="showToggle" class="file-views">
        <button
          type="button"
          class="file-view-btn"
          :class="{ on: !showRaw }"
          :aria-pressed="!showRaw"
          @click="showRaw = false"
        >预览</button>
        <button
          type="button"
          class="file-view-btn"
          :class="{ on: showRaw }"
          :aria-pressed="showRaw"
          @click="showRaw = true"
        >原文</button>
      </div>
      <button type="button" class="file-dl" :disabled="downloading" @click="download">
        {{ downloading ? '下载中…' : '下载' }}
      </button>
    </div>
    <p v-if="downloadError" class="file-error" role="alert">{{ downloadError }}</p>

    <div v-if="state.kind === 'loading'" class="file-note">正在取文件…</div>
    <p v-else-if="state.kind === 'error'" class="file-error" role="alert">{{ state.text }}</p>
    <div
      v-else-if="state.kind === 'text' && isMarkdown(path) && !showRaw"
      class="file-md"
      :class="MD_BODY_CLASS"
      v-html="renderedHtml"
    />
    <pre v-else-if="state.kind === 'text'" class="file-text">{{ state.text }}</pre>
    <div v-else-if="state.kind === 'image'" class="file-image">
      <img :src="state.url" :alt="path" />
    </div>
    <div v-else-if="state.kind === 'large'" class="file-note">
      文件 {{ fmtSize(state.size) }}，超过 256KB 预览上限。
    </div>
    <div v-else class="file-note">这个类型不能内嵌预览。</div>
  </div>
</template>

<style scoped>
.file-view {
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.file-head {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0 0.8rem 0.45rem;
}
.file-path {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--text-xs);
  color: var(--ink-soft);
  font-family: var(--mono, ui-monospace, monospace);
}
.file-size {
  flex: none;
  font-size: 0.7rem;
  color: var(--muted);
}
.file-dl {
  flex: none;
  border: 1px solid var(--line);
  border-radius: 999px;
  background: var(--bg-raised);
  color: var(--ink-soft);
  font-size: var(--text-xs);
  padding: 0.15rem 0.6rem;
  cursor: pointer;
}
.file-dl:disabled { color: var(--muted); cursor: default; }

/* 「预览/原文」切换：两枚小按钮，当前态高亮。 */
.file-views {
  flex: none;
  display: inline-flex;
  gap: 0.2rem;
}
.file-view-btn {
  border: 1px solid var(--line);
  border-radius: 999px;
  background: var(--bg-raised);
  color: var(--muted);
  font-size: var(--text-xs);
  padding: 0.15rem 0.55rem;
  cursor: pointer;
}
.file-view-btn.on {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--accent-ink);
}

.file-text {
  margin: 0;
  padding: 0 0.8rem 0.8rem;
  font-size: 0.78rem;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--ink-soft);
  font-family: var(--mono, ui-monospace, monospace);
}
/* md 预览态的容器：排版规则在全局 .md-body（scoped 样式不跨组件，抄不进这里）。
   这里只管这个容器自己的东西：字号、留白、长字符串换行。 */
.file-md {
  padding: 0 0.8rem 0.8rem;
  font-size: 0.8rem;
  line-height: 1.6;
  color: var(--ink-soft);
  word-break: break-word;
}
.file-image {
  padding: 0 0.8rem 0.8rem;
}
.file-image img {
  max-width: 100%;
  border-radius: 8px;
  border: 1px solid var(--line);
}
.file-note {
  margin: 0;
  padding: 0 0.8rem 0.8rem;
  font-size: var(--text-xs);
  color: var(--muted);
  line-height: 1.5;
}
.file-error {
  margin: 0;
  padding: 0 0.8rem 0.8rem;
  font-size: var(--text-xs);
  color: var(--danger);
  line-height: 1.5;
}
</style>
