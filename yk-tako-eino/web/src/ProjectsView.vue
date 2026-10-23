<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import {
  ApiError,
  createWorkspace,
  deleteSession,
  deleteWorkspace,
  listSessions,
  listWorkspaces,
  renameWorkspace,
  type SessionListItem,
  type Workspace,
} from './api'
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
}

const projects = ref<ProjectRow[]>([])
const loading = ref(true)
const error = ref('')

function rowOf(workspace: Workspace): ProjectRow {
  return { workspace, sessions: null, open: false, loading: false, error: '' }
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
    const sessions = n
      ? `项目里的 ${n} 场对话会一起删除（保留窗口内仍可恢复）。`
      : '项目里还没有对话。'
    return `“${t.project.workspace.name}”会被删除。${sessions}项目目录里的文件会清掉。`
  }
  const title = t.session.title || '未命名对话'
  return `“${title}”的会话档案会被删除，保留窗口内仍可恢复；项目目录里的产出文件不受影响。`
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
      <div>
        <span class="target-label">新对话落点</span>
        <strong>{{ activeName || '默认项目' }}</strong>
      </div>
      <button v-if="props.activeWorkspaceId" type="button" class="text" @click="emit('set-active', null)">
        回到默认项目
      </button>
    </section>

    <form class="create card" @submit.prevent="onCreate">
      <label>
        新建项目
        <input v-model="createName" type="text" placeholder="例如：Q4 调研" autocomplete="off" />
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
              <input v-model="editName" type="text" autocomplete="off" :disabled="editBusy" />
              <button type="submit" class="primary" :disabled="editBusy || !editName.trim()">
                {{ editBusy ? '保存中…' : '保存' }}
              </button>
              <button type="button" class="text" @click="cancelRename">取消</button>
              <p v-if="editError" class="banner error" role="alert">{{ editError }}</p>
            </form>
            <template v-else>
              <strong>{{ p.workspace.name }}</strong>
              <span v-if="p.workspace.id === props.activeWorkspaceId" class="badge">新对话落点</span>
              <span class="meta">创建于 {{ fmtTime(p.workspace.createdAt) }}</span>
            </template>
          </div>
          <div class="actions">
            <button type="button" class="primary" @click="emit('new-chat', p.workspace.id)">开新对话</button>
            <button type="button" @click="toggleSessions(p)">
              {{ p.open ? '收起对话' : (p.sessions ? `对话 ${p.sessions.length}` : '查看对话') }}
            </button>
            <button type="button" @click="startRename(p)">重命名</button>
            <button type="button" class="danger" :disabled="preparingId === p.workspace.id" @click="askDeleteProject(p)">
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
                <span class="meta">{{ fmtTime(s.createdAt) }} · {{ s.id }}</span>
              </div>
              <div class="actions">
                <button type="button" @click="emit('open-session', s.id)">打开</button>
                <button type="button" class="danger" @click="askDeleteSession(p, s)">删除</button>
              </div>
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
  padding: 1.4rem 1.4rem 2.5rem;
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 1rem;
  margin-bottom: 0.9rem;
}
.kicker {
  margin: 0;
  color: var(--accent-ink);
  font-size: 0.75rem;
  letter-spacing: 0.14em;
}
h2 {
  margin: 0.15rem 0 0.2rem;
  font-size: 1.55rem;
  font-weight: 620;
  letter-spacing: -0.03em;
}
.hint {
  margin: 0;
  color: var(--muted);
  font-size: 0.82rem;
}
.card {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 18px;
  padding: 1rem;
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
  border: 1px solid var(--line-strong);
  border-radius: 12px;
  padding: 0.55rem 0.7rem;
  background: #fff;
  font-size: 0.92rem;
}
input:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.12);
}
button {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 999px;
  padding: 0.38rem 0.8rem;
  font-size: 0.8rem;
}
button:hover:not(:disabled) {
  border-color: #cfc6b6;
}
button:disabled {
  opacity: 0.55;
}
.primary {
  border: 0;
  background: var(--ink);
  color: #f6f1e7;
}
.primary:hover:not(:disabled) {
  background: #292524;
}
.danger {
  border-color: #fecdd3;
  background: var(--danger-soft);
  color: var(--danger);
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
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.08);
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
  font-size: 0.74rem;
}
.badge {
  align-self: flex-start;
  color: var(--accent-ink);
  background: var(--accent-soft);
  border-radius: 999px;
  padding: 0.05rem 0.5rem;
  font-size: 0.72rem;
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
