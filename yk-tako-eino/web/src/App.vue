<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useChat, type AskQuestion, type Bubble, type ToolCard } from './chat'
import {
  ApiError,
  getSessionSubagents,
  listSessions,
  listWorkspaces,
  mediaUrl,
  stopRun,
  type MediaRef,
  type RunID,
  type SessionListItem,
  type SkillPreviewSource,
  type SubagentItem,
  type Usage,
  type Workspace,
} from './api'
import { renderSafe } from './markdown'
import Configure from './Configure.vue'
import InviteLanding from './InviteLanding.vue'
import PlanDoc from './PlanDoc.vue'
import PendingInvites from './PendingInvites.vue'
import ProjectsView from './ProjectsView.vue'
import SkillInstallCard from './SkillInstallCard.vue'
import SkillsView from './SkillsView.vue'
import ToolsView from './ToolsView.vue'
import { authExpired, authFetch, authState, clearAuth, loginWithPassword, switchTenant, tenantState } from './auth'
import { onMutation, ensureEventStream, closeEventStream } from './mutation'

type Page = 'chat' | 'projects' | 'skills' | 'tools' | 'agents' | 'team'

function readPage(): Page {
  const saved = sessionStorage.getItem('tako.page')
  return saved === 'projects' ||
    saved === 'skills' ||
    saved === 'tools' ||
    saved === 'agents' ||
    saved === 'team'
    ? saved
    : 'chat'
}

function tokenFromLocation(): string {
  const prefix = '/invite/'
  if (!location.pathname.startsWith(prefix)) return ''
  return decodeURIComponent(location.pathname.slice(prefix.length).replace(/\/+$/, ''))
}

const inviteToken = ref(tokenFromLocation())
function leaveInvite() {
  history.replaceState(null, '', '/')
  inviteToken.value = ''
}

const page = ref<Page>(readPage())
watch(page, (value) => sessionStorage.setItem('tako.page', value))
const configPage = computed<'agents' | 'team'>(() => (page.value === 'team' ? 'team' : 'agents'))

const {
  sessionId, bubbles, draft, attachments, pending, loadingOlder, atStart, busy,
  activeWorkspaceId, sessionGone,
  send, reconcile, loadOlder, newSession, showTenantSession, resumeRun,
  setActiveWorkspace, openSession, closeOnDeleted, answerQuestion,
} = useChat()

// —— 等人面板（doc/47 §5.2，2026-10-23 按 kimi 交互重做）——
// 待答记录可能不止一条（驳回方案后模型会重新提交；并行 ask_user 也会有多条），
// 面板按 pending 排序展示，默认停在最新一条。
// 只算「这一轮还活着」的待答：run 被别的端停掉/崩掉后，行里的 status 仍是 pending（没人去改它），
// 只看 q.status 会让面板永远顶着输入框。气泡已经收口（stopped/error/done）就说明这段等待已经作废。
const pendingAsks = computed(() => {
  const out: { bubble: Bubble; q: AskQuestion }[] = []
  for (const b of bubbles.value) {
    if (b.state !== 'streaming' && b.state !== 'offline') continue
    for (const q of b.questions ?? []) {
      if (q.status === 'pending') out.push({ bubble: b, q })
    }
  }
  return out
})
const askIndex = ref(0)
const ask = computed(() => pendingAsks.value[Math.min(askIndex.value, pendingAsks.value.length - 1)] ?? null)
// 面板的草稿/选中/编辑态只属于当前这一条：换条就清空（对话数据里不存 UI 状态）。
const askPick = ref(-1)
const askDraft = ref('')
const askMode = ref<'answer' | 'edit' | 'revise'>('answer')
const askBusy = ref(false)
const askEl = ref<HTMLElement | null>(null)
const askTextEl = ref<HTMLTextAreaElement | null>(null)

watch(
  () => pendingAsks.value.length,
  (n, prev) => {
    if (n > prev) askIndex.value = n - 1
    else if (askIndex.value >= n) askIndex.value = Math.max(0, n - 1)
    if (n > prev) {
      void nextTick(() => {
        scrollBottom()
        askEl.value?.focus()
      })
    }
  },
)
watch(
  () => ask.value?.q.id ?? '',
  (id) => {
    askPick.value = -1
    askDraft.value = ''
    askMode.value = 'answer'
    void nextTick(() => {
      if (id) askEl.value?.focus()
      else composerEl.value?.focus()
    })
  },
)

function moveAsk(delta: number) {
  const n = pendingAsks.value.length
  if (!n) return
  askIndex.value = Math.min(Math.max(askIndex.value + delta, 0), n - 1)
}

const askAnswerReady = computed(() => {
  const t = ask.value
  if (!t) return false
  if (t.q.kind === 'plan') return true
  return askPick.value >= 0 || askDraft.value.trim() !== ''
})

function pickOption(i: number) {
  askPick.value = i
  askDraft.value = ''
}

function focusOther() {
  askPick.value = -1
  void nextTick(() => askTextEl.value?.focus())
}

function startPlanEdit() {
  askMode.value = 'edit'
  askDraft.value = ask.value?.q.text ?? ''
  void nextTick(() => askTextEl.value?.focus())
}

function startPlanRevise() {
  askMode.value = 'revise'
  askDraft.value = ''
  void nextTick(() => askTextEl.value?.focus())
}

async function submitAsk() {
  const target = ask.value
  if (!target || askBusy.value) return
  const q = target.q
  // 安装确认卡只有两个按钮，没有输入框可提交——按键路径也别把它当问题处理。
  if (q.kind === 'install') return
  askBusy.value = true
  try {
    if (q.kind === 'plan') {
      if (askMode.value === 'revise') {
        await answerQuestion(q, askDraft.value.trim(), 'revise')
      } else if (askMode.value === 'edit') {
        const edited = askDraft.value.trim()
        if (!edited) return
        await answerQuestion(q, edited === q.text.trim() ? '' : edited, 'approve')
      } else {
        await answerQuestion(q, '', 'approve')
      }
      return
    }
    const answer = askPick.value >= 0 ? q.options[askPick.value] ?? '' : askDraft.value.trim()
    if (!answer) return
    await answerQuestion(q, answer)
  } finally {
    askBusy.value = false
  }
}

// 安装确认的点击权与技能页同一把尺：租户 owner/admin（端点也再核一次，403 111203）。
const canApproveSkills = computed(() => {
  const role = tenantState.value.role
  return role === 'owner' || role === 'admin'
})

// 「此来源在本会话中未出现」是前端对**用户气泡文本**的核对：提议只存在模型侧的 args 里，
// 没有服务端字段记录「用户原话里提过它」——用户说过 owner/repo 或链接就算出现过。
// 没提过的来源正是提示注入最可能的样子（从 PDF/网页里读来一条安装串），所以卡片要顶一行提醒。
function sourceSeenInSession(source?: SkillPreviewSource): boolean {
  if (!source) return false
  const needles = [
    source.owner && source.repo ? `${source.owner}/${source.repo}` : '',
    source.namespace && source.repo ? `${source.namespace}/${source.repo}` : '',
    source.url,
  ]
    .filter((s) => s && !s.startsWith('multipart:'))
    .map((s) => s.toLowerCase())
  if (!needles.length) return false
  return bubbles.value.some(
    (b) => b.role === 'user' && needles.some((n) => b.text.toLowerCase().includes(n)),
  )
}

// 安装卡的两个按钮：同一个 answer 端点，decision=approve/cancel。
// 结果由端点在 CAS 后写回 answer 列/card（res.answer），不需要再拉一次列表。
async function answerInstall(decision: 'approve' | 'cancel') {
  const target = ask.value
  if (!target || askBusy.value || !canApproveSkills.value) return
  askBusy.value = true
  try {
    await answerQuestion(target.q, '', decision)
  } finally {
    askBusy.value = false
  }
}

// 数字键选选项、Enter 提交（不抢输入框的按键——输入框自己的 keydown 负责）。
function onAskKeydown(ev: KeyboardEvent) {
  const target = ask.value
  if (!target || ev.isComposing || ev.keyCode === 229) return
  const el = ev.target as HTMLElement | null
  if (el?.tagName === 'INPUT' || el?.tagName === 'TEXTAREA') return
  if (ev.key === 'Enter' && askAnswerReady.value) {
    ev.preventDefault()
    void submitAsk()
    return
  }
  if (target.q.kind !== 'question') return
  if (!/^[1-9]$/.test(ev.key)) return
  const i = Number(ev.key) - 1
  if (i < target.q.options.length) {
    ev.preventDefault()
    pickOption(i)
  }
}

// 面板输入框的 Enter 提交（中文输入法选词的 Enter 只是上屏）。
function onAskTextKeydown(ev: KeyboardEvent) {
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing || ev.keyCode === 229) return
  ev.preventDefault()
  void submitAsk()
}

// —— 技能工具卡（doc/52 C5）——
// 卡片只认 read_skill / use_skill / read_skill_file（chat.ts 的 SKILL_TOOL_NAMES 已筛过）。
// 措辞按工具名分（§2.3：读与用靠工具名区分，不靠返回内容），read_skill_file 单独一行。
function toolArgsOf(t: ToolCard): { name?: string; path?: string } {
  try {
    const parsed = JSON.parse(t.args) as { name?: string; path?: string }
    return parsed && typeof parsed === 'object' ? parsed : {}
  } catch {
    return {}
  }
}

function toolCardLabel(t: ToolCard): string {
  const a = toolArgsOf(t)
  const name = a.name || '（未知技能）'
  if (t.name === 'use_skill') return `使用技能：${name}`
  if (t.name === 'read_skill_file') return `查看技能文件：${name}${a.path ? ` / ${a.path}` : ''}`
  return `查看技能：${name}`
}

// 失败回执是 JSON 信封（116001/116107/116114/116115/116116）；成功回执以 <skill 开头。
// 卡片把 message 摆出来，code 不进正文——人只需要知道「为什么没读到」。
function toolCardFail(t: ToolCard): string {
  if (!t.output || t.output.startsWith('<skill')) return ''
  try {
    const parsed = JSON.parse(t.output) as { code?: unknown; message?: unknown }
    if (typeof parsed.code === 'number' && parsed.code !== 0) {
      return typeof parsed.message === 'string' && parsed.message
        ? parsed.message
        : `技能工具失败（${parsed.code}）`
    }
    return ''
  } catch {
    // 不是信封也不是 <skill：多半是工具层错误文本，按失败展示原文。
    return t.output
  }
}

function toolCardState(t: ToolCard, bubbleState: string): 'running' | 'failed' | 'done' {
  if (toolCardFail(t)) return 'failed'
  if (t.output === undefined) {
    return bubbleState === 'streaming' || bubbleState === 'offline' ? 'running' : 'done'
  }
  return 'done'
}

function askStatusLabel(q: AskQuestion): string {
  if (q.status === 'stale') return '已失效'
  if (q.status === 'pending') {
    if (q.kind === 'plan') return '待批准'
    if (q.kind === 'install') return '待确认'
    return '待回答'
  }
  if (q.kind === 'plan') return q.decision === 'revise' ? '已驳回' : '已批准'
  // 安装行的结果文本由程序写（可能成功也可能过期/失败），标签只能说「已处理」；
  // 取消是例外——decision 就是人的那个动作。
  if (q.kind === 'install') return q.decision === 'cancel' ? '已取消' : '已处理'
  return '已回答'
}

// 看板是 run 的实时进度（doc/41 §5.2），按 kimi 的 TodoPanel 挂在输入区上方：
// 取对话里最后一条看板帧；完成数/总数与折叠都在前端算。
const board = computed(() => {
  for (let i = bubbles.value.length - 1; i >= 0; i--) {
    const text = bubbles.value[i].board
    if (text) return boardRows(text)
  }
  return []
})
const todoRows = computed(() => board.value.filter((r) => r.status !== 'deleted'))
const todoDone = computed(() => todoRows.value.filter((r) => r.status === 'completed').length)
const todoOpen = ref(true)
const todoExpanded = ref(false)
const TODO_VISIBLE = 5
const todoShown = computed(() =>
  todoExpanded.value ? todoRows.value : todoRows.value.slice(0, TODO_VISIBLE),
)
const todoHidden = computed(() => todoRows.value.length - todoShown.value.length)
const todoPercent = computed(() =>
  todoRows.value.length ? Math.round((todoDone.value / todoRows.value.length) * 100) : 0,
)

function todoIcon(status: string): string {
  if (status === 'completed') return '✓'
  if (status === 'in_progress') return '●'
  return '○'
}

function todoLabel(status: string): string {
  if (status === 'completed') return '已完成'
  if (status === 'in_progress') return '进行中'
  if (status === 'pending') return '待办'
  return status
}

// 变更同步（doc/46 Loop A）：别的窗口删了会话/项目——本地当场失效（气泡作废 / 落点清回默认），
// 失效永远跟重拉，事件本身不携带状态。
onMutation((evt) => {
  if (evt.type === 'session_deleted') {
    // 侧栏只在切会话/收尾时重拉，剪掉这一条否则死链一直躺着（点进去是空对话 + 404）。
    removeSessionFromTree(evt.id)
    closeOnDeleted(evt.id)
  } else if (evt.type === 'workspace_deleted' && activeWorkspaceId.value === evt.id) {
    setActiveWorkspace(null)
  }
})

// 服务端变更事件流（doc/46 B3）：登录后挂 EventSource，租户切换换一条流，退出关掉。
// 与 BroadcastChannel 并存——后者是同机快路径，前者是跨机跨端的正路，两源同一份失效逻辑。
watch(authState, (on) => {
  if (on) ensureEventStream()
  else closeEventStream()
}, { immediate: true })
watch(
  () => tenantState.value.id,
  () => {
    if (!authState.value) return
    closeEventStream()
    ensureEventStream()
  },
)

const listEl = ref<HTMLElement | null>(null)
const composerEl = ref<HTMLTextAreaElement | null>(null)
const pinned = ref(true)
const fileEl = ref<HTMLInputElement | null>(null)
const askNew = ref(false)
const copiedKey = ref('')
const lightbox = ref('')
const lightboxZoom = ref(false)
// 换图/关灯箱都要回到适应窗口，否则上一张的放大态会带过来。
watch(lightbox, () => {
  lightboxZoom.value = false
})
const showPassword = ref(false)

const loggedIn = computed(() => authState.value)
const tenant = computed(() => tenantState.value)
const initial = computed(() => (tenant.value.name || '我').slice(0, 1))
const email = ref('')
const password = ref('')
const authBusy = ref(false)
const authError = ref('')

const starters = [
  '这个工作区里现在有哪些文件？',
  '用三句话说说你能帮我做什么',
  '把我下一条消息整理成待办清单',
]

function roleLabel(role: string): string {
  if (role === 'owner') return '所有者'
  if (role === 'admin') return '管理员'
  if (role === 'member') return '成员'
  if (role === 'viewer') return '查看者'
  return role
}

function kindLabel(kind: string): string {
  // 「个人」→「我的」：租户是**用户自己的那一个**（注册时自动建），说「个人」像在说类别。
  if (kind === 'personal') return '我的'
  if (kind === 'team') return '团队'
  return kind
}

type TenantItem = { id: string; name: string; kind: string; role: string; status: string }

const switcherEl = ref<HTMLElement | null>(null)
const menuOpen = ref(false)
// 窄屏抽屉（doc/51 §5）：桌面端 .rail-body 是常驻的 display:contents，
// ≤860px 时同一份 DOM 变成抽屉，导航、树、账户的状态都不复制。
const isMobile = ref(false)
const drawerOpen = ref(false)
const drawerEl = ref<HTMLElement | null>(null)
const menuBtnEl = ref<HTMLButtonElement | null>(null)
let mobileMq: MediaQueryList | null = null
const tenants = ref<TenantItem[]>([])
const switching = ref(false)
const switchError = ref('')
const createOpen = ref(false)
const createName = ref('')
const createError = ref('')
const createBusy = ref(false)

async function loadTenants() {
  const res = await authFetch('/api/tenants')
  const body = (await res.json().catch(() => null)) as {
    code?: number
    message?: string
    data?: { tenants?: TenantItem[] }
  } | null
  if (!res.ok || !body || body.code !== 0) {
    switchError.value = body?.message || '工作区列表加载失败'
    return
  }
  tenants.value = body.data?.tenants ?? []
}

// 侧栏（doc/48 A2 → doc/50）：数据是「项目 → 对话」两层。
//
// 数据 = GET /api/workspaces + GET /api/sessions（**不带 workspaceId**，服务端返回本人全部
// 会话，每行自带 workspaceId）——按 workspaceId 挂到项目下，零后端改动。
//
// 两条实测约束（doc/50 §2）：① 默认项目不保证排第一（服务端按 created_at 升序，本账号
// 「验收项目」比「默认」早 107 ms）→ 客户端把 isDefault 排前；② 存量会话的 workspaceId
// 可能是 null → 只在存在时多出一组「未归项目」。
type SidebarGroup = { workspace: Workspace; sessions: SessionListItem[] }

// A3 起模板渲染这棵树。loose（未归项目）不是真的 Workspace，用 key 区分，模板只认
// name / isDefault / sessions。
type TreeGroup = { key: string; name: string; isDefault: boolean; sessions: SessionListItem[] }

const sidebarGroups = ref<SidebarGroup[]>([])
const looseSessions = ref<SessionListItem[]>([])
const recentLoading = ref(false)
const recentError = ref('')

// 只记**被收起**的项目：首屏默认全展开，不需要初始化。
const collapsed = ref<Set<string>>(new Set())

const treeGroups = computed<TreeGroup[]>(() => {
  const groups: TreeGroup[] = sidebarGroups.value.map((g) => ({
    key: g.workspace.id,
    name: g.workspace.name,
    isDefault: !!g.workspace.isDefault,
    sessions: g.sessions,
  }))
  if (looseSessions.value.length) {
    groups.push({ key: 'loose', name: '未归项目', isDefault: false, sessions: looseSessions.value })
  }
  return groups
})

function toggleProject(key: string) {
  const next = new Set(collapsed.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  collapsed.value = next
}

const byCreatedDesc = (a: SessionListItem, b: SessionListItem) =>
  a.createdAt < b.createdAt ? 1 : a.createdAt > b.createdAt ? -1 : 0

// 整栏 10 条上限（doc/50 §3）：所有会话先排成一条全局时间倒序列表，取前 N 条，
// 分组只渲染这 N 条里属于它的——「展示更多」+10，「收起」回到 10。
const visibleCount = ref(10)
const allSessionsDesc = computed(() => treeGroups.value.flatMap((g) => g.sessions).sort(byCreatedDesc))
const visibleIds = computed(
  () => new Set(allSessionsDesc.value.slice(0, visibleCount.value).map((s) => s.id)),
)
const truncated = computed(() => allSessionsDesc.value.length > visibleCount.value)

// 项目内再限 10 条（用户 2026-10-24 定）：项目自己的「展示全部」只放开它自己，
// 别的项目仍受整栏上限——「收起」把各自放开的部分也一并收回。
const PER_PROJECT_LIMIT = 10
const expandedProjects = ref<Set<string>>(new Set())

// 被截光的分组不应渲染出一条空 <ul>（那会凭空多一段竖线）。
function visibleSessions(g: TreeGroup): SessionListItem[] {
  if (expandedProjects.value.has(g.key)) return g.sessions
  return g.sessions.filter((s) => visibleIds.value.has(s.id)).slice(0, PER_PROJECT_LIMIT)
}

function projectHasMore(g: TreeGroup): boolean {
  return !expandedProjects.value.has(g.key) && g.sessions.length > visibleSessions(g).length
}

function toggleProjectAll(g: TreeGroup) {
  const next = new Set(expandedProjects.value)
  if (next.has(g.key)) next.delete(g.key)
  else next.add(g.key)
  expandedProjects.value = next
}

function collapseAll() {
  visibleCount.value = 10
  expandedProjects.value = new Set()
}

function removeSessionFromTree(id: string) {
  for (const g of sidebarGroups.value) g.sessions = g.sessions.filter((s) => s.id !== id)
  looseSessions.value = looseSessions.value.filter((s) => s.id !== id)
}

async function loadSidebarTree() {
  if (!loggedIn.value) {
    sidebarGroups.value = []
    looseSessions.value = []
    return
  }
  recentLoading.value = true
  recentError.value = ''
  try {
    // 一起等：一条成功一条失败时宁可不显示，也不显示半棵树。
    const [wsRes, sessRes] = await Promise.all([listWorkspaces(), listSessions()])
    const byWorkspace = new Map<string, SessionListItem[]>()
    const loose: SessionListItem[] = []
    for (const s of [...sessRes.sessions].sort(byCreatedDesc)) {
      if (!s.workspaceId) {
        loose.push(s)
        continue
      }
      const bucket = byWorkspace.get(s.workspaceId)
      if (bucket) bucket.push(s)
      else byWorkspace.set(s.workspaceId, [s])
    }
    const workspaces = [...wsRes.workspaces].sort((a, b) => Number(b.isDefault) - Number(a.isDefault))
    sidebarGroups.value = workspaces.map((workspace) => ({
      workspace,
      sessions: byWorkspace.get(workspace.id) ?? [],
    }))
    looseSessions.value = loose
  } catch (e) {
    recentError.value = e instanceof ApiError ? e.message : '会话列表加载失败'
  } finally {
    recentLoading.value = false
  }
}

async function openRecentSession(id: string) {
  // 打开任意会话就收起抽屉（doc/51 §5）：焦点不回菜单按钮——用户已经选了下一步。
  void closeDrawer(false)
  if (id === sessionId.value) return
  page.value = 'chat'
  await openSession(id)
  await nextTick()
  scrollBottom()
}

function syncMobile() {
  isMobile.value = mobileMq?.matches ?? false
  if (!isMobile.value) drawerOpen.value = false
}

async function openDrawer() {
  if (!isMobile.value || drawerOpen.value) return
  menuOpen.value = false
  drawerOpen.value = true
  await nextTick()
  drawerEl.value?.querySelector<HTMLElement>('.nav button')?.focus()
}

async function closeDrawer(returnFocus = true) {
  if (!drawerOpen.value) return
  drawerOpen.value = false
  if (!returnFocus) return
  await nextTick()
  menuBtnEl.value?.focus()
}

function goPage(next: Page) {
  page.value = next
  void closeDrawer(false)
}

// 技能详情里「上传工具」跳到工具区时预填命令名（doc/52 F3）。
const toolsPrefill = ref('')
function openToolsFor(name: string) {
  toolsPrefill.value = name
  goPage('tools')
}

async function toggleMenu() {
  menuOpen.value = !menuOpen.value
  if (!menuOpen.value) {
    createOpen.value = false
    return
  }
  switchError.value = ''
  createError.value = ''
  await loadTenants()
}

async function pickTenant(row: TenantItem) {
  if (switching.value) return
  if (row.id === tenant.value.id) {
    menuOpen.value = false
    return
  }
  switching.value = true
  switchError.value = ''
  try {
    const res = await switchTenant(row.id)
    if (!res.ok) {
      if (res.status !== 401) switchError.value = res.message
      return
    }
    menuOpen.value = false
    createOpen.value = false
  } finally {
    switching.value = false
  }
}

function openCreate() {
  createOpen.value = true
  createName.value = ''
  createError.value = ''
}

async function createWorkspace() {
  if (createBusy.value) return
  createBusy.value = true
  createError.value = ''
  try {
    const res = await authFetch('/api/tenants', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: createName.value }),
    })
    const body = (await res.json().catch(() => null)) as {
      code?: number
      message?: string
      data?: { tenant?: { id?: string } }
    } | null
    if (!res.ok || !body || body.code !== 0 || !body.data?.tenant?.id) {
      createError.value = body?.message || '创建失败'
      return
    }
    const switched = await switchTenant(body.data.tenant.id)
    if (!switched.ok) {
      createError.value = switched.message
      await loadTenants()
      return
    }
    menuOpen.value = false
    createOpen.value = false
  } finally {
    createBusy.value = false
  }
}

function onDocPointer(ev: PointerEvent) {
  if (!menuOpen.value) return
  const root = switcherEl.value
  if (root && ev.target instanceof Node && !root.contains(ev.target)) {
    menuOpen.value = false
    createOpen.value = false
  }
}

async function doLogin() {
  authError.value = ''
  authBusy.value = true
  try {
    const res = await loginWithPassword(email.value, password.value)
    if (!res.ok) {
      // 状态码是给网关分类的，用户只需要知道哪不对（409：… 这种前缀只会让人去搜错误码）。
      authError.value = res.message
      return
    }
    password.value = ''
  } finally {
    authBusy.value = false
  }
}

function logout() {
  clearAuth()
}

// undefined = 没有待切换的项目（普通「新对话」，沿用当前落点）；null = 明确回默认项目。
const pendingWorkspace = ref<string | null | undefined>(undefined)

function hasDirtyChat(): boolean {
  return bubbles.value.length > 0 || draft.value.trim() !== '' || attachments.value.length > 0
}

function focusComposer() {
  void nextTick(() => {
    fitComposer()
    composerEl.value?.focus()
  })
}

function requestNewSession() {
  pendingWorkspace.value = undefined
  if (!hasDirtyChat()) {
    newSession()
    page.value = 'chat'
    focusComposer()
    return
  }
  askNew.value = true
}

function confirmNewSession() {
  askNew.value = false
  if (pendingWorkspace.value !== undefined) setActiveWorkspace(pendingWorkspace.value)
  pendingWorkspace.value = undefined
  newSession()
  page.value = 'chat'
  focusComposer()
}

function cancelNewSession() {
  askNew.value = false
  pendingWorkspace.value = undefined
}

// 项目页「开新对话」：先切落点，再判断当前对话要不要确认丢弃。
function startChatInProject(workspaceId: string | null) {
  if (!hasDirtyChat()) {
    setActiveWorkspace(workspaceId)
    newSession()
    page.value = 'chat'
    focusComposer()
    return
  }
  pendingWorkspace.value = workspaceId
  askNew.value = true
}

async function openSessionFromProjects(id: string) {
  page.value = 'chat'
  await openSession(id)
  await nextTick()
  scrollBottom()
}

// 删的正是当前会话：清空对话页（落点不变，下一场新对话还落同一个项目）。
function onSessionDeleted(id: string) {
  if (sessionId.value === id) newSession()
}

// 删项目连带的会话：当前项目清空、当前会话若在名单里也清空。
function onWorkspaceDeleted(payload: { id: string; sessionIds: string[] }) {
  if (activeWorkspaceId.value === payload.id) setActiveWorkspace(null)
  if (sessionId.value && payload.sessionIds.includes(sessionId.value)) newSession()
}

function applyStarter(text: string) {
  draft.value = text
  void nextTick(() => composerEl.value?.focus())
}

function onSend() {
  send()
  void nextTick(() => {
    fitComposer()
    composerEl.value?.focus()
  })
}

// 叫停这一轮（doc/48 A3）：引擎的 POST /api/runs/:id/stop 早就有（server.go:75），
// 前端一次没调过——方向错了只能刷新页面，那场 run 还在服务端跑着、token 还在烧。
const stopping = ref(false)
const stopError = ref('')

// target 给等人面板用：面板顶掉输入框时气泡可能是 offline（拉不到帧），按 streaming 找会找不到。
async function stopCurrent(target?: Bubble) {
  if (stopping.value) return
  const b = target ?? bubbles.value.find((x) => x.state === 'streaming' && x.runId)
  if (!b || !b.runId) return
  stopping.value = true
  stopError.value = ''
  try {
    await stopRun(b.runId as RunID)
    // 服务端幂等（停了再调仍回 stopped）。这里先本地落定，界面立刻有反应；
    // 轮询随后收到 terminal 帧会再收口一次（两边一致，不会打架）。
    b.state = 'stopped'
  } catch (e) {
    stopError.value = e instanceof ApiError ? e.message : '叫停失败'
  } finally {
    stopping.value = false
  }
}

// 中文输入法选词时 Enter 只是上屏，不能当成发送。
function onDraftKeydown(ev: KeyboardEvent) {
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing || ev.keyCode === 229) return
  ev.preventDefault()
  onSend()
}

function fitComposer() {
  const el = composerEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(el.scrollHeight, 168)}px`
}

watch(draft, () => {
  void nextTick(fitComposer)
})

// 剪贴板在非安全上下文 / 权限被拒时会失败。以前静默吞掉，用户以为复制成功、粘出来是空的。
const copyFailedKey = ref('')

async function copyBubble(key: string, text: string) {
  try {
    await navigator.clipboard.writeText(text)
    copiedKey.value = key
    copyFailedKey.value = ''
    window.setTimeout(() => {
      if (copiedKey.value === key) copiedKey.value = ''
    }, 1400)
  } catch {
    copiedKey.value = ''
    copyFailedKey.value = key
    window.setTimeout(() => {
      if (copyFailedKey.value === key) copyFailedKey.value = ''
    }, 1400)
  }
}

function onWindowKey(ev: KeyboardEvent) {
  if (ev.key !== 'Escape') return
  if (lightbox.value) {
    lightbox.value = ''
    return
  }
  if (drawerOpen.value) {
    void closeDrawer()
    return
  }
  if (menuOpen.value) {
    menuOpen.value = false
    createOpen.value = false
    return
  }
  cancelNewSession()
}

const MAX_ATTACH = 4
const MAX_FILE_BYTES = 5 * 1024 * 1024

const HEIC_RE = /\.(heic|heif)$/i
const EXT_MIME: Record<string, string> = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif',
  webp: 'image/webp', bmp: 'image/bmp', svg: 'image/svg+xml',
  pdf: 'application/pdf', mp3: 'audio/mpeg', wav: 'audio/wav', mp4: 'video/mp4',
  heic: 'image/heic', heif: 'image/heif',
}
const SERVER_MIME = new Set([
  'image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/bmp', 'image/svg+xml',
  'application/pdf', 'audio/mpeg', 'audio/wav', 'video/mp4',
])

function fileMime(f: File): string {
  if (f.type) return f.type.toLowerCase()
  const ext = f.name.split('.').pop()?.toLowerCase() ?? ''
  return EXT_MIME[ext] ?? ''
}

async function toJpeg(f: File): Promise<File> {
  const { default: heic2any } = await import('heic2any')
  const out = await heic2any({ blob: f, toType: 'image/jpeg', quality: 0.92 })
  const blob = Array.isArray(out) ? out[0] : out
  return new File([blob], f.name.replace(HEIC_RE, '.jpg'), { type: 'image/jpeg' })
}

function readBase64(f: File): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result).split(',')[1] ?? '')
    r.onerror = () => reject(r.error)
    r.readAsDataURL(f)
  })
}

const attachNotice = ref('')

async function addFiles(files: File[]) {
  const problems: string[] = []
  for (const raw of files) {
    if (attachments.value.length >= MAX_ATTACH) {
      problems.push(`最多 ${MAX_ATTACH} 个附件`)
      break
    }
    let f = raw
    let mime = fileMime(raw)
    if (mime === 'image/heic' || mime === 'image/heif' || HEIC_RE.test(raw.name)) {
      try {
        f = await toJpeg(raw)
        mime = 'image/jpeg'
      } catch {
        problems.push(`${raw.name}：HEIC 解不开，请先在照片里导出为 JPEG`)
        continue
      }
    }
    if (!SERVER_MIME.has(mime)) {
      problems.push(`${raw.name}：不支持 ${mime || '未知类型'}`)
      continue
    }
    if (f.size > MAX_FILE_BYTES) {
      problems.push(`${raw.name}：${fmtSize(f.size)} 超过 5MB`)
      continue
    }
    attachments.value.push({ name: f.name, img: { mime, dataBase64: await readBase64(f), name: f.name } })
  }
  attachNotice.value = problems.join('；')
}

function attachmentPreview(mime: string, dataBase64: string): string {
  if (!mime.startsWith('image/')) return ''
  return `data:${mime};base64,${dataBase64}`
}

watch(attachments, () => {
  if (!attachments.value.length) attachNotice.value = ''
}, { deep: true })

async function onPickFiles(ev: Event) {
  const input = ev.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  await addFiles(files)
}

function onPaste(ev: ClipboardEvent) {
  const files = Array.from(ev.clipboardData?.files ?? [])
  if (!files.length) return
  ev.preventDefault()
  void addFiles(files)
}

const dragActive = ref(false)
let dragDepth = 0

function canDrop(): boolean {
  return page.value === 'chat' && loggedIn.value
}

function hasFiles(e: DragEvent): boolean {
  return canDrop() && Array.from(e.dataTransfer?.types ?? []).includes('Files')
}

function onDragEnter(e: DragEvent) {
  if (!hasFiles(e)) return
  e.preventDefault()
  dragDepth++
  dragActive.value = true
}

function onDragOver(e: DragEvent) {
  if (!hasFiles(e)) return
  e.preventDefault()
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
}

function onDragLeave() {
  dragDepth = Math.max(0, dragDepth - 1)
  if (!dragDepth) dragActive.value = false
}

async function onDrop(e: DragEvent) {
  e.preventDefault()
  dragDepth = 0
  dragActive.value = false
  const files = Array.from(e.dataTransfer?.files ?? [])
  if (files.length) await addFiles(files)
}

function removeAttachment(i: number) {
  attachments.value.splice(i, 1)
}

function isImage(m: MediaRef): boolean {
  return m.mime.startsWith('image/')
}

function mediaLabel(m: MediaRef): string {
  if (m.name) return m.name
  if (m.seq != null) return `图片 ${m.seq}`
  return `${m.mime} ${fmtSize(m.size)}`
}

// boardRows 把看板紧凑视图解析成行（doc/41 §5.2）：
// 文本形状是引擎侧定的「看板：\n#序号 [状态] 主题」——解析失败就整段不渲染（不硬猜）。
type BoardRow = { no: string; status: string; subject: string }
function boardRows(board: string): BoardRow[] {
  const rows: BoardRow[] = []
  for (const line of board.split('\n')) {
    const m = /^#(\d+)\s+\[(\w+)\]\s*(.*)$/.exec(line.trim())
    if (m) rows.push({ no: m[1], status: m[2], subject: m[3] })
  }
  return rows
}

function fmtSize(n: number): string {
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)}MB`
  if (n >= 1024) return `${Math.round(n / 1024)}KB`
  return `${n}B`
}

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

// 子代理档案条（doc/44 §2.4）：只列当前会话；complete 才给「继续」。
// 继续不是 HTTP 调用——把带 sa_id 的意图写进输入框，交给父 agent 调 resume_subagent。
const subagents = ref<SubagentItem[]>([])
const subagentsOpen = ref(false)
const subagentsLoading = ref(false)
const subagentsError = ref('')
let subagentsToken = 0

async function loadSubagents() {
  const sid = sessionId.value
  const token = ++subagentsToken
  if (!sid) {
    subagents.value = []
    subagentsLoading.value = false
    subagentsError.value = ''
    return
  }
  subagentsLoading.value = true
  subagentsError.value = ''
  try {
    const res = await getSessionSubagents(sid)
    if (token !== subagentsToken) return
    subagents.value = res.subagents
  } catch (e) {
    if (token !== subagentsToken) return
    subagentsError.value = e instanceof ApiError ? e.message : '子代理列表加载失败'
  } finally {
    if (token === subagentsToken) subagentsLoading.value = false
  }
}

function toggleSubagents() {
  subagentsOpen.value = !subagentsOpen.value
  if (subagentsOpen.value) void loadSubagents()
}

function subagentStatus(status: string): string {
  return status === 'complete' ? '已完成' : '未完成'
}

function fmtDateTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

function continueSubagent(sa: SubagentItem) {
  const line = `请用 resume_subagent 继续子代理 ${sa.saId}（${sa.agentName}）：接着它刚才的任务继续做下去，完成后把结果告诉我。`
  // 同一个实例的指令已经在输入框里就不再追加（连点防护）。
  if (draft.value.includes(`resume_subagent 继续子代理 ${sa.saId}`)) {
    focusComposer()
    return
  }
  const current = draft.value.trimEnd()
  draft.value = current ? `${current}\n\n${line}` : line
  focusComposer()
}

// 会话切换：旧会话的档案不能留在新会话的条上；切到有 id 的会话才拉。
watch(sessionId, () => {
  subagents.value = []
  subagentsOpen.value = false
  void loadSubagents()
  void loadSidebarTree()
}, { immediate: true })

// 打开某条会话时展开它所属项目，并保证它没被上限截掉：否则人会「打开了一个在
// 侧栏里看不见的会话」。被截时只放开**这一个项目**，不动整栏上限。也监听
// treeGroups——首屏 sessionId 先到、树后到，只盯 sessionId 会错过这一次。
// expandedProjects 不是监听源，所以用户主动「收起」之后不会被这里立刻顶回去。
watch([sessionId, treeGroups], () => {
  const id = sessionId.value
  if (!id) return
  const group = treeGroups.value.find((g) => g.sessions.some((s) => s.id === id))
  if (!group) return
  if (collapsed.value.has(group.key)) {
    const next = new Set(collapsed.value)
    next.delete(group.key)
    collapsed.value = next
  }
  if (!visibleSessions(group).some((s) => s.id === id)) {
    const next = new Set(expandedProjects.value)
    next.add(group.key)
    expandedProjects.value = next
  }
}, { immediate: true })

// 一轮 run 收口后刷新：子代理 wire 的 terminal 行是跑完才写的，忙时不重拉。
watch(busy, (now, prev) => {
  if (prev && !now) void loadSubagents()
})

function scrollBottom() {
  const el = listEl.value
  if (!el) return
  // **空列表时不要滚**：.list 里装的是「从一句话开始」的空态，滚到底会把开头
  // （小标题 + 大标题）推出视野——窄屏上尤其明显（对话区被顶栏挤矮了）。
  if (!bubbles.value.length) {
    el.scrollTop = 0
    return
  }
  el.scrollTop = el.scrollHeight
}

function jumpBottom() {
  pinned.value = true
  scrollBottom()
}

watch(bubbles, () => {
  if (pinned.value) nextTick(scrollBottom)
}, { deep: true })

const mediaSrc = ref<Record<string, string>>({})
const mediaPending = new Set<string>()

async function loadMedia(path: string) {
  if (mediaSrc.value[path] || mediaPending.has(path)) return
  mediaPending.add(path)
  try {
    const res = await authFetch(mediaUrl(path))
    if (!res.ok) return
    const url = URL.createObjectURL(await res.blob())
    if (mediaSrc.value[path]) {
      URL.revokeObjectURL(url)
      return
    }
    mediaSrc.value = { ...mediaSrc.value, [path]: url }
  } finally {
    mediaPending.delete(path)
  }
}

watch(
  bubbles,
  () => {
    for (const b of bubbles.value) for (const m of b.media ?? []) void loadMedia(m.path)
  },
  { deep: true, immediate: true },
)

watch(authState, async (ok) => {
  if (!ok) {
    newSession()
    cancelNewSession()
    menuOpen.value = false
    return
  }
  // 登录后把页面滚回顶部：窄屏下登录页会把密码框滚进视野，登录后那个滚动位置
  // 会一直留着（body 的 overflow:hidden 挡不住程序化滚动）→ 用户看到半截界面。
  document.body.scrollTop = 0
  document.documentElement.scrollTop = 0
  window.scrollTo(0, 0)
  await loadTenants()
  await loadSidebarTree()
  await reconcile()
  await nextTick()
  scrollBottom()
  composerEl.value?.focus()
})

// 成员页归档、离开也会 switchTenant，但够不着 useChat。
// 登录时 id 从空变成有值，那一轮由上面的 authState 负责，这里跳过，避免拉两次。
watch(
  () => tenant.value.id,
  async (id, prev) => {
    if (!loggedIn.value || !id || !prev || id === prev) return
    await showTenantSession()
    await loadTenants()
    await nextTick()
    scrollBottom()
  },
)

onMounted(async () => {
  mobileMq = window.matchMedia('(max-width: 860px)')
  syncMobile()
  mobileMq.addEventListener('change', syncMobile)
  window.addEventListener('keydown', onWindowKey)
  window.addEventListener('pointerdown', onDocPointer)
  if (!loggedIn.value) return
  await loadTenants()
  await reconcile()
  await nextTick()
  scrollBottom()
  composerEl.value?.focus()
})

onBeforeUnmount(() => {
  mobileMq?.removeEventListener('change', syncMobile)
  window.removeEventListener('keydown', onWindowKey)
  window.removeEventListener('pointerdown', onDocPointer)
  for (const url of Object.values(mediaSrc.value)) URL.revokeObjectURL(url)
})

async function onScroll() {
  const el = listEl.value
  if (!el) return
  pinned.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
  if (el.scrollTop < 40 && !atStart.value) {
    const before = el.scrollHeight
    await loadOlder()
    await nextTick()
    el.scrollTop += el.scrollHeight - before
  }
}
</script>

<template>
  <InviteLanding v-if="inviteToken" :token="inviteToken" @leave="leaveInvite" />
  <div
    v-else
    class="app"
    @dragenter="onDragEnter"
    @dragover="onDragOver"
    @dragleave="onDragLeave"
    @drop="onDrop"
  >
    <aside v-if="loggedIn" class="rail" :class="{ 'drawer-open': drawerOpen }">
      <div class="brand">
        <span class="mark" aria-hidden="true">t</span>
        <div>
          <strong>tako</strong>
          <span class="brand-sub">工作区</span>
        </div>
      </div>

      <div class="new-wrap">
        <button type="button" class="new-chat" @click="requestNewSession">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">
            <path d="M8 3v10M3 8h10" />
          </svg>
          新对话
        </button>
        <div v-if="askNew" class="pop" role="dialog" aria-labelledby="new-chat-title">
          <p id="new-chat-title">新开一段会忘掉浏览器里记住的当前对话。</p>
          <div class="pop-actions">
            <button type="button" class="pop-go" @click="confirmNewSession">仍然新开</button>
            <button type="button" class="pop-stay" @click="cancelNewSession">取消</button>
          </div>
        </div>
      </div>

      <button
        ref="menuBtnEl"
        type="button"
        class="menu-btn"
        aria-label="打开菜单"
        aria-controls="rail-drawer"
        :aria-expanded="drawerOpen"
        @click="openDrawer"
      >
        <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">
          <path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11" />
        </svg>
      </button>

      <div v-if="drawerOpen" class="drawer-backdrop" aria-hidden="true" @click="closeDrawer()"></div>

      <div
        id="rail-drawer"
        ref="drawerEl"
        class="rail-body"
        :role="isMobile ? 'dialog' : undefined"
        :aria-modal="isMobile && drawerOpen ? 'true' : undefined"
        :aria-hidden="isMobile && !drawerOpen ? 'true' : undefined"
        :aria-label="isMobile ? '导航与项目' : undefined"
      >
      <nav class="nav" aria-label="页面">
        <button type="button" :class="{ on: page === 'chat' }" :aria-current="page === 'chat' ? 'page' : undefined" @click="goPage('chat')">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M3 3.5h10v7.2H6.2L3 13z" stroke-linejoin="round" /></svg>
          对话
        </button>
        <button type="button" :class="{ on: page === 'projects' }" :aria-current="page === 'projects' ? 'page' : undefined" @click="goPage('projects')">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M2.5 4.5h4l1.2 1.5h5.8v6.5h-11z" stroke-linejoin="round" /></svg>
          项目
        </button>
        <button type="button" :class="{ on: page === 'agents' }" :aria-current="page === 'agents' ? 'page' : undefined" @click="goPage('agents')">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="8" cy="5.2" r="2.2" /><path d="M3.4 13.2c.7-2.2 2.4-3.2 4.6-3.2s3.9 1 4.6 3.2" stroke-linecap="round" /></svg>
          助手
        </button>
        <button type="button" :class="{ on: page === 'skills' }" :aria-current="page === 'skills' ? 'page' : undefined" @click="goPage('skills')">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M8 2.2l1.5 3.3 3.6.4-2.7 2.4.8 3.5L8 10l-3.2 1.8.8-3.5L2.9 5.9l3.6-.4z" stroke-linejoin="round" /></svg>
          技能
        </button>
        <button type="button" :class="{ on: page === 'tools' }" :aria-current="page === 'tools' ? 'page' : undefined" @click="goPage('tools')">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M10.2 2.6a3.4 3.4 0 0 0-4.3 4.3L3 9.8l3.2 3.2 2.9-2.9a3.4 3.4 0 0 0 4.3-4.3l-2 2-1.6-.4-.4-1.6z" stroke-linejoin="round" /></svg>
          工具
        </button>
        <button type="button" :class="{ on: page === 'team' }" :aria-current="page === 'team' ? 'page' : undefined" @click="goPage('team')">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><rect x="2.5" y="2.5" width="11" height="11" rx="2" /><path d="M5 8h6M8 5v6" stroke-linecap="round" /></svg>
          团队
        </button>
      </nav>

      <!-- 侧栏（doc/50）：项目 → 对话两层树。项目行整行点击收起/展开，默认全展开
           （collapsed 只记被收起的，所以首屏不用初始化）。 -->
      <section v-if="loggedIn" class="recent" aria-label="会话">
        <div class="recent-head">
          <span>项目</span>
          <button v-if="recentError" type="button" class="recent-retry" @click="loadSidebarTree">重试</button>
        </div>
        <p v-if="recentLoading && !treeGroups.length" class="recent-note">正在加载…</p>
        <p v-else-if="recentError" class="recent-note error" role="alert">{{ recentError }}</p>
        <p v-else-if="!treeGroups.length" class="recent-note">还没有项目。</p>
        <div v-else class="tree">
          <div v-for="g in treeGroups" :key="g.key" class="tree-group">
            <button
              type="button"
              class="tree-project"
              :aria-expanded="!collapsed.has(g.key)"
              @click="toggleProject(g.key)"
            >
              <svg v-if="!collapsed.has(g.key)" class="tree-folder" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M6 14l1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2" />
              </svg>
              <svg v-else class="tree-folder" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z" />
              </svg>
              <span class="tree-name">{{ g.name }}</span>
              <span v-if="g.isDefault" class="tree-badge">默认</span>
            </button>
            <ul v-if="!collapsed.has(g.key) && visibleSessions(g).length" class="recent-list tree-sessions">
              <li v-for="s in visibleSessions(g)" :key="s.id">
                <button
                  type="button"
                  class="recent-item"
                  :class="{ on: s.id === sessionId }"
                  :title="s.title || '未命名对话'"
                  @click="openRecentSession(s.id)"
                >
                  <span class="recent-title">{{ s.title || '未命名对话' }}</span>
                  <span class="recent-time">{{ fmtDateTime(s.createdAt) }}</span>
                </button>
              </li>
            </ul>
            <p v-if="!collapsed.has(g.key) && !g.sessions.length" class="recent-note tree-empty">还没有对话</p>
            <button
              v-if="!collapsed.has(g.key) && (expandedProjects.has(g.key) || projectHasMore(g))"
              type="button"
              class="tree-more"
              @click="toggleProjectAll(g)"
            >{{ expandedProjects.has(g.key) ? '收起' : '展示全部' }}</button>
          </div>
        </div>
        <!-- 就地展开比跳去项目页更近一步（doc/50 §3）：原来那条「在项目页看全部 →」不再需要，
             侧栏本身已是全量目录，项目页走导航的「项目」。 -->
        <button v-if="truncated" type="button" class="recent-more" @click="visibleCount += 10">展示更多</button>
        <button v-else-if="allSessionsDesc.length > 10" type="button" class="recent-more" @click="collapseAll">收起</button>
      </section>

      <div class="rail-foot">
        <span class="avatar" aria-hidden="true">{{ initial }}</span>
        <div ref="switcherEl" class="switcher">
          <button
            type="button"
            class="switcher-btn"
            :aria-expanded="menuOpen"
            aria-haspopup="menu"
            @click="toggleMenu"
          >
            <span class="who">
              <strong>{{ tenant.name || '当前工作区' }}</strong>
              <span>{{ roleLabel(tenant.role) || '已登录' }}</span>
            </span>
            <svg class="chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">
              <path d="M4 6.5 8 10.5 12 6.5" />
            </svg>
          </button>
          <div v-if="menuOpen" class="switcher-pop" role="menu">
            <div data-slot="pending-invites">
              <PendingInvites @changed="loadTenants" />
            </div>
            <template v-if="!createOpen">
              <button
                v-for="row in tenants"
                :key="row.id"
                type="button"
                role="menuitem"
                class="switcher-row"
                :class="{ on: row.id === tenant.id }"
                :disabled="switching"
                @click="pickTenant(row)"
              >
                <strong>{{ row.name }}</strong>
                <span class="tag">{{ kindLabel(row.kind) }}</span>
                <span class="tag">{{ roleLabel(row.role) }}</span>
              </button>
              <p v-if="switchError" class="auth-error" role="alert">{{ switchError }}</p>
              <!-- POST /api/tenants 建的是**团队**租户（CreateTeamTenant），说「创建工作区」
                   会让人以为能再建一个自己的——说清楚。 -->
              <button type="button" class="switch-create" @click="openCreate">+ 创建团队工作区</button>
            </template>
            <form v-else class="switch-create-form" @submit.prevent="createWorkspace">
              <label>
                团队工作区名称
                <input v-model="createName" type="text" autocomplete="off" />
              </label>
              <p v-if="createError" class="auth-error" role="alert">{{ createError }}</p>
              <div class="pop-actions">
                <button type="submit" class="pop-go" :disabled="createBusy">{{ createBusy ? '正在创建…' : '创建' }}</button>
                <button type="button" class="pop-stay" @click="createOpen = false">取消</button>
              </div>
            </form>
          </div>
        </div>
        <button type="button" class="logout" @click="logout">退出</button>
      </div>
      </div>
    </aside>

    <div class="stage">
      <div v-if="dragActive" class="drop-hint" aria-hidden="true">松开，加到这条消息里</div>

      <form v-if="!loggedIn" class="gate" @submit.prevent="doLogin">
        <div class="gate-copy">
          <span class="mark lg" aria-hidden="true">t</span>
          <h1>tako</h1>
          <p>登录之后可以对话、改助手、配这个工作区。刷新这个标签页，同一段对话还能接上。</p>
        </div>
        <div class="gate-card">
          <h2>登录</h2>
          <!-- 被系统踢回来 vs 自己退出：同一张表单，得说清是哪一种。 -->
          <p v-if="authExpired" class="auth-note">登录已过期，请重新登录</p>
          <label>
            邮箱
            <input v-model="email" type="email" autocomplete="username" required autofocus />
          </label>
          <label>
            密码
            <span class="secret">
              <input v-model="password" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" required />
              <button type="button" class="ghost-link" @click="showPassword = !showPassword">{{ showPassword ? '隐藏' : '显示' }}</button>
            </span>
          </label>
          <button type="submit" class="primary" :disabled="authBusy">{{ authBusy ? '正在登录…' : '登录' }}</button>
          <p v-if="authError" class="auth-error" role="alert">{{ authError }}</p>
        </div>
      </form>

      <template v-else>
        <Configure v-if="page === 'agents' || page === 'team'" :key="tenant.id" :page="configPage" />
        <SkillsView v-else-if="page === 'skills'" :key="tenant.id" @upload-tool="openToolsFor" />
        <ToolsView v-else-if="page === 'tools'" :key="tenant.id" :prefill-name="toolsPrefill" />
        <ProjectsView
          v-else-if="page === 'projects'"
          :key="tenant.id"
          :active-workspace-id="activeWorkspaceId"
          @open-session="openSessionFromProjects"
          @new-chat="startChatInProject"
          @set-active="setActiveWorkspace"
          @session-deleted="onSessionDeleted"
          @workspace-deleted="onWorkspaceDeleted"
          @clear-active="setActiveWorkspace(null)"
        />

        <div v-show="page === 'chat'" class="thread">
          <p v-if="sessionGone" class="session-gone" role="alert">{{ sessionGone }}</p>
          <!-- 有子代理才显示这条（doc/48 A4）：没有子代理时它白占一行，
               还带一个「展开」按钮诱导用户去点空的。
               列表在会话建立时就拉了（见下面 watch(sessionId)），所以这里判断得准。 -->
          <div
            v-if="sessionId && (subagents.length || subagentsLoading || subagentsError)"
            class="subagents"
          >
            <button type="button" class="subagent-bar" :aria-expanded="subagentsOpen" @click="toggleSubagents">
              <span>子代理 {{ subagents.length }}</span>
              <span v-if="subagentsLoading" class="spinner" aria-hidden="true"></span>
              <span class="sub-toggle">{{ subagentsOpen ? '收起' : '展开' }}</span>
            </button>
            <div v-if="subagentsOpen" class="subagent-panel">
              <p v-if="subagentsError" class="subagent-note error" role="alert">
                {{ subagentsError }}
                <button type="button" class="subagent-retry" @click="loadSubagents">重试</button>
              </p>
              <p v-else-if="subagentsLoading && !subagents.length" class="subagent-note">正在加载子代理档案…</p>
              <p v-else-if="!subagents.length" class="subagent-note">这场对话还没有子代理档案。</p>
              <ul v-else>
                <li v-for="sa in subagents" :key="sa.saId">
                  <div class="subagent-main">
                    <strong>{{ sa.agentName }}</strong>
                    <span class="subagent-id">{{ sa.saId }}</span>
                    <span class="subagent-time">{{ fmtDateTime(sa.createdAt) }}</span>
                  </div>
                  <span class="subagent-status" :class="sa.status">{{ subagentStatus(sa.status) }}</span>
                  <button v-if="sa.status === 'complete'" type="button" class="subagent-go" @click="continueSubagent(sa)">
                    继续
                  </button>
                </li>
              </ul>
            </div>
          </div>

          <section ref="listEl" class="list" @scroll="onScroll">
            <div v-if="!bubbles.length" class="empty">
              <p class="empty-kicker">对话</p>
              <h2>从一句话开始</h2>
              <p class="empty-hint">发出去之后，回复会逐字出现。往上滚可以翻更早的记录。</p>
              <div class="starters">
                <button v-for="s in starters" :key="s" type="button" @click="applyStarter(s)">{{ s }}</button>
              </div>
            </div>

            <div v-if="loadingOlder" class="older">
              <span class="spinner" aria-hidden="true"></span>正在加载更早的记录
            </div>
            <div v-else-if="bubbles.length && !atStart" class="older muted">上滚查看更早的记录</div>

            <article v-for="b in bubbles" :key="b.key" class="turn" :class="b.role">
              <div :class="['bubble', b.role, b.state]">
                <div class="bubble-main">
                  <template v-if="b.media?.length">
                    <template v-for="(m, i) in b.media" :key="`${b.key}:m${i}`">
                      <button
                        v-if="isImage(m) && mediaSrc[m.path]"
                        type="button"
                        class="media-btn"
                        :aria-label="`查看 ${mediaLabel(m)}`"
                        @click="lightbox = mediaSrc[m.path]"
                      >
                        <img class="bubble-media" :src="mediaSrc[m.path]" :alt="mediaLabel(m)" />
                      </button>
                      <p v-else class="bubble-file">
                        <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true"><path d="M4 2.5h5l3 3V13.5H4z" stroke-linejoin="round" /><path d="M9 2.5V6h3.2" /></svg>
                        <span>{{ mediaLabel(m) }}</span>
                        <span class="file-meta">{{ m.mime }} · {{ fmtSize(m.size) }}</span>
                      </p>
                    </template>
                  </template>
                  <!-- markdown 渲染（doc/48 A1）：v6 契约写明「客户端按 markdown 渲染」，
                       此前是纯文本插值——模型的标题、表格、代码块全变成字面符号。
                       用 div 不用 p：markdown 会产出块级元素，<p> 装块级元素会被 HTML 解析器拆开。
                       v-memo：正文没变就别重算——否则流式输出时每个 token 到达都要把
                       **所有**气泡的 markdown 重新 parse + sanitize 一遍。 -->
                  <div v-if="b.text" class="bubble-text" v-html="renderSafe(b.text)" v-memo="[b.text]" />
                  <span
                    v-if="(b.state === 'streaming' || b.state === 'offline') && !pendingAsks.length"
                    class="typing"
                    aria-label="正在生成"
                  >
                    <i></i><i></i><i></i>
                  </span>
                  <p v-if="b.state === 'offline'" class="note">拉不到帧，改看会话状态</p>
                  <p v-if="b.state === 'queued'" class="note">会话忙，这条还在排队</p>
                  <p v-if="b.state === 'stopped'" class="note">已停止，没有写进历史</p>
                  <p v-if="b.state === 'interrupted'" class="note">
                    {{ b.resumable === false ? '这次运行中断了，已不能再续，请重新发一条' : '这次运行中断了' }}
                  </p>
                  <button
                    v-if="b.state === 'interrupted' && b.resumable !== false"
                    type="button"
                    class="resume-btn"
                    @click="resumeRun(b)"
                  >继续这一轮</button>
                  <p v-if="b.state === 'error'" class="note">失败，没有写进历史</p>
                  <!-- 技能工具卡（doc/52 C5）：模型读了什么，人看到同一份回执。
                       展开出来的是当时进模型的原文（含 D33 的 <skill> 包裹标记），不重新读盘；
                       失败把回执里的 message 直接摆出来，不用再展开找。 -->
                  <div v-if="b.role === 'assistant' && b.tools?.length" class="tool-cards">
                    <details
                      v-for="t in b.tools"
                      :key="t.callId"
                      class="tool-card"
                      :data-tool="t.name"
                      :data-state="toolCardState(t, b.state)"
                    >
                      <summary>
                        <span class="tool-name">{{ toolCardLabel(t) }}</span>
                        <span v-if="toolCardState(t, b.state) === 'failed'" class="tool-chip fail">失败</span>
                        <span v-else-if="toolCardState(t, b.state) === 'running'" class="tool-chip">执行中</span>
                      </summary>
                      <p v-if="toolCardFail(t)" class="tool-fail">{{ toolCardFail(t) }}</p>
                      <pre v-if="t.output !== undefined" class="tool-out">{{ t.output }}</pre>
                      <p v-else class="tool-wait">还没有回执。</p>
                    </details>
                  </div>
                </div>
              </div>
              <!-- 用量与复制是这一轮的元信息，不是正文（doc/51 §6）：
                   放进气泡里会跟着气泡宽度走，读起来像脚注；放外面两者都不再挤正文。 -->
              <div
                v-if="b.text || (b.role === 'assistant' && (b.usage || b.usageUnknown))"
                class="bubble-meta"
                :class="b.role"
              >
                <span v-if="b.role === 'assistant' && b.usage" class="usage" :class="usageClass(b.usage)">{{ usageText(b.usage) }}</span>
                <span v-else-if="b.role === 'assistant' && b.usageUnknown" class="usage">用量 ?</span>
                <button
                  v-if="b.text"
                  type="button"
                  class="copy"
                  @click="copyBubble(b.key, b.text)"
                >{{ copyFailedKey === b.key ? '复制失败' : copiedKey === b.key ? '已复制' : '复制' }}</button>
              </div>

              <!-- 问答记录（doc/47 §5.2）：互动本身在底部 dock，这里留的是可回看的档案。
                   方案按文档渲染（标题/列表/代码），问题连同选项与答案一起留档。 -->
              <div v-if="b.questions?.length" class="qa-stack">
                <div
                  v-for="q in b.questions"
                  :key="q.id"
                  :class="['qa-card', q.kind, q.status]"
                >
                  <header class="qa-head">
                    <span class="qa-kind">{{ q.kind === 'plan' ? 'Plan' : q.kind === 'install' ? '安装' : '问题' }}</span>
                    <span :class="['qa-chip', q.status, q.decision]">{{ askStatusLabel(q) }}</span>
                  </header>
                  <template v-if="q.kind === 'plan'">
                    <PlanDoc v-if="q.text" :text="q.text" />
                  </template>
                  <template v-else-if="q.kind === 'install'">
                    <!-- 安装卡只从行 args 重建（D24）：刷新后仍能看清装的是什么、结果是什么。 -->
                    <SkillInstallCard
                      :q="q"
                      :can-approve="canApproveSkills"
                      :seen="sourceSeenInSession(q.install?.source)"
                      :live="b.state === 'streaming' || b.state === 'offline'"
                    />
                  </template>
                  <template v-else>
                    <p class="qa-question">{{ q.text }}</p>
                    <ul v-if="q.options.length" class="qa-opts">
                      <li
                        v-for="(opt, i) in q.options"
                        :key="`${q.id}:o${i}`"
                        :class="{ picked: q.status === 'answered' && q.submitted === opt }"
                      >
                        <span class="qa-num">{{ i + 1 }}</span>
                        <span>{{ opt }}</span>
                      </li>
                    </ul>
                  </template>
                  <p v-if="q.status === 'answered' && q.kind !== 'install'" class="qa-answer">
                    <template v-if="q.kind === 'plan' && q.decision === 'revise'">已退回修改{{ q.submitted ? `：${q.submitted}` : '' }}</template>
                    <template v-else-if="q.kind === 'plan'">已批准执行{{ q.submitted ? '（按你改写后的方案）' : '' }}</template>
                    <template v-else>你的回答：{{ q.submitted || '（空）' }}</template>
                  </p>
                  <p v-else-if="q.status === 'stale' && q.kind !== 'install'" class="qa-note">这条已经失效，引擎重启后没有恢复等待。</p>
                  <p v-if="q.error" class="qa-note error">{{ q.error }}</p>
                </div>
              </div>
            </article>
          </section>

          <p v-if="attachNotice" class="attach-notice" role="alert">{{ attachNotice }}</p>

          <!-- 看板（doc/41 §5.2）：跟 kimi 的 Todo 面板一样挂在输入区上方，
               完成数/总数/折叠都在前端算，模型的眼睛仍看同一份紧凑文本。 -->
          <section v-if="todoRows.length" class="todo-dock" aria-label="任务看板">
            <button type="button" class="todo-head" :aria-expanded="todoOpen" @click="todoOpen = !todoOpen">
              <span class="todo-title">Todo</span>
              <span class="todo-count">{{ todoDone }}/{{ todoRows.length }}</span>
              <span class="todo-meter" aria-hidden="true"><i :style="{ width: `${todoPercent}%` }"></i></span>
              <span class="todo-chev">{{ todoOpen ? '收起' : '展开' }}</span>
            </button>
            <ul v-if="todoOpen">
              <li v-for="row in todoShown" :key="row.no" :class="['todo-row', row.status]">
                <span class="todo-icon" aria-hidden="true">{{ todoIcon(row.status) }}</span>
                <span class="todo-subject">{{ row.subject }}</span>
                <span class="todo-status">{{ todoLabel(row.status) }}</span>
              </li>
              <li v-if="todoHidden > 0 || todoExpanded" class="todo-more">
                <button type="button" @click="todoExpanded = !todoExpanded">
                  {{ todoExpanded ? '收起' : `+${todoHidden} 项` }}
                </button>
              </li>
            </ul>
          </section>

          <!-- 等人面板（doc/47 §5.2）：run 挂起在这一行时顶掉输入框——提问就是这一轮的输入。
               问题：编号选项 + 其他（自由文本）；方案：批准 / 编辑后批准 / 退回修改。 -->
          <aside
            v-if="ask"
            ref="askEl"
            :class="['ask-dock', ask.q.kind]"
            tabindex="-1"
            @keydown="onAskKeydown"
          >
            <header class="ask-dock-head">
              <span class="ask-kicker">{{ ask.q.kind === 'plan' ? '方案审核' : ask.q.kind === 'install' ? '安装确认' : '需要你确认' }}</span>
              <span :class="['qa-chip', ask.q.status, ask.q.decision]">{{ askStatusLabel(ask.q) }}</span>
              <div v-if="pendingAsks.length > 1" class="ask-nav">
                <button type="button" :disabled="askIndex <= 0" aria-label="上一条" @click="moveAsk(-1)">‹</button>
                <span>{{ askIndex + 1 }} / {{ pendingAsks.length }}</span>
                <button type="button" :disabled="askIndex >= pendingAsks.length - 1" aria-label="下一条" @click="moveAsk(1)">›</button>
              </div>
              <!-- 等待期间 composer 被这个面板顶掉，没有出口就只剩等。 -->
              <button
                v-if="ask.bubble.runId"
                type="button"
                class="stop-btn ask-stop"
                :disabled="stopping"
                @click="stopCurrent(ask.bubble)"
              >{{ stopping ? '正在停止…' : '停止' }}</button>
            </header>

            <template v-if="ask.q.kind === 'question'">
              <p class="ask-question">{{ ask.q.text }}</p>
              <ol v-if="ask.q.options.length" class="ask-options">
                <li v-for="(opt, i) in ask.q.options" :key="`${ask.q.id}:o${i}`">
                  <button
                    type="button"
                    :class="['ask-option', { on: askPick === i }]"
                    :aria-pressed="askPick === i"
                    @click="pickOption(i)"
                  >
                    <span class="ask-num">→ [{{ i + 1 }}]</span>
                    <span class="ask-opt-text">{{ opt }}</span>
                    <span class="ask-check" aria-hidden="true">{{ askPick === i ? '✓' : '' }}</span>
                  </button>
                </li>
              </ol>
              <div class="ask-other" :class="{ on: askPick < 0 && askDraft.trim() !== '' }">
                <span class="ask-num">{{ ask.q.options.length ? '其他' : '回答' }}</span>
                <textarea
                  ref="askTextEl"
                  v-model="askDraft"
                  rows="2"
                  :placeholder="ask.q.options.length ? '都不是？直接写你的回答…' : '写你的回答…'"
                  @focus="askPick = -1"
                  @keydown="onAskTextKeydown"
                ></textarea>
              </div>
              <p v-if="ask.q.error" class="ask-error" role="alert">{{ ask.q.error }}</p>
              <footer class="ask-foot">
                <span class="ask-hint">数字键选择 · Enter 提交 · Shift+Enter 换行</span>
                <button type="button" class="ask-btn primary" :disabled="!askAnswerReady || askBusy" @click="submitAsk">
                  {{ askBusy ? '正在提交…' : '提交' }}
                </button>
              </footer>
            </template>

            <!-- 安装确认（doc/52 D24）：程序卡片，只吃帧 args 的候选全表与选中集；
                 模型正文里的任何「安装」按钮都不在这里出现。 -->
            <template v-else-if="ask.q.kind === 'install'">
              <SkillInstallCard
                :q="ask.q"
                :can-approve="canApproveSkills"
                :seen="sourceSeenInSession(ask.q.install?.source)"
                :busy="askBusy"
                live
                interactive
                @approve="answerInstall('approve')"
                @cancel="answerInstall('cancel')"
              />
            </template>

            <template v-else>
              <p class="ask-lede">方案在上方。批准后开始执行；退回修改会按你的意见重写方案再提交。</p>

              <template v-if="askMode === 'edit'">
                <textarea
                  ref="askTextEl"
                  v-model="askDraft"
                  class="ask-textarea"
                  rows="12"
                  @keydown="onAskTextKeydown"
                ></textarea>
                <footer class="ask-foot">
                  <span class="ask-hint">在原文上改；Enter 批准 · Shift+Enter 换行</span>
                  <button type="button" class="ask-btn ghost" @click="askMode = 'answer'">取消</button>
                  <button
                    type="button"
                    class="ask-btn primary"
                    :disabled="!askDraft.trim() || askBusy"
                    @click="submitAsk"
                  >按改后的方案批准</button>
                </footer>
              </template>

              <template v-else-if="askMode === 'revise'">
                <textarea
                  ref="askTextEl"
                  v-model="askDraft"
                  class="ask-textarea"
                  rows="4"
                  placeholder="哪里要改？（可以留空，只退回重做）"
                  @keydown="onAskTextKeydown"
                ></textarea>
                <p v-if="ask.q.error" class="ask-error" role="alert">{{ ask.q.error }}</p>
                <footer class="ask-foot">
                  <span class="ask-hint">Enter 退回 · Shift+Enter 换行</span>
                  <button type="button" class="ask-btn ghost" @click="askMode = 'answer'">取消</button>
                  <button type="button" class="ask-btn danger" :disabled="askBusy" @click="submitAsk">
                    {{ askBusy ? '正在提交…' : '退回修改' }}
                  </button>
                </footer>
              </template>

              <footer v-else class="ask-foot">
                <span v-if="ask.q.error" class="ask-error" role="alert">{{ ask.q.error }}</span>
                <span v-else class="ask-hint">Enter 批准</span>
                <button type="button" class="ask-btn ghost" @click="startPlanRevise">退回修改</button>
                <button type="button" class="ask-btn ghost" @click="startPlanEdit">编辑后批准</button>
                <button type="button" class="ask-btn primary" :disabled="askBusy" @click="submitAsk">
                  {{ askBusy ? '正在提交…' : '批准执行' }}
                </button>
              </footer>
            </template>
          </aside>

          <form v-else class="composer" @submit.prevent="onSend">
            <input
              ref="fileEl"
              type="file"
              accept="image/*,image/heic,image/heif,.heic,.heif,application/pdf,audio/*,video/*"
              multiple
              hidden
              @change="onPickFiles"
            />
            <div v-if="attachments.length" class="chips">
              <span v-for="(a, i) in attachments" :key="i" class="chip">
                <img v-if="attachmentPreview(a.img.mime, a.img.dataBase64)" :src="attachmentPreview(a.img.mime, a.img.dataBase64)" alt="" />
                <span class="chip-name">{{ a.name }}</span>
                <button type="button" class="chip-x" aria-label="移除附件" @click="removeAttachment(i)">×</button>
              </span>
            </div>
            <textarea
              ref="composerEl"
              v-model="draft"
              rows="1"
              enterkeyhint="send"
              :placeholder="pending.length ? `还有 ${pending.length} 条在排队，这轮结束后自动发出` : '写一条消息'"
              aria-label="消息"
              @keydown="onDraftKeydown"
              @paste="onPaste"
            />
            <div class="composer-bar">
              <button
                type="button"
                class="icon-btn"
                :disabled="attachments.length >= MAX_ATTACH"
                aria-label="添加图片或文件"
                title="添加图片或文件。选不了「照片」时，可以从照片 app 拖进来，也可以直接粘贴。"
                @click="fileEl?.click()"
              >
                <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
                  <path d="M13.2 7.6 8 12.8a3.4 3.4 0 0 1-4.8-4.8l5.4-5.4a2.3 2.3 0 0 1 3.2 3.2l-5.4 5.4a1.1 1.1 0 0 1-1.6-1.6L9.6 4.8" />
                </svg>
              </button>
              <span v-if="pending.length" class="hint queue">排队 {{ pending.length }}</span>
              <span v-else-if="busy" class="hint live">正在回复</span>
              <span v-else class="hint">Enter 发送 · Shift+Enter 换行 · 可粘贴图片</span>
              <!-- 叫停这一轮（doc/48 A3）：引擎端点早就有，前端一直没接。 -->
              <button
                v-if="busy"
                type="button"
                class="stop-btn"
                :disabled="stopping"
                @click="stopCurrent()"
              >{{ stopping ? '正在停止…' : '停止' }}</button>
              <button type="submit" class="send" :disabled="!draft.trim() && !attachments.length" aria-label="发送">
                <svg viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                  <path d="M10 16.2V4.4M5.2 8.8 10 4l4.8 4.8" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                </svg>
              </button>
            </div>
          </form>
          <button v-if="!pinned && bubbles.length && !ask" type="button" class="jump" @click="jumpBottom">回到最新</button>
        </div>
      </template>
    </div>

    <!-- 灯箱：Esc 之外要有能点的出口（触屏没有 Esc）；点图在「适应窗口 / 原始大小」间切换。 -->
    <div v-if="lightbox" class="lightbox" role="dialog" aria-label="查看图片" @click="lightbox = ''">
      <img
        :src="lightbox"
        alt=""
        :class="{ zoomed: lightboxZoom }"
        @click.stop="lightboxZoom = !lightboxZoom"
      />
      <button type="button" class="lightbox-close" aria-label="关闭" @click="lightbox = ''">×</button>
    </div>
    <span class="sr-only" aria-live="polite">{{ copyFailedKey ? '复制失败' : copiedKey ? '已复制' : '' }}</span>
  </div>
</template>

<style scoped>
.app {
  height: 100%;
  display: flex;
  background: var(--bg);
}

/* 三段式（doc/51 §4）：head（品牌/新对话/导航）、tree（唯一可伸缩）、foot（账户钉底）。
   底栏不再用 margin-top:auto——那会在树比视口高时把账户卡叠在会话上。 */
.rail {
  width: 15.5rem;
  flex: none;
  display: grid;
  grid-template-rows: auto auto auto minmax(0, 1fr) auto;
  gap: 0.7rem;
  padding: 0.95rem 0.85rem 0.8rem;
  background: var(--sidebar);
  border-right: 1px solid var(--line);
}

/* 桌面端抽屉外壳透明：nav / 树 / 账户仍是 .rail 网格的直接成员。
   窄屏才把同一份 DOM 变成浮层（doc/51 §5）。 */
.rail-body { display: contents; }
.menu-btn,
.drawer-backdrop { display: none; }

.brand,
.rail-foot,
.nav button,
.new-chat,
.composer-bar,
.note,
.older,
.chip,
.bubble-file {
  display: flex;
  align-items: center;
}

.brand {
  gap: 0.65rem;
  padding: 0 0.35rem;
}
.brand strong {
  display: block;
  font-size: 0.98rem;
  letter-spacing: 0.04em;
}
.brand-sub,
.who span,
.hint,
.file-meta {
  color: var(--muted);
  font-size: 0.75rem;
}

.mark {
  width: 1.85rem;
  height: 1.85rem;
  display: grid;
  place-items: center;
  border-radius: var(--radius-control);
  background: var(--ink);
  color: var(--on-ink);
  font-weight: 650;
  letter-spacing: -0.03em;
}
.mark.lg {
  width: 2.6rem;
  height: 2.6rem;
  border-radius: 0.8rem;
  font-size: 1.25rem;
}

.new-chat,
.primary,
.send,
.pop-go {
  border: 0;
  background: var(--accent);
  color: #fff;
}
.new-wrap { position: relative; }
.new-chat {
  gap: 0.4rem;
  justify-content: center;
  height: 32px;
  border-radius: var(--radius-control);
  padding: 0 0.8rem;
  font-size: var(--text-sm);
  font-weight: 500;
}
.new-chat svg {
  width: 0.9rem;
  height: 0.9rem;
}
.new-chat:hover,
.primary:hover:not(:disabled),
.send:hover:not(:disabled) {
  background: var(--accent-ink);
}

.pop {
  position: absolute;
  z-index: 6;
  top: 0;
  left: calc(100% + 0.55rem);
  width: 16rem;
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-panel);
  padding: 0.75rem;
  box-shadow: var(--shadow);
}
.pop p {
  margin: 0 0 0.65rem;
  font-size: 0.82rem;
  line-height: 1.55;
}
.pop-actions {
  display: flex;
  gap: 0.4rem;
}
.pop-go,
.pop-stay,
.logout,
.icon-btn,
.jump,
.ghost-link {
  border-radius: var(--radius-control);
  font-size: var(--text-sm);
}
.pop-go,
.pop-stay {
  border: 0;
  height: 28px;
  padding: 0 0.7rem;
}
.pop-stay,
.logout,
.icon-btn,
.jump {
  background: var(--bg-raised);
  color: var(--ink-soft);
  border: 1px solid var(--line-strong);
}
.pop-stay:hover,
.logout:hover,
.icon-btn:hover:not(:disabled),
.jump:hover {
  background: var(--hover);
  border-color: var(--line-strong);
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 0.05rem;
}
.nav button {
  gap: 0.55rem;
  width: 100%;
  height: 32px;
  text-align: left;
  border: 0;
  background: transparent;
  color: var(--ink-soft);
  border-radius: var(--radius-control);
  padding: 0 0.65rem;
  font-size: var(--text-sm);
}
.nav button svg {
  width: 1rem;
  height: 1rem;
  flex: none;
}
.nav button.on {
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.nav button:hover:not(.on) {
  background: var(--hover);
}

.rail-foot {
  margin-top: 0;
  gap: 0.55rem;
  padding: 0.55rem 0.2rem 0;
  border-top: 1px solid var(--line);
  border-radius: 0;
  background: transparent;
}
.avatar {
  width: 1.9rem;
  height: 1.9rem;
  flex: none;
  display: grid;
  place-items: center;
  border-radius: 50%;
  background: var(--accent-soft);
  color: var(--accent-ink);
  font-size: 0.82rem;
  font-weight: 650;
}
.who {
  min-width: 0;
  flex: 1;
}
.chip-name {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.82rem;
}
/* 工作区名**不截断**（doc/48 A5）：它是用户区分「我在哪个工作区」的唯一标识，
   截成 a7-envelope... 之后多开一个工作区就分不出来了。换成换行显示，不撑破侧边栏。 */
.who strong {
  display: block;
  font-size: 0.82rem;
  line-height: 1.3;
  overflow-wrap: anywhere;
}

/* 最近会话（doc/48 A2） */
.recent {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  min-height: 0;
  overflow: hidden;
}
.recent-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.5rem;
  padding: 0 0.45rem;
  color: var(--muted);
  font-size: var(--text-xs);
  letter-spacing: 0.08em;
}
.recent-retry {
  border: 0;
  background: transparent;
  padding: 0;
  font-size: var(--text-xs);
  color: var(--accent-ink);
}
.recent-note {
  margin: 0;
  padding: 0 0.45rem;
  color: var(--muted);
  font-size: var(--text-xs);
}
.recent-note.error {
  color: var(--danger);
}
.recent-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  /* 行高固定 30px，行间不留缝：10 条正好 300px（doc/51 §4 的预算）。 */
  gap: 0;
  /* 列表内不再出现滚动条（doc/50 §3）：高度由 10 条上限 + 折叠控制。 */
}
/* 项目 → 对话两层树（doc/50）：项目行整行可点，会话缩进并挂一条细竖线。 */
.tree {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  min-height: 0;
  flex: 1;
  overflow-y: auto;
}
.tree-group {
  display: flex;
  flex-direction: column;
  gap: 0.1rem;
}
.tree-project {
  display: flex;
  align-items: center;
  gap: 0.35rem;
  width: 100%;
  height: 34px;
  border: 0;
  background: transparent;
  border-radius: var(--radius-control);
  padding: 0 0.45rem;
  text-align: left;
}
.tree-project:hover {
  background: var(--hover);
}
/* 展开/折叠两态各用一个文件夹图标：项目行与会话行字号接近，靠图标认层级。 */
.tree-folder {
  flex: none;
  width: 0.92rem;
  height: 0.92rem;
  color: var(--accent);
}
.tree-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--text-sm);
  font-weight: 500;
  color: var(--ink);
}
.tree-badge {
  flex: none;
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0 0.3rem;
  font-size: 0.68rem;
  line-height: 1.35;
  color: var(--muted);
}
.tree-sessions {
  margin-left: 0.72rem;
  padding-left: 0.3rem;
  border-left: 1px solid var(--line);
}
.tree-empty {
  padding-left: 0.75rem;
  font-size: var(--text-xs);
}
/* 项目自己的「展示全部」（doc/50 §3）：缩进对齐到会话行文字。 */
.tree-more {
  align-self: flex-start;
  border: 0;
  background: transparent;
  margin-left: 1.02rem;
  padding: 0.05rem 0.45rem;
  font-size: var(--text-xs);
  color: var(--muted);
}
.tree-more:hover {
  color: var(--accent-ink);
}
/* 会话行单行（doc/51 §4）：标题左、时间右，行高 30px；标题与时间不再上下叠。 */
.recent-item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  width: 100%;
  height: 30px;
  border: 0;
  background: transparent;
  border-radius: var(--radius-control);
  padding: 0 0.45rem;
  text-align: left;
}
.recent-item:hover {
  background: var(--hover);
}
.recent-item.on {
  background: var(--accent-soft);
}
.recent-title {
  flex: 1;
  min-width: 0;
  font-size: var(--text-sm);
  color: var(--ink);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.recent-time {
  flex: none;
  font-size: var(--text-xs);
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.recent-more {
  align-self: flex-start;
  border: 0;
  background: transparent;
  padding: 0.1rem 0.45rem;
  font-size: var(--text-xs);
  color: var(--muted);
}
.recent-more:hover {
  color: var(--accent-ink);
}

/* 叫停这一轮（doc/48 A3） */
.stop-btn {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 999px;
  padding: 0.2rem 0.6rem;
  font-size: 0.75rem;
  color: var(--ink-soft);
}
.stop-btn:hover:not(:disabled) {
  border-color: #fecdd3;
  color: var(--danger);
}
.stop-btn:disabled {
  opacity: 0.55;
}
.logout {
  height: 28px;
  padding: 0 0.55rem;
}

.switcher {
  position: relative;
  flex: 1;
  min-width: 0;
}
.switcher-btn {
  display: flex;
  align-items: center;
  gap: 0.2rem;
  width: 100%;
  border: 0;
  background: transparent;
  color: inherit;
  text-align: left;
  padding: 0;
}
.switcher-btn .who { flex: 1; }
.chevron {
  width: 0.85rem;
  height: 0.85rem;
  flex: none;
  color: var(--muted);
}
.switcher-pop {
  position: absolute;
  z-index: 8;
  bottom: calc(100% + 0.45rem);
  left: 0;
  width: 16.5rem;
  max-height: 18rem;
  overflow: auto;
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-panel);
  box-shadow: var(--shadow);
  padding: 0.35rem;
}
.switcher-row,
.switch-create {
  display: flex;
  align-items: baseline;
  gap: 0.4rem;
  width: 100%;
  border: 0;
  background: transparent;
  color: var(--ink);
  border-radius: var(--radius-control);
  padding: 0.45rem 0.55rem;
  text-align: left;
  font-size: 0.82rem;
}
.switcher-row strong {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.82rem;
}
.switcher-row .tag {
  flex: none;
  color: var(--muted);
  font-size: 0.72rem;
}
.switcher-row.on {
  background: var(--accent-soft);
}
.switcher-row:hover:not(:disabled),
.switch-create:hover {
  background: var(--hover);
}
.switcher-row:disabled { opacity: 0.6; }
.switch-create {
  border-top: 1px solid var(--line);
  border-radius: 0;
  margin-top: 0.25rem;
  color: var(--accent-ink);
}
.switch-create-form {
  display: flex;
  flex-direction: column;
  gap: 0.45rem;
  padding: 0.4rem 0.45rem 0.35rem;
}
.switch-create-form label {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  font-size: 0.75rem;
  color: var(--muted);
}
.switch-create-form input {
  height: 36px;
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-control);
  background: var(--bg-raised);
  color: var(--ink);
  padding: 0 0.55rem;
  font-size: var(--text-sm);
}
.switcher-pop .auth-error { margin: 0.3rem 0.4rem; }

.stage {
  position: relative;
  flex: 1;
  min-width: 0;
  /* **必须有 min-height: 0**：窄屏下 .app 变成 column 方向，flex item 的默认
     min-height:auto 会让它不肯收缩到内容以下 → .stage 被对话内容撑高 → body 溢出 →
     页面能滚（登录时聚焦密码框就把页面滚下半截，顶部的导航全被推出视口）。 */
  min-height: 0;
  display: flex;
  flex-direction: column;
}

.gate {
  margin: auto;
  width: min(100% - 2rem, 52rem);
  display: grid;
  grid-template-columns: 1.1fr 0.9fr;
  gap: 2.5rem;
  align-items: center;
}
.gate-copy h1 {
  margin: 0.8rem 0 0.45rem;
  font-size: 2.4rem;
  letter-spacing: 0.04em;
  font-weight: 650;
}
.gate-copy p {
  margin: 0;
  max-width: 28rem;
  color: var(--ink-soft);
  line-height: 1.7;
}
.gate-card {
  display: flex;
  flex-direction: column;
  gap: 0.8rem;
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 1.35rem;
  box-shadow: var(--shadow);
}
.gate-card h2 {
  margin: 0;
  font-size: 1.15rem;
}
.gate-card label,
.secret {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  font-size: 0.8rem;
  color: var(--ink-soft);
}
.secret {
  position: relative;
}
.gate-card input {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 12px;
  padding: 0.7rem 0.8rem;
  font-size: 0.95rem;
}
.gate-card .secret input { padding-right: 3.6rem; }
.gate-card input:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.14);
}
.ghost-link {
  position: absolute;
  right: 0.45rem;
  bottom: 0.45rem;
  border: 0;
  background: var(--bg);
  padding: 0.2rem 0.5rem;
}
.primary {
  margin-top: 0.3rem;
  border-radius: 999px;
  padding: 0.7rem 1rem;
  font-size: 0.95rem;
}
.primary:disabled {
  opacity: 0.55;
}
.auth-error,
.attach-notice {
  margin: 0;
  font-size: 0.8rem;
  line-height: 1.5;
  border-radius: 12px;
}
.auth-error {
  color: var(--danger);
  background: var(--danger-soft);
  padding: 0.55rem 0.7rem;
}
.auth-note {
  margin: 0;
  padding: 0.55rem 0.7rem;
  border-radius: 12px;
  font-size: 0.8rem;
  line-height: 1.5;
  color: var(--warn);
  background: var(--warn-soft);
}

.thread {
  width: min(45rem, calc(100% - 2rem));
  margin: 0 auto;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  padding-bottom: 1rem;
}

/* 子代理档案条（doc/44 §2.4）：默认收起，只占一行；展开是列表，不挤压对话的滚动区。 */
.session-gone {
  margin: 0 0 8px;
  padding: 10px 14px;
  border: 1px solid #fed7aa;
  border-radius: 10px;
  background: var(--warn-soft, #fff7ed);
  color: var(--warn, #b45309);
}

.subagents {
  flex: none;
  padding: 0.85rem 0.2rem 0;
}
.subagent-bar {
  display: flex;
  align-items: center;
  gap: 0.45rem;
  border: 1px solid var(--line);
  background: var(--bg-raised);
  border-radius: 999px;
  padding: 0.28rem 0.7rem;
  font-size: 0.78rem;
  color: var(--ink-soft);
}
.subagent-bar:hover {
  border-color: var(--line-strong);
}
.sub-toggle {
  margin-left: 0.2rem;
  color: var(--muted);
  font-size: 0.72rem;
}
.subagent-panel {
  margin-top: 0.4rem;
  max-height: 14rem;
  overflow: auto;
  border: 1px solid var(--line);
  background: var(--bg-raised);
  border-radius: 14px;
  padding: 0.4rem;
}
.subagent-panel ul {
  list-style: none;
  margin: 0;
  padding: 0;
}
.subagent-panel li {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.45rem 0.5rem;
  border-radius: 10px;
}
.subagent-panel li:hover {
  background: var(--bg);
}
.subagent-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 0.1rem;
}
.subagent-main strong {
  font-size: 0.82rem;
}
.subagent-id {
  color: var(--muted);
  font-size: 0.68rem;
  word-break: break-all;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.subagent-time {
  color: var(--muted);
  font-size: 0.68rem;
}
.subagent-status {
  flex: none;
  border-radius: 999px;
  padding: 0.05rem 0.5rem;
  background: var(--bg);
  color: var(--muted);
  font-size: 0.72rem;
}
.subagent-status.complete {
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.subagent-status.interrupted {
  background: var(--warn-soft);
  color: var(--warn);
}
.subagent-go {
  flex: none;
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 999px;
  padding: 0.2rem 0.7rem;
  font-size: 0.76rem;
}
.subagent-go:hover {
  border-color: #b7d7cc;
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.subagent-note {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  margin: 0.25rem 0;
  color: var(--muted);
  font-size: 0.78rem;
}
.subagent-note.error {
  color: var(--danger);
}
.subagent-retry {
  border: 0;
  background: transparent;
  color: inherit;
  padding: 0;
  font-size: 0.76rem;
  text-decoration: underline;
}
.list {
  flex: 1;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  padding: 1.4rem 0.2rem 1rem;
  scrollbar-width: thin;
  scrollbar-color: var(--line-strong) transparent;
}
.empty {
  margin: auto;
  max-width: 34rem;
  padding: 2rem 0 3rem;
}
.empty-kicker {
  margin: 0 0 0.4rem;
  color: var(--muted);
  font-size: var(--text-xs);
  font-weight: 600;
  letter-spacing: 0.1em;
  text-transform: uppercase;
}
.empty h2 {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.02em;
}
.empty-hint {
  margin: 0.55rem 0 1.2rem;
  color: var(--muted);
  font-size: var(--text-sm);
}
.starters {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
}
.starters button {
  height: 32px;
  text-align: left;
  border: 1px solid var(--line);
  background: var(--bg-raised);
  border-radius: 999px;
  padding: 0 var(--space-3);
  color: var(--ink-soft);
  font-size: var(--text-sm);
}
.starters button:hover {
  border-color: var(--line-strong);
  background: var(--hover);
  color: var(--ink);
}

.older {
  align-self: center;
  gap: 0.4rem;
  font-size: var(--text-xs);
  color: var(--ink-soft);
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0.25rem 0.75rem;
}
.older.muted {
  color: var(--muted);
  background: transparent;
  border-color: transparent;
}
.spinner {
  width: 0.75rem;
  height: 0.75rem;
  border-radius: 50%;
  border: 2px solid var(--line-strong);
  border-top-color: var(--accent);
  animation: spin 0.8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

/* 一轮 = 气泡 + 它带出的问答记录（方案卡是独立文档块，不塞进气泡里） */
.turn {
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.turn.user { align-items: flex-end; }
.turn.assistant { align-items: flex-start; }

.bubble {
  max-width: 86%;
  display: flex;
  align-items: flex-end;
  gap: 0.35rem;
  animation: rise 0.18s ease-out;
}
@keyframes rise {
  from { opacity: 0; transform: translateY(6px); }
  to { opacity: 1; transform: none; }
}
.bubble.user { flex-direction: row-reverse; }
.bubble.assistant .bubble-main { flex: 0 1 auto; }
.bubble-main {
  padding: var(--space-3) var(--space-4);
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-word;
}
.bubble p { margin: 0; }
.bubble.user .bubble-main {
  color: var(--on-ink);
  background: var(--ink);
  border-radius: 14px 14px 4px 14px;
}
.bubble.assistant .bubble-main {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 14px 14px 14px 4px;
}
.bubble.assistant.streaming .bubble-main {
  box-shadow: inset 2px 0 0 var(--accent);
}
.bubble.assistant.streaming .bubble-text::after {
  content: "";
  display: inline-block;
  width: 0.45rem;
  height: 1em;
  margin-left: 0.15rem;
  background: var(--accent);
  opacity: 0.7;
  vertical-align: text-bottom;
  animation: caret 1s steps(1) infinite;
}

/* 气泡正文的 markdown 排版（doc/48 A1）。
   **v-html 注入的内容没有 scoped 的 data-v 属性** → 这些规则必须走 :deep()，
   否则一条都不生效（写普通后代选择器是最容易踩的坑）。 */
.bubble-text {
  margin: 0;
}
.bubble-text :deep(h1),
.bubble-text :deep(h2),
.bubble-text :deep(h3),
.bubble-text :deep(h4) {
  margin: 0.65rem 0 0.3rem;
  line-height: 1.35;
  font-weight: 650;
}
.bubble-text :deep(h1) { font-size: 1.22rem; }
.bubble-text :deep(h2) { font-size: 1.1rem; }
.bubble-text :deep(h3) { font-size: 1rem; }
.bubble-text :deep(h4) { font-size: 0.93rem; }
.bubble-text :deep(p) { margin: 0.32rem 0; }
.bubble-text :deep(ul),
.bubble-text :deep(ol) {
  margin: 0.32rem 0;
  padding-left: 1.35rem;
}
.bubble-text :deep(li) { margin: 0.12rem 0; }
.bubble-text :deep(code) {
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: 5px;
  padding: 0.05rem 0.28rem;
  font-size: 0.86em;
}
.bubble-text :deep(pre) {
  margin: 0.45rem 0;
  padding: 0.55rem 0.65rem;
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: 10px;
  overflow-x: auto;
}
.bubble-text :deep(pre code) {
  background: transparent;
  border: 0;
  padding: 0;
  font-size: 0.82rem;
}
/* 用户气泡是深色底（--ink + --on-ink 字）：上面那套是给浅色助手气泡写的，
   浅底深字的 code / pre / 表头搬进深色气泡就变成白字浅底＝隐形（doc/49 补记 #13）。
   这里只把「浅底深字」的几项在深色气泡里反过来，助手气泡不动。 */
.bubble.user .bubble-text :deep(code) {
  background: rgba(245, 247, 250, 0.16);
  border-color: rgba(245, 247, 250, 0.3);
  color: var(--on-ink);
}
.bubble.user .bubble-text :deep(pre) {
  background: rgba(0, 0, 0, 0.28);
  border-color: rgba(245, 247, 250, 0.22);
}
.bubble.user .bubble-text :deep(th) {
  background: rgba(245, 247, 250, 0.12);
}
.bubble.user .bubble-text :deep(th),
.bubble.user .bubble-text :deep(td) {
  border-color: rgba(245, 247, 250, 0.22);
}
.bubble.user .bubble-text :deep(blockquote) {
  color: rgba(245, 247, 250, 0.78);
  border-left-color: rgba(245, 247, 250, 0.3);
}
.bubble.user .bubble-text :deep(a) {
  color: #a5c8ff;
}
.bubble.user .bubble-text :deep(hr) {
  border-top-color: rgba(245, 247, 250, 0.22);
}
.bubble-text :deep(blockquote) {
  margin: 0.4rem 0;
  padding: 0.05rem 0 0.05rem 0.7rem;
  border-left: 3px solid var(--line-strong);
  color: var(--ink-soft);
}
.bubble-text :deep(table) {
  width: 100%;
  border-collapse: collapse;
  margin: 0.45rem 0;
  font-size: 0.86rem;
}
.bubble-text :deep(th),
.bubble-text :deep(td) {
  border: 1px solid var(--line);
  padding: 0.28rem 0.45rem;
  text-align: left;
}
.bubble-text :deep(th) {
  background: var(--bg);
  font-weight: 650;
}
.bubble-text :deep(a) { color: var(--accent-ink); }
.bubble-text :deep(hr) {
  border: 0;
  border-top: 1px solid var(--line);
  margin: 0.55rem 0;
}
.bubble-text :deep(img) { max-width: 100%; }
@keyframes caret {
  50% { opacity: 0; }
}
.bubble.queued { opacity: 0.72; }
.bubble.assistant.queued .bubble-main { border-style: dashed; }
.bubble.error .bubble-main {
  background: var(--danger-soft);
  border-color: #fecdd3;
  color: var(--danger);
  box-shadow: none;
}
/* 中断（可续）不是失败：用琥珀色，跟 error 的红、正常态的白都区分开。
   页面上一眼看得出「这场还能接」，而不是「这场炸了」。 */
.bubble.interrupted .bubble-main {
  background: var(--warn-soft);
  border-color: #fed7aa;
  color: var(--warn);
  box-shadow: none;
}
/* 「继续这一轮」：只出现在中断且还能续的气泡上。 */
.resume-btn {
  display: inline-block;
  margin-top: 0.45rem;
  padding: 0.3rem 0.9rem;
  border: 1px solid var(--warn);
  border-radius: 999px;
  background: transparent;
  color: var(--warn);
  font-size: 0.8rem;
  line-height: 1.4;
  cursor: pointer;
}
.resume-btn:hover { background: #fff; }
.resume-btn:focus-visible {
  outline: none;
  border-color: var(--warn);
  box-shadow: 0 0 0 3px rgba(154, 52, 18, 0.14);
}
/* 用量与复制：气泡下方一行，12px --muted，gap 8px（doc/51 §6）。 */
.bubble-meta {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 0 var(--space-2);
}
.bubble-meta.user { justify-content: flex-end; }
.bubble-meta.assistant { justify-content: flex-start; }
.copy {
  display: block;
  width: fit-content;
  margin: 0;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--muted);
  font-size: var(--text-xs);
}
.copy:hover { color: var(--ink); }

.typing {
  display: inline-flex;
  gap: 0.28rem;
  height: 1.2em;
  align-items: center;
}
.typing i {
  width: 0.36rem;
  height: 0.36rem;
  border-radius: 50%;
  background: var(--muted);
  animation: blink 1.2s ease-in-out infinite;
}
.typing i:nth-child(2) { animation-delay: 0.15s; }
.typing i:nth-child(3) { animation-delay: 0.3s; }
@keyframes blink {
  0%, 60%, 100% { opacity: 0.3; transform: translateY(0); }
  30% { opacity: 1; transform: translateY(-2px); }
}
.note {
  gap: 0.3rem;
  margin-top: 0.4rem !important;
  font-size: var(--text-xs);
  color: var(--muted);
}
.bubble.error .note { color: var(--danger); }
.bubble.interrupted .note { color: var(--warn); }
.bubble-file {
  gap: 0.4rem;
  flex-wrap: wrap;
  font-size: 0.82rem;
}
.bubble-file svg {
  width: 0.95rem;
  height: 0.95rem;
  flex: none;
}
.media-btn {
  display: block;
  padding: 0;
  border: 0;
  background: transparent;
  margin-bottom: 0.4rem;
}
.bubble-media {
  display: block;
  max-width: min(100%, 20rem);
  max-height: 18rem;
  border-radius: 0.7rem;
  background: #fff;
}
.usage {
  display: inline;
  margin: 0;
  font-size: var(--text-xs);
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.usage.warm { color: var(--warn); }
.usage.hot { color: var(--danger); }

.attach-notice {
  color: var(--warn);
  background: var(--warn-soft);
  border: 1px solid #fed7aa;
  padding: 0.45rem 0.7rem;
}
.drop-hint {
  position: absolute;
  inset: 0.75rem;
  z-index: 20;
  display: grid;
  place-items: center;
  color: var(--accent-ink);
  background: rgba(229, 243, 238, 0.92);
  border: 1.5px dashed #7dbaa8;
  border-radius: 22px;
  pointer-events: none;
  font-size: 1rem;
}
.composer {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-panel);
  padding: var(--space-3);
  box-shadow: var(--shadow);
}
.composer:focus-within {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft), var(--shadow);
}
.composer textarea {
  width: 100%;
  resize: none;
  border: 0;
  outline: none;
  background: transparent;
  padding: 0.15rem var(--space-1) 0;
  line-height: 1.55;
  max-height: 10.5rem;
}
.composer textarea::placeholder { color: var(--muted); }
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 0.35rem;
  padding: 0 0.2rem;
}
.chip {
  gap: 0.35rem;
  max-width: 16rem;
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0.15rem 0.35rem 0.15rem 0.15rem;
  font-size: 0.75rem;
}
.chip img {
  width: 1.45rem;
  height: 1.45rem;
  object-fit: cover;
  border-radius: 999px;
}
.chip-x {
  border: 0;
  background: transparent;
  color: var(--muted);
  padding: 0 0.25rem;
  font-size: 0.95rem;
  line-height: 1;
}
.chip-x:hover { color: var(--danger); }
.composer-bar { gap: var(--space-2); padding-left: 0.1rem; }
.icon-btn,
.send {
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  flex: none;
  padding: 0;
}
.icon-btn { border-radius: 50%; }
.icon-btn svg,
.send svg { width: 1.05rem; height: 1.05rem; }
.icon-btn:disabled { opacity: 0.4; }
.hint {
  margin-right: auto;
  min-width: 0;
}
.hint.live { color: var(--accent-ink); }
.hint.queue { color: var(--warn); }
.send { border-radius: 50%; }
.send:disabled {
  background: var(--hover);
  color: var(--muted);
}

.jump {
  position: absolute;
  left: 50%;
  bottom: 8.2rem;
  transform: translateX(-50%);
  padding: 0.35rem 0.75rem;
  background: var(--bg-raised);
  box-shadow: var(--shadow);
}

.lightbox {
  position: fixed;
  inset: 0;
  z-index: 40;
  display: grid;
  /* safe center：放大到超出视口时可滚动，且滚动范围从图片左上角开始（普通 center 会切掉顶边）。 */
  place-items: safe center;
  overflow: auto;
  padding: 2rem;
  background: rgba(28, 25, 23, 0.78);
}
.lightbox img {
  max-width: min(92vw, 64rem);
  max-height: 88vh;
  border-radius: 14px;
  cursor: zoom-in;
}
.lightbox img.zoomed {
  max-width: none;
  max-height: none;
  border-radius: 8px;
  cursor: zoom-out;
}
.lightbox-close {
  position: fixed;
  top: 1rem;
  right: 1rem;
  width: 2.2rem;
  height: 2.2rem;
  border: 0;
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.16);
  color: #fff;
  font-size: 1.3rem;
  line-height: 1;
}
.lightbox-close:hover {
  background: rgba(255, 255, 255, 0.28);
}

@media (max-width: 860px) {
  .app { flex-direction: column; }
  /* 顶栏（doc/51 §5）：≤860px 只留品牌 / 新对话 / 菜单，树与账户进抽屉。
     doc/48 的五格顶栏在型号小的手机上会把聊天挤到 120px 高。 */
  .rail {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: auto;
    height: 52px;
    flex: none;
    padding: 0 var(--space-3);
    border-right: 0;
    border-bottom: 1px solid var(--line);
    position: relative;
    z-index: 30;
  }
  .brand { padding: 0; }
  .brand-sub { display: none; }
  .new-wrap { position: static; margin-left: auto; }
  .new-chat { height: 32px; padding: 0 var(--space-3); }
  .menu-btn {
    display: grid;
    place-items: center;
    width: 32px;
    height: 32px;
    border: 1px solid var(--line-strong);
    border-radius: var(--radius-control);
    background: var(--bg-raised);
    color: var(--ink);
  }
  .menu-btn:hover { background: var(--hover); }
  .menu-btn svg { width: 1rem; height: 1rem; }

  /* 抽屉 = 同一份 DOM 换个定位（doc/51 §5）：宽度固定，背后加遮罩。 */
  .rail-body { display: none; }
  .rail.drawer-open .rail-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    position: fixed;
    inset: 0 auto 0 0;
    z-index: 40;
    width: 280px;
    max-width: calc(100vw - 3rem);
    padding: var(--space-4) var(--space-3);
    padding-bottom: calc(var(--space-3) + env(safe-area-inset-bottom));
    background: var(--sidebar);
    border-right: 1px solid var(--line);
    box-shadow: var(--shadow);
  }
  .rail.drawer-open .recent { flex: 1; }
  .drawer-backdrop {
    display: block;
    position: fixed;
    inset: 0;
    z-index: 35;
    background: rgba(16, 17, 20, 0.45);
  }
  .rail-foot { padding: 0.55rem 0.2rem 0; border-top: 1px solid var(--line); }
  .who { max-width: none; }
  .switcher-pop {
    bottom: auto;
    top: calc(100% + 0.35rem);
    left: auto;
    right: 0;
    width: min(16.5rem, calc(100vw - 1.4rem));
  }
  .pop {
    top: calc(100% + 0.35rem);
    left: 0.7rem;
    width: min(16rem, calc(100vw - 1.4rem));
  }
  .gate {
    grid-template-columns: 1fr;
    gap: 1.2rem;
    width: min(100% - 1.5rem, 28rem);
  }
  .gate-copy h1 { font-size: 1.8rem; }
  .thread {
    width: min(100% - 1.2rem, 46rem);
    padding-bottom: calc(0.75rem + env(safe-area-inset-bottom));
  }
  .hint:not(.live):not(.queue) { display: none; }
}

/* 窄屏（手机，doc/48 D4）：全站原来只有 860px 一个断点，375px 上侧边栏挤成一团。
   这里只做「压缩」不做「图标化」——四个没有文字的图标反而更难认。 */
@media (max-width: 480px) {
  .rail {
    padding: 0 var(--space-2);
    gap: var(--space-1);
  }
  .new-chat { padding: 0 var(--space-2); white-space: nowrap; }
  .logout { padding: 0.2rem 0.4rem; font-size: 0.72rem; }
  .recent-time { display: none; }
  .composer { padding: var(--space-2) var(--space-3); }
  .gate-card { padding: 0.9rem 0.85rem 1rem; }
}

/* 技能工具卡（doc/52 C5）：一行一句「读了什么」，展开看回执原文 */
.tool-cards {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  margin-top: 0.55rem;
}
.tool-card {
  border: 1px solid var(--line);
  border-radius: 10px;
  background: var(--bg);
  padding: 0.15rem 0.55rem;
  font-size: 0.8rem;
}
.tool-card[data-state='failed'] { border-color: var(--danger-soft); }
.tool-card summary {
  display: flex;
  align-items: center;
  gap: 0.45rem;
  cursor: pointer;
  list-style: none;
  padding: 0.3rem 0;
  color: var(--ink-soft);
}
.tool-card summary::-webkit-details-marker { display: none; }
.tool-name { font-weight: 550; }
.tool-chip {
  margin-left: auto;
  border-radius: 999px;
  padding: 0 0.45rem;
  background: var(--hover);
  color: var(--muted);
  font-size: 0.68rem;
}
.tool-chip.fail { background: var(--danger-soft); color: var(--danger); }
.tool-fail {
  margin: 0 0 0.35rem;
  color: var(--danger);
  font-size: 0.76rem;
  line-height: 1.5;
}
.tool-out {
  margin: 0 0 0.5rem;
  max-height: 18rem;
  overflow: auto;
  padding: 0.5rem 0.6rem;
  border-radius: 8px;
  background: var(--bg-raised);
  border: 1px solid var(--line);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.72rem;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
}
.tool-wait { margin: 0 0 0.4rem; color: var(--muted); font-size: 0.74rem; }

/* 问答记录（doc/47 §5.2）：气泡下方留档，方案按文档块渲染 */
.qa-stack {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.qa-card {
  border: 1px solid var(--line-strong);
  border-radius: 16px;
  background: var(--bg-raised);
  padding: 0.72rem 0.9rem;
  box-shadow: 0 1px 2px rgba(68, 48, 24, 0.04);
}
.qa-card.plan {
  border-color: #bcd6c7;
  background: #f7fbf8;
}
/* 安装确认（doc/52 D24）：与方案卡同一种「程序卡片」，换一套冷色区分授权类动作。 */
.qa-card.install {
  border-color: #c7d4e6;
  background: #f7fafd;
}
.qa-card.stale { opacity: 0.66; }
.qa-head {
  display: flex;
  align-items: center;
  gap: 0.45rem;
  margin-bottom: 0.45rem;
}
.qa-kind {
  font-size: 0.7rem;
  font-weight: 650;
  letter-spacing: 0.1em;
  text-transform: uppercase;
  color: var(--muted);
}
.qa-card.plan .qa-kind { color: #2f7d5b; }
.qa-card.install .qa-kind { color: #2f5d8a; }
.qa-chip {
  font-size: 0.7rem;
  line-height: 1.5;
  border-radius: 999px;
  padding: 0.02rem 0.5rem;
  background: var(--bg);
  color: var(--muted);
}
.qa-chip.pending {
  background: #fff4de;
  color: #a05a00;
}
.qa-chip.answered.approve,
.qa-chip.answered:not(.revise) {
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.qa-chip.answered.revise {
  background: var(--warn-soft);
  color: var(--warn);
}
.qa-question {
  margin: 0;
  font-size: 0.9rem;
  line-height: 1.6;
  white-space: pre-wrap;
}
.qa-opts {
  list-style: none;
  margin: 0.5rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
.qa-opts li {
  display: flex;
  gap: 0.5rem;
  align-items: baseline;
  padding: 0.28rem 0.5rem;
  border-radius: 8px;
  background: var(--bg);
  font-size: 0.84rem;
  line-height: 1.55;
}
.qa-opts li.picked {
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.qa-num {
  flex: none;
  min-width: 1.1rem;
  color: var(--muted);
  font-size: 0.74rem;
  font-variant-numeric: tabular-nums;
}
.qa-answer {
  margin: 0.55rem 0 0;
  font-size: 0.8rem;
  line-height: 1.55;
  color: var(--ink-soft);
  white-space: pre-wrap;
}
.qa-note {
  margin: 0.4rem 0 0;
  font-size: 0.78rem;
  color: var(--muted);
}
.qa-note.error { color: var(--danger); }

/* 看板（doc/41 §5.2 / kimi TodoPanel）：输入区上方，超过 5 条折叠 */
.todo-dock {
  flex: none;
  margin: 0 0 0.5rem;
  border: 1px solid var(--line);
  border-radius: 14px;
  background: rgba(255, 253, 248, 0.9);
  padding: 0.3rem 0.45rem;
}
.todo-head {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  width: 100%;
  border: 0;
  background: transparent;
  color: var(--ink-soft);
  padding: 0.25rem 0.3rem;
  text-align: left;
}
.todo-title {
  font-size: 0.76rem;
  font-weight: 650;
  letter-spacing: 0.06em;
}
.todo-count {
  flex: none;
  font-size: 0.72rem;
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.todo-meter {
  flex: 1;
  height: 4px;
  border-radius: 999px;
  background: var(--bg);
  overflow: hidden;
}
.todo-meter i {
  display: block;
  height: 100%;
  background: var(--accent);
  transition: width 0.3s ease;
}
.todo-chev {
  flex: none;
  font-size: 0.7rem;
  color: var(--muted);
}
.todo-dock ul {
  list-style: none;
  margin: 0.1rem 0 0.1rem;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.05rem;
}
.todo-row {
  display: flex;
  align-items: baseline;
  gap: 0.45rem;
  padding: 0.22rem 0.35rem;
  border-radius: 8px;
  font-size: 0.82rem;
  line-height: 1.5;
}
.todo-row.in_progress { background: var(--accent-soft); }
.todo-icon {
  flex: none;
  width: 0.9rem;
  text-align: center;
  font-size: 0.74rem;
  color: var(--muted);
}
.todo-row.completed .todo-icon,
.todo-row.in_progress .todo-icon { color: var(--accent-ink); }
.todo-subject {
  flex: 1;
  min-width: 0;
}
.todo-row.completed .todo-subject {
  color: var(--muted);
  text-decoration: line-through;
}
.todo-status {
  flex: none;
  font-size: 0.68rem;
  color: var(--muted);
}
.todo-more { padding: 0 0.35rem; }
.todo-more button {
  border: 0;
  background: transparent;
  color: var(--accent-ink);
  font-size: 0.74rem;
  padding: 0.15rem 0;
}

/* 等人面板（doc/47 §5.2）：问题卡与方案卡共用外壳，plan 修饰类换色 */
.ask-dock {
  flex: none;
  display: flex;
  flex-direction: column;
  gap: 0.55rem;
  outline: none;
  border: 1px solid #cdbfa0;
  border-radius: 20px;
  background: linear-gradient(180deg, #fffdf7, #fbf8f0);
  padding: 0.85rem 0.95rem 0.75rem;
  box-shadow: var(--shadow);
}
.ask-dock:focus-visible,
.ask-dock:focus {
  border-color: #b7a67f;
  box-shadow: 0 0 0 4px rgba(154, 138, 99, 0.12), var(--shadow);
}
.ask-dock.plan {
  border-color: #bcd6c7;
  background: linear-gradient(180deg, #fbfefc, #f4faf6);
}
.ask-dock.plan:focus-visible,
.ask-dock.plan:focus {
  border-color: #8fbfac;
  box-shadow: 0 0 0 4px rgba(15, 110, 86, 0.1), var(--shadow);
}
.ask-dock.install {
  border-color: #c7d4e6;
  background: linear-gradient(180deg, #fbfdff, #f4f8fd);
}
.ask-dock.install:focus-visible,
.ask-dock.install:focus {
  border-color: #9fb8d6;
  box-shadow: 0 0 0 4px rgba(47, 93, 138, 0.1), var(--shadow);
}
.ask-dock-head {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}
.ask-kicker {
  font-size: 0.72rem;
  font-weight: 650;
  letter-spacing: 0.1em;
  text-transform: uppercase;
  color: #9a7b2e;
}
.ask-dock.plan .ask-kicker { color: #2f7d5b; }
.ask-dock.install .ask-kicker { color: #2f5d8a; }
.ask-nav {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 0.2rem;
  font-size: 0.72rem;
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.ask-nav button {
  border: 0;
  background: transparent;
  color: var(--ink-soft);
  font-size: 0.95rem;
  line-height: 1;
  padding: 0 0.2rem;
}
.ask-nav button:disabled { opacity: 0.28; }
.ask-stop { margin-left: auto; }
.ask-question {
  margin: 0;
  font-size: 0.95rem;
  line-height: 1.62;
  white-space: pre-wrap;
}
.ask-options {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
}
.ask-option {
  display: flex;
  align-items: center;
  gap: 0.55rem;
  width: 100%;
  border: 1px solid var(--line);
  border-radius: 12px;
  background: #fff;
  padding: 0.5rem 0.65rem;
  text-align: left;
  font-size: 0.88rem;
  line-height: 1.5;
  color: var(--ink);
}
.ask-option:hover { border-color: #c3b18a; }
.ask-option.on {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.ask-opt-text {
  flex: 1;
  min-width: 0;
}
.ask-check {
  flex: none;
  width: 0.9rem;
  text-align: center;
  color: var(--accent-ink);
}
.ask-other {
  display: flex;
  gap: 0.5rem;
  align-items: flex-start;
  border: 1px solid var(--line);
  border-radius: 12px;
  background: #fff;
  padding: 0.45rem 0.55rem;
}
.ask-other.on {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.08);
}
.ask-other .qa-num { padding-top: 0.22rem; }
.ask-other textarea {
  flex: 1;
  min-width: 0;
  border: 0;
  outline: none;
  background: transparent;
  color: inherit;
  font: inherit;
  font-size: 0.88rem;
  line-height: 1.55;
  padding: 0.1rem 0;
  resize: none;
}
.ask-textarea {
  width: 100%;
  box-sizing: border-box;
  border: 1px solid var(--line-strong);
  border-radius: 12px;
  background: #fff;
  color: var(--ink);
  font: inherit;
  font-size: 0.86rem;
  line-height: 1.6;
  padding: 0.6rem 0.7rem;
  resize: vertical;
}
.ask-textarea:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.1);
}
.ask-lede {
  margin: 0;
  font-size: 0.8rem;
  line-height: 1.55;
  color: var(--ink-soft);
}
.ask-foot {
  display: flex;
  align-items: center;
  gap: 0.45rem;
  flex-wrap: wrap;
}
.ask-hint {
  flex: 1;
  min-width: 7rem;
  font-size: 0.72rem;
  color: var(--muted);
}
.ask-btn {
  border: 1px solid var(--line-strong);
  border-radius: 999px;
  background: #fff;
  color: var(--ink-soft);
  font-size: 0.82rem;
  line-height: 1.4;
  padding: 0.42rem 0.9rem;
}
.ask-btn:hover:not(:disabled) {
  border-color: #c3b18a;
  color: var(--ink);
}
.ask-btn.ghost { background: transparent; }
.ask-btn.primary {
  background: var(--ink);
  border-color: var(--ink);
  color: #f6f1e7;
}
.ask-btn.primary:hover:not(:disabled) {
  background: #292524;
  color: #f6f1e7;
}
.ask-btn.primary:disabled {
  opacity: 0.42;
}
.ask-btn.danger {
  background: #fff8f2;
  border-color: #e7c3a8;
  color: #9a3412;
}
.ask-btn.danger:hover:not(:disabled) {
  background: #fff;
  border-color: var(--warn);
  color: var(--warn);
}
.ask-error {
  margin: 0;
  flex: 1;
  min-width: 7rem;
  font-size: 0.78rem;
  color: var(--danger);
}
</style>
