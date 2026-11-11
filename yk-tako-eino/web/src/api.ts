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
  // 'tool_call' / 'tool_result' = 工具帧（19v6-01 §2）：name + args / output，配对键 toolCallId。
  // 对话页把全部工具帧画成工具卡（doc/59），不按工具名筛。
  kind:
    | 'start'
    | 'delta'
    | 'usage'
    | 'terminal'
    | 'resume'
    | 'task'
    | 'question'
    | 'tool_call'
    | 'tool_result'
  agent?: string
  model?: string
  messageId?: string
  // 等帧的发起工具名（ask_user / submit_plan）与原始参数 JSON（options 从这里解析）；
  // 工具帧的 name 同样是工具名，args/output 是参数与结果原文。
  name?: string
  args?: string
  output?: string
  toolCallId?: string
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
  // 工具执行账本行（tool_call / tool_result）也在 runlog 里；对话界面画 user/assistant，
  // 工具行的 payload 解析成工具卡（doc/59：全部工具都进气泡）。
  role: 'user' | 'assistant' | 'tool_call' | 'tool_result'
  messageId?: string
  // 产出该行的 agent：空 = 主代理，非空 = 子代理名字（工具卡据此加前缀）。
  agent: string
  text: string
  // 工具行的结构化负载（tool_call: {name,args,toolCallId}；tool_result: {name,output,toolCallId}）。
  // assistant 行的 payload 被后端解成 usage/segments 后清空，不会出现在这里。
  payload?: string
  // assistant 行的 provider 账单（后端从 payload 列解出注入；user/tool 行不写该字段）。
  usage?: Usage
  // 含「等人」工具（ask_user / submit_plan / skill_install）的 run 在一行正文里留下的切点
  // （doc/47 §5.5）：end = 切点前正文的 rune 长度（按 code point 切），toolCallId = 该次调用。
  // 无提问的 run 不写该字段；最后一段没有切点。
  segments?: { end: number; toolCallId: string }[]
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
  // 发起这次等待的工具调用 id（doc/47 §5.5）：前端据此把问题行挂回 runlog 分段上的切点。
  toolCallId?: string
  // kind=install 是对话内安装的确认行（doc/52 C3）：args 存程序解析出的候选全表与选中集。
  kind: 'question' | 'plan' | 'install'
  question: string
  args: string
  status: 'pending' | 'answered' | 'stale'
  // 方案行的审批结论（doc/47 §5.3）：approve / revise；问题行与 pending 行不写。
  // 安装行同用这两值：approve=装、cancel 由答案端点直接写成文字（这里不出现）。
  decision?: 'approve' | 'revise' | 'cancel'
  // 已答行的人提交内容：question 行=用户原文（历史里画成答案气泡）；plan 行=编辑后的方案
  // 或驳回意见；install 行=程序写回的结论（「已安装技能：x。」或失败原因，doc/52 C5）。
  answer?: string
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
// kind=install 的行**额外**回 answer：程序在 CAS 之后同步装盘，装的结果（已安装／已过期／
// 同名冲突）就是这句话（doc/52 C3）。问题行与方案行不写这个键，契约不变。
export type AnswerResult = { questionId: string; status: string; answer?: string }
export function postAnswer(
  questionId: string,
  answer: string,
  decision?: QuestionDecision | 'cancel',
): Promise<AnswerResult> {
  return request<AnswerResult>(`/api/questions/${encodeURIComponent(questionId)}/answer`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ answer, ...(decision ? { decision } : {}) }),
  })
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

// —— 技能（doc/52 §6.1）——
// 字段序是契约（name→description→source→path→enabled→bytes→requires→missing→updatedAt→valid）。
// enabled 是服务端从 skill_disables 派生的；missing 是**本副本此刻**按过滤后的 PATH 现算的
// （doc/52 D1），不是表里的列——换一台副本可能就不一样。
export type Skill = {
  name: string
  description: string
  source: string
  path: string
  enabled: boolean
  bytes: number
  requires: string[]
  missing: string[]
  updatedAt: string
  valid: boolean
}

export type InvalidSkill = { path: string; reason: string }
export type SkillsResponse = { skills: Skill[]; invalid: InvalidSkill[] }
export type SkillDetail = { skill: Skill; body: string }
// 一条依赖的检查结果（doc/52 D1）：path 只在 found 时出现，且是**本副本**解析到的真实路径。
export type SkillDependency = { name: string; found: boolean; path?: string }
export type SkillCheckResponse = { dependencies: SkillDependency[]; checkedOn: string }
export type SkillRootInfo = { path: string; scope: string; exists: boolean; count: number; error: string }
export type SkillRescanResponse = {
  added: string[]
  updated: string[]
  removed: string[]
  invalid: InvalidSkill[]
  roots: SkillRootInfo[]
}
export type SkillInstallResponse = {
  installed: { name: string; dependencies: SkillDependency[]; warnings: string[] }[]
  warnings: string[]
}

export function listSkills(): Promise<SkillsResponse> {
  return request<SkillsResponse>('/api/skills')
}

export function getSkill(name: string): Promise<SkillDetail> {
  return request<SkillDetail>(`/api/skills/${encodeURIComponent(name)}`)
}

// GET /api/skills/:name/check：在当前副本重查依赖（doc/52 D1）。只读，所有成员都能调。
export function checkSkill(name: string): Promise<SkillCheckResponse> {
  return request<SkillCheckResponse>(`/api/skills/${encodeURIComponent(name)}/check`)
}

// PATCH 只动 skill_disables，不碰正文与行；回整行（enabled 已是新值）。
export function patchSkill(name: string, enabled: boolean): Promise<{ skill: Skill }> {
  return request<{ skill: Skill }>(`/api/skills/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  })
}

export function deleteSkill(name: string): Promise<DeleteStatus> {
  return request<DeleteStatus>(`/api/skills/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

export function rescanSkills(): Promise<SkillRescanResponse> {
  return request<SkillRescanResponse>('/api/skills/rescan', { method: 'POST' })
}

// 预览与安装（doc/52 §6.5 C2/C5）：三种输入——来源串、整条 npx 命令、本地包——走同两个端点。
// preview 只解析不写盘；install 多做原子落盘。
//
// source.url 是**实际要抓的地址**（GitLab 默认分支已解析过），卡片上要能核对它到底去哪拿了东西。
export type SkillPreviewSource = {
  kind: string
  owner: string
  namespace: string
  repo: string
  ref: string
  subpath: string
  url: string
}
// 一行候选。bytes 是整个技能目录的字节和、files 是目录里的普通文件数（与装完列表里的
// bytes 同一个口径）；description 是 frontmatter 原文，卡片上按需截断。
export type SkillPreviewEntry = {
  name: string
  description: string
  bytes: number
  files: number
  requires: string[]
}
// selected = 按这份输入会装哪几个（预览卡据此打勾，用户可改）；ambiguous = 多候选且输入没选。
export type SkillPreviewResponse = {
  source: SkillPreviewSource
  skills: SkillPreviewEntry[]
  selected: string[]
  ambiguous: boolean
  warnings: string[]
}

export type SkillInstallInput = { source?: string; command?: string; skills: string[] }

export function previewSkills(input: { source?: string; command?: string }): Promise<SkillPreviewResponse> {
  return request<SkillPreviewResponse>('/api/skills/preview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function previewSkillPackage(file: File): Promise<SkillPreviewResponse> {
  const fd = new FormData()
  fd.append('file', file)
  return request<SkillPreviewResponse>('/api/skills/preview', { method: 'POST', body: fd })
}

export function installSkills(input: SkillInstallInput): Promise<SkillInstallResponse> {
  return request<SkillInstallResponse>('/api/skills/install', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

// 上传包的安装：文件与「装哪几个」一起上行（skills 逐个 append，all=true 表示全装）。
export function installSkillPackage(
  file: File,
  skills: string[],
  all = false,
): Promise<SkillInstallResponse> {
  const fd = new FormData()
  fd.append('file', file)
  if (all) fd.append('all', 'true')
  for (const name of skills) fd.append('skills', name)
  return request<SkillInstallResponse>('/api/skills/install', { method: 'POST', body: fd })
}

// —— 租户工具（doc/52 D36 F1/F2）——
// 盘就是真源：`<tenant>/tools/<name>/bin/<name>`，不进表、没有 rescan。
// exe 是服务端按文件模式位现算的（false = 装上了但没执行位，run 里会 command not found）；
// path 是本副本盘上的真实路径，只在管理页核对用。
export type TenantTool = {
  name: string
  exe: boolean
  path: string
  bytes: number
  updatedAt: string
}
export type ToolsResponse = { tools: TenantTool[] }

export function listTools(): Promise<ToolsResponse> {
  return request<ToolsResponse>('/api/tools')
}

// 名字是用户字段、不是上传文件名：服务端白名单 `^[a-z0-9][a-z0-9-]{0,63}$` 且拒保留名。
// 同名上传 = 覆盖（旧目录进 `.trash`，rename 原子），正在跑的 run 下一枪就用新字节。
export function uploadTool(name: string, file: File): Promise<{ tool: TenantTool }> {
  const fd = new FormData()
  fd.append('name', name)
  fd.append('file', file)
  return request<{ tool: TenantTool }>('/api/tools', { method: 'POST', body: fd })
}

export function deleteTool(name: string): Promise<DeleteStatus> {
  return request<DeleteStatus>(`/api/tools/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

// —— 持久 Task 与编排（doc/70）——
// 列表按项目拉（GET /api/workspaces/:id/tasks，V1 无游标）；详情单读；停止／恢复是动作。
// 创建不在页面表单里做：模型用 delegate_task / create_orchestration 工具发起，产品 API 由验收脚本打。
export const CODE_TASK_MISSING = 117001 // 找不到这件任务（不存在或不可见，同形 404）
export const CODE_ORCH_MISSING = 117002 // 找不到这个编排组
export const CODE_TASK_NOT_RESUMABLE = 117103 // 这件任务现在不能恢复（非 interrupted / 有活执行者 / 次数耗尽）
export const CODE_TASK_FINISHED = 117104 // 这件任务已经结束，不能停止
export const CODE_ORCH_FINISHED = 117105 // 这个编排组已经结束，不能停止

// 结果里的一个产物引用：path 是**相对项目根**的路径，取字节走项目文件的现成出口
// （workspaceFileUrl + authFetch，见 TasksView；不能直接塞 <a href>，/api/media 要 Bearer 头）。
export type TaskArtifact = { path: string; kind: string }

// submit_task_result 提交的交接摘要；未提交的任务 result 是 null。
export type TaskResult = { summary: string; artifacts: TaskArtifact[]; metadata: Record<string, unknown> }

// 机器错误与展示文案分开（doc/70 §4.3）：code 给分支，message 直接就是给人看的句子。
export type TaskErrorView = { code: string; message: string }

// GET 返回的完整 Task。字段序是契约字节序（taskId → … → updatedAt），interrupted 不入库、
// 是读侧派生：running 且当前 run 租约过期时服务端把 status 写成 interrupted。
export type AgentTask = {
  taskId: string
  orchestrationId: string
  key: string
  content: string
  agent: string
  modelRef: string
  // doc/72 §4.7：两列插在 modelRef 后，旧字段相对顺序不变。
  planCheckpoint: boolean
  createdRevision: number
  workspaceId: string
  status: string
  currentRunId: string
  result: TaskResult | null
  error: TaskErrorView | null
  resultRevision: number
  createdAt: string
  startedAt: string | null
  finishedAt: string | null
  updatedAt: string
}

export type TaskListResponse = { tasks: AgentTask[] }

// 编排组视图的组半：finalTaskId 指向全组唯一的最终 Task；deliveryStatus 空 = 无可交付结果。
// doc/72 §4.7：planRevision / planHold 插在 finalTaskId 后；planHold=true 表示检查点已把新工作暂停。
export type OrchestrationBody = {
  orchestrationId: string
  goal: string
  workspaceId: string
  sourceSessionId: string
  status: string
  finalTaskId: string
  planRevision: number
  planHold: boolean
  deliveryStatus: string
  deliveryRunId: string
  // doc/73 §4.4：交付失败的机器原因（delivery_run_error / delivery_stopped /
  // artifact_invalid）；无失败时为空串。
  deliveryErrorCode: string
  createdAt: string
  finishedAt: string | null
  deliveredAt: string | null
  updatedAt: string
}

export type TaskLink = { parentTaskId: string; childTaskId: string }
export type OrchestrationView = { orchestration: OrchestrationBody; tasks: AgentTask[]; links: TaskLink[] }

export function listWorkspaceTasks(workspaceId: string): Promise<TaskListResponse> {
  return request<TaskListResponse>(`/api/workspaces/${encodeURIComponent(workspaceId)}/tasks`)
}

export function getAgentTask(taskId: string): Promise<AgentTask> {
  return request<AgentTask>(`/api/tasks/${encodeURIComponent(taskId)}`)
}

export function getOrchestration(orchestrationId: string): Promise<OrchestrationView> {
  return request<OrchestrationView>(`/api/orchestrations/${encodeURIComponent(orchestrationId)}`)
}

// 停一件 Task：幂等（已 cancelled 再停仍回 200）；已 succeeded/failed 是 409 117104。
// running 之外的未完成状态（pending/ready/blocked）当场翻 cancelled；running 由服务端顺带收尾 run。
export function stopAgentTask(taskId: string): Promise<{ taskId: string; status: string }> {
  return request<{ taskId: string; status: string }>(`/api/tasks/${encodeURIComponent(taskId)}/stop`, {
    method: 'POST',
  })
}

// 停整组：回 cancelling（还有 running 节点在收）或 cancelled（当场收完）；
// 已终态的组是 409 117105。组状态由服务端调度轮推进，前端收到事件后重拉。
export function stopOrchestration(orchestrationId: string): Promise<{ orchestrationId: string; status: string }> {
  return request<{ orchestrationId: string; status: string }>(
    `/api/orchestrations/${encodeURIComponent(orchestrationId)}/stop`,
    { method: 'POST' },
  )
}

// 恢复一件 interrupted 的 Task：202 = 已有新执行者接管在跑。409 117103 = 现在不能恢复
// （可能刚被别人抢先恢复——多 tab 并发，收到后重拉详情即可）。
export type TaskResumeResult = { taskId: string; runId: string; status: string; resumeCount: number }
export function resumeAgentTask(taskId: string): Promise<TaskResumeResult> {
  return request<TaskResumeResult>(`/api/tasks/${encodeURIComponent(taskId)}/resume`, { method: 'POST' })
}

// —— 计划修订、Task 问题与「需要我处理」（doc/72）——
export const CODE_REVISION_STALE = 117106 // 提交的 baseRevision 已不是当前版本
export const CODE_NOT_REVISABLE = 117107 // 组不在 active，或最终 Task 已开始
export const CODE_REVISION_CONFLICT = 117108 // 同一修订键换了一份命令
export const CODE_QUESTION_EXPIRED = 115102 // 问题已过 expires_at

// 修订命令里的新节点：形状与创建时一致（doc/72 §4.2）。
export type PlanRevisionTaskInput = {
  key: string
  content: string
  agent: string
  modelRef?: string
  planCheckpoint?: boolean
}

export type PlanRevisionCommand = {
  idempotencyKey: string
  baseRevision: number
  reason: string
  addTasks: PlanRevisionTaskInput[]
  addLinks: { parent: string; child: string }[]
  cancelTasks: string[]
  finalTaskKey: string
}

// 201 的 data：addedTaskIds 与命令里的 addTasks 同序。
export type PlanRevisionResult = {
  revisionId: string
  orchestrationId: string
  revision: number
  finalTaskId: string
  planHold: boolean
  addedTaskIds: string[]
}

// 历史行保存原命令：addLinks 里是 key（parent/child），不是任务 id。
export type PlanRevisionRow = {
  revisionId: string
  revision: number
  baseRevision: number
  reason: string
  addTasks: { key: string; content: string; agent: string; modelRef: string; planCheckpoint: boolean }[]
  addLinks: { parent: string; child: string }[]
  cancelTasks: string[]
  finalTaskKey: string
  sourceRunId: string
  createdAt: string
}

export type ContinueResult = { orchestrationId: string; revision: number; planHold: boolean }

// Task 当前执行上的问题行（复用 run 问题视图，doc/72 §4.5）。
// expiresAt 只在建行时设了截止时间才出现；普通主会话的问答行没有这一键。
export type TaskQuestion = QuestionRow & { expiresAt?: string }

// 关注聚合的一行：不适用字段是空串，不省键（doc/72 §4.6）。
export type AttentionKind = 'question' | 'plan_review' | 'interrupted' | 'delivery_failed'
export type AttentionItem = {
  kind: AttentionKind | string
  taskId: string
  orchestrationId: string
  title: string
  count: number
  questionId: string
  runId: string
  // doc/73 §4.4：这一行对应的协调通知；没有对应通知时为空串。前端据此调用 retry。
  notificationId: string
  status: string
  updatedAt: string
}

export function createPlanRevision(
  orchestrationId: string,
  cmd: PlanRevisionCommand,
): Promise<PlanRevisionResult> {
  return request<PlanRevisionResult>(
    `/api/orchestrations/${encodeURIComponent(orchestrationId)}/revisions`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cmd),
    },
  )
}

// 解除计划暂停：版本对、组 active 才放行；plan_hold 已是 false 时是幂等空操作。
export function continueOrchestration(orchestrationId: string, baseRevision: number): Promise<ContinueResult> {
  return request<ContinueResult>(
    `/api/orchestrations/${encodeURIComponent(orchestrationId)}/continue`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ baseRevision }),
    },
  )
}

export function listPlanRevisions(orchestrationId: string): Promise<{ revisions: PlanRevisionRow[] }> {
  return request<{ revisions: PlanRevisionRow[] }>(
    `/api/orchestrations/${encodeURIComponent(orchestrationId)}/revisions`,
  )
}

export function listTaskQuestions(taskId: string, status?: string): Promise<TaskQuestion[]> {
  const query = status ? `?status=${encodeURIComponent(status)}` : ''
  return request<TaskQuestion[]>(`/api/tasks/${encodeURIComponent(taskId)}/questions${query}`)
}

export function getTaskAttention(workspaceId: string): Promise<{ items: AttentionItem[] }> {
  return request<{ items: AttentionItem[] }>(
    `/api/workspaces/${encodeURIComponent(workspaceId)}/task-attention`,
  )
}

// —— 协调通知的恢复面（doc/73）——
export const CODE_NOTIFICATION_MISSING = 117003 // 找不到这条通知（不存在／不可见／跨租户／来源会话已删除）
export const CODE_NOTIFICATION_NOT_RETRYABLE = 117109 // 这条通知现在不能重新处理
export const CODE_ARTIFACT_INVALID = 117110 // 结果产物已不可读取

// 一次处理尝试：同一通知先后由哪些 run 处理。status 现读自 runs，与 resume 无关；
// 同 run 的崩溃续跑只增 epoch，不增 attempt。
export type NotificationAttempt = {
  runId: RunID
  attempt: number
  status: string
  createdAt: string
}

// 结果定位（doc/73 §9）：finalTaskId + resultRevision 永远指向当前持久结果；
// 摘要与产物现读自该 Task，不用过期通知快照。非整组完成通知的 result 是 null。
export type NotificationResult = {
  finalTaskId: string
  resultRevision: number
  summary: string
  artifacts: TaskArtifact[]
}

// GET /api/notifications/:id 的完整视图（字段序是契约字节序）。
export type NotificationDetail = {
  notificationId: string
  orchestrationId: string
  taskId: string
  kind: string
  status: string
  runId: RunID
  attempt: number
  errorCode: string
  result: NotificationResult | null
  createdAt: string
  claimedAt: string | null
  failedAt: string | null
  consumedAt: string | null
  updatedAt: string
  attempts: NotificationAttempt[]
}

// POST retry 的 data（202 新尝试 / 200 已有在处理的 run 或计划决策已接受）。
export type NotificationRetryResult = {
  notificationId: string
  runId: RunID
  status: string
  attempt: number
}

export function getNotification(notificationId: string): Promise<NotificationDetail> {
  return request<NotificationDetail>(`/api/notifications/${encodeURIComponent(notificationId)}`)
}

// 人工重新处理：只从 failed 出发；服务端在同一事务建新 run／attempt。
// 202 新尝试；200 已有一场在处理的 run（双击／多副本）或计划决策已接受。
export function retryNotification(notificationId: string): Promise<NotificationRetryResult> {
  return request<NotificationRetryResult>(
    `/api/notifications/${encodeURIComponent(notificationId)}/retry`,
    { method: 'POST' },
  )
}

