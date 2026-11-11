<script setup lang="ts">
// 任务页（doc/70 §11）：项目选择器 → Task 列表 → 详情抽屉（含编排组视图）。
//
// 这一页是观测与干预面：列出本人项目下的持久 Task，看状态、结果与依赖，
// 在 interrupted 时恢复、在未完成时停止。创建不在页面表单里——Task 由对话里的
// 助手用 delegate_task / create_orchestration 派发（通用工具卡，doc/59），页面只消费
// 服务端真源；所有「变了」都靠事件重拉（agent_tasks_changed / agent_orchestrations_changed），
// 刷新或换标签页后仍从 PG 重建，不存本地状态。
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  ApiError,
  getAgentTask,
  getOrchestration,
  listWorkspaceTasks,
  listWorkspaces,
  resumeAgentTask,
  stopAgentTask,
  stopOrchestration,
  workspaceFileUrl,
  type AgentTask,
  type OrchestrationView,
  type TaskResult,
  type Workspace,
} from './api'
import { authFetch } from './auth'
import ConfirmDialog from './ConfirmDialog.vue'
import { onMutation } from './mutation'

const props = defineProps<{ activeWorkspaceId?: string | null }>()

const phase = ref<'loading' | 'ready' | 'error'>('loading')
const banner = ref('')
const notice = ref('')
const busy = ref(false)

const workspaces = ref<Workspace[]>([])
const workspaceId = ref('')

const tasks = ref<AgentTask[]>([])
const query = ref('')

// 详情抽屉：任务详情与编排组视图共用一个抽屉，组视图由详情里的「查看编排组」进入。
const detail = ref<AgentTask | null>(null)
const detailLoading = ref(false)
const orch = ref<OrchestrationView | null>(null)

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
    const res = await listWorkspaceTasks(workspaceId.value)
    tasks.value = res.tasks
    phase.value = 'ready'
  } catch (err) {
    phase.value = 'error'
    banner.value = err instanceof ApiError ? err.message : '任务列表没拉到'
  }
}

function fail(err: unknown, fallback: string) {
  if (err instanceof ApiError) {
    banner.value = err.message
    return
  }
  banner.value = fallback
}

watch(workspaceId, () => void loadTasks())

async function openDetail(row: AgentTask) {
  banner.value = ''
  orch.value = null
  detail.value = row
  detailLoading.value = true
  try {
    detail.value = await getAgentTask(row.taskId)
  } catch (err) {
    detail.value = null
    fail(err, '详情没拉到')
  } finally {
    detailLoading.value = false
  }
}

async function reloadDetail(taskId: string) {
  if (detail.value?.taskId !== taskId) return
  try {
    detail.value = await getAgentTask(taskId)
  } catch {
    /* 抽屉里那行旧的先留着；下一次事件或重开会再对齐 */
  }
}

async function reloadOrch(id: string) {
  if (orch.value?.orchestration.orchestrationId !== id) return
  try {
    orch.value = await getOrchestration(id)
  } catch {
    /* 同上 */
  }
}

function closeDetail() {
  detail.value = null
  orch.value = null
}

async function openOrch(id: string) {
  banner.value = ''
  detailLoading.value = true
  try {
    orch.value = await getOrchestration(id)
  } catch (err) {
    fail(err, '编排组没拉到')
  } finally {
    detailLoading.value = false
  }
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

// 产物下载复用项目文件出口（doc/48 B1 的字节面）：path 是相对项目根的路径，
// /api/media 要 Bearer 头，先 fetch 成 blob 再触发下载（不能直接 <a href>，会 401）。
async function downloadArtifact(task: AgentTask, artifactPath: string) {
  banner.value = ''
  try {
    const res = await authFetch(workspaceFileUrl(task.workspaceId, artifactPath))
    if (!res.ok) {
      banner.value = `取不到 ${artifactPath}`
      return
    }
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = artifactPath.split('/').pop() || 'artifact'
    document.body.appendChild(a)
    a.click()
    a.remove()
    // 留一会儿再回收：浏览器还在读这个 blob，立刻 revoke 会让下载拿到空文件。
    window.setTimeout(() => URL.revokeObjectURL(url), 10_000)
  } catch {
    banner.value = `取不到 ${artifactPath}`
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
const ORCH_STATUS_LABEL: Record<string, string> = {
  active: '进行中',
  succeeded: '已完成',
  failed: '失败',
  cancelling: '停止中',
  cancelled: '已取消',
}
const DELIVERY_LABEL: Record<string, string> = {
  pending: '待交付',
  delivering: '交付中',
  delivered: '已交付',
  failed: '交付失败',
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

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  void (async () => {
    try {
      await loadWorkspaces()
    } catch (err) {
      phase.value = 'error'
      fail(err, '项目清单没拉到')
      return
    }
    await loadTasks()
  })()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  offMutation()
  if (reloadDelay) clearTimeout(reloadDelay)
})
</script>

<template>
  <div class="tasks">
    <header class="page-head">
      <div>
        <p class="kicker">任务</p>
        <h2>交给助手的长活</h2>
        <p class="hint">
          任务由对话里的助手派发（delegate_task / create_orchestration），后台独立执行；
          中断的任务可以从这里接管继续，最终结果会交回来源对话。
        </p>
      </div>
      <div class="head-actions">
        <select v-model="workspaceId" aria-label="选择项目" data-testid="tasks-workspace">
          <option v-if="!workspaces.length" value="">（还没有项目）</option>
          <option v-for="w in workspaces" :key="w.id" :value="w.id">{{ w.name }}</option>
        </select>
      </div>
    </header>

    <p v-if="banner" class="banner error" role="alert">{{ banner }}</p>
    <p v-if="notice" class="banner ok" role="status">{{ notice }}</p>

    <section class="card">
      <div class="list-head">
        <h3>任务 <span class="count">{{ tasks.length }}</span></h3>
        <input v-model="query" type="search" placeholder="按 key 或内容筛选" aria-label="筛选任务" />
      </div>

      <p v-if="phase === 'loading'" class="muted">正在加载…</p>
      <p v-else-if="phase === 'error'" class="muted">列表没拉到，稍后重试。</p>
      <p v-else-if="!workspaceId" class="muted">先在上面选一个项目。</p>
      <p v-else-if="!filtered.length" class="muted">
        {{ tasks.length ? '没有匹配的任务。' : '这个项目还没有任务。在对话里让助手用任务功能派发。' }}
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
                  </template>
                </p>
              </div>
              <div class="sheet-actions">
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
            <p v-if="detailLoading" class="muted">正在读取…</p>
          </template>

          <!-- —— 任务详情（doc/70 §11.3）—— -->
          <template v-else-if="detail">
            <header class="sheet-head">
              <div>
                <h3><code>{{ detail.key }}</code></h3>
                <p class="muted">
                  <span class="tag" :class="detail.status" data-testid="task-status">{{ STATUS_LABEL[detail.status] ?? detail.status }}</span>
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

            <p v-if="detail.error" class="task-error" role="alert">
              <strong>{{ detail.error.message }}</strong>
              <code class="err-code">{{ detail.error.code }}</code>
            </p>

            <section class="sec">
              <h4>指令</h4>
              <pre class="body">{{ detail.content }}</pre>
            </section>

            <section v-if="detail.result" class="sec" data-testid="task-result">
              <h4>结果 <span class="muted">（revision {{ detail.resultRevision }}）</span></h4>
              <p class="result-summary">{{ detail.result.summary }}</p>
              <div v-if="detail.result.artifacts.length" class="artifacts">
                <p class="muted">产物（{{ artifactName(detail.result) }}，存放在项目目录里）：</p>
                <ul>
                  <li v-for="a in detail.result.artifacts" :key="a.path">
                    <button type="button" class="artifact" @click="downloadArtifact(detail!, a.path)">
                      {{ a.path }}
                    </button>
                    <span class="muted">{{ a.kind }}</span>
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
              <dd>{{ fmtWhen(detail.createdAt) }}</dd>
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
.tag.d-delivered { background: #dcfce7; color: #15803d; }
.tag.d-delivering { background: #e0f2fe; color: #0369a1; }
.tag.d-pending { background: #fef3c7; color: #92400e; }
.tag.d-failed { background: #fee2e2; color: #b91c1c; }
.tag.final { background: #fff7ed; color: #c2410c; }

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
</style>
