<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'

const props = defineProps<{
  title: string
  body: string
  confirmText?: string
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
          <button type="button" class="danger" :disabled="busy" @click="emit('confirm')">
            {{ busy ? '正在删除…' : (confirmText || '删除') }}
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
  padding: 1.5rem;
  background: rgba(28, 25, 23, 0.45);
}

.dialog {
  width: min(100%, 27rem);
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius);
  padding: 1.25rem 1.3rem 1.1rem;
  box-shadow: 0 24px 60px rgba(28, 25, 23, 0.22);
}

h2 {
  margin: 0 0 0.5rem;
  font-size: 1.05rem;
}

.body {
  margin: 0 0 0.85rem;
  color: var(--ink-soft);
  font-size: 0.86rem;
  line-height: 1.65;
  white-space: pre-wrap;
}

.error {
  margin: 0 0 0.8rem;
  color: var(--danger);
  background: var(--danger-soft);
  border-radius: 12px;
  padding: 0.55rem 0.7rem;
  font-size: 0.8rem;
  line-height: 1.5;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 0.5rem;
}

button {
  border-radius: 999px;
  padding: 0.45rem 0.95rem;
  font-size: 0.82rem;
}

.cancel {
  border: 1px solid var(--line-strong);
  background: transparent;
  color: var(--ink-soft);
}

.cancel:hover:not(:disabled) {
  background: #fff;
  border-color: #cfc6b6;
}

.danger {
  border: 0;
  background: var(--danger);
  color: #fff;
}

.danger:hover:not(:disabled) {
  background: #881337;
}

button:disabled {
  opacity: 0.55;
}
</style>
