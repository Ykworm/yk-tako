<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from './configureApi'
import { readToken, rememberRole, switchTenant, tenantState } from './auth'

type Member = { userId: string; email: string; role: string; status: string }
type Invite = { id: string; email: string; role: string; status: string; expiresAt: string }

const noticeKey = 'tako.members.notice'
const tenantId = tenantState.value.id

const phase = ref<'loading' | 'personal' | 'team' | 'error'>('loading')
const meId = ref('')
const myRole = ref(tenantState.value.role)
const members = ref<Member[]>([])
const invites = ref<Invite[]>([])
const banner = ref('')
const busy = ref(false)
const roleEpoch = ref(0)

const inviteEmail = ref('')
const inviteRole = ref('member')
const inviteLink = ref('')
const copied = ref(false)
const linkEl = ref<HTMLInputElement | null>(null)

const confirmRemove = ref('')
const transferOpen = ref(false)
const transferTarget = ref('')
const confirmLeave = ref(false)
const confirmArchive = ref(false)

const isOwner = computed(() => myRole.value === 'owner')
const others = computed(() => members.value.filter((m) => m.userId !== meId.value))

function roleLabel(role: string): string {
  if (role === 'owner') return '所有者'
  if (role === 'admin') return '管理员'
  if (role === 'member') return '成员'
  if (role === 'viewer') return '查看者'
  return role
}

function statusLabel(status: string): string {
  if (status === 'pending') return '待处理'
  if (status === 'accepted') return '已接受'
  if (status === 'declined') return '已拒绝'
  if (status === 'revoked') return '已撤销'
  return status
}

function formatWhen(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function roleOptions(row: Member): { value: string; label: string }[] {
  const opts = [
    { value: 'admin', label: '管理员' },
    { value: 'member', label: '成员' },
    { value: 'viewer', label: '查看者' },
  ]
  if (row.role === 'owner') opts.unshift({ value: 'owner', label: '所有者' })
  return opts
}

function fail(message: string) {
  banner.value = message || '请求失败'
}

async function reloadTeam() {
  const [meRes, memberRes, inviteRes] = await Promise.all([
    api<{ userId: string; role: string }>('GET', '/api/auth/me'),
    api<{ members: Member[] }>('GET', `/api/tenants/${tenantId}/members`),
    api<{ invitations: Invite[] }>('GET', `/api/tenants/${tenantId}/invitations`),
  ])
  if (meRes.body.code !== 0 || !meRes.body.data) {
    fail(meRes.body.message)
    phase.value = 'error'
    return
  }
  if (memberRes.body.code !== 0 || !memberRes.body.data) {
    fail(memberRes.body.message)
    phase.value = 'error'
    return
  }
  if (inviteRes.body.code !== 0 || !inviteRes.body.data) {
    fail(inviteRes.body.message)
    phase.value = 'error'
    return
  }
  meId.value = meRes.body.data.userId
  members.value = memberRes.body.data.members
  invites.value = inviteRes.body.data.invitations
  const mine = members.value.find((m) => m.userId === meId.value)
  myRole.value = mine?.role || meRes.body.data.role
  rememberRole(myRole.value)
  if (!isOwner.value) {
    transferOpen.value = false
    confirmArchive.value = false
  }
  phase.value = 'team'
}

async function load() {
  const saved = sessionStorage.getItem(noticeKey)
  if (saved) {
    sessionStorage.removeItem(noticeKey)
    banner.value = saved
  }
  const res = await api<{ tenant: { kind: string } }>('GET', `/api/tenants/${tenantId}`)
  if (res.body.code !== 0 || !res.body.data?.tenant) {
    fail(res.body.message)
    phase.value = 'error'
    return
  }
  if (res.body.data.tenant.kind === 'personal') {
    phase.value = 'personal'
    return
  }
  await reloadTeam()
}

onMounted(() => {
  void load()
})

async function onRole(row: Member, ev: Event) {
  const next = (ev.target as HTMLSelectElement).value
  if (!next || next === row.role) return
  banner.value = ''
  busy.value = true
  try {
    const res = await api('PUT', `/api/tenants/${tenantId}/members/${row.userId}`, { role: next })
    if (res.body.code !== 0) {
      fail(res.body.message)
      roleEpoch.value++
      return
    }
    await reloadTeam()
  } finally {
    busy.value = false
  }
}

async function removeMember(row: Member) {
  banner.value = ''
  busy.value = true
  try {
    const res = await api('DELETE', `/api/tenants/${tenantId}/members/${row.userId}`)
    if (res.body.code !== 0) {
      fail(res.body.message)
      return
    }
    confirmRemove.value = ''
    await reloadTeam()
  } finally {
    busy.value = false
  }
}

async function sendInvite() {
  banner.value = ''
  copied.value = false
  inviteLink.value = ''
  busy.value = true
  try {
    const res = await api<{ inviteToken?: string }>('POST', `/api/tenants/${tenantId}/invitations`, {
      email: inviteEmail.value,
      role: inviteRole.value,
    })
    if (res.body.code !== 0 || !res.body.data?.inviteToken) {
      fail(res.body.message)
      return
    }
    inviteLink.value = `${location.origin}/invite/${res.body.data.inviteToken}`
    inviteEmail.value = ''
    await reloadTeam()
  } finally {
    busy.value = false
  }
}

async function copyLink() {
  if (!inviteLink.value) return
  try {
    await navigator.clipboard.writeText(inviteLink.value)
    copied.value = true
  } catch {
    linkEl.value?.focus()
    linkEl.value?.select()
    const ok = document.execCommand('copy')
    copied.value = ok
    if (!ok) fail('复制失败，请选中链接手动复制')
  }
}

async function revoke(row: Invite) {
  banner.value = ''
  busy.value = true
  try {
    const res = await api('DELETE', `/api/tenants/${tenantId}/invitations/${row.id}`)
    if (res.body.code !== 0) {
      fail(res.body.message)
      return
    }
    await reloadTeam()
  } finally {
    busy.value = false
  }
}

async function transfer() {
  if (!transferTarget.value) return
  banner.value = ''
  busy.value = true
  try {
    const res = await api('POST', `/api/tenants/${tenantId}/transfer-owner`, { userId: transferTarget.value })
    if (res.body.code !== 0) {
      fail(res.body.message)
      return
    }
    transferOpen.value = false
    transferTarget.value = ''
    await reloadTeam()
  } finally {
    busy.value = false
  }
}

// 归档或离开一旦成功，当前这张 access token 就不再是这个工作区的成员，
// 接着再调 switch-tenant 会 403。先切到个人工作区，再用切走前抄下来的团队 token 做这次操作。
// 失败则切回来。Configure 用工作区 id 做 key，一切就重挂载，失败文案放 sessionStorage 让新页面读。
async function depart(method: string, path: string) {
  banner.value = ''
  busy.value = true
  const teamToken = readToken()
  const teamId = tenantId
  sessionStorage.removeItem(noticeKey)
  try {
    const list = await api<{ tenants: { id: string; kind: string }[] }>('GET', '/api/tenants')
    const personal = list.body.data?.tenants?.find((t) => t.kind === 'personal')
    if (list.body.code !== 0 || !personal) {
      fail(list.body.message || '找不到个人工作区')
      busy.value = false
      return
    }
    const switched = await switchTenant(personal.id)
    if (!switched.ok) {
      fail(switched.message)
      busy.value = false
      return
    }
    const result = await callWithToken(method, path, teamToken)
    if (result.ok) return
    sessionStorage.setItem('tako.team.sub.' + teamId, 'members')
    sessionStorage.setItem(noticeKey, result.message)
    await switchTenant(teamId)
  } catch {
    sessionStorage.setItem('tako.team.sub.' + teamId, 'members')
    sessionStorage.setItem(noticeKey, '连不上引擎')
    await switchTenant(teamId)
  }
}

async function callWithToken(method: string, path: string, token: string): Promise<{ ok: boolean; message: string }> {
  let res: Response
  try {
    res = await fetch(path, { method, headers: { Authorization: 'Bearer ' + token } })
  } catch {
    return { ok: false, message: '连不上引擎' }
  }
  if (res.status === 204) return { ok: true, message: '' }
  const body = (await res.json().catch(() => null)) as { code?: number; message?: string } | null
  if (res.ok && body && body.code === 0) return { ok: true, message: '' }
  return { ok: false, message: body?.message || '请求失败' }
}

function leave() {
  confirmLeave.value = false
  void depart('POST', `/api/tenants/${tenantId}/leave`)
}

function archive() {
  confirmArchive.value = false
  void depart('DELETE', `/api/tenants/${tenantId}`)
}
</script>

<template>
  <div class="stack">
    <p v-if="phase === 'loading'" class="muted">正在加载…</p>
    <p v-else-if="phase === 'personal'" class="card" data-personal-empty>个人工作区没有成员</p>
    <p v-if="banner" class="banner" role="alert" data-banner>{{ banner }}</p>

    <template v-if="phase === 'team'">
      <section class="card">
        <h3>成员</h3>
        <ul class="people">
          <li v-for="row in members" :key="row.userId" class="person" :data-email="row.email" data-member-row>
            <span class="email">{{ row.email }}</span>
            <select
              v-if="isOwner"
              :key="row.userId + ':' + roleEpoch"
              :value="row.role"
              :disabled="busy"
              :aria-label="row.email + ' 的角色'"
              data-role-select
              @change="onRole(row, $event)"
            >
              <option v-for="opt in roleOptions(row)" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
            </select>
            <span v-else class="role" data-role-label>{{ roleLabel(row.role) }}</span>
            <span v-if="row.userId === meId" class="own">
              <template v-if="!confirmLeave">
                <button type="button" :disabled="busy" data-leave @click="confirmLeave = true">离开团队</button>
              </template>
              <template v-else>
                <button type="button" class="danger" :disabled="busy" data-leave-confirm @click="leave">确认离开</button>
                <button type="button" :disabled="busy" @click="confirmLeave = false">取消</button>
              </template>
              <template v-if="isOwner">
                <button v-if="!transferOpen" type="button" :disabled="busy" data-transfer @click="transferOpen = true">转移所有者</button>
                <template v-else>
                  <select v-model="transferTarget" :disabled="busy" aria-label="新的所有者" data-transfer-target>
                    <option value="">选一个人</option>
                    <option v-for="m in others" :key="m.userId" :value="m.userId">{{ m.email }}</option>
                  </select>
                  <button type="button" :disabled="busy || !transferTarget" data-transfer-confirm @click="transfer">确认转移</button>
                  <button type="button" :disabled="busy" @click="transferOpen = false">取消</button>
                </template>
              </template>
            </span>
            <span v-else-if="isOwner" class="own">
              <template v-if="confirmRemove !== row.userId">
                <button type="button" :disabled="busy" data-remove @click="confirmRemove = row.userId">移除</button>
              </template>
              <template v-else>
                <button type="button" class="danger" :disabled="busy" data-remove-confirm @click="removeMember(row)">确认移除</button>
                <button type="button" :disabled="busy" @click="confirmRemove = ''">取消</button>
              </template>
            </span>
          </li>
        </ul>
      </section>

      <section class="card">
        <h3>邀请</h3>
        <form v-if="isOwner" class="invite" data-invite-form @submit.prevent="sendInvite">
          <label>邮箱<input v-model="inviteEmail" type="email" required autocomplete="off" data-invite-email /></label>
          <label>角色
            <select v-model="inviteRole" data-invite-role>
              <option value="admin">管理员</option>
              <option value="member">成员</option>
              <option value="viewer">查看者</option>
            </select>
          </label>
          <button type="submit" class="primary" :disabled="busy">邀请</button>
        </form>
        <div v-if="inviteLink" class="link-box" data-invite-result>
          <label>邀请链接
            <input ref="linkEl" readonly :value="inviteLink" data-invite-link />
          </label>
          <button type="button" data-copy @click="copyLink">{{ copied ? '已复制' : '复制' }}</button>
        </div>
        <p v-if="!invites.length" class="muted">还没有邀请。</p>
        <ul v-else class="people">
          <li v-for="row in invites" :key="row.id" class="person" data-invite-row :data-invite-email="row.email">
            <span class="email">{{ row.email }}</span>
            <span class="role">{{ roleLabel(row.role) }}</span>
            <span class="role" data-invite-status>{{ statusLabel(row.status) }}</span>
            <span class="muted">{{ formatWhen(row.expiresAt) }}</span>
            <button v-if="isOwner && row.status === 'pending'" type="button" :disabled="busy" data-revoke @click="revoke(row)">撤销</button>
          </li>
        </ul>
      </section>

      <section v-if="isOwner" class="card danger-zone" data-danger>
        <h3>危险区</h3>
        <p class="muted">归档之后，这个工作区会从切换器里消失。</p>
        <div class="own">
          <button v-if="!confirmArchive" type="button" class="danger" :disabled="busy" data-archive @click="confirmArchive = true">归档这个工作区</button>
          <template v-else>
            <span>归档不能恢复。</span>
            <button type="button" class="danger" :disabled="busy" data-archive-confirm @click="archive">确认归档</button>
            <button type="button" :disabled="busy" @click="confirmArchive = false">取消</button>
          </template>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.stack { display: flex; flex-direction: column; gap: 0.85rem; }
.card {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 18px;
  padding: 1rem;
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}
h3 { margin: 0; font-size: 1rem; }
.people {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.45rem;
}
.person {
  display: flex;
  flex-wrap: wrap;
  gap: 0.45rem;
  align-items: center;
}
.email { font-weight: 600; }
.role, .muted { color: var(--muted); font-size: 0.82rem; }
.muted { margin: 0; }
.own { display: flex; flex-wrap: wrap; gap: 0.4rem; align-items: center; }
.invite {
  display: grid;
  grid-template-columns: minmax(0, 1.4fr) minmax(8rem, 0.6fr) auto;
  gap: 0.7rem;
  align-items: end;
}
label {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  font-size: 0.78rem;
  color: var(--ink-soft);
}
input, select {
  width: 100%;
  border: 1px solid var(--line-strong);
  border-radius: 12px;
  padding: 0.55rem 0.7rem;
  background: #fff;
  font-size: 0.92rem;
}
.person select, .own select { width: auto; min-width: 7rem; padding: 0.35rem 0.55rem; }
input:focus, select:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.12);
}
button {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 999px;
  padding: 0.38rem 0.8rem;
  font-size: 0.8rem;
}
button:hover:not(:disabled) { border-color: #cfc6b6; }
.primary { border: 0; background: var(--ink); color: #f6f1e7; }
.primary:disabled, button:disabled { opacity: 0.55; }
.danger { border-color: #fecdd3; background: var(--danger-soft); color: var(--danger); }
.banner {
  margin: 0;
  border-radius: 12px;
  padding: 0.5rem 0.7rem;
  font-size: 0.82rem;
  color: var(--danger);
  background: var(--danger-soft);
}
.link-box { display: flex; gap: 0.45rem; align-items: end; }
.link-box label { flex: 1; }
.danger-zone { border-color: #fecdd3; }
@media (max-width: 720px) {
  .invite { grid-template-columns: 1fr; }
}
</style>
