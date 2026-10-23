<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from './configureApi'
import { switchTenant } from './auth'

type Invite = {
  id: string
  email: string
  role: string
  status: string
  expiresAt: string
  tenantId: string
  tenantName: string
}

const emit = defineEmits<{ changed: [] }>()

const items = ref<Invite[]>([])
const open = ref(false)
const busy = ref(false)
const banner = ref('')
const ask = ref<Invite | null>(null)
const askNote = ref('')

function roleLabel(role: string): string {
  if (role === 'owner') return '所有者'
  if (role === 'admin') return '管理员'
  if (role === 'member') return '成员'
  if (role === 'viewer') return '查看者'
  return role
}

function formatWhen(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

async function load() {
  const res = await api<{ invitations: Invite[] }>('GET', '/api/me/invitations')
  if (res.body.code !== 0 || !res.body.data) {
    banner.value = res.body.message || '邀请加载失败'
    return
  }
  items.value = res.body.data.invitations
  if (!items.value.length && !ask.value) open.value = false
}

onMounted(() => {
  void load()
})

async function accept(row: Invite) {
  if (busy.value) return
  banner.value = ''
  busy.value = true
  try {
    const res = await api('POST', `/api/me/invitations/${row.id}/accept`)
    if (res.body.code !== 0 && res.body.code !== 111406) {
      banner.value = res.body.message || '请求失败'
      return
    }
    ask.value = row
    askNote.value = res.body.code === 111406 ? (res.body.message || '你已经是这个工作区的成员') : '已加入'
    open.value = true
    await load()
    emit('changed')
  } finally {
    busy.value = false
  }
}

async function decline(row: Invite) {
  if (busy.value) return
  banner.value = ''
  busy.value = true
  try {
    const res = await api('POST', `/api/me/invitations/${row.id}/decline`)
    if (res.body.code !== 0) {
      banner.value = res.body.message || '请求失败'
      return
    }
    if (ask.value?.id === row.id) ask.value = null
    await load()
    emit('changed')
  } finally {
    busy.value = false
  }
}

async function go() {
  const row = ask.value
  if (!row || busy.value) return
  busy.value = true
  banner.value = ''
  try {
    const res = await switchTenant(row.tenantId)
    if (!res.ok) {
      banner.value = res.message
      return
    }
    ask.value = null
    askNote.value = ''
    emit('changed')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div v-if="items.length || ask" class="pending" data-pending-invites>
    <button
      type="button"
      class="head"
      :data-pending-count="items.length"
      @click="open = !open"
    >待处理邀请（{{ items.length }}）</button>
    <div v-if="open" class="list">
      <p v-if="ask" class="ask" data-switch-ask>{{ askNote }}。切换到 {{ ask.tenantName }}？</p>
      <div v-if="ask" class="actions">
        <button type="button" class="yes" :disabled="busy" data-switch-yes @click="go">切换过去</button>
        <button type="button" :disabled="busy" data-switch-no @click="ask = null">留在这里</button>
      </div>
      <div v-for="row in items" :key="row.id" class="item" data-pending-row :data-tenant="row.tenantName">
        <strong>{{ row.tenantName }}</strong>
        <span>{{ roleLabel(row.role) }}</span>
        <span class="when">{{ formatWhen(row.expiresAt) }}前有效</span>
        <span class="actions">
          <button type="button" :disabled="busy" data-pending-accept @click="accept(row)">接受</button>
          <button type="button" :disabled="busy" data-pending-decline @click="decline(row)">拒绝</button>
        </span>
      </div>
      <p v-if="banner" class="banner" role="alert" data-pending-banner>{{ banner }}</p>
    </div>
  </div>
</template>

<style scoped>
.pending { margin-bottom: 0.25rem; }
.head {
  width: 100%;
  height: 30px;
  text-align: left;
  border: 0;
  border-radius: var(--radius-control);
  background: var(--accent-soft);
  color: var(--accent-ink);
  font-size: var(--text-sm);
  font-weight: 600;
  padding: 0 var(--space-2);
}
.list {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-2) 0 var(--space-1) var(--space-1);
}
.item {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-1) var(--space-2);
  align-items: center;
  font-size: var(--text-xs);
  color: var(--ink-soft);
}
.item strong { color: var(--ink); font-size: var(--text-sm); }
.when { color: var(--muted); }
.actions { display: flex; gap: var(--space-1); }
.actions button, .yes {
  height: 24px;
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  border-radius: var(--radius-control);
  padding: 0 var(--space-2);
  font-size: var(--text-xs);
}
.actions button:hover:not(:disabled), .yes:hover:not(:disabled) { background: var(--hover); }
.ask { margin: 0; font-size: var(--text-sm); color: var(--ink); }
.banner {
  margin: 0;
  border-radius: var(--radius-control);
  padding: var(--space-2);
  font-size: var(--text-xs);
  color: var(--danger);
  background: var(--danger-soft);
}
</style>
