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
        <p v-if="mode === 'pending' && myEmail && !sameAccount" data-invite-mismatch>
          这个邀请不属于当前账号（{{ preview.email }}）
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
  padding: 1.5rem;
  background: var(--bg);
}
.card {
  width: min(100%, 28rem);
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 18px;
  padding: 1.3rem 1.2rem 1.4rem;
  display: flex;
  flex-direction: column;
  gap: 0.8rem;
}
.kicker {
  margin: 0;
  color: var(--accent-ink);
  font-size: 0.75rem;
  letter-spacing: 0.14em;
}
h1 {
  margin: 0;
  font-size: 1.25rem;
  font-weight: 620;
  letter-spacing: -0.03em;
  line-height: 1.45;
}
.muted { margin: 0; color: var(--muted); }
.form { display: flex; flex-direction: column; gap: 0.7rem; }
label {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  font-size: 0.78rem;
  color: var(--ink-soft);
}
input {
  width: 100%;
  border: 1px solid var(--line-strong);
  border-radius: 12px;
  padding: 0.55rem 0.7rem;
  background: #fff;
  font-size: 0.92rem;
}
input[readonly] { background: var(--bg); color: var(--ink-soft); }
input:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.12);
}
.secret { position: relative; display: block; }
.secret .text {
  position: absolute;
  right: 0.45rem;
  top: 50%;
  transform: translateY(-50%);
}
.actions { display: flex; flex-wrap: wrap; gap: 0.45rem; }
button {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 999px;
  padding: 0.42rem 0.85rem;
  font-size: 0.82rem;
}
.primary { border: 0; background: var(--ink); color: #f6f1e7; }
button:disabled { opacity: 0.55; }
.text { border: 0; background: transparent; color: var(--muted); }
.banner {
  margin: 0;
  border-radius: 12px;
  padding: 0.5rem 0.7rem;
  font-size: 0.82rem;
  color: var(--danger);
  background: var(--danger-soft);
}
</style>
