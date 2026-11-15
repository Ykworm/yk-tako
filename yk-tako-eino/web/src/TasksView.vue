<script setup lang="ts">
// 任务页（doc/70 §11）：项目选择器 → Task 列表 → 详情抽屉（含编排组视图）。
//
// 这一页是观测与干预面：列出本人项目下的持久 Task，看状态、结果与依赖，
// 在 interrupted 时恢复、在未完成时停止。创建不在页面表单里——Task 由对话里的
// 助手用 delegate_task / create_orchestration 派发（通用工具卡，doc/59），页面只消费
// 服务端真源；所有「变了」都靠事件重拉（agent_tasks_changed / agent_orchestrations_changed），
// 刷新或换标签页后仍从 PG 重建，不存本地状态。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  ApiError,
  CODE_JOB_NOT_STOPPABLE,
  continueOrchestration,
  getAgentTask,
  getJobOutput,
  getOrchestration,
  getOrchestrationHandoff,
  getOrchestrationJourney,
  JOB_STATUS_LABEL,
  listPlanRevisions,
  listTaskQuestions,
  listWorkspaceJobs,
  listWorkspaceTasks,
  listWorkspaces,
  postAnswer,
  resumeAgentTask,
  retryTaskPublication,
  stopAgentTask,
  stopJob,
  stopOrchestration,
  taskArtifactUrl,
  workspaceFileUrl,
  type AgentTask,
  type Job,
  type JobOutput,
  type OrchestrationHandoff,
  type OrchestrationJourney,
  type OrchestrationView,
  type PlanRevisionRow,
  type TaskQuestion,
  type TaskResult,
  type TaskWorkspaceView,
  type Workspace,
} from './api'
import { authFetch } from './auth'
import { renderSafe } from './markdown'
import ConfirmDialog from './ConfirmDialog.vue'
import NotificationRecovery from './NotificationRecovery.vue'
import { onMutation } from './mutation'

const props = defineProps<{
  activeWorkspaceId?: string | null
  // doc/80：当前活动会话 id。非空时「本会话」档可用且默认落本会话；换会话按新会话重来。
  activeSessionId?: string | null
  // doc/77：对话页右侧面板的嵌入模式——隐藏页头与项目选择器，作用域由 App 传死；
  // 列表、详情抽屉与全部操作与任务页共用同一份实现。
  embedded?: boolean
  // 事件流重连／窗口聚焦时由 App 递增，嵌入实例据此重拉（任务页不传）。
  reloadNonce?: number
  // 左侧「任务」处理项定位（doc/72 §8.2）：App 用显式 id 传过来，不引入新路由。
  focusWorkspaceId?: string
  focusTaskId?: string
  focusOrchestrationId?: string
  focusQuestionId?: string
  // doc/73 §7.2：交付失败处理项带着通知 id，打开恢复面板（重试、结果定位、attempt 历史）。
  focusNotificationId?: string
  focusNonce?: number
}>()

const emit = defineEmits<{
  openSession: [sessionId: string]
  // doc/80 §4.5：档位变化上报给宿主（对话页右栏据此给项目级处理条加限定词标题）。
  scopeChange: [scope: 'session' | 'project']
}>()

const phase = ref<'loading' | 'ready' | 'error'>('loading')
const banner = ref('')
const notice = ref('')
const busy = ref(false)

const workspaces = ref<Workspace[]>([])
const workspaceId = ref('')

const tasks = ref<AgentTask[]>([])
const query = ref('')
// 列表档位（doc/80 §4.3）：本会话 / 本项目全部。默认能拿到当前会话就本会话，否则本项目全部；
// 不落 localStorage——默认表达「我现在在看哪一场」，不是长期偏好，记住会让「新会话默认本会话」当场失效。
const scope = ref<'session' | 'project'>(props.activeSessionId ? 'session' : 'project')

// 详情抽屉：任务详情与编排组视图共用一个抽屉，组视图由详情里的「查看编排组」进入。
const detail = ref<AgentTask | null>(null)
const detailLoading = ref(false)
const orch = ref<OrchestrationView | null>(null)
const revisions = ref<PlanRevisionRow[]>([])
const revisionsLoading = ref(false)
const planBusy = ref(false)
// 恢复面板内联在列表上方（doc/73 §7.2）：不把完整问题或 DAG 塞进左侧处理项。
const activeNotificationId = ref('')
// 继任组的重规划交接（doc/75 §9.2）：机器字段与正文都来自 GET handoff，按需拉取；
// 交接写一次就不再变，打开详情时清空，避免把上一组的交接显示给下一组。
const handoff = ref<OrchestrationHandoff | null>(null)
const handoffOpen = ref(false)
const handoffLoading = ref(false)
const handoffError = ref('')

// 路径账本（doc/82 §10.3）：打开一个编排组时从 journey 端点读只读投影，前端不从模型回答重建。
// 与 handoff 同规矩——打开详情时清空，避免把上一组的账本显示给下一组。
const journey = ref<OrchestrationJourney | null>(null)
const journeyLoading = ref(false)
const journeyError = ref('')

// 后台命令分组（doc/82 §10.2）：行=命令摘要+状态+来源；展开=尾部输出+全文路径；
// running 可停、stopping 置灰显示「停止中」。列表随项目拉，事件到了重拉（终态靠重拉对齐）。
const jobs = ref<Job[]>([])
const jobsPhase = ref<'loading' | 'ready' | 'error'>('loading')
const jobsError = ref('')
const expandedJobs = ref<string[]>([])
const jobOutputs = ref<Record<string, JobOutput>>({})
const jobStopBusy = ref('')
let jobsTimer: ReturnType<typeof setInterval> | null = null
let jobsLoading = false

// Task 问题区（doc/72 §5）：pending 按 createdAt,id 的 FIFO 排，第一张可答；
// answered／stale 只读展示。问题正文行是 Task 当前 run 的 run_questions 行。
const questions = ref<TaskQuestion[]>([])
const questionsLoading = ref(false)
const answerDraft = ref('')
const answerPick = ref(-1)
const answerBusy = ref(false)
const focusedQuestionId = ref('')

const pendingQuestions = computed(() => questions.value.filter((q) => q.status === 'pending'))
const answeredQuestions = computed(() => questions.value.filter((q) => q.status !== 'pending'))
// 只有 Task 自己还在 running 才能答：停止、中断（租约失效）、终态后遗留 pending 都不再可答。
const canAnswer = computed(() => detail.value?.status === 'running' && !!detail.value.currentRunId)
const activeQuestion = computed(() => (canAnswer.value ? (pendingQuestions.value[0] ?? null) : null))
const activeOptions = computed(() => (activeQuestion.value ? questionOptions(activeQuestion.value) : []))

// 换到下一张卡（上一张答完，或服务端补来更早的行）就清掉草稿，避免把作答串到另一题。
watch(
  () => activeQuestion.value?.questionId,
  () => {
    answerDraft.value = ''
    answerPick.value = -1
  },
)

// 停止确认：单件与整组共用一个 ConfirmDialog 桽。
const pendingStopTask = ref<AgentTask | null>(null)
const pendingStopOrch = ref<OrchestrationView | null>(null)

// 靠边的项：编排组视图里 parent → child 的呈现用名字（key）而不是长 id。
const taskKeyById = computed(() => {
  const m: Record<string, string> = {}
  for (const t of orch.value?.tasks ?? []) m[t.taskId] = t.key
  return m
})

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return tasks.value
  return tasks.value.filter(
    (t) => t.key.toLowerCase().includes(q) || t.content.toLowerCase().includes(q),
  )
})

// 账本时间线（doc/82 §10.3）：把端点的五种步骤归一成可渲染的一行——前端只做形状适配，
// 不补任何事实（失败／取消节点、复用、继任、交付尝试都照端点给的写）。
type JourneyRow = {
  id: string
  at: string
  kind: 'plan' | 'task' | 'reuse' | 'successor' | 'delivery'
  label: string
  text: string
  added: string[]
  cancelled: string[]
}
const journeyRows = computed<JourneyRow[]>(() => {
  const rows: JourneyRow[] = []
  const steps = journey.value?.steps ?? []
  steps.forEach((s, i) => {
    if (s.type === 'plan_revision') {
      rows.push({
        id: `rev-${s.revision}-${i}`,
        at: s.at,
        kind: 'plan',
        label: `计划 v${s.revision}`,
        text: s.reason,
        added: s.added,
        cancelled: s.cancelled ?? [],
      })
    } else if (s.type === 'task') {
      rows.push({
        id: `task-${s.taskId}`,
        at: '',
        kind: 'task',
        label: `${s.taskKey} · ${STATUS_LABEL[s.status] ?? s.status}`,
        text: s.reason ?? '',
        added: [],
        cancelled: [],
      })
    } else if (s.type === 'reuse') {
      rows.push({
        id: `reuse-${s.taskId}-${i}`,
        at: s.at,
        kind: 'reuse',
        label: '复用前代结果',
        text: `${s.taskId}（resultRevision ${s.resultRevision}）`,
        added: [],
        cancelled: [],
      })
    } else if (s.type === 'successor') {
      rows.push({
        id: `succ-${s.orchestrationId}-${i}`,
        at: s.at,
        kind: 'successor',
        label: '继任组',
        text: s.orchestrationId,
        added: [],
        cancelled: [],
      })
    } else {
      rows.push({
        id: `del-${s.attempt}-${i}`,
        at: s.at,
        kind: 'delivery',
        label: `交付尝试 ${s.attempt}`,
        text: `run ${s.runId}`,
        added: [],
        cancelled: [],
      })
    }
  })
  return rows
})

// 停止门槛（doc/70 §11.6）：running（含读侧派生的 interrupted——存储仍是 running）
// 与尚未执行（pending/ready/blocked）可停；已终态不显示按钮。
const STOPPABLE = new Set(['pending', 'ready', 'blocked', 'running', 'interrupted'])
function stoppable(t: AgentTask): boolean {
  return STOPPABLE.has(t.status)
}

let reloadDelay: ReturnType<typeof setTimeout> | null = null
function scheduleReload() {
  if (reloadDelay) clearTimeout(reloadDelay)
  reloadDelay = setTimeout(() => void loadTasks(), 120)
}

let jobsReloadDelay: ReturnType<typeof setTimeout> | null = null
function scheduleJobsReload() {
  if (jobsReloadDelay) clearTimeout(jobsReloadDelay)
  jobsReloadDelay = setTimeout(() => void loadJobs(true), 120)
}

const offMutation = onMutation((evt) => {
  // 事件只通知、不带状态：收到就重拉（doc/46 §3.1）。别的窗口/别的副本的推进都会到这里。
  if (evt.type === 'agent_tasks_changed') {
    scheduleReload()
    if (detail.value?.taskId === evt.id) void reloadDetail(evt.id)
    if (orch.value?.tasks.some((t) => t.taskId === evt.id)) void reloadOrch(orch.value.orchestration.orchestrationId)
  } else if (evt.type === 'agent_orchestrations_changed') {
    // 组级事件也重拉列表：停整组当场把 pending/blocked 翻 cancelled，之后只发组事件，
    // 不重拉的话列表行停在旧状态、停止按钮还挂着（施工包 §10.3：收到就重拉）。
    scheduleReload()
    if (orch.value?.orchestration.orchestrationId === evt.id) void reloadOrch(evt.id)
    if (detail.value && detail.value.orchestrationId === evt.id) void reloadDetail(detail.value.taskId)
  } else if (evt.type === 'background_jobs_changed') {
    // 命令事件不带终态正文（doc/82 §10.4）：重拉列表；展开着的那条同时刷尾部输出。
    scheduleJobsReload()
    if (expandedJobs.value.includes(evt.id)) void loadJobOutput(evt.id)
  }
})

async function loadWorkspaces() {
  const res = await listWorkspaces()
  workspaces.value = res.workspaces
  // 默认当前项目（App.vue 的 activeWorkspaceId）；它不在清单里（已删）就落第一项。
  const wanted = props.activeWorkspaceId ?? ''
  workspaceId.value = res.workspaces.some((w) => w.id === wanted)
    ? wanted
    : (res.workspaces[0]?.id ?? '')
}

async function loadTasks() {
  if (!workspaceId.value) {
    tasks.value = []
    phase.value = 'ready'
    return
  }
  try {
    // 本会话档：把当前会话 id 交给服务端缩小范围（doc/80 §4.1）；本项目全部档不传。
    const sid = scope.value === 'session' ? (props.activeSessionId ?? '') : ''
    const res = await listWorkspaceTasks(workspaceId.value, sid)
    tasks.value = res.tasks
    phase.value = 'ready'
  } catch (err) {
    phase.value = 'error'
    banner.value = err instanceof ApiError ? err.message : '任务列表没拉到'
  }
}

// 手动换项目（doc/80 §4.3）：当前会话不属于那个项目，档位落「本项目全部」。
function onWorkspacePick(e: Event) {
  const v = (e.target as HTMLSelectElement).value
  if (v !== workspaceId.value) workspaceId.value = v
  scope.value = 'project'
}

// —— 后台命令分组（doc/82 §10.2）——
// silent=true 是事件/轮询触发的重拉：不动 phase，避免闪烁；失败也只在列表空时提示。
async function loadJobs(silent = false) {
  if (!workspaceId.value) {
    jobs.value = []
    jobsPhase.value = 'ready'
    syncJobsTimer()
    return
  }
  if (jobsLoading) return
  jobsLoading = true
  if (!silent) jobsPhase.value = 'loading'
  try {
    const sid = scope.value === 'session' ? (props.activeSessionId ?? '') : ''
    const res = await listWorkspaceJobs(workspaceId.value, sid)
    jobs.value = res.jobs
    jobsPhase.value = 'ready'
    // 展开着的命令若输出可能变了（非终态），顺手刷一次尾部输出。
    for (const id of expandedJobs.value) {
      const row = jobs.value.find((j) => j.jobId === id)
      if (row && (row.status === 'running' || row.status === 'stopping')) void loadJobOutput(id)
    }
  } catch (err) {
    // 显式拉取失败进错误态；silent 重拉失败时手里还有旧列表就原样保留等下一轮，只在列表空时降级。
    if (!silent || jobs.value.length === 0) {
      jobsPhase.value = 'error'
      jobsError.value = err instanceof ApiError ? err.message : '命令列表没拉到'
    }
  } finally {
    jobsLoading = false
    syncJobsTimer()
  }
}

// 轮询快照（施工包 §6.3：不新增「等待 30 秒」端点，页面轮询快照 + 事件重拉）。
// 只在列表里还有未终态命令时开表，全终态就停表——终态由事件到达后的一次重拉对齐。
function syncJobsTimer() {
  const live = jobs.value.some((j) => j.status === 'running' || j.status === 'stopping')
  if (live && !jobsTimer) {
    jobsTimer = setInterval(() => void loadJobs(true), 4000)
  } else if (!live && jobsTimer) {
    clearInterval(jobsTimer)
    jobsTimer = null
  }
}

async function loadJobOutput(jobId: string) {
  try {
    jobOutputs.value[jobId] = await getJobOutput(jobId)
  } catch {
    /* 输出暂时读不到就留旧值；下一次事件或轮询会对齐 */
  }
}

// 展开/收起一条命令；首次展开拉尾部输出与全文路径。
function toggleJob(job: Job) {
  const i = expandedJobs.value.indexOf(job.jobId)
  if (i >= 0) {
    expandedJobs.value = expandedJobs.value.filter((id) => id !== job.jobId)
    return
  }
  expandedJobs.value = [...expandedJobs.value, job.jobId]
  if (!jobOutputs.value[job.jobId]) void loadJobOutput(job.jobId)
}

// running 与 stopping 都显示停止按钮（stopping 置灰显示「停止中」）；终态不显示。
function jobStoppable(job: Job): boolean {
  return job.status === 'running' || job.status === 'stopping'
}

async function stopJobNow(job: Job) {
  if (jobStopBusy.value || !jobStoppable(job)) return
  banner.value = ''
  jobStopBusy.value = job.jobId
  try {
    const res = await stopJob(job.jobId)
    notice.value = res.status === 'stopping' ? '停止请求已受理，进程正在退出' : '这条命令已经是终态'
    await loadJobs(true)
    void loadJobOutput(job.jobId)
  } catch (err) {
    // 409 117118 = 已经在停止中（别的入口抢先）：重拉显示服务端现状。
    const api = err instanceof ApiError ? err : null
    if (api?.code === CODE_JOB_NOT_STOPPABLE) notice.value = api.message || '这条命令正在停止中'
    else fail(err, '停止失败，请再试一次')
    await loadJobs(true)
  } finally {
    jobStopBusy.value = ''
  }
}

function jobSummary(job: Job): string {
  const one = job.command.replace(/\s+/g, ' ').trim()
  return one.length > 80 ? `${one.slice(0, 80)}…` : one
}

function fail(err: unknown, fallback: string) {
  if (err instanceof ApiError) {
    banner.value = err.message
    return
  }
  banner.value = fallback
}

// 档位或项目变了就重拉（任务与命令各一份）；两者同一次 flush 一起变时只拉一次。
watch([workspaceId, scope], () => {
  void loadTasks()
  expandedJobs.value = []
  jobOutputs.value = {}
  void loadJobs()
})

// 换会话（doc/80 §4.3）：档位回到默认（能拿到会话就本会话）。任务页不换 key，靠这条重拉；
// 嵌入实例换会话时由 ChatDock 换 key 整体重挂载，这条只在任务页内生效。
watch(
  () => props.activeSessionId,
  (v) => {
    const next = v ? 'session' : 'project'
    // 档位变了由上面的 watch 重拉；档位没变（会话 A→B 仍是「本会话」）时这里必须自己重拉，
    // 否则列表停在上一场会话的结果上。
    if (scope.value === next) {
      void loadTasks()
      void loadJobs(true)
    } else {
      scope.value = next
    }
  },
)

// 档位变化上报给宿主（doc/80 §4.5）：对话页右栏据此给项目级处理条加「· 本项目」限定词。
watch(scope, (v) => emit('scopeChange', v))

async function loadQuestions(task: AgentTask) {
  if (!task.currentRunId) {
    questions.value = []
    return
  }
  questionsLoading.value = true
  try {
    questions.value = await listTaskQuestions(task.taskId)
  } catch (err) {
    questions.value = []
    fail(err, '问题列表没拉到')
  } finally {
    questionsLoading.value = false
  }
}

async function openDetail(row: AgentTask, questionId = '') {
  banner.value = ''
  orch.value = null
  revisions.value = []
  resetHandoff()
  resetJourney()
  detail.value = row
  focusedQuestionId.value = questionId
  answerDraft.value = ''
  answerPick.value = -1
  detailLoading.value = true
  try {
    detail.value = await getAgentTask(row.taskId)
  } catch (err) {
    detail.value = null
    fail(err, '详情没拉到')
  } finally {
    detailLoading.value = false
  }
  if (detail.value) await loadQuestions(detail.value)
  if (questionId) await focusQuestionCard(questionId)
}

async function focusQuestionCard(questionId: string) {
  await nextTick()
  const el = document.querySelector<HTMLElement>(`[data-question="${questionId}"]`)
  el?.scrollIntoView({ block: 'center', behavior: 'smooth' })
}

async function reloadDetail(taskId: string) {
  if (detail.value?.taskId !== taskId) return
  try {
    detail.value = await getAgentTask(taskId)
  } catch {
    /* 抽屉里那行旧的先留着；下一次事件或重开会再对齐 */
    return
  }
  await loadQuestions(detail.value)
}

async function loadRevisions(id: string) {
  revisionsLoading.value = true
  try {
    revisions.value = (await listPlanRevisions(id)).revisions
  } catch {
    revisions.value = []
  } finally {
    revisionsLoading.value = false
  }
}

// 路径账本（doc/82 §10.3）：只读投影，「变了」靠组事件重拉；打开一个组时先清空，
// 拉到后填上。前端只呈现端点给的事实，不从模型回答或本地状态重建账本。
function resetJourney() {
  journey.value = null
  journeyLoading.value = false
  journeyError.value = ''
}

async function loadJourney(id: string) {
  journeyLoading.value = true
  journeyError.value = ''
  try {
    journey.value = await getOrchestrationJourney(id)
  } catch (err) {
    journey.value = null
    journeyError.value = err instanceof ApiError ? err.message : '路径账本没拉到'
  } finally {
    journeyLoading.value = false
  }
}

async function reloadOrch(id: string) {
  if (orch.value?.orchestration.orchestrationId !== id) return
  try {
    orch.value = await getOrchestration(id)
  } catch {
    /* 同上 */
    return
  }
  await loadRevisions(id)
  await loadJourney(id)
}

function resetHandoff() {
  handoff.value = null
  handoffOpen.value = false
  handoffLoading.value = false
  handoffError.value = ''
}

function openReplanOrigin() {
  const id = orch.value?.orchestration.replanOf
  if (id) void openOrch(id)
}

async function toggleHandoff() {
  const id = orch.value?.orchestration.orchestrationId
  if (!id || handoffLoading.value) return
  if (handoffOpen.value) {
    handoffOpen.value = false
    return
  }
  handoffOpen.value = true
  if (handoff.value) return
  handoffLoading.value = true
  handoffError.value = ''
  try {
    handoff.value = await getOrchestrationHandoff(id)
  } catch (err) {
    handoffError.value = err instanceof ApiError ? err.message : '交接文件没拉到'
  } finally {
    handoffLoading.value = false
  }
}

function closeDetail() {
  detail.value = null
  orch.value = null
  revisions.value = []
  questions.value = []
  focusedQuestionId.value = ''
  answerDraft.value = ''
  answerPick.value = -1
  resetHandoff()
  resetJourney()
}

async function openOrch(id: string) {
  banner.value = ''
  detail.value = null
  questions.value = []
  resetHandoff()
  resetJourney()
  detailLoading.value = true
  try {
    orch.value = await getOrchestration(id)
  } catch (err) {
    fail(err, '编排组没拉到')
    return
  } finally {
    detailLoading.value = false
  }
  await loadRevisions(id)
  await loadJourney(id)
}

function openTaskFromOrch(taskId: string) {
  const row = orch.value?.tasks.find((t) => t.taskId === taskId)
  if (row) void openDetail(row)
}

async function stopConfirmed() {
  const row = pendingStopTask.value
  const group = pendingStopOrch.value
  pendingStopTask.value = null
  pendingStopOrch.value = null
  if (!row && !group) return
  banner.value = ''
  busy.value = true
  try {
    if (row) {
      const res = await stopAgentTask(row.taskId)
      notice.value = res.status === 'cancelled' ? `已停止：${row.key}` : `停止请求已受理：${row.key}`
      if (detail.value?.taskId === row.taskId) await reloadDetail(row.taskId)
      await loadTasks()
    } else if (group) {
      const id = group.orchestration.orchestrationId
      const res = await stopOrchestration(id)
      notice.value = res.status === 'cancelled' ? '编排组已停止' : '停止请求已受理，正在收尾在跑的任务'
      if (orch.value?.orchestration.orchestrationId === id) await reloadOrch(id)
      await loadTasks()
    }
  } catch (err) {
    fail(err, '停止失败，请再试一次')
  } finally {
    busy.value = false
  }
}

async function resume(row: AgentTask) {
  if (busy.value) return
  banner.value = ''
  busy.value = true
  try {
    await resumeAgentTask(row.taskId)
    notice.value = `已接管继续执行：${row.key}`
    await reloadDetail(row.taskId)
    await loadTasks()
  } catch (err) {
    // 409 117103：可能别的 tab 刚抢先恢复过——重拉详情对齐，再显示服务端的文案。
    fail(err, '恢复失败，请再试一次')
    await reloadDetail(row.taskId)
  } finally {
    busy.value = false
  }
}

// 人工重新发布（doc/74 §10.3）：只从 blocked 出发，服务端铸同一执行 session 的新处理 run。
// 动作期间防双击，但不吞服务端冲突——409 117112（别的入口抢先／状态已推进）重拉对齐再显示文案。
const republishBusy = ref(false)
async function republish(row: AgentTask) {
  if (republishBusy.value) return
  banner.value = ''
  republishBusy.value = true
  try {
    const res = await retryTaskPublication(row.taskId)
    notice.value = `重新发布已开始（发布轮次 ${res.publicationEpoch}）`
    await reloadDetail(row.taskId)
    await loadTasks()
  } catch (err) {
    fail(err, '重新发布失败，请再试一次')
    await reloadDetail(row.taskId)
  } finally {
    republishBusy.value = false
  }
}

async function continuePlan() {
  const o = orch.value?.orchestration
  if (!o || !o.planHold || planBusy.value) return
  banner.value = ''
  planBusy.value = true
  try {
    await continueOrchestration(o.orchestrationId, o.planRevision)
    notice.value = '已继续执行：新计划从现在开始领取任务'
    await reloadOrch(o.orchestrationId)
    await loadTasks()
  } catch (err) {
    // 117106 = 版本已经变了：重拉组，界面自动换成新版本，再让用户决定。
    fail(err, '继续失败，请再试一次')
    await reloadOrch(o.orchestrationId)
  } finally {
    planBusy.value = false
  }
}

function questionOptions(q: TaskQuestion): string[] {
  try {
    const parsed = JSON.parse(q.args) as { options?: unknown }
    return Array.isArray(parsed.options)
      ? parsed.options.filter((x): x is string => typeof x === 'string')
      : []
  } catch {
    return []
  }
}

async function submitTaskAnswer(q: TaskQuestion) {
  const opts = questionOptions(q)
  const picked = answerPick.value >= 0 ? (opts[answerPick.value] ?? '') : ''
  const text = (picked || answerDraft.value).trim()
  if (!text || answerBusy.value) return
  banner.value = ''
  answerBusy.value = true
  try {
    await postAnswer(q.questionId, text)
    notice.value = '答案已送达，Task 会接着执行'
    answerDraft.value = ''
    answerPick.value = -1
    if (detail.value) {
      await reloadDetail(detail.value.taskId)
      await loadTasks()
    }
  } catch (err) {
    // 115101 已被别的入口答过 / 115102 已过期：重拉一次让列表回到服务端事实。
    fail(err, '回答失败，请再试一次')
    if (detail.value) await reloadDetail(detail.value.taskId)
  } finally {
    answerBusy.value = false
  }
}

// 产物下载：shared 与 isolated 都有不可变结果（doc/76 §4/§5），统一走 artifact 出口——
// 按当前结果版本读结果字节，不读项目里的同路径文件（中间结果不进 content）。
// 出口要 Bearer 头，先 fetch 成 blob 再触发下载（不能直接 <a href>，会 401）。
async function downloadArtifact(task: AgentTask, artifactPath: string) {
  await downloadUrl(taskArtifactUrl(task.taskId, artifactPath), artifactPath)
}

// 「查看项目当前文件」恒走项目文件出口：隔离任务看的也是原项目现状，
// 与结果版本产物是两个出口，不混用（施工包 §11.2）。
async function downloadProjectFile(task: AgentTask, path: string) {
  await downloadUrl(workspaceFileUrl(task.workspaceId, path), path)
}

async function downloadUrl(url: string, path: string) {
  banner.value = ''
  try {
    const res = await authFetch(url)
    if (!res.ok) {
      banner.value = `取不到 ${path}`
      return
    }
    const blob = await res.blob()
    const blobUrl = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = blobUrl
    a.download = path.split('/').pop() || 'artifact'
    document.body.appendChild(a)
    a.click()
    a.remove()
    // 留一会儿再回收：浏览器还在读这个 blob，立刻 revoke 会让下载拿到空文件。
    window.setTimeout(() => URL.revokeObjectURL(blobUrl), 10_000)
  } catch {
    banner.value = `取不到 ${path}`
  }
}

function artifactName(t: TaskResult | null): string {
  return t?.artifacts.length ? `${t.artifacts.length} 个` : '0 个'
}

const STATUS_LABEL: Record<string, string> = {
  pending: '待执行',
  ready: '待领取',
  running: '执行中',
  succeeded: '已完成',
  failed: '失败',
  blocked: '被阻塞',
  cancelled: '已取消',
  interrupted: '已中断',
}
// 命令状态词表（doc/82 §6.2）与通知恢复面板共用一份，定义在 api.ts。
const ORCH_STATUS_LABEL: Record<string, string> = {
  active: '进行中',
  succeeded: '已完成',
  failed: '失败',
  cancelling: '停止中',
  cancelled: '已取消',
  // doc/75 §4.2：继任组的两个未收口状态，都不可调度、不可发布、阻止项目删除。
  preparing: '正在准备继任方案',
  waiting_previous: '等待原组结束',
}
const DELIVERY_LABEL: Record<string, string> = {
  pending: '待交付',
  delivering: '交付中',
  delivered: '已交付',
  failed: '交付失败',
}
// 交付失败的机器原因（doc/73 §4.5）：展示给人读句子，原码不进主文案。
const DELIVERY_ERR_LABEL: Record<string, string> = {
  delivery_run_error: '交付执行出错',
  delivery_stopped: '交付被停止',
  artifact_invalid: '结果产物已不可读取',
}
// 工作区读面词表（doc/74 §4.3）：全是通用值，页面不认识 Git 内部状态。
const PROVIDER_LABEL: Record<string, string> = {
  pending: '准备中',
  git: 'Git 项目',
  directory: '普通目录',
}
const VERIFY_LABEL: Record<string, string> = {
  '': '未运行',
  running: '运行中',
  passed: '通过',
  failed: '未通过',
}
const PUB_LABEL: Record<string, string> = {
  '': '未发布',
  pending: '待发布',
  publishing: '发布中',
  published: '已发布',
  blocked: '发布受阻',
}
// 发布受阻的通用机器原因（doc/74 §4.5）：blocked 只带这三码，其余原因不进发布面板。
const PUB_ERR_LABEL: Record<string, string> = {
  workspace_changed: '项目已变化',
  workspace_dirty: '项目有未保存内容',
  publication_recovery_required: '需要人工检查',
}

function isPublicationError(code: string): boolean {
  return code in PUB_ERR_LABEL
}

function verificationText(ws: TaskWorkspaceView): string {
  if (!ws.verification.configured) return '未配置验证'
  const label = VERIFY_LABEL[ws.verification.status] ?? ws.verification.status
  // 验证失败展示退出状态（施工包 §11.2）；退出码只在证据收尾后出现。
  if (ws.verification.status === 'failed' && ws.verification.exitCode !== undefined) {
    return `${label}（退出码 ${ws.verification.exitCode}）`
  }
  return label
}

function fmtWhen(iso: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function summary(t: AgentTask): string {
  const one = t.content.replace(/\s+/g, ' ').trim()
  return one.length > 96 ? `${one.slice(0, 96)}…` : one
}

function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Escape' && (detail.value || orch.value)) closeDetail()
}

// 左侧入口定位了一次就只消费一次：nonce 不变（比如从详情里关掉抽屉）不会又把抽屉弹开。
let lastFocusNonce = -1
async function applyFocus() {
  const nonce = props.focusNonce ?? 0
  if (nonce === lastFocusNonce) return
  lastFocusNonce = nonce
  const wid = props.focusWorkspaceId ?? ''
  if (wid && wid !== workspaceId.value && workspaces.value.some((w) => w.id === wid)) {
    workspaceId.value = wid
  }
  // 定位到某条任务/编排组时落「本项目全部」（doc/80 §4.3）：否则本会话档会把这个抽屉里的任务过滤掉。
  if (props.focusOrchestrationId || props.focusTaskId) scope.value = 'project'
  // 通知恢复面板与自动打开的组详情可以同时存在：失败通知既要在组里看交付状态，
  // 也要在面板里看结果定位与重试。新的定位没有通知 id 时顺手关掉旧面板。
  activeNotificationId.value = props.focusNotificationId ?? ''
  if (props.focusOrchestrationId) {
    await openOrch(props.focusOrchestrationId)
    return
  }
  if (!props.focusTaskId) return
  let row = tasks.value.find((t) => t.taskId === props.focusTaskId)
  if (!row) {
    try {
      row = await getAgentTask(props.focusTaskId)
    } catch (err) {
      fail(err, '任务没拉到')
      return
    }
  }
  await openDetail(row, props.focusQuestionId ?? '')
}

// 重试成功／状态被别的入口推进：列表与组详情都从服务端重拉，不在本地改状态。
async function onNotificationChanged() {
  await loadTasks()
  if (orch.value) await reloadOrch(orch.value.orchestration.orchestrationId)
}

watch(
  () => props.focusNonce,
  () => void applyFocus(),
)

// 事件流重连／窗口聚焦（doc/77）：嵌入实例由 App 递增 reloadNonce，列表与已打开详情重拉。
watch(
  () => props.reloadNonce,
  () => {
    void loadTasks()
    void loadJobs(true)
    if (detail.value) void reloadDetail(detail.value.taskId)
    if (orch.value) void reloadOrch(orch.value.orchestration.orchestrationId)
  },
)

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  void (async () => {
    // 嵌入实例（doc/77）不消费挂载前就存在的定位：左栏处理项写的是任务页那份 nonce，
    // 面板重挂载（切项目／回到对话页）时不能把旧定位再弹一次。
    if (props.embedded) {
      workspaceId.value = props.activeWorkspaceId ?? ''
      lastFocusNonce = props.focusNonce ?? 0
      await loadTasks()
      await loadJobs()
      return
    }
    try {
      await loadWorkspaces()
    } catch (err) {
      phase.value = 'error'
      fail(err, '项目清单没拉到')
      return
    }
    await loadTasks()
    await loadJobs()
    await applyFocus()
  })()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  offMutation()
  if (reloadDelay) clearTimeout(reloadDelay)
  if (jobsReloadDelay) clearTimeout(jobsReloadDelay)
  if (jobsTimer) clearInterval(jobsTimer)
})
</script>

<template>
  <div class="tasks" :class="{ embedded }">
    <header v-if="!embedded" class="page-head">
      <div>
        <p class="kicker">任务</p>
        <h2>交给助手的长活</h2>
        <p class="hint">
          任务由对话里的助手派发（delegate_task / create_orchestration），后台独立执行；
          中断的任务可以从这里接管继续，最终结果会交回来源对话。
        </p>
      </div>
      <div class="head-actions">
        <select :value="workspaceId" aria-label="选择项目" data-testid="tasks-workspace" @change="onWorkspacePick">
          <option v-if="!workspaces.length" value="">（还没有项目）</option>
          <option v-for="w in workspaces" :key="w.id" :value="w.id">{{ w.name }}</option>
        </select>
      </div>
    </header>

    <p v-if="banner" class="banner error" role="alert">{{ banner }}</p>
    <p v-if="notice" class="banner ok" role="status">{{ notice }}</p>

    <NotificationRecovery
      v-if="activeNotificationId"
      :notification-id="activeNotificationId"
      @close="activeNotificationId = ''"
      @open-session="emit('openSession', $event)"
      @changed="onNotificationChanged"
    />

    <section class="card">
      <div class="list-head">
        <h3>任务 <span class="count">{{ tasks.length }}</span></h3>
        <div class="scope-switch" role="group" aria-label="任务范围">
          <button
            type="button"
            :class="{ on: scope === 'session' }"
            :disabled="!activeSessionId"
            :title="activeSessionId ? '只看本会话派发的任务' : '当前没有活动会话'"
            data-testid="tasks-scope-session"
            @click="scope = 'session'"
          >本会话</button>
          <button
            type="button"
            :class="{ on: scope === 'project' }"
            data-testid="tasks-scope-project"
            @click="scope = 'project'"
          >本项目全部</button>
        </div>
        <input v-model="query" type="search" placeholder="按 key 或内容筛选" aria-label="筛选任务" />
      </div>

      <p v-if="phase === 'loading'" class="muted">正在加载…</p>
      <p v-else-if="phase === 'error'" class="muted">列表没拉到，稍后重试。</p>
      <p v-else-if="!workspaceId" class="muted">{{ embedded ? '先在左侧选一个项目。' : '先在上面选一个项目。' }}</p>
      <p v-else-if="!filtered.length && (tasks.length || scope !== 'session')" class="muted">
        <template v-if="tasks.length">没有匹配的任务。</template>
        <template v-else>这个项目还没有任务。在对话里让助手用任务功能派发。</template>
      </p>

      <ul v-else class="task-list" data-testid="tasks-list">
        <li v-for="row in filtered" :key="row.taskId" class="task" :data-task="row.taskId">
          <button type="button" class="task-main" @click="openDetail(row)">
            <span class="task-title">
              <code class="task-key">{{ row.key }}</code>
              <span class="tag" :class="row.status">{{ STATUS_LABEL[row.status] ?? row.status }}</span>
              <span class="task-agent">{{ row.agent }}</span>
            </span>
            <span class="task-desc">{{ summary(row) }}</span>
            <span class="meta">
              <span v-if="row.currentRunId">run {{ row.currentRunId.slice(0, 12) }}…</span>
              <span v-if="row.resultRevision">已交结果</span>
              <span>更新于 {{ fmtWhen(row.updatedAt) }}</span>
            </span>
          </button>
        </li>
      </ul>
    </section>

    <!-- 命令分组（doc/82 §10.2）：后台命令活过前台等待窗口后从这里观察与干预；
         与任务共用一个入口，不做独立视图。终态靠事件重拉对齐，未终态期间页面轮询快照。 -->
    <section class="card" data-testid="jobs-card">
      <div class="list-head">
        <h3>命令 <span class="count">{{ jobs.length }}</span></h3>
        <p class="muted">助手启动的后台命令：输出落在运行资源目录，活过前台等待窗口。</p>
      </div>
      <p v-if="jobsPhase === 'loading'" class="muted">正在加载…</p>
      <p v-else-if="jobsPhase === 'error'" class="muted">{{ jobsError }}</p>
      <p v-else-if="!workspaceId" class="muted">{{ embedded ? '先在左侧选一个项目。' : '先在上面选一个项目。' }}</p>
      <p v-else-if="!jobs.length" class="muted">这个项目还没有后台命令。</p>
      <ul v-else class="job-list" data-testid="jobs-list">
        <li v-for="j in jobs" :key="j.jobId" class="job" :data-job="j.jobId">
          <div class="job-main">
            <button type="button" class="job-toggle" @click="toggleJob(j)">
              <span class="job-title">
                <code class="job-cmd">{{ jobSummary(j) }}</code>
                <span class="tag" :class="j.status" data-testid="job-status">{{ JOB_STATUS_LABEL[j.status] ?? j.status }}</span>
              </span>
              <span class="meta">
                <span v-if="j.sourceTaskId">来源 Task {{ j.sourceTaskId.slice(0, 12) }}…</span>
                <span v-else>来源会话 {{ j.sourceSessionId.slice(0, 12) }}…</span>
                <span>开始 {{ fmtWhen(j.startedAt) }}</span>
              </span>
            </button>
            <button
              v-if="jobStoppable(j)"
              type="button"
              class="danger job-stop"
              :disabled="j.status === 'stopping' || jobStopBusy === j.jobId"
              data-testid="job-stop"
              @click="stopJobNow(j)"
            >{{ j.status === 'stopping' ? '停止中' : '停止' }}</button>
          </div>
          <div v-if="expandedJobs.includes(j.jobId)" class="job-detail" data-testid="job-detail">
            <template v-if="jobOutputs[j.jobId]">
              <p class="muted">退出码：{{ jobOutputs[j.jobId].exitCode ?? '—' }}</p>
              <p class="muted">stdout 尾部：</p>
              <pre class="body job-out">{{ jobOutputs[j.jobId].stdout || '（还没有输出）' }}</pre>
              <template v-if="jobOutputs[j.jobId].stderr">
                <p class="muted">stderr 尾部：</p>
                <pre class="body job-out err">{{ jobOutputs[j.jobId].stderr }}</pre>
              </template>
              <p class="muted job-paths">
                全文路径：
                <code>{{ jobOutputs[j.jobId].stdoutPath }}</code>
                <template v-if="jobOutputs[j.jobId].stderrPath"> · <code>{{ jobOutputs[j.jobId].stderrPath }}</code></template>
              </p>
            </template>
            <p v-else class="muted">正在读取输出…</p>
          </div>
        </li>
      </ul>
    </section>

    <!-- 详情抽屉：默认任务详情；「查看编排组」切到组视图，从组里再点任务回去。 -->
    <Teleport to="body">
      <div v-if="detail || orch" class="sheet-overlay" @click.self="closeDetail">
        <aside class="sheet" role="dialog" aria-modal="true" :aria-label="orch ? '编排组' : '任务详情'">
          <!-- —— 编排组视图（doc/70 §11.4）—— -->
          <template v-if="orch">
            <header class="sheet-head">
              <div>
                <h3>编排组</h3>
                <p class="muted">
                  <span class="tag" :class="orch.orchestration.status">{{ ORCH_STATUS_LABEL[orch.orchestration.status] ?? orch.orchestration.status }}</span>
                  <template v-if="orch.orchestration.deliveryStatus">
                    · 交付：<span class="tag" :class="`d-${orch.orchestration.deliveryStatus}`">{{ DELIVERY_LABEL[orch.orchestration.deliveryStatus] }}</span>
                    <template v-if="orch.orchestration.deliveryErrorCode">
                      <span class="delivery-err" data-testid="orch-delivery-error">
                        （{{ DELIVERY_ERR_LABEL[orch.orchestration.deliveryErrorCode] ?? orch.orchestration.deliveryErrorCode }}）
                      </span>
                    </template>
                  </template>
                  <span class="orch-rev"> · 计划 v{{ orch.orchestration.planRevision }}</span>
                  <span v-if="orch.orchestration.planHold" class="tag hold" data-testid="orch-plan-hold">计划待决策</span>
                  · <span data-testid="orch-workspace-mode">{{ orch.orchestration.workspace ? '隔离工作区' : '共享工作区' }}</span>
                  <template v-if="orch.orchestration.workspace">
                    （{{ PROVIDER_LABEL[orch.orchestration.workspace.provider] ?? orch.orchestration.workspace.provider }}）
                    <template v-if="orch.orchestration.workspace.publication.status">
                      · 发布：<span class="tag" :class="`p-${orch.orchestration.workspace.publication.status}`" data-testid="orch-pub-status">{{ PUB_LABEL[orch.orchestration.workspace.publication.status] ?? orch.orchestration.workspace.publication.status }}</span>
                    </template>
                  </template>
                  <template v-if="orch.orchestration.replanOf">
                    · 由
                    <button type="button" class="orch-link" data-testid="orch-replan-origin" @click="openReplanOrigin">
                      {{ orch.orchestration.replanOf }}
                    </button>
                    重规划而来（第 {{ orch.orchestration.replanDepth }} 层）
                  </template>
                </p>
              </div>
              <div class="sheet-actions">
                <button
                  v-if="orch.orchestration.workspace?.publication.status === 'blocked'"
                  type="button"
                  class="primary"
                  data-testid="orch-pub-blocked"
                  @click="openTaskFromOrch(orch.orchestration.finalTaskId)"
                >处理发布受阻</button>
                <button
                  v-if="orch.orchestration.planHold && orch.orchestration.status === 'active'"
                  type="button"
                  class="primary"
                  :disabled="planBusy"
                  data-testid="orch-continue"
                  @click="continuePlan"
                >继续执行</button>
                <button
                  v-if="!['succeeded', 'failed', 'cancelled'].includes(orch.orchestration.status)"
                  type="button"
                  class="danger"
                  :disabled="busy"
                  data-testid="orch-stop"
                  @click="pendingStopOrch = orch"
                >停止整组</button>
                <button type="button" @click="closeDetail">关闭</button>
              </div>
            </header>
            <p class="orch-goal">{{ orch.orchestration.goal }}</p>
            <p class="muted final-node">
              最终节点：
              <code>{{ taskKeyById[orch.orchestration.finalTaskId] ?? orch.orchestration.finalTaskId }}</code>
              <template v-if="orch.orchestration.planHold">
                · 检查点已暂停领取新工作；查看修订历史后显式继续，或回到来源对话让协调者复核。
              </template>
            </p>
            <!-- 重规划交接（doc/75 §9.2）：关联与复用走机器字段，正文按安全 Markdown 渲染。 -->
            <section v-if="orch.orchestration.replanOf" class="sec handoff-panel" data-testid="orch-handoff">
              <h4>重规划交接</h4>
              <p class="muted">
                这一组由前代编排组创建；计划版本从 v0 开始，复用条目只取自交接链上已成功的结果。
              </p>
              <button
                type="button"
                class="orch-link"
                data-testid="orch-handoff-toggle"
                @click="toggleHandoff"
              >{{ handoffOpen ? '收起交接文件' : '查看交接文件' }}</button>
              <p v-if="handoffLoading" class="muted">正在读取…</p>
              <p v-else-if="handoffError" class="banner error" role="alert">{{ handoffError }}</p>
              <template v-else-if="handoffOpen && handoff">
                <p class="muted handoff-reuse" data-testid="orch-handoff-reuse">
                  复用前代结果：
                  <template v-if="handoff.reuse.length">
                    <code v-for="r in handoff.reuse" :key="r.taskId" class="handoff-reuse-item">{{ r.taskId }}（resultRevision {{ r.resultRevision }}）</code>
                  </template>
                  <template v-else>无</template>
                </p>
                <div class="handoff-body" data-testid="orch-handoff-body" v-html="renderSafe(handoff.body)" />
              </template>
            </section>
            <div class="dep-map" data-testid="orch-deps">
              <p class="muted">依赖（parent → child）：</p>
              <ul v-if="orch.links.length">
                <li v-for="l in orch.links" :key="`${l.parentTaskId}>${l.childTaskId}`">
                  <code>{{ taskKeyById[l.parentTaskId] ?? l.parentTaskId }}</code>
                  →
                  <code>{{ taskKeyById[l.childTaskId] ?? l.childTaskId }}</code>
                </li>
              </ul>
              <p v-else class="muted">没有依赖边（单件任务）。</p>
            </div>
            <ul class="task-list in-sheet" data-testid="orch-tasks">
              <li v-for="t in orch.tasks" :key="t.taskId" class="task" :data-task="t.taskId">
                <button type="button" class="task-main" @click="openTaskFromOrch(t.taskId)">
                  <span class="task-title">
                    <code class="task-key">{{ t.key }}</code>
                    <span class="tag" :class="t.status">{{ STATUS_LABEL[t.status] ?? t.status }}</span>
                    <span v-if="t.taskId === orch.orchestration.finalTaskId" class="tag final">最终</span>
                    <span class="task-agent">{{ t.agent }}</span>
                  </span>
                  <span class="task-desc">{{ summary(t) }}</span>
                </button>
              </li>
            </ul>

            <!-- 版本时间线（doc/72 §8.2）：逐版展示原命令；修订由协调者工具或 API 触发，页面不画 JSON 编辑器。 -->
            <section class="sec" data-testid="orch-revisions">
              <h4>计划修订 <span class="count">{{ revisions.length }}</span></h4>
              <p v-if="revisionsLoading" class="muted">正在读取…</p>
              <p v-else-if="!revisions.length" class="muted">还没有修订；组按创建时的计划执行。</p>
              <ol v-else class="rev-list">
                <li v-for="r in revisions" :key="r.revisionId" class="rev" :data-rev="r.revision">
                  <div class="rev-head">
                    <strong>v{{ r.revision }}</strong>
                    <span class="muted">基于 v{{ r.baseRevision }}</span>
                    <span class="muted">{{ fmtWhen(r.createdAt) }}</span>
                  </div>
                  <p class="rev-reason">{{ r.reason }}</p>
                  <p v-if="r.addTasks.length" class="muted rev-line">
                    新增：
                    <code v-for="t in r.addTasks" :key="t.key">{{ t.key }}</code>
                  </p>
                  <p v-if="r.addLinks.length" class="muted rev-line">
                    依赖：
                    <span v-for="l in r.addLinks" :key="`${l.parent}>${l.child}`" class="rev-link">
                      <code>{{ l.parent }}</code> → <code>{{ l.child }}</code>
                    </span>
                  </p>
                  <p v-if="r.cancelTasks.length" class="muted rev-line">
                    取消：
                    <code v-for="k in r.cancelTasks" :key="k">{{ k }}</code>
                  </p>
                  <p v-if="r.finalTaskKey" class="muted rev-line">
                    最终节点改为：<code>{{ r.finalTaskKey }}</code>
                  </p>
                  <p v-if="r.sourceRunId" class="muted rev-line">来源 run {{ r.sourceRunId }}</p>
                </li>
              </ol>
            </section>

            <!-- 路径账本（doc/82 §10.3）：程序从持久记录派生的只读投影，页面不从模型回答重建。
                 概览计数 + 时间线 + 计划差异 + 继 handoff，全部照端点的 bytes 呈现。 -->
            <section class="sec" data-testid="orch-journey">
              <h4>路径账本</h4>
              <p v-if="journeyLoading" class="muted">正在读取…</p>
              <p v-else-if="journeyError" class="banner error" role="alert">{{ journeyError }}</p>
              <template v-else-if="journey">
                <p class="muted journey-sum" data-testid="orch-journey-summary">
                  计划版本 {{ journey.summary.planRevisions }} · 节点 {{ journey.summary.tasksCreated }}
                  （成功 {{ journey.summary.succeeded }}／失败 {{ journey.summary.failed }}／取消 {{ journey.summary.cancelled }}／受阻 {{ journey.summary.blocked }}）
                  · 复用 {{ journey.summary.reusedResults }} · 继任组 {{ journey.summary.successorGroups }}
                  · 交付尝试 {{ journey.summary.deliveryAttempts }}
                </p>
                <ol v-if="journeyRows.length" class="rev-list" data-testid="orch-journey-steps">
                  <li v-for="row in journeyRows" :key="row.id" class="rev" :class="`k-${row.kind}`">
                    <div class="rev-head">
                      <strong>{{ row.label }}</strong>
                      <span v-if="row.at" class="muted">{{ fmtWhen(row.at) }}</span>
                    </div>
                    <p v-if="row.text" class="rev-reason">{{ row.text }}</p>
                    <p v-if="row.added.length" class="muted rev-line">
                      新增：
                      <code v-for="k in row.added" :key="k">{{ k }}</code>
                    </p>
                    <p v-if="row.cancelled.length" class="muted rev-line">
                      取消：
                      <code v-for="k in row.cancelled" :key="k">{{ k }}</code>
                    </p>
                  </li>
                </ol>
                <p v-else class="muted">还没有可呈现的账本记录。</p>
                <p v-if="journey.relatedOrchestrationIds.length" class="muted journey-related">
                  关联组：
                  <button
                    v-for="rid in journey.relatedOrchestrationIds"
                    :key="rid"
                    type="button"
                    class="orch-link"
                    data-testid="orch-journey-related"
                    @click="openOrch(rid)"
                  >{{ rid }}</button>
                </p>
              </template>
            </section>
            <p v-if="detailLoading" class="muted">正在读取…</p>
          </template>

          <!-- —— 任务详情（doc/70 §11.3）—— -->
          <template v-else-if="detail">
            <header class="sheet-head">
              <div>
                <h3><code>{{ detail.key }}</code></h3>
                <p class="muted">
                  <span class="tag" :class="detail.status" data-testid="task-status">{{ STATUS_LABEL[detail.status] ?? detail.status }}</span>
                  <span v-if="detail.planCheckpoint" class="tag hold" data-testid="task-plan-checkpoint">检查点</span>
                  <span class="tag ws" data-testid="task-workspace-mode">{{ detail.workspace ? '隔离工作区' : '共享工作区' }}</span>
                  · 执行者 {{ detail.agent }}
                  <template v-if="detail.currentRunId"> · run {{ detail.currentRunId }}</template>
                </p>
              </div>
              <div class="sheet-actions">
                <button
                  v-if="stoppable(detail)"
                  type="button"
                  class="danger"
                  :disabled="busy"
                  data-testid="task-stop"
                  @click="pendingStopTask = detail"
                >停止</button>
                <button
                  v-if="detail.status === 'interrupted'"
                  type="button"
                  class="primary"
                  :disabled="busy"
                  data-testid="task-resume"
                  @click="resume(detail)"
                >继续执行</button>
                <button type="button" @click="closeDetail">关闭</button>
              </div>
            </header>

            <!-- 发布三码的错误正文是 Provider 诊断（可能很长）：不在顶层横幅展开，
                 收进发布面板的折叠详情（施工包 §11.2）；其余错误照旧横幅。 -->
            <p v-if="detail.error && !isPublicationError(detail.error.code)" class="task-error" role="alert">
              <strong>{{ detail.error.message }}</strong>
              <code class="err-code">{{ detail.error.code }}</code>
            </p>

            <!-- 工作区与发布面板（doc/74 §11.3）：只有隔离组有 workspace 读面。 -->
            <section v-if="detail.workspace" class="sec ws-panel" data-testid="task-workspace">
              <h4>工作区与发布</h4>
              <p class="muted">
                {{ PROVIDER_LABEL[detail.workspace.provider] ?? detail.workspace.provider }}
                · 验证：<span data-testid="task-verify-status">{{ verificationText(detail.workspace) }}</span>
                <template v-if="detail.workspace.verification.status === 'failed' && detail.workspace.verification.outputPath">
                  · 输出位置 <code>{{ detail.workspace.verification.outputPath }}</code>（相对组目录）
                </template>
              </p>
              <p class="muted">
                发布：<span class="tag" :class="`p-${detail.workspace.publication.status || 'none'}`" data-testid="task-pub-status">{{ PUB_LABEL[detail.workspace.publication.status] ?? detail.workspace.publication.status }}</span>
                <template v-if="detail.workspace.publication.epoch"> · 轮次 {{ detail.workspace.publication.epoch }}</template>
                <strong
                  v-if="detail.workspace.publication.status === 'blocked' && detail.workspace.publication.errorCode"
                  class="pub-reason"
                  data-testid="task-pub-reason"
                >{{ PUB_ERR_LABEL[detail.workspace.publication.errorCode] ?? detail.workspace.publication.errorCode }}</strong>
              </p>
              <template v-if="detail.workspace.publication.status === 'blocked'">
                <p class="muted">
                  结果已保存，写回项目没有完成。处理上面的原因后重新发布；父任务不会重跑。
                </p>
                <details v-if="detail.error" class="ws-diag">
                  <summary>Provider 诊断详情</summary>
                  <pre class="body">{{ detail.error.message }}</pre>
                </details>
                <button
                  type="button"
                  class="primary ws-republish"
                  :disabled="republishBusy"
                  data-testid="task-republish"
                  @click="republish(detail)"
                >{{ republishBusy ? '正在创建处理执行…' : '重新发布' }}</button>
              </template>
            </section>

            <section class="sec">
              <h4>指令</h4>
              <pre class="body">{{ detail.content }}</pre>
            </section>

            <!-- 问题区（doc/72 §5）：FIFO，一次只放开最早一张；answered／stale 只读。 -->
            <section v-if="questionsLoading || questions.length" class="sec" data-testid="task-questions">
              <h4>问题 <span class="count">{{ pendingQuestions.length }}</span></h4>
              <p v-if="questionsLoading" class="muted">正在读取…</p>
              <ul v-else class="q-list">
                <li
                  v-for="q in pendingQuestions"
                  :key="q.questionId"
                  class="q-card"
                  :class="{ active: activeQuestion?.questionId === q.questionId, focused: focusedQuestionId === q.questionId }"
                  :data-question="q.questionId"
                >
                  <div class="q-head">
                    <span class="tag pending">等回答</span>
                    <span class="muted">{{ fmtWhen(q.createdAt) }}</span>
                    <span v-if="q.expiresAt" class="muted">截止 {{ fmtWhen(q.expiresAt) }}</span>
                  </div>
                  <p class="q-text">{{ q.question }}</p>
                  <template v-if="activeQuestion?.questionId === q.questionId">
                    <div v-if="activeOptions.length" class="q-options">
                      <button
                        v-for="(opt, i) in activeOptions"
                        :key="opt"
                        type="button"
                        class="q-option"
                        :class="{ on: answerPick === i }"
                        @click="answerPick = answerPick === i ? -1 : i"
                      >{{ opt }}</button>
                    </div>
                    <textarea
                      v-model="answerDraft"
                      class="q-input"
                      rows="2"
                      placeholder="输入回答；也可以点上面的选项"
                    ></textarea>
                    <footer class="q-foot">
                      <span class="muted">提交后这一问就定了，不能再改。</span>
                      <button
                        type="button"
                        class="primary"
                        :disabled="answerBusy || (!answerDraft.trim() && answerPick < 0)"
                        @click="submitTaskAnswer(q)"
                      >提交回答</button>
                    </footer>
                  </template>
                  <p v-else class="muted">前面还有待答问题，按提交顺序依次回答。</p>
                </li>
                <li v-for="q in answeredQuestions" :key="q.questionId" class="q-card done" :data-question="q.questionId">
                  <div class="q-head">
                    <span class="tag" :class="q.status === 'answered' ? 'succeeded' : 'cancelled'">
                      {{ q.status === 'answered' ? '已回答' : '已过期' }}
                    </span>
                    <span class="muted">{{ fmtWhen(q.createdAt) }}</span>
                  </div>
                  <p class="q-text">{{ q.question }}</p>
                  <p v-if="q.answer" class="q-answer">{{ q.answer }}</p>
                </li>
              </ul>
              <p v-if="pendingQuestions.length && !canAnswer" class="muted">
                任务已不在执行，遗留问题不能再回答。
              </p>
            </section>

            <section v-if="detail.result" class="sec" data-testid="task-result">
              <h4>结果 <span class="muted">（revision {{ detail.resultRevision }}）</span></h4>
              <p class="result-summary">{{ detail.result.summary }}</p>
              <div v-if="detail.result.artifacts.length" class="artifacts">
                <!-- 隔离结果的产物按不可变版本读取；shared 仍在项目目录里（施工包 §11.2）。 -->
                <p v-if="detail.workspace" class="muted">产物（{{ artifactName(detail.result) }}，该结果版本）：</p>
                <p v-else class="muted">产物（{{ artifactName(detail.result) }}，存放在项目目录里）：</p>
                <ul>
                  <li v-for="a in detail.result.artifacts" :key="a.path">
                    <button type="button" class="artifact" @click="downloadArtifact(detail!, a.path)">
                      {{ a.path }}
                    </button>
                    <span class="muted">{{ a.kind }}</span>
                    <button
                      v-if="detail.workspace"
                      type="button"
                      class="artifact alt"
                      data-testid="task-project-file"
                      @click="downloadProjectFile(detail!, a.path)"
                    >查看项目当前文件</button>
                  </li>
                </ul>
              </div>
            </section>

            <dl class="facts">
              <dt>所属组</dt>
              <dd>
                <button type="button" class="orch-link" data-testid="task-orch-link" @click="openOrch(detail.orchestrationId)">
                  查看编排组
                </button>
              </dd>
              <dt>创建</dt>
              <dd>
                {{ fmtWhen(detail.createdAt) }}
                <span class="muted">（计划 v{{ detail.createdRevision }}）</span>
              </dd>
              <dt>检查点</dt>
              <dd>{{ detail.planCheckpoint ? '完成后暂停组，等计划复核' : '否' }}</dd>
              <dt>开始</dt>
              <dd>{{ fmtWhen(detail.startedAt) }}</dd>
              <dt>结束</dt>
              <dd>{{ fmtWhen(detail.finishedAt) }}</dd>
            </dl>
            <p v-if="detailLoading" class="muted">正在读取…</p>
          </template>
        </aside>
      </div>
    </Teleport>

    <ConfirmDialog
      v-if="pendingStopTask"
      title="停止这件任务？"
      :body="`「${pendingStopTask.key}」会被取消：排队中的不再执行，正在跑的会被叫停。取消后不能恢复执行。`"
      confirm-text="停止任务"
      :busy="busy"
      :error="banner"
      @confirm="stopConfirmed"
      @cancel="pendingStopTask = null"
    />
    <ConfirmDialog
      v-if="pendingStopOrch"
      title="停止整个编排组？"
      :body="`这个目标下的全部任务都会被取消：排队中的不再执行，正在跑的会被叫停。已完成的任务结果保留。取消后不能恢复执行。`"
      confirm-text="停止整组"
      :busy="busy"
      :error="banner"
      @confirm="stopConfirmed"
      @cancel="pendingStopOrch = null"
    />
  </div>
</template>

<style scoped>
.tasks {
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
/* 嵌入对话页右栏（doc/77 / doc/78）：外层 .dock-pane 负责滚动，这里不再自己滚。 */
.tasks.embedded {
  flex: none;
  width: auto;
  max-width: none;
  margin: 0;
  padding: 0 0.6rem 1rem;
  overflow: visible;
  gap: var(--space-2);
}
/* 列表头一行（doc/80）：标题 + 两档切换 + 筛选框，同 ToolsView/SkillsView 的写法。 */
.list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  flex-wrap: wrap;
}
.list-head input { width: min(100%, 16rem); }
.tasks.embedded .list-head {
  padding: 0 0.15rem;
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
h4 { margin: 0 0 0.35rem; font-size: var(--text-sm); font-weight: 600; }
.hint, .muted {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-sm);
}
.head-actions select {
  font-size: var(--text-sm);
  padding: 0.4rem 0.6rem;
}
.count { color: var(--muted); font-weight: 400; font-size: var(--text-sm); }
/* 两档切换（doc/80 §4.3）：本会话 / 本项目全部，放在列表头那一行。 */
.scope-switch {
  display: inline-flex;
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 999px;
  overflow: hidden;
}
.scope-switch button {
  border: 0;
  background: none;
  color: var(--muted);
  font: inherit;
  font-size: var(--text-xs);
  padding: 0.25rem 0.7rem;
  cursor: pointer;
}
.scope-switch button:hover:not(:disabled) { color: var(--ink); }
.scope-switch button.on {
  background: var(--accent-soft, #eef2ff);
  color: var(--accent-ink, #3730a3);
}
.scope-switch button:disabled { opacity: 0.45; cursor: default; }
.linkish {
  border: 0;
  background: none;
  color: var(--accent-ink, #3730a3);
  font: inherit;
  padding: 0 0.15rem;
  cursor: pointer;
  text-decoration: underline;
}

/* 命令分组（doc/82 §10.2）：行=命令摘要+状态+来源，展开=尾部输出+全文路径。 */
.job-list {
  list-style: none;
  margin: 0.4rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.job-main {
  display: flex;
  align-items: stretch;
  gap: 0.5rem;
}
.job-toggle {
  flex: 1;
  min-width: 0;
  text-align: left;
  background: none;
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 10px;
  padding: 0.6rem 0.8rem;
  cursor: pointer;
  font: inherit;
  color: inherit;
}
.job-toggle:hover { border-color: var(--accent, #6366f1); }
.job-title {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  min-width: 0;
}
.job-cmd {
  font-weight: 600;
  overflow-wrap: anywhere;
}
.job-stop { align-self: center; }
.job-detail {
  border: 1px solid var(--line, #e5e7eb);
  border-top: 0;
  border-radius: 0 0 10px 10px;
  padding: 0.6rem 0.8rem;
  margin-top: -1px;
}
.job-out {
  max-height: 16rem;
  overflow: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  margin: 0.2rem 0 0.5rem;
}
.job-out.err { color: #b91c1c; }
.job-paths { overflow-wrap: anywhere; }

/* 账本时间线（doc/82 §10.3）：与计划修订同一个 ol 皮肤，按步骤类型加一点色彩区分。 */
.journey-sum { overflow-wrap: anywhere; }
.journey-related { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; }
.rev.k-task { border-left: 3px solid var(--accent, #6366f1); }
.rev.k-successor { border-left: 3px solid #b45309; }
.rev.k-delivery { border-left: 3px solid #0e7490; }

.task-list {
  list-style: none;
  margin: 0.4rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.task-main {
  display: block;
  width: 100%;
  text-align: left;
  background: none;
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 10px;
  padding: 0.65rem 0.8rem;
  cursor: pointer;
  font: inherit;
  color: inherit;
}
.task-main:hover { border-color: var(--accent, #6366f1); }
.task-title {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}
.task-key { font-weight: 600; }
.task-agent { color: var(--muted); font-size: var(--text-xs); }
.delivery-err { color: #b91c1c; font-size: var(--text-xs); }
.task-desc {
  display: block;
  margin-top: 0.25rem;
  font-size: var(--text-sm);
  color: var(--muted);
}
.meta {
  display: flex;
  gap: 0.9rem;
  margin-top: 0.3rem;
  color: var(--muted);
  font-size: var(--text-xs);
}

/* 状态：一个词一个底色，可区分、不装饰（doc/70 §11.5）。 */
.tag {
  display: inline-block;
  padding: 0.05rem 0.5rem;
  border-radius: 999px;
  font-size: var(--text-xs);
  background: var(--bg-soft, #f3f4f6);
  color: var(--muted);
}
.tag.pending, .tag.ready { background: #eef2ff; color: #4338ca; }
.tag.running { background: #e0f2fe; color: #0369a1; }
.tag.interrupted { background: #fef3c7; color: #92400e; }
.tag.succeeded { background: #dcfce7; color: #15803d; }
.tag.failed { background: #fee2e2; color: #b91c1c; }
.tag.blocked { background: #f3e8ff; color: #7e22ce; }
.tag.cancelled { background: var(--bg-soft, #f3f4f6); color: var(--muted); }
.tag.active { background: #e0f2fe; color: #0369a1; }
.tag.cancelling { background: #fef3c7; color: #92400e; }
/* doc/75 §4.2：继任组出生与等待原组两个未收口状态（都不可调度）。 */
.tag.preparing { background: #fef3c7; color: #92400e; }
.tag.waiting_previous { background: #eef2ff; color: #4338ca; }
.tag.d-delivered { background: #dcfce7; color: #15803d; }
.tag.d-delivering { background: #e0f2fe; color: #0369a1; }
.tag.d-pending { background: #fef3c7; color: #92400e; }
.tag.d-failed { background: #fee2e2; color: #b91c1c; }
.tag.final { background: #fff7ed; color: #c2410c; }
.tag.hold { background: #fef3c7; color: #92400e; }
.tag.ws { background: #eef2ff; color: #4338ca; }
.tag.p-none { background: var(--bg-soft, #f3f4f6); color: var(--muted); }
.tag.p-pending { background: #fef3c7; color: #92400e; }
.tag.p-publishing { background: #e0f2fe; color: #0369a1; }
.tag.p-published { background: #dcfce7; color: #15803d; }
.tag.p-blocked { background: #fee2e2; color: #b91c1c; }
.orch-rev { margin-left: 0.15rem; }
.final-node { margin: 0; font-size: var(--text-sm); }
.final-node code { font-size: var(--text-xs); }

.sheet-overlay {
  position: fixed;
  inset: 0;
  background: rgb(0 0 0 / 0.32);
  display: flex;
  justify-content: flex-end;
  z-index: 60;
}
.sheet {
  width: min(34rem, 92vw);
  height: 100%;
  overflow: auto;
  background: var(--bg, #fff);
  padding: 1.2rem 1.4rem 2rem;
  display: flex;
  flex-direction: column;
  gap: 0.9rem;
}
.sheet-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 0.8rem;
}
.sheet-actions {
  display: flex;
  gap: 0.5rem;
  flex-shrink: 0;
}
.orch-goal { margin: 0; font-size: var(--text-sm); }
.dep-map ul {
  list-style: none;
  margin: 0.2rem 0 0;
  padding: 0;
  font-size: var(--text-sm);
  display: flex;
  flex-direction: column;
  gap: 0.15rem;
}
.task-list.in-sheet .task-main { padding: 0.5rem 0.7rem; }

.task-error {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border-radius: 10px;
  background: #fee2e2;
  color: #b91c1c;
  font-size: var(--text-sm);
}
.err-code { margin-left: 0.5rem; opacity: 0.75; }
.sec .body {
  margin: 0;
  padding: 0.65rem 0.8rem;
  border-radius: 10px;
  background: var(--bg-soft, #f6f7f9);
  font-size: var(--text-sm);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 16rem;
  overflow: auto;
}
.result-summary {
  margin: 0;
  font-size: var(--text-sm);
  white-space: pre-wrap;
  word-break: break-word;
}
.artifacts ul {
  list-style: none;
  margin: 0.3rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  font-size: var(--text-sm);
}
.artifact {
  background: none;
  border: none;
  padding: 0;
  font: inherit;
  color: var(--accent, #4f46e5);
  text-decoration: underline;
  cursor: pointer;
}
.artifact.alt { color: var(--muted); font-size: var(--text-xs); }

/* 工作区与发布面板：受阻时是处理入口，诊断默认折叠（施工包 §11.2）。 */
.ws-panel {
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 10px;
  padding: 0.7rem 0.9rem;
}
.ws-panel p { margin: 0.25rem 0; }
.pub-reason { color: #b91c1c; margin-left: 0.35rem; }
.ws-diag { margin: 0.35rem 0; font-size: var(--text-sm); }
.ws-diag summary { cursor: pointer; color: var(--muted); }
.ws-diag .body { margin-top: 0.35rem; max-height: 12rem; }
.ws-republish { margin-top: 0.35rem; }
.facts {
  display: grid;
  grid-template-columns: 4.5rem 1fr;
  gap: 0.3rem 0.8rem;
  margin: 0;
  font-size: var(--text-sm);
}
.facts dt { color: var(--muted); }
.facts dd { margin: 0; }
.orch-link {
  background: none;
  border: none;
  padding: 0;
  font: inherit;
  color: var(--accent, #4f46e5);
  text-decoration: underline;
  cursor: pointer;
}

/* 重规划交接（doc/75 §9.2）：正文是程序渲染的 Markdown，样式与聊天气泡同族。 */
.handoff-panel {
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 10px;
  padding: 0.7rem 0.9rem;
}
.handoff-panel p { margin: 0.25rem 0; }
.handoff-reuse-item { margin-right: 0.6rem; }
.handoff-body {
  margin-top: 0.45rem;
  padding: 0.65rem 0.8rem;
  border-radius: 10px;
  background: var(--bg-soft, #f6f7f9);
  font-size: var(--text-sm);
  word-break: break-word;
  max-height: 22rem;
  overflow: auto;
}
.handoff-body :deep(h1),
.handoff-body :deep(h2),
.handoff-body :deep(h3) {
  margin: 0.6rem 0 0.3rem;
  line-height: 1.35;
}
.handoff-body :deep(h1) { font-size: 1.1rem; }
.handoff-body :deep(h2) { font-size: 1rem; }
.handoff-body :deep(h3) { font-size: 0.95rem; }
.handoff-body :deep(p) { margin: 0.3rem 0; }
.handoff-body :deep(ul),
.handoff-body :deep(ol) { margin: 0.3rem 0; padding-left: 1.3rem; }
.handoff-body :deep(li) { margin: 0.1rem 0; }
.handoff-body :deep(code) {
  background: rgba(0, 0, 0, 0.05);
  border-radius: 4px;
  padding: 0.05rem 0.25rem;
  font-size: 0.92em;
}
.handoff-body :deep(pre) {
  background: rgba(0, 0, 0, 0.06);
  border-radius: 8px;
  padding: 0.5rem 0.6rem;
  overflow: auto;
}

/* 修订时间线：一条版本一行块，命令原样展示（doc/72 §8.2）。 */
.rev-list {
  list-style: none;
  margin: 0.3rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.rev {
  border-left: 2px solid var(--line, #e5e7eb);
  padding: 0.1rem 0 0.1rem 0.7rem;
}
.rev-head {
  display: flex;
  align-items: baseline;
  gap: 0.6rem;
  font-size: var(--text-sm);
}
.rev-reason {
  margin: 0.2rem 0;
  font-size: var(--text-sm);
  white-space: pre-wrap;
  word-break: break-word;
}
.rev-line {
  margin: 0.1rem 0;
  font-size: var(--text-xs);
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0.35rem;
}
.rev-line code { font-size: var(--text-xs); }
.rev-link { display: inline-flex; gap: 0.25rem; }

/* Task 问题卡：主会话问询面板的简化版（doc/72 §8.2）。 */
.q-list {
  list-style: none;
  margin: 0.3rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.55rem;
}
.q-card {
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 10px;
  padding: 0.65rem 0.8rem;
  display: flex;
  flex-direction: column;
  gap: 0.45rem;
}
.q-card.active { border-color: var(--accent, #6366f1); }
.q-card.focused { box-shadow: 0 0 0 3px var(--accent-soft, #eef2ff); }
.q-card.done { background: var(--bg-soft, #f6f7f9); }
.q-head {
  display: flex;
  align-items: baseline;
  gap: 0.6rem;
  font-size: var(--text-xs);
}
.q-text {
  margin: 0;
  font-size: var(--text-sm);
  white-space: pre-wrap;
  word-break: break-word;
}
.q-answer {
  margin: 0;
  font-size: var(--text-sm);
  color: var(--ink-soft, #374151);
  white-space: pre-wrap;
  word-break: break-word;
}
.q-options {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
}
.q-option {
  text-align: left;
  font: inherit;
  font-size: var(--text-sm);
  padding: 0.4rem 0.6rem;
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 8px;
  background: transparent;
  color: inherit;
  cursor: pointer;
}
.q-option:hover { border-color: var(--accent, #6366f1); }
.q-option.on {
  border-color: var(--accent, #6366f1);
  background: var(--accent-soft, #eef2ff);
}
.q-input {
  width: 100%;
  box-sizing: border-box;
  font: inherit;
  font-size: var(--text-sm);
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 8px;
  resize: vertical;
}
.q-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.6rem;
  font-size: var(--text-xs);
}
</style>
