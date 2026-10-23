// 登录态的唯一存放处：配置页与对话页共用，谁登录两边都算登录。
// 键名沿用 U1 时期的 tako.configure.*，已登录的标签页不用重新登录。
import { ref } from 'vue'

const tokenKey = 'tako.configure.token'
const refreshKey = 'tako.configure.refresh'
const tenantKey = 'tako.configure.tenant'
const tenantNameKey = 'tako.configure.tenantName'
const roleKey = 'tako.configure.role'

export function readToken(): string {
  return sessionStorage.getItem(tokenKey) || ''
}

export function readRefreshToken(): string {
  return sessionStorage.getItem(refreshKey) || ''
}

export function readTenant(): { id: string; name: string; role: string } {
  return {
    id: sessionStorage.getItem(tenantKey) || '',
    name: sessionStorage.getItem(tenantNameKey) || '',
    role: sessionStorage.getItem(roleKey) || '',
  }
}

function hasSession(): boolean {
  return readToken() !== '' && readTenant().id !== ''
}

// 响应式登录态：api.ts 刷新失败会 clearAuth，页面据此退回登录表单。
export const authState = ref(hasSession())
// 被登出的原因：true = token 续不上（过期/刷新令牌也失效），登录表单要说一句「登录已过期」。
// 用户自己点「退出」不置这个标记——同一张表单，两种来路不能长成一模一样。
export const authExpired = ref(false)
// 当前工作区单独做成响应式：切换时人已经登录，authState 仍是 true，顶栏要靠这里改名字和角色。
export const tenantState = ref(readTenant())

export function saveAuth(
  accessToken: string,
  refreshToken: string,
  tenantID: string,
  tenantName: string,
  role: string,
) {
  sessionStorage.setItem(tokenKey, accessToken)
  sessionStorage.setItem(refreshKey, refreshToken)
  sessionStorage.setItem(tenantKey, tenantID)
  sessionStorage.setItem(tenantNameKey, tenantName)
  sessionStorage.setItem(roleKey, role)
  tenantState.value = { id: tenantID, name: tenantName, role }
  authExpired.value = false
  authState.value = hasSession()
}

// 顶栏角色来自 sessionStorage。改自己的角色或转走所有者之后，人还停在这个工作区，
// 没有一次新的 switch-tenant 把 role 写回来，所以成员页重拉名单后单独记一笔。
export function rememberRole(role: string) {
  const cur = readTenant()
  if (!cur.id || cur.role === role) return
  sessionStorage.setItem(roleKey, role)
  tenantState.value = { id: cur.id, name: cur.name, role }
}

// 刷新只换 token 对，工作区信息来自登录时那一次。
export function updateTokens(accessToken: string, refreshToken: string) {
  sessionStorage.setItem(tokenKey, accessToken)
  sessionStorage.setItem(refreshKey, refreshToken)
  authState.value = hasSession()
}

// reason='expired' = 系统把用户登出（续不上），不是用户自己要退。表单据此解释来路。
export function clearAuth(reason?: 'expired') {
  sessionStorage.removeItem(tokenKey)
  sessionStorage.removeItem(refreshKey)
  sessionStorage.removeItem(tenantKey)
  sessionStorage.removeItem(tenantNameKey)
  sessionStorage.removeItem(roleKey)
  tenantState.value = { id: '', name: '', role: '' }
  authExpired.value = reason === 'expired'
  authState.value = false
}

type LoginData = {
  accessToken: string
  refreshToken?: string
  activeTenant: { id: string; name: string }
  memberships: { tenantId: string; role: string }[]
}

export type LoginResult = { ok: true } | { ok: false; status: number; message: string }

export async function loginWithPassword(email: string, password: string): Promise<LoginResult> {
  let res: Response
  try {
    res = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password }),
    })
  } catch {
    return { ok: false, status: 0, message: '连不上引擎' }
  }
  const body = (await res.json().catch(() => null)) as { code?: number; message?: string; data?: LoginData } | null
  if (!res.ok || !body || body.code !== 0 || !body.data) {
    return { ok: false, status: res.status, message: body?.message || '登录失败' }
  }
  const data = body.data
  const role = data.memberships.find((m) => m.tenantId === data.activeTenant.id)?.role || ''
  saveAuth(data.accessToken, data.refreshToken || '', data.activeTenant.id, data.activeTenant.name, role)
  return { ok: true }
}

export type SwitchResult = { ok: true } | { ok: false; status: number; code: number; message: string }

// 切换工作区：用 sessionStorage 里的 refreshToken 换一对新 token，并改写当前工作区。
// 不走 authFetch。access 过期时先刷新再重发，否则重试仍带着已经被轮换掉的旧 refreshToken。
export async function switchTenant(tenantId: string): Promise<SwitchResult> {
  if (!readRefreshToken()) {
    clearAuth()
    return { ok: false, status: 401, code: 111105, message: '登录已失效，请重新登录' }
  }
  const first = await postSwitch(tenantId, readRefreshToken())
  if (first.kind === 'unauthorized' && first.code === 111104) {
    if (!(await refreshAccessToken())) {
      return { ok: false, status: 401, code: 111105, message: '登录已失效，请重新登录' }
    }
    return settleSwitch(await postSwitch(tenantId, readRefreshToken()))
  }
  return settleSwitch(first)
}

type SwitchPost =
  | { kind: 'network' }
  | { kind: 'unauthorized'; code: number; message: string }
  | { kind: 'fail'; status: number; code: number; message: string }
  | { kind: 'ok'; data: LoginData }

async function postSwitch(tenantId: string, refreshToken: string): Promise<SwitchPost> {
  let res: Response
  try {
    res = await fetch('/api/auth/switch-tenant', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: 'Bearer ' + readToken(),
      },
      body: JSON.stringify({ tenantId, refreshToken }),
    })
  } catch {
    return { kind: 'network' }
  }
  const body = (await res.json().catch(() => null)) as { code?: number; message?: string; data?: LoginData } | null
  if (res.status === 401) {
    return { kind: 'unauthorized', code: body?.code ?? 111105, message: body?.message || '登录已失效，请重新登录' }
  }
  if (!res.ok || !body || body.code !== 0 || !body.data?.accessToken || !body.data.activeTenant) {
    return { kind: 'fail', status: res.status, code: body?.code ?? 0, message: body?.message || '切换失败' }
  }
  return { kind: 'ok', data: body.data }
}

function settleSwitch(result: SwitchPost): SwitchResult {
  if (result.kind === 'network') return { ok: false, status: 0, code: 0, message: '连不上引擎' }
  if (result.kind === 'unauthorized') {
    clearAuth()
    return { ok: false, status: 401, code: result.code, message: result.message }
  }
  if (result.kind === 'fail') return { ok: false, status: result.status, code: result.code, message: result.message }
  const data = result.data
  const role = data.memberships.find((m) => m.tenantId === data.activeTenant.id)?.role || ''
  saveAuth(data.accessToken, data.refreshToken || '', data.activeTenant.id, data.activeTenant.name, role)
  return { ok: true }
}

// refresh token 是轮换的：并发 401 必须共用同一次刷新，第二个请求拿旧 refresh 会 401 并清掉登录态。
let refreshing: Promise<boolean> | null = null

export function refreshAccessToken(): Promise<boolean> {
  if (!refreshing) {
    refreshing = doRefresh().finally(() => {
      refreshing = null
    })
  }
  return refreshing
}

async function doRefresh(): Promise<boolean> {
  const rt = readRefreshToken()
  if (!rt) return false
  let res: Response
  try {
    res = await fetch('/api/auth/refresh', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refreshToken: rt }),
    })
  } catch {
    // 网络错误不代表登录失效；保留 token，下一拍还能再试。
    return false
  }
  const body = (await res.json().catch(() => null)) as {
    code?: number
    data?: { accessToken?: string; refreshToken?: string }
  } | null
  if (!res.ok || !body || body.code !== 0 || !body.data?.accessToken) {
    clearAuth('expired')
    return false
  }
  updateTokens(body.data.accessToken, body.data.refreshToken || rt)
  return true
}

async function rawFetch(url: string, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers)
  const token = readToken()
  if (token) headers.set('Authorization', 'Bearer ' + token)
  return fetch(url, { ...init, headers })
}

// 所有需要登录的请求都走这里：带 Bearer；401 时刷新一次再重试（只重试一次）。
export async function authFetch(url: string, init?: RequestInit): Promise<Response> {
  const res = await rawFetch(url, init)
  if (res.status !== 401) return res
  if (!readRefreshToken()) {
    // 旧会话只有 access token（U1 时期登录）：过期后无法续，退回登录表单。
    clearAuth('expired')
    return res
  }
  if (!(await refreshAccessToken())) return res
  return rawFetch(url, init)
}
