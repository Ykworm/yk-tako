<script setup lang="ts">
// 回复结尾的「改动的文件」（doc/78 §A5）：清单由服务端从 runlog 派生，页面不猜、
// 不读模型正文。只挂助手气泡的最后一段（无 segmentCallId）且 run 已到终态。
import { ref, watch } from 'vue'
import { getRunChangedFiles, type RunChangedFile } from './api'

const props = defineProps<{
  runId: string | null
  state: string
  segment?: string
}>()
const emit = defineEmits<{ open: [path: string] }>()

const TERMINAL = new Set(['done', 'error', 'stopped', 'interrupted'])
// 按 runId 缓存：翻历史、切会话回来不再重复请求。失效只有一个入口——状态离开终态时清掉
// （续跑会把中断的 run 拉回运行态，旧清单不能再挂在那儿）。
const cache = new Map<string, RunChangedFile[]>()

const files = ref<RunChangedFile[]>([])
const error = ref('')

watch(
  () => [props.runId, props.state] as const,
  async ([runId, state]) => {
    error.value = ''
    if (!runId || props.segment || !TERMINAL.has(state)) {
      files.value = []
      if (runId) cache.delete(runId)
      return
    }
    const hit = cache.get(runId)
    if (hit) {
      files.value = hit
      return
    }
    try {
      const res = await getRunChangedFiles(runId)
      cache.set(runId, res.files)
      if (props.runId === runId) files.value = res.files
    } catch {
      if (props.runId !== runId) return
      files.value = []
      error.value = '改动文件没拉到'
    }
  },
  { immediate: true },
)
</script>

<template>
  <p v-if="error" class="changed-error" role="alert">{{ error }}</p>
  <div v-else-if="files.length" class="changed-files">
    <span class="changed-title">改动的文件</span>
    <div class="changed-list">
      <button
        v-for="file in files"
        :key="file.path"
        type="button"
        class="changed-item"
        :title="file.path"
        @click="emit('open', file.path)"
      >
        <span class="changed-path">{{ file.path }}</span>
        <span :class="['changed-op', file.op]">{{ file.op === 'write' ? '写入' : '编辑' }}</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.changed-files {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  margin: 0.2rem 0 0.35rem;
  max-width: 46rem;
}
.changed-title {
  font-size: var(--text-xs);
  color: var(--muted);
  letter-spacing: 0.04em;
}
.changed-list {
  display: flex;
  flex-wrap: wrap;
  gap: 0.3rem;
}
.changed-item {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  max-width: 100%;
  border: 1px solid var(--line);
  border-radius: 8px;
  background: var(--bg-raised);
  color: var(--ink-soft);
  font: inherit;
  font-size: var(--text-xs);
  padding: 0.28rem 0.5rem;
  cursor: pointer;
}
.changed-item:hover {
  border-color: var(--accent);
  color: var(--accent-ink);
}
.changed-path {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--mono, ui-monospace, monospace);
}
.changed-op {
  flex: none;
  font-size: 0.68rem;
  color: var(--muted);
}
.changed-op.write { color: var(--accent-ink); }
.changed-error {
  margin: 0.2rem 0 0.35rem;
  font-size: var(--text-xs);
  color: var(--danger);
}
</style>
