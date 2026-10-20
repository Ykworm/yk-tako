<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useChat } from './chat'
import { mediaUrl, type MediaRef, type Usage } from './api'
import Configure from './Configure.vue'
import InviteLanding from './InviteLanding.vue'
import PendingInvites from './PendingInvites.vue'
import { authFetch, authState, clearAuth, loginWithPassword, switchTenant, tenantState } from './auth'

type Page = 'chat' | 'agents' | 'team'

function readPage(): Page {
  const saved = sessionStorage.getItem('tako.page')
  return saved === 'agents' || saved === 'team' ? saved : 'chat'
}

function tokenFromLocation(): string {
  const prefix = '/invite/'
  if (!location.pathname.startsWith(prefix)) return ''
  return decodeURIComponent(location.pathname.slice(prefix.length).replace(/\/+$/, ''))
}

const inviteToken = ref(tokenFromLocation())
function leaveInvite() {
  history.replaceState(null, '', '/')
  inviteToken.value = ''
}

const page = ref<Page>(readPage())
watch(page, (value) => sessionStorage.setItem('tako.page', value))
const configPage = computed<'agents' | 'team'>(() => (page.value === 'team' ? 'team' : 'agents'))

const { bubbles, draft, attachments, pending, loadingOlder, atStart, busy, send, reconcile, loadOlder, newSession, showTenantSession } = useChat()

const listEl = ref<HTMLElement | null>(null)
const composerEl = ref<HTMLTextAreaElement | null>(null)
const pinned = ref(true)
const fileEl = ref<HTMLInputElement | null>(null)
const askNew = ref(false)
const copiedKey = ref('')
const lightbox = ref('')
const showPassword = ref(false)

const loggedIn = computed(() => authState.value)
const tenant = computed(() => tenantState.value)
const initial = computed(() => (tenant.value.name || '我').slice(0, 1))
const email = ref('')
const password = ref('')
const authBusy = ref(false)
const authError = ref('')

const starters = [
  '这个工作区里现在有哪些文件？',
  '用三句话说说你能帮我做什么',
  '把我下一条消息整理成待办清单',
]

function roleLabel(role: string): string {
  if (role === 'owner') return '所有者'
  if (role === 'admin') return '管理员'
  if (role === 'member') return '成员'
  if (role === 'viewer') return '查看者'
  return role
}

function kindLabel(kind: string): string {
  if (kind === 'personal') return '个人'
  if (kind === 'team') return '团队'
  return kind
}

type TenantItem = { id: string; name: string; kind: string; role: string; status: string }

const switcherEl = ref<HTMLElement | null>(null)
const menuOpen = ref(false)
const tenants = ref<TenantItem[]>([])
const switching = ref(false)
const switchError = ref('')
const createOpen = ref(false)
const createName = ref('')
const createError = ref('')
const createBusy = ref(false)

async function loadTenants() {
  const res = await authFetch('/api/tenants')
  const body = (await res.json().catch(() => null)) as {
    code?: number
    message?: string
    data?: { tenants?: TenantItem[] }
  } | null
  if (!res.ok || !body || body.code !== 0) {
    switchError.value = body?.message || '工作区列表加载失败'
    return
  }
  tenants.value = body.data?.tenants ?? []
}

async function toggleMenu() {
  menuOpen.value = !menuOpen.value
  if (!menuOpen.value) {
    createOpen.value = false
    return
  }
  switchError.value = ''
  createError.value = ''
  await loadTenants()
}

async function pickTenant(row: TenantItem) {
  if (switching.value) return
  if (row.id === tenant.value.id) {
    menuOpen.value = false
    return
  }
  switching.value = true
  switchError.value = ''
  try {
    const res = await switchTenant(row.id)
    if (!res.ok) {
      if (res.status !== 401) switchError.value = res.message
      return
    }
    menuOpen.value = false
    createOpen.value = false
  } finally {
    switching.value = false
  }
}

function openCreate() {
  createOpen.value = true
  createName.value = ''
  createError.value = ''
}

async function createWorkspace() {
  if (createBusy.value) return
  createBusy.value = true
  createError.value = ''
  try {
    const res = await authFetch('/api/tenants', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: createName.value }),
    })
    const body = (await res.json().catch(() => null)) as {
      code?: number
      message?: string
      data?: { tenant?: { id?: string } }
    } | null
    if (!res.ok || !body || body.code !== 0 || !body.data?.tenant?.id) {
      createError.value = body?.message || '创建失败'
      return
    }
    const switched = await switchTenant(body.data.tenant.id)
    if (!switched.ok) {
      createError.value = switched.message
      await loadTenants()
      return
    }
    menuOpen.value = false
    createOpen.value = false
  } finally {
    createBusy.value = false
  }
}

function onDocPointer(ev: PointerEvent) {
  if (!menuOpen.value) return
  const root = switcherEl.value
  if (root && ev.target instanceof Node && !root.contains(ev.target)) {
    menuOpen.value = false
    createOpen.value = false
  }
}

async function doLogin() {
  authError.value = ''
  authBusy.value = true
  try {
    const res = await loginWithPassword(email.value, password.value)
    if (!res.ok) {
      authError.value = res.status ? `${res.status}：${res.message}` : res.message
      return
    }
    password.value = ''
  } finally {
    authBusy.value = false
  }
}

function logout() {
  clearAuth()
}

function requestNewSession() {
  const dirty = bubbles.value.length > 0 || draft.value.trim() !== '' || attachments.value.length > 0
  if (!dirty) {
    newSession()
    page.value = 'chat'
    return
  }
  askNew.value = true
}

function confirmNewSession() {
  askNew.value = false
  newSession()
  page.value = 'chat'
}

function applyStarter(text: string) {
  draft.value = text
  void nextTick(() => composerEl.value?.focus())
}

function onSend() {
  send()
  void nextTick(() => {
    fitComposer()
    composerEl.value?.focus()
  })
}

// 中文输入法选词时 Enter 只是上屏，不能当成发送。
function onDraftKeydown(ev: KeyboardEvent) {
  if (ev.key !== 'Enter' || ev.shiftKey || ev.isComposing || ev.keyCode === 229) return
  ev.preventDefault()
  onSend()
}

function fitComposer() {
  const el = composerEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(el.scrollHeight, 168)}px`
}

watch(draft, () => {
  void nextTick(fitComposer)
})

async function copyBubble(key: string, text: string) {
  try {
    await navigator.clipboard.writeText(text)
    copiedKey.value = key
    window.setTimeout(() => {
      if (copiedKey.value === key) copiedKey.value = ''
    }, 1400)
  } catch {
    copiedKey.value = ''
  }
}

function onWindowKey(ev: KeyboardEvent) {
  if (ev.key !== 'Escape') return
  if (lightbox.value) {
    lightbox.value = ''
    return
  }
  if (menuOpen.value) {
    menuOpen.value = false
    createOpen.value = false
    return
  }
  askNew.value = false
}

const MAX_ATTACH = 4
const MAX_FILE_BYTES = 5 * 1024 * 1024

const HEIC_RE = /\.(heic|heif)$/i
const EXT_MIME: Record<string, string> = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif',
  webp: 'image/webp', bmp: 'image/bmp', svg: 'image/svg+xml',
  pdf: 'application/pdf', mp3: 'audio/mpeg', wav: 'audio/wav', mp4: 'video/mp4',
  heic: 'image/heic', heif: 'image/heif',
}
const SERVER_MIME = new Set([
  'image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/bmp', 'image/svg+xml',
  'application/pdf', 'audio/mpeg', 'audio/wav', 'video/mp4',
])

function fileMime(f: File): string {
  if (f.type) return f.type.toLowerCase()
  const ext = f.name.split('.').pop()?.toLowerCase() ?? ''
  return EXT_MIME[ext] ?? ''
}

async function toJpeg(f: File): Promise<File> {
  const { default: heic2any } = await import('heic2any')
  const out = await heic2any({ blob: f, toType: 'image/jpeg', quality: 0.92 })
  const blob = Array.isArray(out) ? out[0] : out
  return new File([blob], f.name.replace(HEIC_RE, '.jpg'), { type: 'image/jpeg' })
}

function readBase64(f: File): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result).split(',')[1] ?? '')
    r.onerror = () => reject(r.error)
    r.readAsDataURL(f)
  })
}

const attachNotice = ref('')

async function addFiles(files: File[]) {
  const problems: string[] = []
  for (const raw of files) {
    if (attachments.value.length >= MAX_ATTACH) {
      problems.push(`最多 ${MAX_ATTACH} 个附件`)
      break
    }
    let f = raw
    let mime = fileMime(raw)
    if (mime === 'image/heic' || mime === 'image/heif' || HEIC_RE.test(raw.name)) {
      try {
        f = await toJpeg(raw)
        mime = 'image/jpeg'
      } catch {
        problems.push(`${raw.name}：HEIC 解不开，请先在照片里导出为 JPEG`)
        continue
      }
    }
    if (!SERVER_MIME.has(mime)) {
      problems.push(`${raw.name}：不支持 ${mime || '未知类型'}`)
      continue
    }
    if (f.size > MAX_FILE_BYTES) {
      problems.push(`${raw.name}：${fmtSize(f.size)} 超过 5MB`)
      continue
    }
    attachments.value.push({ name: f.name, img: { mime, dataBase64: await readBase64(f), name: f.name } })
  }
  attachNotice.value = problems.join('；')
}

function attachmentPreview(mime: string, dataBase64: string): string {
  if (!mime.startsWith('image/')) return ''
  return `data:${mime};base64,${dataBase64}`
}

watch(attachments, () => {
  if (!attachments.value.length) attachNotice.value = ''
}, { deep: true })

async function onPickFiles(ev: Event) {
  const input = ev.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  await addFiles(files)
}

function onPaste(ev: ClipboardEvent) {
  const files = Array.from(ev.clipboardData?.files ?? [])
  if (!files.length) return
  ev.preventDefault()
  void addFiles(files)
}

const dragActive = ref(false)
let dragDepth = 0

function canDrop(): boolean {
  return page.value === 'chat' && loggedIn.value
}

function hasFiles(e: DragEvent): boolean {
  return canDrop() && Array.from(e.dataTransfer?.types ?? []).includes('Files')
}

function onDragEnter(e: DragEvent) {
  if (!hasFiles(e)) return
  e.preventDefault()
  dragDepth++
  dragActive.value = true
}

function onDragOver(e: DragEvent) {
  if (!hasFiles(e)) return
  e.preventDefault()
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
}

function onDragLeave() {
  dragDepth = Math.max(0, dragDepth - 1)
  if (!dragDepth) dragActive.value = false
}

async function onDrop(e: DragEvent) {
  e.preventDefault()
  dragDepth = 0
  dragActive.value = false
  const files = Array.from(e.dataTransfer?.files ?? [])
  if (files.length) await addFiles(files)
}

function removeAttachment(i: number) {
  attachments.value.splice(i, 1)
}

function isImage(m: MediaRef): boolean {
  return m.mime.startsWith('image/')
}

function mediaLabel(m: MediaRef): string {
  if (m.name) return m.name
  if (m.seq != null) return `图片 ${m.seq}`
  return `${m.mime} ${fmtSize(m.size)}`
}

function fmtSize(n: number): string {
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)}MB`
  if (n >= 1024) return `${Math.round(n / 1024)}KB`
  return `${n}B`
}

function fmtTok(n: number): string {
  if (n >= 1000) {
    const k = n / 1000
    return `${k >= 100 ? Math.round(k) : Number(k.toFixed(1))}k`
  }
  return String(n)
}
function usageText(u: Usage): string {
  const a = fmtTok(u.promptTokens)
  const line = `↑${a} ↓${fmtTok(u.completionTokens)}`
  if (!u.window) return line
  const pct = (u.promptTokens / u.window) * 100
  const p = pct > 0 && pct < 1 ? '<1' : String(Math.round(pct))
  return `${line} · ${a}/${fmtTok(u.window)} (${p}%)`
}
function usageClass(u: Usage): string {
  if (!u.window) return ''
  const pct = (u.promptTokens / u.window) * 100
  return pct > 90 ? 'hot' : pct > 70 ? 'warm' : ''
}

function scrollBottom() {
  const el = listEl.value
  if (el) el.scrollTop = el.scrollHeight
}

function jumpBottom() {
  pinned.value = true
  scrollBottom()
}

watch(bubbles, () => {
  if (pinned.value) nextTick(scrollBottom)
}, { deep: true })

const mediaSrc = ref<Record<string, string>>({})
const mediaPending = new Set<string>()

async function loadMedia(path: string) {
  if (mediaSrc.value[path] || mediaPending.has(path)) return
  mediaPending.add(path)
  try {
    const res = await authFetch(mediaUrl(path))
    if (!res.ok) return
    const url = URL.createObjectURL(await res.blob())
    if (mediaSrc.value[path]) {
      URL.revokeObjectURL(url)
      return
    }
    mediaSrc.value = { ...mediaSrc.value, [path]: url }
  } finally {
    mediaPending.delete(path)
  }
}

watch(
  bubbles,
  () => {
    for (const b of bubbles.value) for (const m of b.media ?? []) void loadMedia(m.path)
  },
  { deep: true, immediate: true },
)

watch(authState, async (ok) => {
  if (!ok) {
    newSession()
    askNew.value = false
    menuOpen.value = false
    return
  }
  await loadTenants()
  await reconcile()
  await nextTick()
  scrollBottom()
  composerEl.value?.focus()
})

// 成员页归档、离开也会 switchTenant，但够不着 useChat。
// 登录时 id 从空变成有值，那一轮由上面的 authState 负责，这里跳过，避免拉两次。
watch(
  () => tenant.value.id,
  async (id, prev) => {
    if (!loggedIn.value || !id || !prev || id === prev) return
    await showTenantSession()
    await loadTenants()
    await nextTick()
    scrollBottom()
  },
)

onMounted(async () => {
  window.addEventListener('keydown', onWindowKey)
  window.addEventListener('pointerdown', onDocPointer)
  if (!loggedIn.value) return
  await loadTenants()
  await reconcile()
  await nextTick()
  scrollBottom()
  composerEl.value?.focus()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onWindowKey)
  window.removeEventListener('pointerdown', onDocPointer)
  for (const url of Object.values(mediaSrc.value)) URL.revokeObjectURL(url)
})

async function onScroll() {
  const el = listEl.value
  if (!el) return
  pinned.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
  if (el.scrollTop < 40 && !atStart.value) {
    const before = el.scrollHeight
    await loadOlder()
    await nextTick()
    el.scrollTop += el.scrollHeight - before
  }
}
</script>

<template>
  <InviteLanding v-if="inviteToken" :token="inviteToken" @leave="leaveInvite" />
  <div
    v-else
    class="app"
    @dragenter="onDragEnter"
    @dragover="onDragOver"
    @dragleave="onDragLeave"
    @drop="onDrop"
  >
    <aside v-if="loggedIn" class="rail">
      <div class="brand">
        <span class="mark" aria-hidden="true">t</span>
        <div>
          <strong>tako</strong>
          <span class="brand-sub">工作区</span>
        </div>
      </div>

      <div class="new-wrap">
        <button type="button" class="new-chat" @click="requestNewSession">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">
            <path d="M8 3v10M3 8h10" />
          </svg>
          新对话
        </button>
        <div v-if="askNew" class="pop" role="dialog" aria-labelledby="new-chat-title">
          <p id="new-chat-title">新开一段会忘掉浏览器里记住的当前对话。</p>
          <div class="pop-actions">
            <button type="button" class="pop-go" @click="confirmNewSession">仍然新开</button>
            <button type="button" class="pop-stay" @click="askNew = false">取消</button>
          </div>
        </div>
      </div>

      <nav class="nav" aria-label="页面">
        <button type="button" :class="{ on: page === 'chat' }" :aria-current="page === 'chat' ? 'page' : undefined" @click="page = 'chat'">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M3 3.5h10v7.2H6.2L3 13z" stroke-linejoin="round" /></svg>
          对话
        </button>
        <button type="button" :class="{ on: page === 'agents' }" :aria-current="page === 'agents' ? 'page' : undefined" @click="page = 'agents'">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="8" cy="5.2" r="2.2" /><path d="M3.4 13.2c.7-2.2 2.4-3.2 4.6-3.2s3.9 1 4.6 3.2" stroke-linecap="round" /></svg>
          助手
        </button>
        <button type="button" :class="{ on: page === 'team' }" :aria-current="page === 'team' ? 'page' : undefined" @click="page = 'team'">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><rect x="2.5" y="2.5" width="11" height="11" rx="2" /><path d="M5 8h6M8 5v6" stroke-linecap="round" /></svg>
          团队
        </button>
      </nav>

      <div class="rail-foot">
        <span class="avatar" aria-hidden="true">{{ initial }}</span>
        <div ref="switcherEl" class="switcher">
          <button
            type="button"
            class="switcher-btn"
            :aria-expanded="menuOpen"
            aria-haspopup="menu"
            @click="toggleMenu"
          >
            <span class="who">
              <strong>{{ tenant.name || '当前工作区' }}</strong>
              <span>{{ roleLabel(tenant.role) || '已登录' }}</span>
            </span>
            <svg class="chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">
              <path d="M4 6.5 8 10.5 12 6.5" />
            </svg>
          </button>
          <div v-if="menuOpen" class="switcher-pop" role="menu">
            <div data-slot="pending-invites">
              <PendingInvites @changed="loadTenants" />
            </div>
            <template v-if="!createOpen">
              <button
                v-for="row in tenants"
                :key="row.id"
                type="button"
                role="menuitem"
                class="switcher-row"
                :class="{ on: row.id === tenant.id }"
                :disabled="switching"
                @click="pickTenant(row)"
              >
                <strong>{{ row.name }}</strong>
                <span class="tag">{{ kindLabel(row.kind) }}</span>
                <span class="tag">{{ roleLabel(row.role) }}</span>
              </button>
              <p v-if="switchError" class="auth-error" role="alert">{{ switchError }}</p>
              <button type="button" class="switch-create" @click="openCreate">+ 创建工作区</button>
            </template>
            <form v-else class="switch-create-form" @submit.prevent="createWorkspace">
              <label>
                工作区名称
                <input v-model="createName" type="text" autocomplete="off" />
              </label>
              <p v-if="createError" class="auth-error" role="alert">{{ createError }}</p>
              <div class="pop-actions">
                <button type="submit" class="pop-go" :disabled="createBusy">{{ createBusy ? '正在创建…' : '创建' }}</button>
                <button type="button" class="pop-stay" @click="createOpen = false">取消</button>
              </div>
            </form>
          </div>
        </div>
        <button type="button" class="logout" @click="logout">退出</button>
      </div>
    </aside>

    <div class="stage">
      <div v-if="dragActive" class="drop-hint" aria-hidden="true">松开，加到这条消息里</div>

      <form v-if="!loggedIn" class="gate" @submit.prevent="doLogin">
        <div class="gate-copy">
          <span class="mark lg" aria-hidden="true">t</span>
          <h1>tako</h1>
          <p>登录之后可以对话、改助手、配这个工作区。刷新这个标签页，同一段对话还能接上。</p>
        </div>
        <div class="gate-card">
          <h2>登录</h2>
          <label>
            邮箱
            <input v-model="email" type="email" autocomplete="username" required autofocus />
          </label>
          <label>
            密码
            <span class="secret">
              <input v-model="password" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" required />
              <button type="button" class="ghost-link" @click="showPassword = !showPassword">{{ showPassword ? '隐藏' : '显示' }}</button>
            </span>
          </label>
          <button type="submit" class="primary" :disabled="authBusy">{{ authBusy ? '正在登录…' : '登录' }}</button>
          <p v-if="authError" class="auth-error" role="alert">{{ authError }}</p>
        </div>
      </form>

      <template v-else>
        <Configure v-if="page !== 'chat'" :key="tenant.id" :page="configPage" />

        <div v-show="page === 'chat'" class="thread">
          <section ref="listEl" class="list" @scroll="onScroll">
            <div v-if="!bubbles.length" class="empty">
              <p class="empty-kicker">对话</p>
              <h2>从一句话开始</h2>
              <p class="empty-hint">发出去之后，回复会逐字出现。往上滚可以翻更早的记录。</p>
              <div class="starters">
                <button v-for="s in starters" :key="s" type="button" @click="applyStarter(s)">{{ s }}</button>
              </div>
            </div>

            <div v-if="loadingOlder" class="older">
              <span class="spinner" aria-hidden="true"></span>正在加载更早的记录
            </div>
            <div v-else-if="bubbles.length && !atStart" class="older muted">上滚查看更早的记录</div>

            <article v-for="b in bubbles" :key="b.key" :class="['bubble', b.role, b.state]">
              <div class="bubble-main">
                <template v-if="b.media?.length">
                  <template v-for="(m, i) in b.media" :key="`${b.key}:m${i}`">
                    <button
                      v-if="isImage(m) && mediaSrc[m.path]"
                      type="button"
                      class="media-btn"
                      :aria-label="`查看 ${mediaLabel(m)}`"
                      @click="lightbox = mediaSrc[m.path]"
                    >
                      <img class="bubble-media" :src="mediaSrc[m.path]" :alt="mediaLabel(m)" />
                    </button>
                    <p v-else class="bubble-file">
                      <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true"><path d="M4 2.5h5l3 3V13.5H4z" stroke-linejoin="round" /><path d="M9 2.5V6h3.2" /></svg>
                      <span>{{ mediaLabel(m) }}</span>
                      <span class="file-meta">{{ m.mime }} · {{ fmtSize(m.size) }}</span>
                    </p>
                  </template>
                </template>
                <p v-if="b.text" class="bubble-text">{{ b.text }}</p>
                <span v-else-if="b.state === 'streaming' || b.state === 'offline'" class="typing" aria-label="正在生成">
                  <i></i><i></i><i></i>
                </span>
                <p v-if="b.state === 'offline'" class="note">拉不到帧，改看会话状态</p>
                <p v-if="b.state === 'queued'" class="note">会话忙，这条还在排队</p>
                <p v-if="b.state === 'stopped'" class="note">已停止，没有写进历史</p>
                <p v-if="b.state === 'error'" class="note">失败，没有写进历史</p>
                <span v-if="b.role === 'assistant' && b.usage" class="usage" :class="usageClass(b.usage)">{{ usageText(b.usage) }}</span>
                <span v-else-if="b.role === 'assistant' && b.usageUnknown" class="usage">用量 ?</span>
                <button
                  v-if="b.text"
                  type="button"
                  class="copy"
                  @click="copyBubble(b.key, b.text)"
                >{{ copiedKey === b.key ? '已复制' : '复制' }}</button>
              </div>
            </article>
          </section>

          <p v-if="attachNotice" class="attach-notice" role="alert">{{ attachNotice }}</p>
          <form class="composer" @submit.prevent="onSend">
            <input
              ref="fileEl"
              type="file"
              accept="image/*,image/heic,image/heif,.heic,.heif,application/pdf,audio/*,video/*"
              multiple
              hidden
              @change="onPickFiles"
            />
            <div v-if="attachments.length" class="chips">
              <span v-for="(a, i) in attachments" :key="i" class="chip">
                <img v-if="attachmentPreview(a.img.mime, a.img.dataBase64)" :src="attachmentPreview(a.img.mime, a.img.dataBase64)" alt="" />
                <span class="chip-name">{{ a.name }}</span>
                <button type="button" class="chip-x" aria-label="移除附件" @click="removeAttachment(i)">×</button>
              </span>
            </div>
            <textarea
              ref="composerEl"
              v-model="draft"
              rows="1"
              enterkeyhint="send"
              :placeholder="pending.length ? `还有 ${pending.length} 条在排队，这轮结束后自动发出` : '写一条消息'"
              aria-label="消息"
              @keydown="onDraftKeydown"
              @paste="onPaste"
            />
            <div class="composer-bar">
              <button
                type="button"
                class="icon-btn"
                :disabled="attachments.length >= MAX_ATTACH"
                aria-label="添加图片或文件"
                title="添加图片或文件。选不了「照片」时，可以从照片 app 拖进来，也可以直接粘贴。"
                @click="fileEl?.click()"
              >
                <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true">
                  <path d="M13.2 7.6 8 12.8a3.4 3.4 0 0 1-4.8-4.8l5.4-5.4a2.3 2.3 0 0 1 3.2 3.2l-5.4 5.4a1.1 1.1 0 0 1-1.6-1.6L9.6 4.8" />
                </svg>
              </button>
              <span v-if="pending.length" class="hint queue">排队 {{ pending.length }}</span>
              <span v-else-if="busy" class="hint live">正在回复</span>
              <span v-else class="hint">Enter 发送 · Shift+Enter 换行 · 可粘贴图片</span>
              <button type="submit" class="send" :disabled="!draft.trim() && !attachments.length" aria-label="发送">
                <svg viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                  <path d="M10 16.2V4.4M5.2 8.8 10 4l4.8 4.8" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                </svg>
              </button>
            </div>
          </form>
          <button v-if="!pinned && bubbles.length" type="button" class="jump" @click="jumpBottom">回到最新</button>
        </div>
      </template>
    </div>

    <div v-if="lightbox" class="lightbox" role="dialog" aria-label="查看图片" @click="lightbox = ''">
      <img :src="lightbox" alt="" />
    </div>
    <span class="sr-only" aria-live="polite">{{ copiedKey ? '已复制' : '' }}</span>
  </div>
</template>

<style scoped>
.app {
  height: 100%;
  display: flex;
  background:
    radial-gradient(42rem 24rem at 100% -8rem, rgba(15, 110, 86, 0.08), transparent 60%),
    var(--bg);
}

.rail {
  width: 15.5rem;
  flex: none;
  display: flex;
  flex-direction: column;
  gap: 0.85rem;
  padding: 1.1rem 0.85rem 0.9rem;
  background: rgba(232, 226, 214, 0.72);
  border-right: 1px solid var(--line);
}

.brand,
.rail-foot,
.nav button,
.new-chat,
.composer-bar,
.note,
.older,
.chip,
.bubble-file {
  display: flex;
  align-items: center;
}

.brand {
  gap: 0.65rem;
  padding: 0.15rem 0.35rem 0.2rem;
}
.brand strong {
  display: block;
  font-size: 0.98rem;
  letter-spacing: 0.04em;
}
.brand-sub,
.who span,
.hint,
.file-meta {
  color: var(--muted);
  font-size: 0.75rem;
}

.mark {
  width: 1.85rem;
  height: 1.85rem;
  display: grid;
  place-items: center;
  border-radius: 0.6rem;
  background: var(--ink);
  color: #f6f1e7;
  font-weight: 650;
  letter-spacing: -0.03em;
}
.mark.lg {
  width: 2.6rem;
  height: 2.6rem;
  border-radius: 0.8rem;
  font-size: 1.25rem;
}

.new-chat,
.primary,
.send,
.pop-go {
  border: 0;
  background: var(--ink);
  color: #f6f1e7;
}
.new-wrap { position: relative; }
.new-chat {
  gap: 0.4rem;
  justify-content: center;
  border-radius: 999px;
  padding: 0.55rem 0.8rem;
  font-size: 0.86rem;
}
.new-chat svg {
  width: 0.9rem;
  height: 0.9rem;
}
.new-chat:hover,
.primary:hover:not(:disabled),
.send:hover:not(:disabled) {
  background: #292524;
}

.pop {
  position: absolute;
  z-index: 6;
  top: 0;
  left: calc(100% + 0.55rem);
  width: 16rem;
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: 14px;
  padding: 0.75rem;
  box-shadow: var(--shadow);
}
.pop p {
  margin: 0 0 0.65rem;
  font-size: 0.82rem;
  line-height: 1.55;
}
.pop-actions {
  display: flex;
  gap: 0.4rem;
}
.pop-go,
.pop-stay,
.logout,
.icon-btn,
.jump,
.ghost-link {
  border-radius: 999px;
  font-size: 0.78rem;
}
.pop-go,
.pop-stay {
  border: 0;
  padding: 0.35rem 0.7rem;
}
.pop-stay,
.logout,
.icon-btn,
.jump {
  background: transparent;
  color: var(--ink-soft);
  border: 1px solid var(--line-strong);
}
.pop-stay:hover,
.logout:hover,
.icon-btn:hover:not(:disabled),
.jump:hover {
  background: #fff;
  border-color: #cfc6b6;
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
}
.nav button {
  gap: 0.55rem;
  width: 100%;
  text-align: left;
  border: 0;
  background: transparent;
  color: var(--ink-soft);
  border-radius: 12px;
  padding: 0.5rem 0.65rem;
  font-size: 0.9rem;
}
.nav button svg {
  width: 1rem;
  height: 1rem;
  flex: none;
}
.nav button.on {
  background: var(--bg-raised);
  color: var(--ink);
  box-shadow: 0 1px 2px rgba(68, 48, 24, 0.06);
}
.nav button:hover:not(.on) {
  background: rgba(255, 253, 248, 0.55);
}

.rail-foot {
  margin-top: auto;
  gap: 0.55rem;
  padding: 0.45rem;
  border-radius: 14px;
  background: rgba(255, 253, 248, 0.55);
}
.avatar {
  width: 1.9rem;
  height: 1.9rem;
  flex: none;
  display: grid;
  place-items: center;
  border-radius: 50%;
  background: var(--accent-soft);
  color: var(--accent-ink);
  font-size: 0.82rem;
  font-weight: 650;
}
.who {
  min-width: 0;
  flex: 1;
}
.who strong,
.chip-name {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.82rem;
}
.logout {
  padding: 0.28rem 0.55rem;
}

.switcher {
  position: relative;
  flex: 1;
  min-width: 0;
}
.switcher-btn {
  display: flex;
  align-items: center;
  gap: 0.2rem;
  width: 100%;
  border: 0;
  background: transparent;
  color: inherit;
  text-align: left;
  padding: 0;
}
.switcher-btn .who { flex: 1; }
.chevron {
  width: 0.85rem;
  height: 0.85rem;
  flex: none;
  color: var(--muted);
}
.switcher-pop {
  position: absolute;
  z-index: 8;
  bottom: calc(100% + 0.45rem);
  left: 0;
  width: 16.5rem;
  max-height: 18rem;
  overflow: auto;
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: 14px;
  box-shadow: var(--shadow);
  padding: 0.35rem;
}
.switcher-row,
.switch-create {
  display: flex;
  align-items: baseline;
  gap: 0.4rem;
  width: 100%;
  border: 0;
  background: transparent;
  color: var(--ink);
  border-radius: 10px;
  padding: 0.45rem 0.55rem;
  text-align: left;
  font-size: 0.82rem;
}
.switcher-row strong {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.82rem;
}
.switcher-row .tag {
  flex: none;
  color: var(--muted);
  font-size: 0.72rem;
}
.switcher-row.on {
  background: var(--accent-soft);
}
.switcher-row:hover:not(:disabled),
.switch-create:hover {
  background: rgba(255, 253, 248, 0.9);
}
.switcher-row:disabled { opacity: 0.6; }
.switch-create {
  border-top: 1px solid var(--line);
  border-radius: 0;
  margin-top: 0.25rem;
  color: var(--accent-ink);
}
.switch-create-form {
  display: flex;
  flex-direction: column;
  gap: 0.45rem;
  padding: 0.4rem 0.45rem 0.35rem;
}
.switch-create-form label {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  font-size: 0.75rem;
  color: var(--muted);
}
.switch-create-form input {
  border: 1px solid var(--line-strong);
  border-radius: 10px;
  background: #fff;
  color: var(--ink);
  padding: 0.4rem 0.55rem;
  font-size: 0.86rem;
}
.switcher-pop .auth-error { margin: 0.3rem 0.4rem; }

.stage {
  position: relative;
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.gate {
  margin: auto;
  width: min(100% - 2rem, 52rem);
  display: grid;
  grid-template-columns: 1.1fr 0.9fr;
  gap: 2.5rem;
  align-items: center;
}
.gate-copy h1 {
  margin: 0.8rem 0 0.45rem;
  font-size: 2.4rem;
  letter-spacing: 0.04em;
  font-weight: 650;
}
.gate-copy p {
  margin: 0;
  max-width: 28rem;
  color: var(--ink-soft);
  line-height: 1.7;
}
.gate-card {
  display: flex;
  flex-direction: column;
  gap: 0.8rem;
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 1.35rem;
  box-shadow: var(--shadow);
}
.gate-card h2 {
  margin: 0;
  font-size: 1.15rem;
}
.gate-card label,
.secret {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  font-size: 0.8rem;
  color: var(--ink-soft);
}
.secret {
  position: relative;
}
.gate-card input {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 12px;
  padding: 0.7rem 0.8rem;
  font-size: 0.95rem;
}
.gate-card .secret input { padding-right: 3.6rem; }
.gate-card input:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.14);
}
.ghost-link {
  position: absolute;
  right: 0.45rem;
  bottom: 0.45rem;
  border: 0;
  background: var(--bg);
  padding: 0.2rem 0.5rem;
}
.primary {
  margin-top: 0.3rem;
  border-radius: 999px;
  padding: 0.7rem 1rem;
  font-size: 0.95rem;
}
.primary:disabled {
  opacity: 0.55;
}
.auth-error,
.attach-notice {
  margin: 0;
  font-size: 0.8rem;
  line-height: 1.5;
  border-radius: 12px;
}
.auth-error {
  color: var(--danger);
  background: var(--danger-soft);
  padding: 0.55rem 0.7rem;
}

.thread {
  width: min(46rem, calc(100% - 2rem));
  margin: 0 auto;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  padding-bottom: 1rem;
}
.list {
  flex: 1;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 0.85rem;
  padding: 1.4rem 0.2rem 1rem;
  scrollbar-width: thin;
  scrollbar-color: var(--line-strong) transparent;
}
.empty {
  margin: auto;
  max-width: 34rem;
  padding: 2rem 0 3rem;
}
.empty-kicker {
  margin: 0 0 0.4rem;
  color: var(--accent-ink);
  font-size: 0.75rem;
  letter-spacing: 0.14em;
  text-transform: uppercase;
}
.empty h2 {
  margin: 0;
  font-size: 2rem;
  font-weight: 620;
  letter-spacing: -0.03em;
}
.empty-hint {
  margin: 0.55rem 0 1.2rem;
  color: var(--muted);
}
.starters {
  display: flex;
  flex-direction: column;
  gap: 0.45rem;
}
.starters button {
  text-align: left;
  border: 1px solid var(--line);
  background: rgba(255, 253, 248, 0.8);
  border-radius: 14px;
  padding: 0.75rem 0.9rem;
  color: var(--ink-soft);
}
.starters button:hover {
  border-color: #b7d7cc;
  background: #fff;
  color: var(--ink);
}

.older {
  align-self: center;
  gap: 0.4rem;
  font-size: 0.75rem;
  color: var(--ink-soft);
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0.25rem 0.75rem;
}
.older.muted {
  color: var(--muted);
  background: transparent;
  border-color: transparent;
}
.spinner {
  width: 0.75rem;
  height: 0.75rem;
  border-radius: 50%;
  border: 2px solid #d5ddd6;
  border-top-color: var(--accent);
  animation: spin 0.8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

.bubble {
  max-width: min(100%, 40rem);
  display: flex;
  align-items: flex-end;
  gap: 0.35rem;
  animation: rise 0.18s ease-out;
}
@keyframes rise {
  from { opacity: 0; transform: translateY(6px); }
  to { opacity: 1; transform: none; }
}
.bubble.user { align-self: flex-end; flex-direction: row-reverse; }
.bubble.assistant { align-self: flex-start; }
.bubble-main {
  padding: 0.72rem 0.95rem;
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-word;
}
.bubble p { margin: 0; }
.bubble.user .bubble-main {
  color: #f6f1e7;
  background: var(--ink);
  border-radius: 1.15rem 1.15rem 0.4rem 1.15rem;
}
.bubble.assistant .bubble-main {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 0.4rem 1.15rem 1.15rem 1.15rem;
}
.bubble.assistant.streaming .bubble-main {
  box-shadow: inset 2px 0 0 var(--accent);
}
.bubble.assistant.streaming .bubble-text::after {
  content: "";
  display: inline-block;
  width: 0.45rem;
  height: 1em;
  margin-left: 0.15rem;
  background: var(--accent);
  opacity: 0.7;
  vertical-align: text-bottom;
  animation: caret 1s steps(1) infinite;
}
@keyframes caret {
  50% { opacity: 0; }
}
.bubble.queued { opacity: 0.72; }
.bubble.assistant.queued .bubble-main { border-style: dashed; }
.bubble.error .bubble-main {
  background: var(--danger-soft);
  border-color: #fecdd3;
  color: var(--danger);
  box-shadow: none;
}
.copy {
  display: block;
  width: fit-content;
  margin-top: 0.4rem;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--muted);
  font-size: 0.72rem;
}
.bubble.user .copy { color: rgba(246, 241, 231, 0.72); }
.copy:hover { color: var(--ink); }
.bubble.user .copy:hover { color: #f6f1e7; }

.typing {
  display: inline-flex;
  gap: 0.28rem;
  height: 1.2em;
  align-items: center;
}
.typing i {
  width: 0.36rem;
  height: 0.36rem;
  border-radius: 50%;
  background: #b7b0a4;
  animation: blink 1.2s ease-in-out infinite;
}
.typing i:nth-child(2) { animation-delay: 0.15s; }
.typing i:nth-child(3) { animation-delay: 0.3s; }
@keyframes blink {
  0%, 60%, 100% { opacity: 0.3; transform: translateY(0); }
  30% { opacity: 1; transform: translateY(-2px); }
}
.note {
  gap: 0.3rem;
  margin-top: 0.4rem !important;
  font-size: 0.75rem;
  color: var(--muted);
}
.bubble.error .note { color: var(--danger); }
.bubble-file {
  gap: 0.4rem;
  flex-wrap: wrap;
  font-size: 0.82rem;
}
.bubble-file svg {
  width: 0.95rem;
  height: 0.95rem;
  flex: none;
}
.media-btn {
  display: block;
  padding: 0;
  border: 0;
  background: transparent;
  margin-bottom: 0.4rem;
}
.bubble-media {
  display: block;
  max-width: min(100%, 20rem);
  max-height: 18rem;
  border-radius: 0.7rem;
  background: #fff;
}
.usage {
  display: block;
  margin-top: 0.4rem;
  font-size: 0.72rem;
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.usage.warm { color: #b45309; }
.usage.hot { color: var(--danger); }

.attach-notice {
  color: var(--warn);
  background: var(--warn-soft);
  border: 1px solid #fed7aa;
  padding: 0.45rem 0.7rem;
}
.drop-hint {
  position: absolute;
  inset: 0.75rem;
  z-index: 20;
  display: grid;
  place-items: center;
  color: var(--accent-ink);
  background: rgba(229, 243, 238, 0.92);
  border: 1.5px dashed #7dbaa8;
  border-radius: 22px;
  pointer-events: none;
  font-size: 1rem;
}
.composer {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: 22px;
  padding: 0.7rem 0.7rem 0.55rem;
  box-shadow: var(--shadow);
}
.composer:focus-within {
  border-color: #8fbfb0;
  box-shadow: 0 0 0 4px rgba(15, 110, 86, 0.1), var(--shadow);
}
.composer textarea {
  width: 100%;
  resize: none;
  border: 0;
  outline: none;
  background: transparent;
  padding: 0.25rem 0.35rem 0;
  line-height: 1.55;
  max-height: 10.5rem;
}
.composer textarea::placeholder { color: #a8a29e; }
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 0.35rem;
  padding: 0 0.2rem;
}
.chip {
  gap: 0.35rem;
  max-width: 16rem;
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0.15rem 0.35rem 0.15rem 0.15rem;
  font-size: 0.75rem;
}
.chip img {
  width: 1.45rem;
  height: 1.45rem;
  object-fit: cover;
  border-radius: 999px;
}
.chip-x {
  border: 0;
  background: transparent;
  color: var(--muted);
  padding: 0 0.25rem;
  font-size: 0.95rem;
  line-height: 1;
}
.chip-x:hover { color: var(--danger); }
.composer-bar { gap: 0.55rem; padding-left: 0.1rem; }
.icon-btn,
.send {
  width: 2.15rem;
  height: 2.15rem;
  display: grid;
  place-items: center;
  flex: none;
  padding: 0;
}
.icon-btn { border-radius: 50%; }
.icon-btn svg,
.send svg { width: 1.05rem; height: 1.05rem; }
.icon-btn:disabled { opacity: 0.4; }
.hint {
  margin-right: auto;
  min-width: 0;
}
.hint.live { color: var(--accent-ink); }
.hint.queue { color: #b45309; }
.send { border-radius: 50%; }
.send:disabled {
  background: #e7e0d4;
  color: #a8a29e;
}

.jump {
  position: absolute;
  left: 50%;
  bottom: 8.2rem;
  transform: translateX(-50%);
  padding: 0.35rem 0.75rem;
  background: var(--bg-raised);
  box-shadow: var(--shadow);
}

.lightbox {
  position: fixed;
  inset: 0;
  z-index: 40;
  display: grid;
  place-items: center;
  padding: 2rem;
  background: rgba(28, 25, 23, 0.78);
}
.lightbox img {
  max-width: min(92vw, 64rem);
  max-height: 88vh;
  border-radius: 14px;
}

@media (max-width: 860px) {
  .app { flex-direction: column; }
  .rail {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    grid-template-areas:
      "brand new foot"
      "nav nav nav";
    align-items: center;
    width: auto;
    border-right: 0;
    border-bottom: 1px solid var(--line);
    padding: 0.65rem 0.7rem 0.55rem;
    gap: 0.45rem 0.55rem;
  }
  .brand { grid-area: brand; }
  .new-wrap { grid-area: new; }
  .nav { grid-area: nav; flex-direction: row; }
  .brand-sub,
  .rail-foot .who span { display: none; }
  .nav button { width: auto; padding: 0.4rem 0.55rem; }
  .rail-foot { grid-area: foot; margin: 0; background: transparent; padding: 0; }
  .who { max-width: 4.6rem; }
  .switcher-pop {
    bottom: auto;
    top: calc(100% + 0.35rem);
    left: auto;
    right: 0;
    width: min(16.5rem, calc(100vw - 1.4rem));
  }
  .rail { position: relative; }
  .new-wrap { position: static; }
  .pop {
    top: calc(100% + 0.35rem);
    left: 0.7rem;
    width: min(16rem, calc(100vw - 1.4rem));
  }
  .gate {
    grid-template-columns: 1fr;
    gap: 1.2rem;
    width: min(100% - 1.5rem, 28rem);
  }
  .gate-copy h1 { font-size: 1.8rem; }
  .thread { width: min(100% - 1.2rem, 46rem); }
  .hint:not(.live):not(.queue) { display: none; }
}
</style>
