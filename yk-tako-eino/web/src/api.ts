// 协议层：19v6-01 §1–§4 期望字节的 TS 镜像。U1 只定义，U2–U5 消费。
// **信封层（doc/40）**：`request` 是唯一拆包入口——成功剥出 `data`，失败抛 `ApiError`（带 code/message）。
// 下面的返回类型不用改：信封里的 `data` 就是原来那个业务体，字段名一个没动。
// 命名类型在 TS 里只是别名、不拦传参——「两种游标别混」靠参数名分开
// （eventOffset / runlogOffset 各是各端点的参数，不存在统一 cursor 抽象）。
import { authFetch } from './auth'

export type SessionID = string
export type RunID = string

export type Frame = {
  eventOffset?: number
  // 'resume' = 续跑种子帧（doc/30 §8.3）：**text 是整段已产出正文**，不是增量片段——
  // 它的语义跟 'delta' 相反，别用同一套追加逻辑（见 chat.ts 的 resume 分支）。
  // 'task' = 看板结构帧（doc/41 §5.2）：text 是当前全量看板的紧凑视图，name 是触发它的工具名。
  // 'question' = 等帧（doc/47 §5.2）：run 挂起等人，messageId 是 run_questions 行 id。
  kind: 'start' | 'delta' | 'usage' | 'terminal' | 'resume' | 'task' | 'question'
  agent?: string
  model?: string
  messageId?: string
  // 等帧的发起工具名（ask_user / submit_plan）与原始参数 JSON（options 从这里解析）。
  name?: string
  args?: string
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
  // 工具执行账本行（tool_call / tool_result）也在 runlog 里；对话界面只画 user/assistant。
  role: 'user' | 'assistant' | 'tool_call' | 'tool_result'
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

// 项目（doc/35）：服务端只回这几个字段。
// isDefault（doc/48 C1）：默认项目**不可删**（服务端 409 114005）——页面据此把删除按钮藏起来，
// 别让用户点一个必然失败的按钮、还先许诺「文件会清掉」。
export type Workspace = {
  id: string
  name: string
  createdAt: string
  visibility: string
  owner: boolean
  isDefault?: boolean
}

export type WorkspaceListResponse = { workspaces: Workspace[] }
export type WorkspaceResponse = { workspace: Workspace }

// GET /api/sessions?workspaceId= 的一行。workspaceId=null 是存量游离会话（新形态下每场都会挂项目）。
export type SessionListItem = {
  id: SessionID
  title: string
  visibility: string
  owner: boolean
  createdAt: string
  workspaceId: string | null
}

export type SessionListResponse = { sessions: SessionListItem[] }

// GET /api/sessions/:id/subagents 的一行（doc/39）：有 terminal 行是 complete，没有是 interrupted。
export type SubagentItem = {
  saId: string
  agentName: string
  status: string
  createdAt: string
  finishedAt?: string
}

export type SubagentListResponse = { subagents: SubagentItem[] }

// DELETE 两个端点成功后都回 {status:"deleted"}。
export type DeleteStatus = { status: string }

export type SessionStatus = {
  liveRunId: RunID
  runlogOffset: number
  lastRunStatus: string
  // doc/30 §10.3：最新一场 run 的 id（无 run 时是空串）。lastRunStatus='interrupted' 时
  // 它就是「崩溃后可被接管的那一场」——续跑的入口就靠这两个字段。
  lastRunId: RunID
  // doc/35 A4：会话挂着的项目；存量游离会话是 null。
  workspaceId: string | null
}

// GET /api/runs/{runId}（doc/30 §10.1）：读侧派生，不新增存储状态。
// status='interrupted' = 行还挂着 running 但租约已过期（执行者没了，账本还在）；
// resumable = 中断且续跑次数未用尽（次数上限由服务端 -resume-max 定，前端不显示）。
export type RunState = {
  runId: RunID
  sessionId: SessionID
  status: string
  resumeCount: number
  resumable: boolean
}

// POST /api/runs/{runId}/resume 的 202 响应（无请求体）。202 = 已有执行者接管并在跑，
// 不等于跑完了；客户端接着去拉 frames（从 eventOffset=0——旧帧已被 seedResume 删掉）。
export type ResumeResult = {
  runId: RunID
  status: string
  resumeCount: number
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

// 等人行（doc/47 §5.3）：PG 是真源，刷新/换窗口按它补齐正在等什么。
// plan 行的 question 字段存方案全文；args 是发起工具的原始参数 JSON。
export type QuestionRow = {
  questionId: string
  runId: RunID
  kind: 'question' | 'plan'
  question: string
  args: string
  status: 'pending' | 'answered' | 'stale'
  // 方案行的审批结论（doc/47 §5.3）：approve / revise；问题行与 pending 行不写。
  decision?: 'approve' | 'revise'
  createdAt: string
}

// 信封（doc/40）：所有 JSON 接口统一回 {code,message,data}。
// 成功：code=0、message 空串、data 是业务体；失败：code 是数字业务码、message 是弹窗文案、
// data 为 null（或放诊断细节，如 413 的 limit/tokens、400 的 reason）。
export type Envelope<T> = { code: number; message: string; data: T | null }

// 业务码（doc/40 §4）。页面靠 code 分支，不靠拆 message 的句子。
export const CODE_SESSION_MISSING = 112001 // 找不到这场会话
export const CODE_RUN_MISSING = 112002 // 找不到这次运行
export const CODE_SESSION_RUNNING = 112101 // 这场会话正在跑，等它跑完再发
export const CODE_NOT_RESUMABLE = 112102 // 这场 run 已经定稿，没有可续的
export const CODE_RUN_ALIVE = 112103 // 这场 run 还活着，别抢
export const CODE_RESUME_EXHAUSTED = 112104 // 续跑次数用完了，重新发一轮
export const CODE_WORKSPACE_MISSING = 114001 // 找不到这个项目
export const CODE_QUESTION_MISSING = 115002 // 找不到这个提问
export const CODE_QUESTION_ANSWERED = 115101 // 这个问题已经答过了

// ApiError 是失败信封的载体：code 给分支用，message 直接就是弹窗文案（Error 自带的 message），
// data 是可选的诊断细节。
export class ApiError extends Error {
  readonly code: number
  readonly data: unknown
  constructor(
    readonly status: number,
    readonly envelope: Envelope<unknown>,
  ) {
    super(envelope.message || `api ${status}`)
    this.name = 'ApiError'
    this.code = envelope.code
    this.data = envelope.data
  }
}

// request 是前端**唯一**的拆包入口：成功剥出 data，失败抛 ApiError。
// 各接口的返回类型不用改——信封里的 data 就是原来那个业务体，字段名一个没动。
async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await authFetch(url, init)
  let body: Envelope<T> | null = null
  try {
    body = (await res.json()) as Envelope<T>
  } catch {
    // 非 JSON 响应（代理错误页等），body 保持 null
  }
  if (!res.ok || !body || body.code !== 0) {
    throw new ApiError(
      res.status,
      body ?? { code: -1, message: `请求失败（HTTP ${res.status}）`, data: null },
    )
  }
  return body.data as T
}

// 入口图（doc/32 A1）：随 POST /api/runs 上行，服务端落盘换占位行——对话里只有 path。
// name 是用户上传文件名（A6 通信字段，可省）。
export type PostImage = { mime: string; dataBase64: string; name?: string }

// 占位行（doc/32 §3）：`[media seq=… name="…" path=… mime=… size=…B sha=…；…]` 的解析切片。
// 出现在 user 行 text 与工具回执里；前端经 GET /api/media?path=… 换回字节流显示。
// name/seq 是 A6 通信字段：seq 恒在（全会话序号）、name 可省；回执变体两者皆无——
// 三个捕获组全部可缺省容忍（存量行是只读事实，不迁移）。
export type MediaRef = { path: string; mime: string; size: number; sha: string; seq?: number; name?: string }

const MEDIA_LINE =
  /^\[media(?: seq=(\d+))?(?: name="([^"]*)")? path=(\S+) mime=(\S+) size=(\d+)B sha=(\S+?)[；;]/

// parseMedia 从一段文本里解析全部占位行：返回引用列表与剥掉占位行后的正文
// （占位行是给模型/存储的坐标，页面上由 <img>/文件片替代它）。
export function parseMedia(text: string): { media: MediaRef[]; display: string } {
  const media: MediaRef[] = []
  const lines = text.split('\n').filter((line) => {
    const m = MEDIA_LINE.exec(line)
    if (!m) return true
    media.push({
      path: m[3],
      mime: m[4],
      size: Number(m[5]),
      sha: m[6],
      ...(m[1] ? { seq: Number(m[1]) } : {}),
      ...(m[2] ? { name: m[2] } : {}),
    })
    return false
  })
  return { media, display: lines.join('\n') }
}

export function mediaUrl(path: string): string {
  return `/api/media?path=${encodeURIComponent(path)}`
}

// POST /api/runs（19v6-01 §1）：sessionId 省略 = 懒建会话；带上 = 只认登记过的。
// workspaceId 只对新会话生效：已有会话带项目 id 不会改绑定，反而让一个被删项目在预检上把这一发挡掉。
export function postRuns(
  messageId: string,
  text: string,
  sessionId?: SessionID,
  images?: PostImage[],
  workspaceId?: string,
): Promise<PostRunsResponse> {
  return request<PostRunsResponse>('/api/runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      ...(sessionId ? { sessionId } : {}),
      ...(!sessionId && workspaceId ? { workspaceId } : {}),
      messageId,
      text,
      ...(images?.length ? { images } : {}),
    }),
  })
}

// 项目（doc/35）：空间只有属主可见；顺序由服务端定（created_at,id 升序）。
export function listWorkspaces(): Promise<WorkspaceListResponse> {
  return request<WorkspaceListResponse>('/api/workspaces')
}

export function createWorkspace(name: string): Promise<WorkspaceResponse> {
  return request<WorkspaceResponse>('/api/workspaces', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
}

export function renameWorkspace(id: string, name: string): Promise<WorkspaceResponse> {
  return request<WorkspaceResponse>(`/api/workspaces/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
}

export function deleteWorkspace(id: string): Promise<DeleteStatus> {
  return request<DeleteStatus>(`/api/workspaces/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

// GET /api/workspaces/{id}/files?path=：项目目录里的文件清单（doc/48 B1）。
//
// path 是**相对项目根**的路径（默认 "."）。服务端已经把内部目录（.media / .tako）
// 与符号链接滤掉了，前端不用再判。排序也是服务端排的：目录在前，同类按名升序。
export type WorkspaceFileEntry = {
  name: string
  kind: 'file' | 'dir'
  // 只有文件才有这两个键（目录**不带**，不是 0）。
  size?: number
  modifiedAt?: string
}

export type WorkspaceFilesResponse = {
  path: string
  entries: WorkspaceFileEntry[]
}

export function listWorkspaceFiles(
  workspaceId: string,
  path = '.',
): Promise<WorkspaceFilesResponse> {
  const query = path && path !== '.' ? `?path=${encodeURIComponent(path)}` : ''
  return request<WorkspaceFilesResponse>(
    `/api/workspaces/${encodeURIComponent(workspaceId)}/files${query}`,
  )
}

// 项目内某个文件的字节出口：path 形如 workspaces/<wid>/<相对路径>（与 media 占位行同格式）。
//
// **不能**直接塞进 `<a href>`：/api/media 挂在 authn 上要 Bearer 头，浏览器直接导航
// 不会带 Authorization（会 401）。下载得先 fetch 成 blob 再触发（见 ProjectsView）。
export function workspaceFileUrl(workspaceId: string, relPath: string): string {
  return `/api/media?path=${encodeURIComponent(`workspaces/${workspaceId}/${relPath}`)}`
}

export function listSessions(workspaceId?: string): Promise<SessionListResponse> {
  const query = workspaceId ? `?workspaceId=${encodeURIComponent(workspaceId)}` : ''
  return request<SessionListResponse>(`/api/sessions${query}`)
}

export function deleteSession(sessionId: SessionID): Promise<DeleteStatus> {
  return request<DeleteStatus>(`/api/sessions/${encodeURIComponent(sessionId)}`, { method: 'DELETE' })
}

// 子代理档案（doc/39）：wire 目录不存在时是空数组。
export function getSessionSubagents(sessionId: SessionID): Promise<SubagentListResponse> {
  return request<SubagentListResponse>(
    `/api/sessions/${encodeURIComponent(sessionId)}/subagents`,
  )
}

// GET /api/sessions/{sessionId}（19v6-01 §4）：404 = 找不到这场会话（code 112001）。
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

// GET /api/runs/{runId}：读单场 run 的派生状态（doc/30 §10.1）。未知 run 是 404
// （code 112002）；其余状态原样透出（complete/error/stopped）。
export function getRun(runId: RunID): Promise<RunState> {
  return request<RunState>(`/api/runs/${encodeURIComponent(runId)}`)
}

// GET /api/runs/{runId}/questions：不传 status 拉全部（前端按 id 对帧判 pending/answered）。
export function getQuestions(runId: RunID, status?: string): Promise<QuestionRow[]> {
  const query = status ? `?status=${encodeURIComponent(status)}` : ''
  return request<QuestionRow[]>(`/api/runs/${encodeURIComponent(runId)}/questions${query}`)
}

// POST /api/questions/{id}/answer：一答终答（CAS）。409 115101 = 已被别的入口答过。
// decision 只给方案行：approve（可带改写全文）/ revise（驳回意见）。问题行不传。
export type QuestionDecision = 'approve' | 'revise'
export function postAnswer(
  questionId: string,
  answer: string,
  decision?: QuestionDecision,
): Promise<{ questionId: string; status: string }> {
  return request<{ questionId: string; status: string }>(
    `/api/questions/${encodeURIComponent(questionId)}/answer`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ answer, ...(decision ? { decision } : {}) }),
    },
  )
}

// POST /api/runs/{runId}/stop：叫停一场正在跑的 run（doc/17 §4）。
//
// 服务端用条件 UPDATE 裁决竞态：1 行 = 停止赢过正常收尾，0 行 = 已经是终态、原样回读。
// **幂等**——停了再调一次仍回 stopped，不报错（前端停了之后不必关心结果）。
// 跨副本：打到别的副本时由跑着那侧的 watchdog 在下一个续约 tick 看见续约 CAS 失败再 cancel。
export function stopRun(runId: RunID): Promise<{ status: string }> {
  return request<{ status: string }>(`/api/runs/${encodeURIComponent(runId)}/stop`, {
    method: 'POST',
  })
}

// POST /api/runs/{runId}/resume：接管一场崩溃的 run（doc/30 §8.2）。**无请求体**。
//
// 409 的三种语义要分开处置，调用方按 **code** 分支（doc/40 §4.2，doc/30 §10.2）：
//   - CODE_RUN_ALIVE (112103)        → 已经有人在续了（多 tab / 刷新并发）：不是失败，转去重建帧面；
//   - CODE_NOT_RESUMABLE (112102)    → 这场已定稿或已终止，没有可续的东西；
//   - CODE_RESUME_EXHAUSTED (112104) → 续跑次数用尽（服务端 -resume-max，默认 3）。
export function postResume(runId: RunID): Promise<ResumeResult> {
  return request<ResumeResult>(`/api/runs/${encodeURIComponent(runId)}/resume`, { method: 'POST' })
}
