// 协议层：19v6-01 §1–§4 期望字节的 TS 镜像。U1 只定义，U2–U5 消费。
// 命名类型在 TS 里只是别名、不拦传参——「两种游标别混」靠参数名分开
// （eventOffset / runlogOffset 各是各端点的参数，不存在统一 cursor 抽象）。

export type SessionID = string
export type RunID = string

export type Frame = {
  eventOffset?: number
  kind: 'start' | 'delta' | 'usage' | 'terminal'
  agent?: string
  model?: string
  messageId?: string
  text?: string
  status?: string
  // usage 帧（doc/27 §4a）负载：账单缺失时引擎不发帧，这里不会出现全 0。
  promptTokens?: number
  completionTokens?: number
  reasoningTokens?: number
  totalTokens?: number
  window?: number
}

// usage 账单展示切片（doc/27 §4a/§4b）：帧与 log 行 assistant 行的 usage 字段共用同形状。
// window = 引擎 -compact-window 配置值；0 = 未配窗，只显示花销不显示占用。
export type Usage = { promptTokens: number; completionTokens: number; window: number }

export type Row = {
  runlogOffset?: number
  runId: RunID
  role: 'user' | 'assistant'
  messageId?: string
  text: string
  // assistant 行的 provider 账单（后端从 payload 列解出注入；user/tool 行不写该字段）。
  usage?: Usage
}

export type PostRunsResponse = {
  sessionId: SessionID
  runId: RunID
  messageId: string
}

export type SessionStatus = {
  liveRunId: RunID
  runlogOffset: number
  lastRunStatus: string
}

export type FramesResponse = {
  frames: Frame[]
  nextOffset: number
  status: 'generating' | 'done' | 'gone'
}

export type LogResponse = {
  rows: Row[]
  nextOffset: number
}

export type ApiErrorBody = { error: string }

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly body: ApiErrorBody | null,
  ) {
    super(`api ${status}: ${JSON.stringify(body)}`)
  }
}

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, init)
  if (!res.ok) {
    let body: ApiErrorBody | null = null
    try {
      body = (await res.json()) as ApiErrorBody
    } catch {
      // 非 JSON 错误体（契约外），body 保持 null
    }
    throw new ApiError(res.status, body)
  }
  return (await res.json()) as T
}

// POST /api/runs（19v6-01 §1）：sessionId 省略 = 懒建会话；带上 = 只认登记过的。
export function postRuns(messageId: string, text: string, sessionId?: SessionID): Promise<PostRunsResponse> {
  return request<PostRunsResponse>('/api/runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(sessionId ? { sessionId, messageId, text } : { messageId, text }),
  })
}

// GET /api/sessions/{sessionId}（19v6-01 §4）：404 = session_not_found。
export function getSession(sessionId: SessionID): Promise<SessionStatus> {
  return request<SessionStatus>(`/api/sessions/${encodeURIComponent(sessionId)}`)
}

// GET /api/runs/{runId}/frames（19v6-01 §2）：未知/过期 run 不是 404，是 200 status:gone。
export function getFrames(runId: RunID, eventOffset: number): Promise<FramesResponse> {
  return request<FramesResponse>(
    `/api/runs/${encodeURIComponent(runId)}/frames?eventOffset=${eventOffset}`,
  )
}

// GET /api/sessions/{sessionId}/log（19v6-01 §3）：游标是 runlogOffset，初始值不是 0（窗口化）。
export function getLog(sessionId: SessionID, runlogOffset: number): Promise<LogResponse> {
  return request<LogResponse>(
    `/api/sessions/${encodeURIComponent(sessionId)}/log?runlogOffset=${runlogOffset}`,
  )
}
