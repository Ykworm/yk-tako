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
  authState.value = hasSession()
}

// 刷新只换 token 对，工作区信息来自登录时那一次。
export function updateTokens(accessToken: string, refreshToken: string) {
  sessionStorage.setItem(tokenKey, accessToken)
  sessionStorage.setItem(refreshKey, refreshToken)
  authState.value = hasSession()
}

export function clearAuth() {
  sessionStorage.removeItem(tokenKey)
  sessionStorage.removeItem(refreshKey)
  sessionStorage.removeItem(tenantKey)
  sessionStorage.removeItem(tenantNameKey)
  sessionStorage.removeItem(roleKey)
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
    clearAuth()
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
    clearAuth()
    return res
  }
  if (!(await refreshAccessToken())) return res
  return rawFetch(url, init)
}
