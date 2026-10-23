<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { authFetch, clearAuth, loginWithPassword, readToken, switchTenant } from './auth'

const props = defineProps<{ token: string }>()
const emit = defineEmits<{ leave: [] }>()

type Preview = {
  tenantName: string
  role: string
  status: string
  invitationId: string
  email: string
  expiresAt: string
}

type Mode = 'loading' | 'invalid' | 'closed' | 'pending' | 'register' | 'login' | 'ask' | 'already'

const mode = ref<Mode>('loading')
const preview = ref<Preview | null>(null)
const myEmail = ref('')
const banner = ref('')
const note = ref('')
const busy = ref(false)
const password = ref('')
const showPassword = ref(false)
const beforeIds = ref<string[]>([])

const sameAccount = computed(() => {
  const invite = preview.value?.email.toLowerCase() || ''
  return invite !== '' && invite === myEmail.value.toLowerCase()
})

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

function expired(iso: string): boolean {
  const t = Date.parse(iso)
  return Number.isNaN(t) || t <= Date.now()
}

async function currentEmail(): Promise<string> {
  if (!readToken()) return ''
  const res = await authFetch('/api/auth/me')
  const body = (await res.json().catch(() => null)) as { code?: number; data?: { email?: string } } | null
  if (!res.ok || !body || body.code !== 0) return ''
  return body.data?.email || ''
}

async function tenantIds(): Promise<string[]> {
  const res = await authFetch('/api/tenants')
  const body = (await res.json().catch(() => null)) as {
    code?: number
    data?: { tenants?: { id: string; name: string }[] }
  } | null
  if (!res.ok || !body || body.code !== 0) return []
  return (body.data?.tenants ?? []).map((t) => t.id)
}

async function load() {
  mode.value = 'loading'
  banner.value = ''
  const res = await fetch('/api/invitations/' + encodeURIComponent(props.token))
  const body = (await res.json().catch(() => null)) as {
    code?: number
    message?: string
    data?: Preview
  } | null
  if (!res.ok || !body || body.code !== 0 || !body.data) {
    note.value = body?.message || '邀请无效或已过期'
    mode.value = 'invalid'
    return
  }
  preview.value = body.data
  if (body.data.status === 'revoked' || expired(body.data.expiresAt)) {
    note.value = '邀请无效或已过期'
    mode.value = 'invalid'
    return
  }
  if (body.data.status === 'accepted') {
    note.value = '这封邀请已经接受'
    mode.value = 'closed'
    myEmail.value = await currentEmail()
    return
  }
  if (body.data.status === 'declined') {
    note.value = '这封邀请已经拒绝'
    mode.value = 'closed'
    return
  }
  if (body.data.status !== 'pending') {
    note.value = '邀请无效或已过期'
    mode.value = 'invalid'
    return
  }
  myEmail.value = await currentEmail()
  mode.value = 'pending'
}

onMounted(() => {
  void load()
})

function showRegister() {
  banner.value = ''
  password.value = ''
  mode.value = 'register'
}

function logoutAndRegister() {
  clearAuth()
  myEmail.value = ''
  showRegister()
}

async function register() {
  const row = preview.value
  if (!row || busy.value) return
  banner.value = ''
  busy.value = true
  try {
    const res = await fetch('/api/auth/register-by-invite', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: props.token, email: row.email, password: password.value }),
    })
    const body = (await res.json().catch(() => null)) as { code?: number; message?: string } | null
    if (body?.code === 111401) {
      clearAuth()
      myEmail.value = ''
      password.value = ''
      banner.value = '该邮箱已注册，请直接登录后再接受邀请'
      mode.value = 'login'
      return
    }
    if (!res.ok || !body || body.code !== 0) {
      banner.value = body?.message || '请求失败'
      return
    }
    clearAuth()
    myEmail.value = ''
    password.value = ''
    note.value = `已加入 ${row.tenantName}，请登录`
    mode.value = 'login'
  } finally {
    busy.value = false
  }
}

async function login() {
  const row = preview.value
  if (!row || busy.value) return
  banner.value = ''
  busy.value = true
  try {
    const res = await loginWithPassword(row.email, password.value)
    if (!res.ok) {
      banner.value = res.message || '登录失败'
      return
    }
    emit('leave')
  } finally {
    busy.value = false
  }
}

async function accept() {
  const row = preview.value
  if (!row || busy.value) return
  banner.value = ''
  busy.value = true
  try {
    beforeIds.value = await tenantIds()
    const res = await authFetch('/api/me/invitations/' + encodeURIComponent(row.invitationId) + '/accept', { method: 'POST' })
    const body = (await res.json().catch(() => null)) as { code?: number; message?: string } | null
    if (body?.code === 111406) {
      note.value = body.message || '你已经是这个工作区的成员'
      mode.value = 'already'
      return
    }
    if (!res.ok || !body || body.code !== 0) {
      banner.value = body?.message || '请求失败'
      return
    }
    note.value = '已加入'
    mode.value = 'ask'
  } finally {
    busy.value = false
  }
}

async function findTeamId(): Promise<string> {
  const name = preview.value?.tenantName || ''
  const res = await authFetch('/api/tenants')
  const body = (await res.json().catch(() => null)) as {
    code?: number
    data?: { tenants?: { id: string; name: string }[] }
  } | null
  const rows = body?.data?.tenants ?? []
  const added = rows.find((t) => t.name === name && !beforeIds.value.includes(t.id))
  if (added) return added.id
  return rows.find((t) => t.name === name)?.id || ''
}

async function goTeam() {
  if (busy.value) return
  busy.value = true
  banner.value = ''
  try {
    const id = await findTeamId()
    if (!id) {
      banner.value = '已经加入，切换器里还没有这个工作区'
      return
    }
    const res = await switchTenant(id)
    if (!res.ok) {
      banner.value = res.message
      return
    }
    emit('leave')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="invite">
    <div class="card">
      <p class="kicker">邀请</p>
      <p v-if="mode === 'loading'" class="muted">正在读取邀请…</p>

      <template v-else-if="mode === 'invalid'">
        <h1 data-invite-invalid>{{ note }}</h1>
        <button type="button" class="primary" data-go-login @click="emit('leave')">去登录</button>
      </template>

      <template v-else-if="preview && (mode === 'closed' || mode === 'pending' || mode === 'register' || mode === 'login' || mode === 'ask' || mode === 'already')">
        <h1 data-invite-preview>{{ preview.tenantName }} 邀请你成为{{ roleLabel(preview.role) }}，{{ formatWhen(preview.expiresAt) }}前有效</h1>

        <p v-if="mode === 'closed'" data-invite-closed>{{ note }}</p>
        <p v-if="mode === 'ask'" data-invite-joined>{{ note }}。切换到 {{ preview.tenantName }}？</p>
        <p v-if="mode === 'already'" data-invite-already>{{ note }}</p>
        <p v-if="mode === 'login' && note" data-invite-login-note>{{ note }}</p>
        <!-- 说的是「你（当前账号）」不是邀请目标，括号里就得印当前账号；
             印 invited email 等于让用户拿邀请邮箱和邀请邮箱对比，永远对得上。 -->
        <p v-if="mode === 'pending' && myEmail && !sameAccount" data-invite-mismatch>
          这个邀请不属于当前账号（{{ myEmail }}）
        </p>

        <div v-if="mode === 'pending' && sameAccount" class="actions">
          <button type="button" class="primary" :disabled="busy" data-accept @click="accept">接受邀请</button>
        </div>
        <div v-else-if="mode === 'pending' && myEmail" class="actions">
          <button type="button" data-logout-switch @click="logoutAndRegister">退出并换账号</button>
          <button type="button" data-use-invite-email @click="showRegister">用邀请邮箱注册</button>
        </div>
        <div v-else-if="mode === 'ask' || mode === 'already'" class="actions">
          <button type="button" class="primary" :disabled="busy" data-switch-yes @click="goTeam">切换过去</button>
          <button type="button" :disabled="busy" data-switch-no @click="emit('leave')">留在这里</button>
        </div>
        <div v-else-if="mode === 'closed'" class="actions">
          <button v-if="sameAccount" type="button" class="primary" :disabled="busy" data-switch-yes @click="goTeam">切换过去</button>
          <button type="button" data-go-login @click="emit('leave')">去登录</button>
        </div>

        <form v-if="mode === 'register' || (mode === 'pending' && !myEmail)" class="form" data-register-form @submit.prevent="register">
          <label>邮箱<input :value="preview.email" type="email" readonly data-register-email /></label>
          <label>密码
            <span class="secret">
              <input v-model="password" :type="showPassword ? 'text' : 'password'" minlength="8" autocomplete="new-password" required data-register-password />
              <button type="button" class="text" @click="showPassword = !showPassword">{{ showPassword ? '隐藏' : '显示' }}</button>
            </span>
          </label>
          <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在注册…' : '注册并加入' }}</button>
        </form>

        <form v-if="mode === 'login'" class="form" data-login-form @submit.prevent="login">
          <label>邮箱<input :value="preview.email" type="email" readonly data-login-email /></label>
          <label>密码
            <input v-model="password" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" required data-login-password />
          </label>
          <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在登录…' : '登录' }}</button>
        </form>

        <p v-if="banner" class="banner" role="alert" data-invite-banner>{{ banner }}</p>
      </template>
    </div>
  </section>
</template>

<style scoped>
.invite {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: var(--space-5);
  background: var(--bg);
}
.card {
  width: min(100%, 28rem);
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-panel);
  padding: var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.kicker {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-xs);
  font-weight: 600;
  letter-spacing: 0.1em;
}
h1 {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: 600;
  letter-spacing: -0.02em;
  line-height: 1.4;
}
.muted { margin: 0; color: var(--muted); }
.form { display: flex; flex-direction: column; gap: var(--space-3); }
label {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: var(--text-xs);
  color: var(--ink-soft);
}
input {
  width: 100%;
  height: 36px;
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-control);
  padding: 0 var(--space-3);
  background: var(--bg-raised);
  font-size: var(--text-sm);
}
input[readonly] { background: var(--bg); color: var(--ink-soft); }
input:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
.secret { position: relative; display: block; }
.secret input { padding-right: 3.4rem; }
.secret .text {
  position: absolute;
  right: 4px;
  top: 50%;
  transform: translateY(-50%);
  height: 28px;
  padding: 0 var(--space-2);
}
.actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
/* 卡片是 flex 列：直系按钮会被拉成整卡宽，主按钮看着像一整行。 */
.card > button { align-self: flex-start; }
button {
  height: 32px;
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  border-radius: var(--radius-control);
  padding: 0 var(--space-4);
  font-size: var(--text-sm);
  font-weight: 500;
}
button:hover:not(:disabled) { background: var(--hover); }
.primary { border-color: var(--accent); background: var(--accent); color: #fff; }
.primary:hover:not(:disabled) { border-color: var(--accent-ink); background: var(--accent-ink); }
button:disabled { opacity: 0.55; }
.text { border: 0; background: transparent; color: var(--muted); }
.text:hover:not(:disabled) { background: var(--hover); }
.banner {
  margin: 0;
  border-radius: var(--radius-control);
  padding: var(--space-2) var(--space-3);
  font-size: var(--text-sm);
  color: var(--danger);
  background: var(--danger-soft);
}
</style>
