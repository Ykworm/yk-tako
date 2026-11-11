<script setup lang="ts">
// 协调通知的恢复面板（doc/73 §7.2）：通知状态、人读错误、结果摘要与产物、每次处理 attempt，
// 以及「重新处理」。它只消费服务端真源：事件到了就重拉，不在本地把失败标成已读。
//
// 重试成功后进入来源会话观察当前 run（复用既有轮询），不另写交付轮询。
import { onBeforeUnmount, ref, watch } from 'vue'
import {
  ApiError,
  CODE_ARTIFACT_INVALID,
  CODE_NOTIFICATION_NOT_RETRYABLE,
  getNotification,
  getOrchestration,
  retryNotification,
  workspaceFileUrl,
  type NotificationDetail,
} from './api'
import { authFetch } from './auth'
import { onMutation } from './mutation'

const props = defineProps<{ notificationId: string }>()
const emit = defineEmits<{
  close: []
  openSession: [sessionId: string]
  changed: []
}>()

const detail = ref<NotificationDetail | null>(null)
const sourceSessionId = ref('')
const workspaceId = ref('')
const phase = ref<'loading' | 'ready' | 'error'>('loading')
const error = ref('')
const notice = ref('')
const retrying = ref(false)

const KIND_LABEL: Record<string, string> = {
  task_failed: 'Task 失败',
  task_interrupted: 'Task 中断',
  orchestration_finished: '整组完成交付',
  plan_review_required: '计划待决策',
}
const NOTIF_STATUS_LABEL: Record<string, string> = {
  pending: '待处理',
  claimed: '处理中',
  consumed: '已处理',
  failed: '失败',
}
const RUN_STATUS_LABEL: Record<string, string> = {
  running: '执行中',
  complete: '已完成',
  error: '出错',
  stopped: '已停止',
  interrupted: '已中断',
}
// 机器原因词表（doc/73 §4.5）：为什么这次消费没完成。
const ERR_LABEL: Record<string, string> = {
  delivery_run_error: '交付执行出错',
  delivery_stopped: '交付被停止',
  artifact_invalid: '结果产物已不可读取',
}

function errText(code: string): string {
  return ERR_LABEL[code] ?? code
}

async function load() {
  if (!props.notificationId) return
  phase.value = 'loading'
  error.value = ''
  try {
    const n = await getNotification(props.notificationId)
    detail.value = n
    phase.value = 'ready'
    // 来源会话与项目根不在通知详情里：从编排组视图取（产物下载要用项目 id）。
    try {
      const o = await getOrchestration(n.orchestrationId)
      sourceSessionId.value = o.orchestration.sourceSessionId
      workspaceId.value = o.orchestration.workspaceId
    } catch {
      sourceSessionId.value = ''
      workspaceId.value = ''
    }
  } catch (e) {
    detail.value = null
    phase.value = 'error'
    error.value = e instanceof ApiError ? e.message : '通知没拉到'
  }
}

watch(() => props.notificationId, () => void load(), { immediate: true })

const offMutation = onMutation((evt) => {
  const n = detail.value
  if (!n) return
  if (evt.type === 'agent_orchestrations_changed' && evt.id === n.orchestrationId) void load()
  if (evt.type === 'agent_tasks_changed' && n.taskId && evt.id === n.taskId) void load()
})
onBeforeUnmount(offMutation)

async function retry() {
  const n = detail.value
  if (!n || n.status !== 'failed' || retrying.value) return
  retrying.value = true
  notice.value = ''
  error.value = ''
  try {
    const r = await retryNotification(n.notificationId)
    notice.value =
      r.status === 'consumed'
        ? '这次计划决策已经被接受，通知已收口，不需要再处理。'
        : '已创建恢复执行，正在进入来源会话。'
    await load()
    emit('changed')
    // 200 已有在处理的 run 与 202 新尝试都进来源会话看同一个 run；consumed 不跳。
    if (r.status === 'claimed' && r.runId && sourceSessionId.value) {
      emit('openSession', sourceSessionId.value)
    }
  } catch (e) {
    const api = e instanceof ApiError ? e : null
    if (api?.code === CODE_NOTIFICATION_NOT_RETRYABLE) {
      // 状态已经变了（别的 tab 抢先、已消费、或 run 已终态）：重拉显示服务端现状。
      notice.value = api.message || '这条通知现在不能重新处理'
      await load()
      emit('changed')
    } else if (api?.code === CODE_ARTIFACT_INVALID) {
      // 产物失效不伪装成已恢复：保留说明，通知仍是 failed。
      // load() 开头会清 error，说明要在重拉之后写，否则用户看不到这条反馈。
      await load()
      error.value = api.message || '结果产物已不可读取'
    } else {
      error.value = api?.message || '重新处理失败，请稍后再试'
    }
  } finally {
    retrying.value = false
  }
}

async function downloadArtifact(path: string) {
  if (!workspaceId.value) {
    error.value = '项目信息没拉到，暂时取不到产物'
    return
  }
  try {
    const res = await authFetch(workspaceFileUrl(workspaceId.value, path))
    if (!res.ok) {
      error.value = `取不到 ${path}`
      return
    }
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = path.split('/').pop() || 'artifact'
    document.body.appendChild(a)
    a.click()
    a.remove()
    window.setTimeout(() => URL.revokeObjectURL(url), 10_000)
  } catch {
    error.value = `取不到 ${path}`
  }
}

function fmtWhen(iso: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
</script>

<template>
  <section class="notif card" data-testid="notification-recovery">
    <header class="notif-head">
      <div>
        <h3>协调通知</h3>
        <p v-if="detail" class="muted">
          <span class="tag" :class="`n-${detail.status}`">{{ NOTIF_STATUS_LABEL[detail.status] ?? detail.status }}</span>
          · {{ KIND_LABEL[detail.kind] ?? detail.kind }}
          <template v-if="detail.attempt"> · 当前处理尝试 {{ detail.attempt }}</template>
        </p>
      </div>
      <div class="notif-actions">
        <button
          v-if="detail?.status === 'failed'"
          type="button"
          class="primary"
          :disabled="retrying"
          data-testid="notification-retry"
          @click="retry"
        >{{ retrying ? '正在创建恢复执行…' : '重新处理' }}</button>
        <button
          v-if="sourceSessionId"
          type="button"
          data-testid="notification-session"
          @click="emit('openSession', sourceSessionId)"
        >进入来源会话</button>
        <button type="button" @click="emit('close')">关闭</button>
      </div>
    </header>

    <p v-if="phase === 'loading'" class="muted">正在读取…</p>
    <p v-else-if="phase === 'error'" class="banner error" role="alert">{{ error }}</p>
    <template v-else-if="detail">
      <p v-if="notice" class="banner ok" role="status">{{ notice }}</p>
      <p v-if="error" class="banner error" role="alert">{{ error }}</p>

      <p v-if="detail.errorCode" class="muted notif-error" data-testid="notification-error">
        失败原因：<strong>{{ errText(detail.errorCode) }}</strong>
        <code>{{ detail.errorCode }}</code>
      </p>
      <p class="muted">
        通知 {{ fmtWhen(detail.createdAt) }}
        <template v-if="detail.claimedAt"> · 领取 {{ fmtWhen(detail.claimedAt) }}</template>
        <template v-if="detail.failedAt"> · 失败 {{ fmtWhen(detail.failedAt) }}</template>
        <template v-if="detail.consumedAt"> · 处理完 {{ fmtWhen(detail.consumedAt) }}</template>
      </p>

      <section v-if="detail.result" class="notif-result" data-testid="notification-result">
        <h4>结果定位</h4>
        <p class="muted">
          最终节点 <code>{{ detail.result.finalTaskId }}</code> · 结果版本 v{{ detail.result.resultRevision }}
        </p>
        <p class="notif-summary">{{ detail.result.summary }}</p>
        <ul v-if="detail.result.artifacts.length" class="notif-artifacts">
          <li v-for="a in detail.result.artifacts" :key="a.path">
            <button type="button" class="link" @click="downloadArtifact(a.path)">
              {{ a.path }}
            </button>
            <span class="muted">{{ a.kind }}</span>
          </li>
        </ul>
        <p v-else class="muted">这份结果没有声明产物。</p>
      </section>

      <section class="notif-attempts" data-testid="notification-attempts">
        <h4>处理历史 <span class="count">{{ detail.attempts.length }}</span></h4>
        <p v-if="!detail.attempts.length" class="muted">还没有处理过。</p>
        <ul v-else>
          <li v-for="a in detail.attempts" :key="a.runId">
            <strong>#{{ a.attempt }}</strong>
            <code>{{ a.runId }}</code>
            <span class="tag" :class="`r-${a.status}`">{{ RUN_STATUS_LABEL[a.status] ?? (a.status || '未知') }}</span>
            <span class="muted">{{ fmtWhen(a.createdAt) }}</span>
          </li>
        </ul>
      </section>
    </template>
  </section>
</template>

<style scoped>
.notif {
  border: 1px solid var(--line, #e5e7eb);
  border-radius: 10px;
  padding: 16px 18px;
  background: var(--surface, #fff);
  margin-bottom: 16px;
}
.notif-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
}
.notif-head h3 {
  margin: 0 0 4px;
  font-size: 15px;
}
.notif-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.notif-actions button {
  border: 1px solid var(--line, #e5e7eb);
  background: var(--surface, #fff);
  border-radius: 6px;
  padding: 5px 10px;
  font-size: 13px;
  cursor: pointer;
}
.notif-actions .primary {
  background: #2563eb;
  border-color: #2563eb;
  color: #fff;
}
.notif-actions .primary:disabled {
  opacity: 0.6;
  cursor: default;
}
.notif-result,
.notif-attempts {
  margin-top: 14px;
}
.notif-result h4,
.notif-attempts h4 {
  margin: 0 0 6px;
  font-size: 13px;
}
.notif-summary {
  white-space: pre-wrap;
  margin: 6px 0;
}
.notif-artifacts {
  list-style: none;
  padding: 0;
  margin: 6px 0;
}
.notif-artifacts li {
  display: flex;
  gap: 8px;
  align-items: baseline;
  padding: 2px 0;
}
.notif-artifacts .link {
  border: none;
  background: none;
  color: #2563eb;
  cursor: pointer;
  padding: 0;
  font-family: inherit;
  font-size: 13px;
  text-decoration: underline;
}
.notif-attempts ul {
  list-style: none;
  padding: 0;
  margin: 0;
}
.notif-attempts li {
  display: flex;
  gap: 10px;
  align-items: baseline;
  padding: 4px 0;
  font-size: 13px;
  flex-wrap: wrap;
}
.muted {
  color: var(--muted, #6b7280);
}
.count {
  color: var(--muted, #6b7280);
  font-weight: normal;
}
.banner {
  border-radius: 6px;
  padding: 8px 10px;
  font-size: 13px;
  margin: 10px 0;
}
.banner.error {
  background: #fee2e2;
  color: #b91c1c;
}
.banner.ok {
  background: #dcfce7;
  color: #15803d;
}
.tag {
  display: inline-block;
  border-radius: 999px;
  padding: 1px 8px;
  font-size: 12px;
  line-height: 18px;
}
.tag.n-pending,
.tag.n-claimed,
.tag.r-running {
  background: #e0f2fe;
  color: #0369a1;
}
.tag.n-consumed,
.tag.r-complete {
  background: #dcfce7;
  color: #15803d;
}
.tag.n-failed,
.tag.r-error {
  background: #fee2e2;
  color: #b91c1c;
}
.tag.r-stopped,
.tag.r-interrupted {
  background: #fef3c7;
  color: #92400e;
}
</style>
