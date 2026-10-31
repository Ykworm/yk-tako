<script setup lang="ts">
// 工具页（doc/52 F3）：列出/上传/删除租户工具根里的可执行文件。
//
// 这一页替掉「缺依赖就找运维」：技能 `requires` 里声明的命令，owner/admin 从这儿传一个
// 可执行文件进来，下一场 run 的 bash 就能按名字找到它（F2 的 PATH 前置）。
// 页面只做文件级的事——**没有「检测」「试跑」按钮**：引擎不在这里执行上传物，
// 能不能跑由 run 里的那一次 bash 回答（与 D22「装的时候没有围栏」同一条线）。
//
// 盘就是真源：列表 = GET /api/tools 扫盘，没有表、没有 rescan。覆盖/删除后靠
// tools_changed 事件让别的窗口重拉（doc/46 §7.6，事件只通知、不带状态）。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  ApiError,
  deleteTool,
  listSkills,
  listTools,
  uploadTool,
  type Skill,
  type TenantTool,
} from './api'
import { tenantState } from './auth'
import ConfirmDialog from './ConfirmDialog.vue'
import { onMutation } from './mutation'

const props = defineProps<{
  // 从技能详情「上传工具」跳过来时预填的命令名（doc/52 F3）。
  prefillName?: string
}>()

const phase = ref<'loading' | 'ready' | 'error'>('loading')
const tools = ref<TenantTool[]>([])
const skills = ref<Skill[]>([])
const banner = ref('')
const notice = ref('')
const busy = ref(false)
const query = ref('')

const nameInput = ref('')
const file = ref<File | null>(null)
const fileEl = ref<HTMLInputElement | null>(null)
const nameEl = ref<HTMLInputElement | null>(null)

// 覆盖确认（同名上传 = 替换，旧目录进 .trash）；删除确认。
const pendingOverwrite = ref<{ name: string; file: File } | null>(null)
const pendingDelete = ref<TenantTool | null>(null)

const canAdmin = computed(() => {
  const role = tenantState.value.role
  return role === 'owner' || role === 'admin'
})

// 与 POST /api/tools 同一套白名单（服务端仍是裁判）：这里只为把错误挡在点按钮之前。
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/
const nameHint = computed(() => {
  const n = nameInput.value.trim()
  if (!n || NAME_RE.test(n)) return ''
  return '名字只能用小写字母、数字和 -，且以字母或数字开头（最长 64）。'
})
const canSubmit = computed(() => !!file.value && !!nameInput.value.trim() && !nameHint.value)

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return tools.value
  return tools.value.filter((t) => t.name.toLowerCase().includes(q))
})

// 「被哪些技能 requires 引用」：从技能列表现算（盘上的工具没有反向索引）。
function usedBy(name: string): string[] {
  return skills.value.filter((s) => s.requires.includes(name)).map((s) => s.name)
}

let reloadTimer: ReturnType<typeof setTimeout> | null = null

function scheduleReload() {
  if (reloadTimer) clearTimeout(reloadTimer)
  reloadTimer = setTimeout(() => void load(), 120)
}

const offMutation = onMutation((evt) => {
  if (evt.type === 'tools_changed') scheduleReload()
  // 技能的 requires 会变（装机/删除），引用列跟着重拉；工具本身不用动。
  if (evt.type === 'skills_changed') scheduleReload()
})

async function load() {
  try {
    const res = await listTools()
    tools.value = res.tools
    phase.value = 'ready'
  } catch (err) {
    phase.value = 'error'
    banner.value = err instanceof ApiError ? err.message : '工具列表没拉到'
  }
}

async function loadSkills() {
  try {
    skills.value = (await listSkills()).skills
  } catch {
    // 引用列是辅助信息：拉不到就留空，不挡住工具列表本身。
    skills.value = []
  }
}

function fail(err: unknown) {
  banner.value = err instanceof ApiError ? err.message : '请求失败，请再试一次'
}

function pickFile(ev: Event) {
  const input = ev.target as HTMLInputElement
  const picked = input.files?.[0] ?? null
  file.value = picked
  // 名字还空着时用文件名兜底（去掉扩展名）：多数人传的就是 `jq`、`ffmpeg` 这种名字。
  if (picked && !nameInput.value.trim()) {
    nameInput.value = picked.name.replace(/\.[^.]+$/, '').toLowerCase().replace(/[^a-z0-9-]+/g, '-')
  }
  input.value = ''
}

function submit() {
  banner.value = ''
  notice.value = ''
  const name = nameInput.value.trim()
  if (!canSubmit.value) {
    banner.value = '先填命令名并选一个文件'
    return
  }
  // 同名 = 替换：先问一句，并说清「正在跑的 run 从下一次调用起用新版本」。
  if (tools.value.some((t) => t.name === name)) {
    if (file.value) pendingOverwrite.value = { name, file: file.value }
    return
  }
  void doUpload(name, file.value!)
}

async function doUpload(name: string, picked: File) {
  if (busy.value) return
  busy.value = true
  banner.value = ''
  try {
    const res = await uploadTool(name, picked)
    pendingOverwrite.value = null
    file.value = null
    nameInput.value = ''
    notice.value = `已上传工具：${res.tool.name}（${fmtBytes(res.tool.bytes)}）`
    await load()
  } catch (err) {
    fail(err)
  } finally {
    busy.value = false
  }
}

async function removeConfirmed() {
  const row = pendingDelete.value
  if (!row) return
  banner.value = ''
  busy.value = true
  try {
    await deleteTool(row.name)
    tools.value = tools.value.filter((t) => t.name !== row.name)
    pendingDelete.value = null
    notice.value = `已删除工具：${row.name}`
  } catch (err) {
    fail(err)
  } finally {
    busy.value = false
  }
}

function fmtWhen(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

// 预填只在「从技能详情跳过来」时发生；组件挂载时 prop 已经有了，所以 immediate。
watch(
  () => props.prefillName,
  async (name) => {
    if (!name) return
    nameInput.value = name
    await nextTick()
    nameEl.value?.focus()
  },
  { immediate: true },
)

onMounted(() => {
  void load()
  void loadSkills()
})

onBeforeUnmount(() => {
  offMutation()
  if (reloadTimer) clearTimeout(reloadTimer)
})
</script>

<template>
  <div class="tools">
    <header class="page-head">
      <div>
        <p class="kicker">工具</p>
        <h2>团队自己的命令行</h2>
        <p class="hint">
          上传一个可执行文件，团队里所有对话（含子代理）就能按名字直接跑它。它只是文件——引擎不探测格式，
          能不能跑由对话里那一次 bash 回答。
        </p>
      </div>
    </header>

    <p v-if="banner" class="banner error" role="alert">{{ banner }}</p>
    <p v-if="notice" class="banner ok" role="status">{{ notice }}</p>

    <!-- 上传面：名字是用户字段（PATH 里的命令名），不是上传文件名。 -->
    <section v-if="canAdmin" class="card">
      <h3>上传工具</h3>
      <p class="muted">
        命令名会成为 PATH 里的名字（如 <code>jq</code>）；同名再传就是替换，旧版本进回收目录。
        正在跑的对话不会中断，下一次调用起用新版本。
      </p>
      <div class="upload-row">
        <input
          ref="nameEl"
          v-model="nameInput"
          type="text"
          placeholder="命令名，如 jq"
          aria-label="命令名"
          @keydown.enter.prevent="submit"
        />
        <button type="button" :disabled="busy" @click="fileEl?.click()">
          {{ file ? file.name : '选文件' }}
        </button>
        <input ref="fileEl" type="file" hidden aria-label="工具文件" @change="pickFile" />
        <button type="button" class="primary" :disabled="busy || !canSubmit" @click="submit">
          {{ busy ? '正在上传…' : '上传' }}
        </button>
      </div>
      <p v-if="nameHint" class="hint warn">{{ nameHint }}</p>
    </section>

    <section class="card">
      <div class="list-head">
        <h3>已上传 <span class="count">{{ tools.length }}</span></h3>
        <input v-model="query" type="search" placeholder="按名字筛选" aria-label="筛选工具" />
      </div>

      <p v-if="phase === 'loading'" class="muted">正在加载…</p>
      <p v-else-if="phase === 'error'" class="muted">列表没拉到，稍后重试。</p>
      <p v-else-if="!filtered.length" class="muted">
        {{ tools.length ? '没有匹配的工具。' : '还没有工具。技能的依赖缺什么，就从这儿传一个上来。' }}
      </p>

      <ul v-else class="tool-list">
        <li v-for="row in filtered" :key="row.name" class="tool" :data-tool="row.name">
          <div class="tool-main">
            <div class="tool-title">
              <code class="name">{{ row.name }}</code>
              <span v-if="!row.exe" class="tag warn" title="文件没有执行位，run 里会 command not found">
                没有执行位
              </span>
            </div>
            <p class="meta">
              <span>{{ fmtBytes(row.bytes) }}</span>
              <span>更新于 {{ fmtWhen(row.updatedAt) }}</span>
              <span v-if="usedBy(row.name).length">被技能引用：{{ usedBy(row.name).join('、') }}</span>
              <span v-else class="muted">没有技能引用</span>
            </p>
            <p class="path"><code>{{ row.path }}</code></p>
          </div>
          <div class="tool-actions">
            <button v-if="canAdmin" type="button" class="danger" @click="pendingDelete = row">删除</button>
          </div>
        </li>
      </ul>
    </section>

    <ConfirmDialog
      v-if="pendingOverwrite"
      title="替换同名工具？"
      :body="`会用它替换现有的「${pendingOverwrite.name}」。正在跑的对话不受影响，下一次调用起用新版本；旧版本留在回收目录里。`"
      confirm-text="替换"
      tone="primary"
      busy-text="正在上传…"
      :busy="busy"
      :error="banner"
      @confirm="doUpload(pendingOverwrite.name, pendingOverwrite.file)"
      @cancel="pendingOverwrite = null"
    />

    <ConfirmDialog
      v-if="pendingDelete"
      title="删除这个工具？"
      :body="`「${pendingDelete.name}」会从团队工具里移除。正在跑的对话不受影响，之后再调用会 command not found；盘上的目录会移进回收目录。`"
      confirm-text="删除"
      :busy="busy"
      :error="banner"
      @confirm="removeConfirmed"
      @cancel="pendingDelete = null"
    />
  </div>
</template>

<style scoped>
.tools {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 28px 32px 40px;
  width: 100%;
  max-width: 53.75rem;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 1rem;
  margin-bottom: 0.4rem;
}
.kicker {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-xs);
  font-weight: 600;
  letter-spacing: 0.1em;
  text-transform: uppercase;
}
h2 {
  margin: 0.15rem 0 0.2rem;
  font-size: var(--text-xl);
  font-weight: 600;
  letter-spacing: -0.02em;
}
h3 { margin: 0; font-size: var(--text-lg); font-weight: 600; }
.hint, .muted {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-sm);
}
.hint.warn { color: var(--warn); }
.card {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  padding: 1.25rem;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.banner {
  margin: 0;
  border-radius: var(--radius-control);
  padding: var(--space-2) var(--space-3);
  font-size: var(--text-sm);
}
.banner.error { color: var(--danger); background: var(--danger-soft); }
.banner.ok { color: var(--accent-ink); background: var(--accent-soft); }
.upload-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.upload-row input[type='text'] { flex: 1; min-width: 12rem; }
input[type='text'], input[type='search'] {
  height: 34px;
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-control);
  padding: 0 var(--space-3);
  background: var(--bg-raised);
  font-size: var(--text-sm);
}
input:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
button {
  height: 32px;
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  border-radius: var(--radius-control);
  padding: 0 var(--space-3);
  font-size: var(--text-sm);
  font-weight: 500;
}
button:hover:not(:disabled) { background: var(--hover); }
button:disabled { opacity: 0.55; }
.primary { border-color: var(--accent); background: var(--accent); color: #fff; }
.primary:hover:not(:disabled) { border-color: var(--accent-ink); background: var(--accent-ink); }
.danger { border-color: var(--danger); background: var(--bg-raised); color: var(--danger); }
.danger:hover:not(:disabled) { background: var(--danger-soft); }
.list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  flex-wrap: wrap;
}
.list-head input { width: min(100%, 16rem); }
.count { color: var(--muted); font-size: var(--text-sm); font-weight: 500; }
.tool-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}
.tool {
  display: flex;
  gap: var(--space-4);
  align-items: flex-start;
  justify-content: space-between;
  padding: var(--space-3) 0;
  border-top: 1px solid var(--line);
}
.tool:first-child { border-top: 0; }
.tool-main { min-width: 0; }
.tool-title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.name {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-base);
  font-weight: 600;
  color: var(--ink);
}
.tag {
  border-radius: 999px;
  padding: 1px 7px;
  font-size: var(--text-xs);
  font-weight: 500;
}
.tag.warn { color: var(--warn); background: var(--warn-soft); }
.meta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  margin: var(--space-1) 0 0;
  color: var(--muted);
  font-size: var(--text-xs);
}
.path {
  margin: 2px 0 0;
  color: var(--muted);
  font-size: var(--text-xs);
  word-break: break-all;
}
.path code { font-size: var(--text-xs); }
.tool-actions { display: flex; gap: var(--space-2); flex-shrink: 0; }
@media (max-width: 720px) {
  .tools { padding: 20px 16px 32px; }
  .page-head { flex-direction: column; align-items: flex-start; }
  .tool { flex-direction: column; }
}
</style>
