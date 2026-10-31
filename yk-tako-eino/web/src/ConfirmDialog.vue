<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'

const props = defineProps<{
  title: string
  body: string
  confirmText?: string
  // 确认动作的语义色：删除用 danger，覆盖/替换这类不是坏事的动作用 primary
  // （红色「替换」会让人以为要丢东西，实际旧版本进了 .trash）。
  tone?: 'danger' | 'primary'
  // 忙时的按钮文案；不传沿用删除场景的「正在删除…」。
  busyText?: string
  busy?: boolean
  error?: string
}>()

const emit = defineEmits<{ confirm: []; cancel: [] }>()

const cancelEl = ref<HTMLButtonElement | null>(null)

function onKeydown(ev: KeyboardEvent) {
  if (ev.key !== 'Escape') return
  ev.preventDefault()
  if (!props.busy) emit('cancel')
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  cancelEl.value?.focus()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
})

function onOverlay() {
  if (!props.busy) emit('cancel')
}
</script>

<template>
  <Teleport to="body">
    <div class="overlay" @click.self="onOverlay">
      <div
        class="dialog"
        role="dialog"
        aria-modal="true"
        :aria-label="title"
        aria-describedby="confirm-body"
      >
        <h2>{{ title }}</h2>
        <p id="confirm-body" class="body">{{ body }}</p>
        <slot />
        <p v-if="error" class="error" role="alert">{{ error }}</p>
        <div class="actions">
          <button ref="cancelEl" type="button" class="cancel" :disabled="busy" @click="emit('cancel')">
            取消
          </button>
          <button type="button" :class="tone || 'danger'" :disabled="busy" @click="emit('confirm')">
            {{ busy ? (busyText || '正在删除…') : (confirmText || '删除') }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: grid;
  place-items: center;
  padding: var(--space-5);
  background: rgba(16, 17, 20, 0.45);
}

.dialog {
  width: min(100%, 27rem);
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-panel);
  padding: var(--space-5);
  box-shadow: var(--shadow);
}

h2 {
  margin: 0 0 var(--space-2);
  font-size: var(--text-lg);
  font-weight: 600;
}

.body {
  margin: 0 0 var(--space-4);
  color: var(--ink-soft);
  font-size: var(--text-base);
  line-height: 1.65;
  white-space: pre-wrap;
}

.error {
  margin: 0 0 var(--space-3);
  color: var(--danger);
  background: var(--danger-soft);
  border-radius: var(--radius-control);
  padding: var(--space-2) var(--space-3);
  font-size: var(--text-sm);
  line-height: 1.5;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}

button {
  height: 32px;
  border-radius: var(--radius-control);
  padding: 0 var(--space-4);
  font-size: var(--text-sm);
  font-weight: 500;
}

.cancel {
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  color: var(--ink-soft);
}

.cancel:hover:not(:disabled) {
  background: var(--hover);
}

.danger {
  border: 1px solid var(--danger);
  background: var(--danger);
  color: #fff;
}

.danger:hover:not(:disabled) {
  border-color: #b42318;
  background: #b42318;
}

.primary {
  border: 1px solid var(--accent);
  background: var(--accent);
  color: #fff;
}

.primary:hover:not(:disabled) {
  border-color: var(--accent-ink);
  background: var(--accent-ink);
}

button:disabled {
  opacity: 0.55;
}
</style>
