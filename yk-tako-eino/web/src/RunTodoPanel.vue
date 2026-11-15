<script setup lang="ts">
// Todo 标签（doc/78 §A6）：读 GET /api/runs/{id}/tasks（run_tasks 的只读口）。
// boardText 只是重拉信号——看板帧到了说明 run_tasks 可能变了，数据本身来自表。
import { computed, ref, watch } from 'vue'
import { ApiError, getRunTasks, type RunTaskRow } from './api'

const props = defineProps<{
  runId: string | null
  boardText: string
  reloadNonce: number
}>()

const rows = ref<RunTaskRow[]>([])
const total = ref(0)
const done = ref(0)
const loading = ref(false)
const error = ref('')
const loadedRun = ref('')

const percent = computed(() => (total.value ? Math.round((done.value / total.value) * 100) : 0))

function statusLabel(status: string): string {
  if (status === 'completed') return '已完成'
  if (status === 'in_progress') return '进行中'
  if (status === 'pending') return '待办'
  return status
}

async function load() {
  const runId = props.runId
  if (!runId) {
    rows.value = []
    total.value = 0
    done.value = 0
    error.value = ''
    loadedRun.value = ''
    loading.value = false
    return
  }
  loading.value = true
  error.value = ''
  try {
    const res = await getRunTasks(runId)
    if (props.runId !== runId) return
    rows.value = res.rows
    total.value = res.total
    done.value = res.done
    loadedRun.value = runId
  } catch (e) {
    if (props.runId !== runId) return
    rows.value = []
    total.value = 0
    done.value = 0
    error.value = e instanceof ApiError ? e.message : '待办没拉到'
  } finally {
    if (props.runId === runId) loading.value = false
  }
}

watch(
  () => [props.runId, props.boardText, props.reloadNonce],
  () => void load(),
  { immediate: true },
)
</script>

<template>
  <div class="todo-panel">
    <p v-if="!runId" class="todo-note">本场还没有待办。</p>
    <template v-else>
      <div v-if="loadedRun === runId" class="todo-head">
        <span class="todo-title">Todo</span>
        <span class="todo-count">{{ done }}/{{ total }}</span>
        <span class="todo-meter" aria-hidden="true"><i :style="{ width: `${percent}%` }"></i></span>
      </div>
      <p v-if="error" class="todo-error" role="alert">
        {{ error }}
        <button type="button" @click="load">重试</button>
      </p>
      <p v-else-if="loading && !rows.length" class="todo-note">正在拉待办…</p>
      <p v-else-if="!rows.length" class="todo-note">本场没有待办。</p>
      <ul v-else class="todo-list">
        <li v-for="row in rows" :key="row.no" :class="['todo-row', row.status]">
          <div class="todo-line">
            <span class="todo-no">{{ row.no }}</span>
            <span class="todo-subject">{{ row.subject }}</span>
            <span class="todo-status">{{ statusLabel(row.status) }}</span>
          </div>
          <p v-if="row.skillName" class="todo-skill">技能：{{ row.skillName }}</p>
          <details v-if="row.description || row.result" class="todo-detail">
            <summary>详情</summary>
            <p v-if="row.description" class="todo-desc">{{ row.description }}</p>
            <p v-if="row.result" class="todo-result">{{ row.result }}</p>
          </details>
        </li>
      </ul>
    </template>
  </div>
</template>

<style scoped>
.todo-panel {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  padding: 0 0.65rem 0.8rem;
}
.todo-head {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.1rem 0.15rem;
}
.todo-title {
  font-size: 0.74rem;
  font-weight: 650;
  letter-spacing: 0.06em;
  color: var(--ink-soft);
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
.todo-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
}
.todo-row {
  padding: 0.35rem 0.4rem;
  border-radius: 8px;
  font-size: var(--text-xs);
  line-height: 1.5;
}
.todo-row.in_progress { background: var(--accent-soft); }
.todo-line {
  display: flex;
  align-items: baseline;
  gap: 0.45rem;
}
.todo-no {
  flex: none;
  min-width: 1.1rem;
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.todo-subject {
  flex: 1;
  min-width: 0;
  color: var(--ink-soft);
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
.todo-skill {
  margin: 0.15rem 0 0 1.55rem;
  font-size: 0.68rem;
  color: var(--muted);
}
.todo-detail {
  margin: 0.2rem 0 0 1.55rem;
  font-size: 0.72rem;
  color: var(--muted);
}
.todo-detail summary {
  cursor: pointer;
  color: var(--accent-ink);
  font-size: 0.7rem;
}
.todo-desc,
.todo-result {
  margin: 0.25rem 0 0;
  white-space: pre-wrap;
  word-break: break-word;
}
.todo-result { color: var(--ink-soft); }
.todo-note {
  margin: 0;
  padding: 0.2rem 0.15rem;
  font-size: var(--text-xs);
  color: var(--muted);
  line-height: 1.5;
}
.todo-error {
  margin: 0;
  padding: 0.2rem 0.15rem;
  font-size: var(--text-xs);
  color: var(--danger);
  line-height: 1.5;
}
.todo-error button {
  border: 0;
  background: none;
  color: var(--accent-ink);
  font-size: var(--text-xs);
  cursor: pointer;
  padding: 0 0.25rem;
}
</style>
