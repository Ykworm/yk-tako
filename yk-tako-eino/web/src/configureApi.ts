// 配置页的请求层。登录态在 auth.ts，与对话页共用同一份 token。
import { authFetch } from './auth'

export type Envelope<T> = { code: number; message: string; data: T | null }

export type ToolPolicy = { mode: string; names: string[] }

export type AgentRow = {
  id: string
  defaultId: string
  source: 'default' | 'override' | 'custom' | string
  overridden: boolean
  name: string
  description: string
  spec: string
  modelRef: string
  toolPolicy: ToolPolicy
  visibility: string
}

export type McpServer = { command: string; args: string[]; env: Record<string, string> }

export type RuntimeConfig = {
  modelRef: string
  mcpServers: Record<string, McpServer>
  secrets: Record<string, string>
  sandbox: { network: boolean }
}

export type McpItem = { name: string; state: string; tools: string[]; reason?: string }

export async function api<T>(method: string, path: string, body?: unknown): Promise<{ status: number; body: Envelope<T> }> {
  const headers: Record<string, string> = {}
  let payload: string | undefined
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const res = await authFetch(path, { method, headers, body: payload })
  if (res.status === 204) {
    return { status: 204, body: { code: 0, message: '', data: null } }
  }
  const text = await res.text()
  if (!text) {
    return { status: res.status, body: { code: res.ok ? 0 : -1, message: '', data: null } }
  }
  const parsed = JSON.parse(text) as Envelope<T>
  return { status: res.status, body: parsed }
}

export function errorText(status: number, body: Envelope<unknown>): string {
  const msg = body.message || '请求失败'
  return status + '：' + msg
}
