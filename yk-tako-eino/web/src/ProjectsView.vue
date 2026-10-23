<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import {
  ApiError,
  createWorkspace,
  deleteSession,
  deleteWorkspace,
  listSessions,
  listWorkspaces,
  listWorkspaceFiles,
  renameWorkspace,
  workspaceFileUrl,
  type SessionListItem,
  type Workspace,
  type WorkspaceFileEntry,
} from './api'
import { authFetch } from './auth'
import ConfirmDialog from './ConfirmDialog.vue'
import { broadcastMutation, onMutation } from './mutation'

const props = defineProps<{ activeWorkspaceId: string | null }>()

const emit = defineEmits<{
  'open-session': [id: string]
  'new-chat': [workspaceId: string | null]
  'set-active': [workspaceId: string | null]
  'session-deleted': [id: string]
  'workspace-deleted': [payload: { id: string; sessionIds: string[] }]
  'clear-active': []
}>()

type ProjectRow = {
  workspace: Workspace
  // null = 还没拉过（展开时才拉）；[] = 拉过且没有会话。
  sessions: SessionListItem[] | null
  open: boolean
  loading: boolean
  error: string
  // 文件区（doc/48 B1）：null = 没拉过（展开时才拉）。
  files: FileRow | null
  filesOpen: boolean
}

// FileRow 是「当前所在目录」的一份清单。path 相对项目根（"." = 根）。
// preview 记「文件名 → 图片 blob URL」；text 记「文件名 → 文本内容」。
type FileRow = {
  path: string
  entries: WorkspaceFileEntry[]
  loading: boolean
  error: string
  preview: Record<string, string>
  text: Record<string, string>
}

// 能不能「预览」：服务端条目**不带 mime**（mime 是字节出口的事），所以按扩展名近似。
// 判错的后果只是「少一个预览按钮」/「多按一次才提示」，不会出错内容。
// 为什么不看 Content-Type：/api/media 的 mime 表（mediaMimeByExt）只覆盖图片/pdf/音视频，
// .md / .csv / .txt 落到 application/octet-stream，blob.type 判断不出文本。
const IMAGE_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'svg', 'ico'])
const TEXT_EXT = new Set([
  'txt', 'md', 'markdown', 'csv', 'json', 'log', 'yml', 'yaml', 'toml',
  'html', 'css', 'js', 'ts', 'go', 'py', 'sh', 'xml', 'ini', 'conf',
])

function extOf(name: string): string {
  return name.split('.').pop()?.toLowerCase() ?? ''
}

function canPreview(name: string): boolean {
  const ext = extOf(name)
  return IMAGE_EXT.has(ext) || TEXT_EXT.has(ext)
}

const projects = ref<ProjectRow[]>([])
const loading = ref(true)
const error = ref('')

function rowOf(workspace: Workspace): ProjectRow {
  return { workspace, sessions: null, open: false, loading: false, error: '', files: null, filesOpen: false }
}

function errText(e: unknown, fallback: string): string {
  return e instanceof ApiError ? e.message : fallback
}

function fmtTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', { dateStyle: 'medium', timeStyle: 'short' })
}

const activeName = computed(
  () => projects.value.find((p) => p.workspace.id === props.activeWorkspaceId)?.workspace.name ?? '',
)

// 人已经站在默认项目上时，「回到默认项目」是个点了没反应的按钮——藏掉；
// 列表还没加载出来（找不到这一项）时按「不是默认」处理，宁可多给一个出口。
const activeIsDefault = computed(() => {
  const p = projects.value.find((x) => x.workspace.id === props.activeWorkspaceId)
  return !!p?.workspace.isDefault
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await listWorkspaces()
    projects.value = res.workspaces.map(rowOf)
    if (props.activeWorkspaceId && !res.workspaces.some((w) => w.id === props.activeWorkspaceId)) {
      // 当前项目在别处被删了：清掉落点，下一场新对话回默认项目，而不是撞 114001。
      emit('clear-active')
    }
  } catch (e) {
    error.value = errText(e, '项目列表加载失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)

// 变更同步（doc/46 Loop A）：别的窗口删了会话/项目、改了名，本列表当场跟着变，
// 不等人手动点「刷新」；load() 里还自带「落点项目没了就清落点」的自愈。
const offMutation = onMutation((evt) => {
  if (evt.type === 'session_deleted') {
    for (const p of projects.value) {
      if (p.sessions) p.sessions = p.sessions.filter((s) => s.id !== evt.id)
    }
    return
  }
  void load()
})
onUnmounted(offMutation)

const createName = ref('')
const createBusy = ref(false)
const createError = ref('')

async function onCreate() {
  if (createBusy.value || !createName.value.trim()) return
  createBusy.value = true
  createError.value = ''
  try {
    const res = await createWorkspace(createName.value.trim())
    projects.value.push(rowOf(res.workspace))
    createName.value = ''
  } catch (e) {
    createError.value = errText(e, '创建失败')
  } finally {
    createBusy.value = false
  }
}

const editId = ref('')
const editName = ref('')
const editBusy = ref(false)
const editError = ref('')

function startRename(p: ProjectRow) {
  editId.value = p.workspace.id
  editName.value = p.workspace.name
  editError.value = ''
}

function cancelRename() {
  editId.value = ''
}

async function saveRename(p: ProjectRow) {
  if (editBusy.value || !editName.value.trim()) return
  editBusy.value = true
  editError.value = ''
  try {
    const res = await renameWorkspace(p.workspace.id, editName.value.trim())
    p.workspace = res.workspace
    editId.value = ''
    broadcastMutation({ type: 'workspace_renamed', id: p.workspace.id })
  } catch (e) {
    editError.value = errText(e, '改名失败')
  } finally {
    editBusy.value = false
  }
}

async function loadSessions(p: ProjectRow) {
  p.loading = true
  p.error = ''
  try {
    p.sessions = (await listSessions(p.workspace.id)).sessions
  } catch (e) {
    p.error = errText(e, '会话列表加载失败')
  } finally {
    p.loading = false
  }
}

async function toggleSessions(p: ProjectRow) {
  p.open = !p.open
  if (p.open && p.sessions === null) await loadSessions(p)
}

// ── 文件区（doc/48 B1）────────────────────────────────────────────────
//
// agent 写出的文件落在项目目录里。此前用户只能看见聊天里那行文字：看不到目录、
// 拿不到字节。这一区把「列目录」和「取字节」接上：
//   列表 = GET /api/workspaces/{id}/files?path=
//   字节 = GET /api/media?path=workspaces/{wid}/<相对路径>（复用现成出口，不新开下载端点）
//
// 字节出口挂在 authn 上要 Bearer 头，所以**不能**直接塞进 <a href>——浏览器直接导航
// 不带 Authorization（会 401）。取字节一律先 fetch 成 blob，再决定是预览还是下载。

function fmtSize(n: number): string {
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)}MB`
  if (n >= 1024) return `${Math.round(n / 1024)}KB`
  return `${n}B`
}

async function loadFiles(p: ProjectRow, path = '.') {
  // **别写成 `p.files ?? (p.files = {...})`**：赋值表达式的值是**原始对象**（raw），
  // 不是 reactive proxy——后面 `row.entries = …` 就绕过了响应式 set 拦截器，
  // 数据到了、DOM 不更新（列表永远是空的）。分开写，row 才是 proxy。
  if (!p.files) {
    p.files = { path, entries: [], loading: false, error: '', preview: {}, text: {} }
  }
  const row = p.files
  row.loading = true
  row.error = ''
  try {
    const res = await listWorkspaceFiles(p.workspace.id, path)
    row.path = res.path
    row.entries = res.entries
  } catch (e) {
    row.error = errText(e, '文件列表加载失败')
    row.entries = []
  } finally {
    row.loading = false
  }
}

async function toggleFiles(p: ProjectRow) {
  p.filesOpen = !p.filesOpen
  if (p.filesOpen && !p.files) await loadFiles(p, '.')
}

// filePathOf 把「当前目录 + 一个名字」拼成相对项目根的路径。
function filePathOf(p: ProjectRow, name: string): string {
  const base = p.files?.path ?? '.'
  return base === '.' ? name : `${base}/${name}`
}

async function openDir(p: ProjectRow, entry: WorkspaceFileEntry) {
  await loadFiles(p, filePathOf(p, entry.name))
}

// crumbs 是从项目根到当前目录的每一段（根是 "."，没有段）。
function crumbs(p: ProjectRow): string[] {
  const path = p.files?.path ?? '.'
  if (path === '.') return []
  return path.split('/').filter(Boolean)
}

async function gotoCrumb(p: ProjectRow, idx: number) {
  const parts = crumbs(p).slice(0, idx + 1)
  await loadFiles(p, parts.length ? parts.join('/') : '.')
}

// 取字节：一次 fetch 拿 blob，图片内联预览，其他触发下载。
async function fetchBlob(p: ProjectRow, entry: WorkspaceFileEntry): Promise<Blob | null> {
  const row = p.files
  if (!row) return null
  row.error = ''
  const rel = filePathOf(p, entry.name)
  try {
    const res = await authFetch(workspaceFileUrl(p.workspace.id, rel))
    if (!res.ok) {
      row.error = `取不到 ${entry.name}`
      return null
    }
    return await res.blob()
  } catch {
    row.error = `取不到 ${entry.name}`
    return null
  }
}

// 预览：图片出缩略图，文本出内容，其他类型告诉用户去下载。
const TEXT_PREVIEW_MAX = 2000

async function previewFile(p: ProjectRow, entry: WorkspaceFileEntry) {
  const row = p.files
  if (!row) return
  const blob = await fetchBlob(p, entry)
  if (!blob) return
  const ext = extOf(entry.name)
  if (IMAGE_EXT.has(ext)) {
    const url = URL.createObjectURL(blob)
    const old = row.preview[entry.name]
    row.preview[entry.name] = url
    if (old) URL.revokeObjectURL(old)
    return
  }
  if (TEXT_EXT.has(ext)) {
    const text = await blob.text()
    row.text[entry.name] =
      text.length > TEXT_PREVIEW_MAX
        ? `${text.slice(0, TEXT_PREVIEW_MAX)}\n…（还有 ${text.length - TEXT_PREVIEW_MAX} 个字，用「下载」拿全文）`
        : text
    return
  }
  row.error = `${entry.name} 不能直接看，用「下载」拿走。`
}

async function downloadFile(p: ProjectRow, entry: WorkspaceFileEntry) {
  const blob = await fetchBlob(p, entry)
  if (!blob) return
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = entry.name
  document.body.appendChild(a)
  a.click()
  a.remove()
  // 留一会儿再回收：浏览器还在读这个 blob，立刻 revoke 会让下载拿到空文件。
  window.setTimeout(() => URL.revokeObjectURL(url), 10_000)
}

type DeleteTarget =
  | { kind: 'workspace'; project: ProjectRow; sessionIds: string[] }
  | { kind: 'session'; project: ProjectRow; session: SessionListItem }

const target = ref<DeleteTarget | null>(null)
const confirmBusy = ref(false)
const confirmError = ref('')
const preparingId = ref('')

async function askDeleteProject(p: ProjectRow) {
  confirmError.value = ''
  // 总是重拉，不用缓存快照：级联文案和「当前会话是否被连带删掉」的判断都必须用最新列表
  // （另一个 tab 可能刚在这个项目里建/删了会话）。
  preparingId.value = p.workspace.id
  await loadSessions(p)
  preparingId.value = ''
  if (p.error) return
  target.value = {
    kind: 'workspace',
    project: p,
    sessionIds: (p.sessions ?? []).map((s) => s.id),
  }
}

function askDeleteSession(p: ProjectRow, session: SessionListItem) {
  confirmError.value = ''
  target.value = { kind: 'session', project: p, session }
}

const dialogTitle = computed(() => {
  if (!target.value) return ''
  return target.value.kind === 'workspace' ? '删除项目？' : '删除这场对话？'
})

const dialogBody = computed(() => {
  const t = target.value
  if (!t) return ''
  if (t.kind === 'workspace') {
    const n = t.sessionIds.length
    // 不写死天数：保留窗口由服务端的 -archive-ttl 决定（默认 168h = 7 天，可配）。
    // 用户真正需要知道的是「删除 ≠ 立刻消失」——那正是他不敢点删除的原因。
    const sessions = n
      ? `项目里的 ${n} 场对话会一起删除（之后一段时间内还能找回，但会被自动清理）。`
      : '项目里还没有对话。'
    return `“${t.project.workspace.name}”会被删除。${sessions}项目目录里的文件会清掉。`
  }
  const title = t.session.title || '未命名对话'
  return `“${title}”的会话档案会被删除（之后一段时间内还能找回，但会被自动清理）；项目目录里的产出文件不受影响。`
})

async function confirmDelete() {
  const t = target.value
  if (!t || confirmBusy.value) return
  confirmBusy.value = true
  confirmError.value = ''
  try {
    if (t.kind === 'workspace') {
      await deleteWorkspace(t.project.workspace.id)
      broadcastMutation({ type: 'workspace_deleted', id: t.project.workspace.id })
      projects.value = projects.value.filter((x) => x !== t.project)
      emit('workspace-deleted', {
        id: t.project.workspace.id,
        sessionIds: t.sessionIds,
      })
    } else {
      await deleteSession(t.session.id)
      broadcastMutation({ type: 'session_deleted', id: t.session.id })
      if (t.project.sessions) {
        t.project.sessions = t.project.sessions.filter((s) => s.id !== t.session.id)
      }
      emit('session-deleted', t.session.id)
    }
    target.value = null
  } catch (e) {
    // 默认项目 114005、有活 run 114006/114007 都把服务端文案原样留在弹窗里。
    confirmError.value = errText(e, '删除失败')
  } finally {
    confirmBusy.value = false
  }
}

function closeDialog() {
  if (confirmBusy.value) return
  target.value = null
  confirmError.value = ''
}
</script>

<template>
  <div class="projects">
    <header class="page-head">
      <div>
        <p class="kicker">项目</p>
        <h2>我的项目</h2>
        <p class="hint">项目里的多场对话共用同一个目录。项目只有你能看见。</p>
      </div>
      <button type="button" class="refresh" :disabled="loading" @click="load">刷新</button>
    </header>

    <section class="target card">
      <!-- 「默认进入」读起来像一项配置，实际是「上一次待在哪」：新对话跟着当前落点走。 -->
      <div>
        <span class="target-label">新对话进入</span>
        <strong>{{ activeName || '默认' }}</strong>
      </div>
      <button
        v-if="props.activeWorkspaceId && !activeIsDefault"
        type="button"
        class="text"
        @click="emit('set-active', null)"
      >
        回到默认项目
      </button>
    </section>

    <form class="create card" @submit.prevent="onCreate">
      <label>
        新建项目
        <input v-model="createName" type="text" maxlength="100" placeholder="例如：Q4 调研" autocomplete="off" />
      </label>
      <button type="submit" class="primary" :disabled="createBusy || !createName.trim()">
        {{ createBusy ? '正在创建…' : '创建' }}
      </button>
      <p v-if="createError" class="banner error" role="alert">{{ createError }}</p>
    </form>

    <p v-if="error" class="banner error" role="alert">
      {{ error }}
      <button type="button" class="text" @click="load">重试</button>
    </p>
    <p v-if="loading" class="placeholder">正在加载项目…</p>
    <p v-else-if="!projects.length && !error" class="placeholder">
      还没有项目。新建一个，或者直接在默认项目里开始对话。
    </p>

    <ul v-else class="project-list">
      <li v-for="p in projects" :key="p.workspace.id" class="project card" :class="{ active: p.workspace.id === props.activeWorkspaceId }">
        <div class="project-head">
          <div class="project-title">
            <form v-if="editId === p.workspace.id" class="rename" @submit.prevent="saveRename(p)">
              <input v-model="editName" type="text" maxlength="100" autocomplete="off" :disabled="editBusy" />
              <button type="submit" class="primary" :disabled="editBusy || !editName.trim()">
                {{ editBusy ? '保存中…' : '保存' }}
              </button>
              <button type="button" class="text" @click="cancelRename">取消</button>
              <p v-if="editError" class="banner error" role="alert">{{ editError }}</p>
            </form>
            <template v-else>
              <strong>{{ p.workspace.name }}</strong>
              <span v-if="p.workspace.isDefault" class="badge default">默认</span>
              <span v-if="p.workspace.id === props.activeWorkspaceId" class="badge">默认进入</span>
              <span class="meta">创建于 {{ fmtTime(p.workspace.createdAt) }}</span>
            </template>
          </div>
          <div class="actions">
            <button type="button" class="primary" @click="emit('new-chat', p.workspace.id)">开新对话</button>
            <button type="button" @click="toggleSessions(p)">
              {{ p.open ? '收起对话' : (p.sessions ? `对话 ${p.sessions.length}` : '查看对话') }}
            </button>
            <button type="button" @click="toggleFiles(p)">
              {{ p.filesOpen ? '收起文件' : '文件' }}
            </button>
            <!-- 默认项目不可删、也不该改名（服务端 409 114005）：藏掉这两个按钮，
                 别让用户点一个必然失败的按钮、还先许诺「文件会清掉」。 -->
            <button v-if="!p.workspace.isDefault" type="button" @click="startRename(p)">重命名</button>
            <button
              v-if="!p.workspace.isDefault"
              type="button"
              class="danger"
              :disabled="preparingId === p.workspace.id"
              @click="askDeleteProject(p)"
            >
              {{ preparingId === p.workspace.id ? '正在读取…' : '删除' }}
            </button>
          </div>
        </div>

        <p v-if="p.error" class="banner error" role="alert">
          {{ p.error }}
          <button type="button" class="text" @click="loadSessions(p)">重试</button>
        </p>

        <div v-if="p.open" class="sessions">
          <p v-if="p.loading" class="placeholder">正在加载对话…</p>
          <p v-else-if="!p.sessions?.length" class="placeholder">这个项目里还没有对话。</p>
          <ul v-else>
            <li v-for="s in p.sessions" :key="s.id" class="session">
              <div class="session-main">
                <strong>{{ s.title || '未命名对话' }}</strong>
                <!-- 内部 session id 不给用户看（doc/48 D3）：它读不出任何含义，
                     却比时间更显眼。要查就放进 title 属性。 -->
                <span class="meta" :title="s.id">{{ fmtTime(s.createdAt) }}</span>
              </div>
              <div class="actions">
                <button type="button" @click="emit('open-session', s.id)">打开</button>
                <button type="button" class="danger" @click="askDeleteSession(p, s)">删除</button>
              </div>
            </li>
          </ul>
        </div>

        <div v-if="p.filesOpen" class="files">
          <div class="files-bar">
            <nav class="crumbs" aria-label="目录位置">
              <button type="button" class="crumb" @click="gotoCrumb(p, -1)">项目根</button>
              <template v-for="(c, i) in crumbs(p)" :key="c + i">
                <span class="crumb-sep" aria-hidden="true">/</span>
                <button type="button" class="crumb" @click="gotoCrumb(p, i)">{{ c }}</button>
              </template>
            </nav>
            <span v-if="p.files?.loading" class="muted">正在加载…</span>
          </div>

          <p v-if="p.files?.error" class="banner error" role="alert">{{ p.files.error }}</p>
          <p v-else-if="p.files && !p.files.entries.length && !p.files.loading" class="placeholder">
            这个目录是空的。
          </p>
          <ul v-else-if="p.files" class="file-list">
            <li v-for="e in p.files.entries" :key="e.name" class="file">
              <div class="file-main">
                <button
                  v-if="e.kind === 'dir'"
                  type="button"
                  class="file-name dir"
                  @click="openDir(p, e)"
                >{{ e.name }}/</button>
                <span v-else class="file-name">{{ e.name }}</span>
                <span class="meta">{{ e.kind === 'dir' ? '目录' : fmtSize(e.size ?? 0) }}</span>
                <span v-if="e.modifiedAt" class="meta">{{ fmtTime(e.modifiedAt) }}</span>
              </div>
              <div v-if="e.kind === 'file'" class="actions">
                <button v-if="canPreview(e.name)" type="button" @click="previewFile(p, e)">预览</button>
                <button type="button" @click="downloadFile(p, e)">下载</button>
              </div>
              <img
                v-if="p.files?.preview[e.name]"
                class="file-preview"
                :src="p.files.preview[e.name]"
                :alt="e.name"
              />
              <pre v-if="p.files?.text[e.name]" class="file-text">{{ p.files.text[e.name] }}</pre>
            </li>
          </ul>
        </div>
      </li>
    </ul>

    <ConfirmDialog
      v-if="target"
      :title="dialogTitle"
      :body="dialogBody"
      :busy="confirmBusy"
      :error="confirmError"
      @confirm="confirmDelete"
      @cancel="closeDialog"
    />
  </div>
</template>

<style scoped>
.projects {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 28px 32px 40px;
  /* 宽屏限宽（doc/48 D1）：内容拉满 1400px+ 时输入框宽得离谱、中间全是空白。
     **width + max-width 都要**：父级是 column 方向的 flex（.stage），
     margin:auto 会吃掉交叉轴剩余空间、让本项按内容宽度收缩——
     只写 max-width 时，内容少的页面（如成员页）会自动缩成半宽。 */
  width: 100%;
  max-width: 53.75rem;
  margin: 0 auto;
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 1rem;
  margin-bottom: 1.2rem;
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
.hint {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-sm);
}
.card {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  padding: 1.25rem;
}
.target {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.8rem;
  margin-bottom: 0.75rem;
}
.target-label {
  display: block;
  color: var(--muted);
  font-size: 0.75rem;
}
.target strong {
  font-size: 0.95rem;
}
.create {
  display: flex;
  align-items: flex-end;
  gap: 0.6rem;
  flex-wrap: wrap;
  margin-bottom: 0.9rem;
}
.create label {
  flex: 1;
  min-width: 14rem;
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  font-size: 0.78rem;
  color: var(--ink-soft);
}
input {
  width: 100%;
  height: 36px;
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-control);
  padding: 0 0.7rem;
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
  padding: 0 0.8rem;
  font-size: var(--text-sm);
  font-weight: 500;
}
button:hover:not(:disabled) {
  background: var(--hover);
}
button:disabled {
  opacity: 0.55;
}
.primary {
  border: 0;
  background: var(--accent);
  color: #fff;
}
.primary:hover:not(:disabled) {
  background: var(--accent-ink);
}
.danger {
  border-color: var(--danger);
  background: var(--bg-raised);
  color: var(--danger);
}
.danger:hover:not(:disabled) {
  background: var(--danger-soft);
}
.text {
  border: 0;
  background: transparent;
  color: var(--muted);
  padding-inline: 0.4rem;
}
.refresh {
  flex: none;
}
.project-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.7rem;
}
.project.active {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
.project-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 0.8rem;
  flex-wrap: wrap;
}
.project-title {
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
  min-width: 12rem;
}
.project-title strong {
  font-size: 1rem;
}
.meta {
  color: var(--muted);
  font-size: var(--text-xs);
}
.badge {
  align-self: flex-start;
  color: var(--accent-ink);
  background: var(--accent-soft);
  border-radius: 999px;
  padding: 0.05rem 0.5rem;
  font-size: 0.72rem;
}
/* 「默认项目」与「默认进入」是两个意思，用底色分开（doc/48 C1/C4）。 */
.badge.default {
  color: var(--ink-soft);
  background: var(--bg);
  border: 1px solid var(--line-strong);
}
.actions {
  display: flex;
  gap: 0.4rem;
  flex-wrap: wrap;
  align-items: center;
}
.rename {
  display: flex;
  gap: 0.4rem;
  align-items: center;
  flex-wrap: wrap;
}
.rename input {
  width: 14rem;
}
.sessions {
  margin-top: 0.8rem;
  border-top: 1px solid var(--line);
  padding-top: 0.7rem;
}
.sessions ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}
.session {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.7rem;
  padding: 0.45rem 0.55rem;
  border-radius: 12px;
  background: var(--bg);
}
.session-main {
  display: flex;
  flex-direction: column;
  gap: 0.1rem;
  min-width: 0;
}
/* 文件区（doc/48 B1） */
.files {
  margin-top: 0.8rem;
  border-top: 1px solid var(--line);
  padding-top: 0.7rem;
}
.files-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.6rem;
  flex-wrap: wrap;
  margin-bottom: 0.45rem;
}
.crumbs {
  display: flex;
  align-items: center;
  gap: 0.15rem;
  flex-wrap: wrap;
}
.crumb {
  border: 0;
  background: transparent;
  padding: 0.1rem 0.25rem;
  font-size: 0.78rem;
  color: var(--accent-ink);
}
.crumb:hover {
  text-decoration: underline;
}
.crumb-sep {
  color: var(--muted);
  font-size: 0.78rem;
}
.file-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}
.file {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.7rem;
  padding: 0.4rem 0.5rem;
  border-radius: 12px;
  background: var(--bg);
  flex-wrap: wrap;
}
.file-main {
  display: flex;
  align-items: baseline;
  gap: 0.45rem;
  min-width: 0;
  flex-wrap: wrap;
}
.file-name {
  font-size: 0.86rem;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 26rem;
}
button.file-name {
  border: 0;
  background: transparent;
  padding: 0;
  color: var(--accent-ink);
  text-align: left;
}
button.file-name:hover {
  text-decoration: underline;
}
.file-preview {
  flex-basis: 100%;
  max-width: 22rem;
  max-height: 12rem;
  object-fit: contain;
  border: 1px solid var(--line);
  border-radius: 10px;
  background: #fff;
}
.file-text {
  flex-basis: 100%;
  max-height: 14rem;
  overflow: auto;
  margin: 0.2rem 0 0;
  padding: 0.55rem 0.65rem;
  border: 1px solid var(--line);
  border-radius: 10px;
  background: #fff;
  font-size: 0.76rem;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
}
.session-main strong {
  font-size: 0.86rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.placeholder {
  margin: 0.4rem 0;
  color: var(--muted);
  font-size: 0.82rem;
}
.banner {
  margin: 0.4rem 0 0;
  font-size: 0.8rem;
  line-height: 1.5;
  border-radius: 12px;
  padding: 0.5rem 0.65rem;
}
.banner.error {
  color: var(--danger);
  background: var(--danger-soft);
}
@media (max-width: 860px) {
  .projects {
    padding: 1rem 0.8rem 2rem;
  }
  .project-head {
    flex-direction: column;
  }
}
</style>
