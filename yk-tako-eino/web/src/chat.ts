import { ref } from 'vue'
import { readTenant } from './auth'
import {
  ApiError,
  CODE_NOT_RESUMABLE,
  CODE_RESUME_EXHAUSTED,
  CODE_RUN_ALIVE,
  CODE_SESSION_MISSING,
  CODE_SESSION_RUNNING,
  CODE_WORKSPACE_MISSING,
  getFrames,
  getLog,
  getRun,
  getSession,
  parseMedia,
  postResume,
  postRuns,
  type MediaRef,
  type PostImage,
  type Row,
  type RunID,
  type SessionID,
  type Usage,
} from './api'

export type Bubble = {
  key: string
  runId: RunID | null
  role: 'user' | 'assistant'
  text: string
  // 媒体引用（doc/32 A5）：从行内占位行解析；图片渲染成 <img>（GET /api/media 换字节流），
  // 其他媒体给文件片。text 是剥掉占位行后的正文——占位行是模型/存储的坐标，不上页面。
  media?: MediaRef[]
  // 'interrupted' = 这场 run 崩了，但账本还在、可以被接管（doc/30 §8）。
  // 它不是失败：失败没有可续的东西，中断有。
  state: 'queued' | 'streaming' | 'offline' | 'done' | 'error' | 'stopped' | 'interrupted'
  usage?: Usage
  // 本 tab 亲历收尾却全程无 usage 帧（厂商不报账单）→ 显示「用量 ?」，不静默。
  usageUnknown?: boolean
  // interrupted 态专用：还能不能续（续跑次数没耗尽）。false = 次数用尽，只能重发。
  resumable?: boolean
  // board 是任务看板的结构态（doc/41 §5.2 的 kind=task 帧）：紧凑视图文本。
  // 它是**结构**（前端按行解析渲染状态色），不是让前端去猜自然语言回执。
  board?: string
}

// 附件（composer 选用）：name 供 chip 显示，img 随 postRuns 上行。
export type Attachment = { name: string; img: PostImage }

// 一条待投投递 = 已渲染的 user 气泡 + 它那条 assistant 气泡 + 请求前就定下的 messageId
// （19v6-01 §1 情形 3：重发不换 id）。
// workspaceId 只在懒建会话时用：发送那一刻决定这条消息落哪个项目（doc/35）。
type Pending = {
  messageId: string
  text: string
  assistant: Bubble
  images?: PostImage[]
  workspaceId?: string | null
}

const LS_SESSION = 'tako.sessionId'
const LS_WORKSPACE = 'tako.workspaceId'

// 每个工作区各记自己的会话。切走再切回来要能把原来的对话拉出来，不能共用一个键。
function tenantSessionKey(tenantId: string): string {
  return tenantId ? `${LS_SESSION}.${tenantId}` : LS_SESSION
}

function readStoredSession(): SessionID | null {
  const tenantId = readTenant().id
  const key = tenantSessionKey(tenantId)
  const scoped = localStorage.getItem(key)
  if (scoped) return scoped as SessionID
  if (!tenantId) return null
  const legacy = localStorage.getItem(LS_SESSION)
  if (!legacy) return null
  localStorage.setItem(key, legacy)
  localStorage.removeItem(LS_SESSION)
  return legacy as SessionID
}

function writeStoredSession(id: SessionID, tenantId: string) {
  localStorage.setItem(tenantSessionKey(tenantId), id)
  if (localStorage.getItem(LS_SESSION) === id) localStorage.removeItem(LS_SESSION)
}

// 当前项目也只属当前租户：切走再切回来，各看各的落点（doc/35：服务端不保存「我正在看哪个项目」）。
function tenantWorkspaceKey(tenantId: string): string {
  return tenantId ? `${LS_WORKSPACE}.${tenantId}` : LS_WORKSPACE
}

function readStoredWorkspace(): string | null {
  return localStorage.getItem(tenantWorkspaceKey(readTenant().id))
}

function writeStoredWorkspace(id: string | null, tenantId: string) {
  const key = tenantWorkspaceKey(tenantId)
  if (id) localStorage.setItem(key, id)
  else localStorage.removeItem(key)
}
const POLL_MS = 400
// 冗余信号核对节奏：每 STATUS_EVERY 拍 frames 顺带查一次 session（≈2s 一次 GET）。
// 放闸与定稿是同一笔 PG 事务（19v6-03 D1：UPDATE runs 落终态 + assistant 行入 runlog），
// 所以「闸已空」必然意味着「runlog 已定稿」；terminal 帧是 commit 后的 Redis 半、可能比闸晚到，
// 收口本就以 log 为准，这一拍只会让收口提前，不会误收。
const STATUS_EVERY = 5
// 窗口化初始游标 = max(0, 高水位 - HISTORY_WINDOW)（19v6-01 §3：初始值不是 0；窗口大小客户端定）。
// 10 = 刷新后先见最近五轮，更早的上滚翻页拿（U5）。
const HISTORY_WINDOW = 10
const PAGE = 10

export function useChat() {
  const sessionId = ref<SessionID | null>(null)
  const bubbles = ref<Bubble[]>([])
  const draft = ref('')
  const attachments = ref<Attachment[]>([])
  const busy = ref(false)
  const pending = ref<Pending[]>([])
  const loadingOlder = ref(false)
  const atStart = ref(true)
  // 新对话落点（doc/44 §2.2）：null = 让服务端挂默认项目。
  const activeWorkspaceId = ref<string | null>(readStoredWorkspace())

  // 对齐游标：只在拉 log 时前进（19v6-02 不变量）。翻页游标是 loadOlder 里的临时值，不碰它。
  let runlogOffset = 0
  // 已渲染最老一行的 offset（含本 tab 乐观渲染的气泡所对应的行）；0 = 还没渲染任何历史。
  let oldestOffset = 0
  let pollToken = 0
  let pumping = false
  // 本 tab 投递时乐观渲染过的 user 行（气泡当场就有）的 messageId 集合。收口补渲染（drainLog）
  // 拉到的 log 行里，靠它认出自己的 user 行跳过，免得画两遍；messageId 是投递前就定好、
  // 服务端原样回写的那个（19v6-01 §1）。
  const renderedMessageIds = new Set<string>()

  function push(b: Bubble): Bubble {
    bubbles.value.push(b)
    return b
  }

  function newMessageId(): string {
    return `m_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`
  }

  // 投递 = 乐观渲染两条气泡，真正发不发交给 pump（忙时留在队里，不变成错误）。
  // 带图投递：乐观 user 气泡先只出文字，图在收口 drainLog 认出自己的 user 行时按服务端
  // 真源回填（path/sha 由服务端内容寻址铸出，本地不预演第二套）。
  function send() {
    const text = draft.value.trim()
    const imgs = attachments.value.map((a) => a.img)
    if (!text && !imgs.length) return
    draft.value = ''
    attachments.value = []
    const messageId = newMessageId()
    renderedMessageIds.add(messageId)
    push({ key: `${messageId}:user`, runId: null, role: 'user', text, state: 'done' })
    const assistant = push({ key: `${messageId}:assistant`, runId: null, role: 'assistant', text: '', state: 'queued' })
    pending.value.push({
      messageId,
      text,
      assistant,
      images: imgs.length ? imgs : undefined,
      workspaceId: sessionId.value ? undefined : activeWorkspaceId.value,
    })
    void pump()
  }

  // 切当前项目只改「下一场新对话落哪」；已经打开的会话不跟着换项目。
  function setActiveWorkspace(id: string | null) {
    activeWorkspaceId.value = id
    writeStoredWorkspace(id, readTenant().id)
  }

  // 打开项目页里的一场会话：丢掉屏幕上的旧对话，按同一个 reconcile 四步重建。
  async function openSession(id: SessionID) {
    pollToken++
    sessionId.value = null
    pending.value = []
    bubbles.value = []
    draft.value = ''
    attachments.value = []
    busy.value = false
    loadingOlder.value = false
    renderedMessageIds.clear()
    runlogOffset = 0
    oldestOffset = 0
    atStart.value = true
    writeStoredSession(id, readTenant().id)
    return reconcile()
  }

  // 队列泵（19v6-01 §1「409 = 客户端本地排队、本轮收口后补投」）：本 tab 有 run 在跑、
  // 或闸被别的 tab 占着，都退避重查而不是把消息判死。
  async function pump() {
    if (pumping) return
    pumping = true
    const token = pollToken
    try {
      while (token === pollToken && pending.value.length) {
        if (busy.value || !(await gateIdle())) {
          await sleep(POLL_MS)
          continue
        }
        await deliver(pending.value.shift() as Pending)
      }
    } finally {
      pumping = false
    }
  }

  // 对齐游标是 **session 作用域**的：本地会话一被丢弃（404 自愈 / 新对话），旧 session 的高水位
  // 对新会话毫无意义——留着它，收口就会从「新会话里根本不存在的偏移」往下拉，永远拉不到定稿行。
  function dropSession() {
    const id = sessionId.value
    sessionId.value = null
    localStorage.removeItem(tenantSessionKey(readTenant().id))
    if (id && localStorage.getItem(LS_SESSION) === id) localStorage.removeItem(LS_SESSION)
    runlogOffset = 0
    oldestOffset = 0
    atStart.value = true
  }

  // 变更同步（doc/46 Loop A）：这场会话在别的窗口被删了。整页气泡作废、清存储键，
  // 给页面一条可展示的死因；用户再发言会懒建新会话，不拦。
  const sessionGone = ref('')
  function closeOnDeleted(id: string) {
    if (!sessionId.value || sessionId.value !== id) return
    pollToken++
    dropSession()
    pending.value = []
    bubbles.value = []
    draft.value = ''
    attachments.value = []
    busy.value = false
    renderedMessageIds.clear()
    sessionGone.value = '这场会话已在其它窗口删除。'
  }

  // gateIdle：闸空 = 会话没有活 run（19v6-01 §4）。404 = 本地 sid 已被 reset 清成僵尸，
  // 它必然不占闸——丢掉本地值连同游标，这一条改走懒建；其余查不到一律算「不空」，宁可下一拍再试。
  async function gateIdle(): Promise<boolean> {
    const sid = sessionId.value
    if (!sid) return true
    try {
      return !(await getSession(sid)).liveRunId
    } catch (e) {
      if (isNotFound(e)) {
        dropSession()
        return true
      }
      return false
    }
  }

  async function deliver(p: Pending) {
    const assistant = p.assistant
    const token = pollToken
    const tenantAtSend = readTenant().id
    assistant.state = 'streaming'
    assistant.text = ''
    busy.value = true
    try {
      const resp = await postWithFreshSession(p)
      writeStoredSession(resp.sessionId, tenantAtSend)
      if (token !== pollToken) return
      sessionId.value = resp.sessionId
      assistant.runId = resp.runId
      await pollFrames(resp.runId, assistant, 0, token)
    } catch (e) {
      if (token !== pollToken) return
      if (isBusy(e)) {
        // 别的客户端抢到了闸：什么都不写（19v6-01 §1），所以原样回队首等补投，不是投递失败。
        // 但「新对话」之后的补投属于另一个会话，不能把它塞回队列（幽灵消息）。
        assistant.state = 'queued'
        if (token === pollToken) pending.value.unshift(p)
      } else {
        assistant.state = 'error'
        // message 就是信封里的弹窗文案（doc/40），不用再拼错误串。
        assistant.text = e instanceof ApiError ? `投递失败：${e.message}` : '投递失败'
      }
    } finally {
      if (token === pollToken) busy.value = false
    }
  }

  // 僵尸 sessionId 自愈：本地存的 sid 被 dev reset / 空重启清走之后，带它投递只会在 19v6-01 §1 的准入
  // 检查上稳定 404（「带 = 只认登记过的，不隐式创建」）。客户端的补救是认它不存在——
  // 丢掉本地值、不带 sid 重投一次走懒建。只重投这一次，不把 500 之类拖进循环。
  async function postWithFreshSession(p: Pending) {
    const sid = sessionId.value
    // p.workspaceId 是发送那一刻定下的落点；null 表示「明确用默认项目」，不能被事后切换的项目覆盖。
    const targetWorkspace = p.workspaceId !== undefined ? p.workspaceId : activeWorkspaceId.value
    // 已有会话永远不带 workspaceId（带也不会改绑定，反而会被一个已删项目在预检上挡掉）。
    const workspaceId = sid ? undefined : targetWorkspace ?? undefined
    try {
      return await postRuns(p.messageId, p.text, sid ?? undefined, p.images, workspaceId)
    } catch (e) {
      if (sid && isNotFound(e)) {
        dropSession()
        return await postRuns(p.messageId, p.text, undefined, p.images, targetWorkspace ?? undefined)
      }
      // 落点项目已被删（114001）：清落点、回落默认项目重试一次（doc/46 A2 自愈）。
      // 只对「发送时定下的落点仍等于当前落点」下手——排队期间被人切过落点就不乱清；只重投这一次。
      if (!sid && isWorkspaceMissing(e) && targetWorkspace && activeWorkspaceId.value === targetWorkspace) {
        setActiveWorkspace(null)
        return await postRuns(p.messageId, p.text, undefined, p.images, undefined)
      }
      throw e
    }
  }

  // 19v6-01 §2：delta 按 eventOffset 顺序追加，本地游标 = nextOffset。
  // terminal 帧 / status:gone / liveRunId 易主，三者任一即停轮询并收口（19v6-02 第 4 步），不押注单一信号。
  async function pollFrames(runId: RunID, assistant: Bubble, eventOffset: number, token: number) {
    let ticks = 0
    for (;;) {
      if (token !== pollToken) return
      let resp
      try {
        resp = await getFrames(runId, eventOffset)
      } catch {
        // frames 拉不动（断网/引擎重启）不等于这场 run 死了：半截气泡留着，改查 session，
        // 闸一空就收口拉 log 定稿全文。
        assistant.state = 'offline'
        await waitSettled(runId, token)
        if (token === pollToken) await closeOut(runId, assistant, null)
        return
      }
      let terminalStatus: string | null = null
      for (const f of resp.frames) {
        if (f.kind === 'resume') {
          // resume 帧（doc/30 §8.3）的 text 是**账本里已产出的整段正文**，不是增量片段——
          // 语义跟 delta 正好相反：这里是**替换**，用 += 会把崩溃前的正文重复拼一遍。
          assistant.text = f.text ?? ''
        } else if (f.kind === 'delta' && f.text) assistant.text += f.text
        // usage 帧（doc/27 §4a）：每枪一帧，多枪取最后一帧（= 下一枪的基线）；
        // 子代理帧（agent 非空）第一版不渲染，跳过。
        if (f.kind === 'usage' && !f.agent) {
          assistant.usage = {
            promptTokens: f.promptTokens ?? 0,
            completionTokens: f.completionTokens ?? 0,
            window: f.window ?? 0,
          }
        }
        if (f.kind === 'task' && !f.agent) {
          // 看板帧：text = 当前全量看板的紧凑视图（模型的眼睛同一份文本，前端按行渲染）。
          assistant.board = f.text ?? ''
        }
        if (f.kind === 'terminal') terminalStatus = f.status ?? 'complete'
      }
      eventOffset = resp.nextOffset
      if (terminalStatus !== null || resp.status === 'gone') {
        await closeOut(runId, assistant, terminalStatus)
        return
      }
      await sleep(POLL_MS)
      if (++ticks % STATUS_EVERY === 0 && (await runSettled(runId, token))) {
        if (token === pollToken) await closeOut(runId, assistant, null)
        return
      }
    }
  }

  // 续跑（doc/30 §8）：接管一场崩溃的 run。
  //
  // 202 = 已有执行者接管并在跑（不等于跑完）。接着**从 eventOffset=0 重拉帧**——
  // seedResume 把 runlive 整个删掉重建了（redisstore.go:102），客户端手上的旧游标
  // 全部作废，拿旧游标去拉只会拉到空。
  //
  // 三个 409 分开处置（doc/30 §10.2），这是最容易做错的地方：
  //   run_alive        → 已经有人在续了（多 tab / 刷新并发）：**不是失败**，跟着重建帧面；
  //   not_resumable    → 这场已定稿或已终止：回 log 收口（多半是别人替它跑完了）；
  //   resume_exhausted → 续跑次数用尽：告诉用户重发，不再给按钮。
  async function resumeRun(b: Bubble) {
    const runId = b.runId
    if (!runId) return
    const token = pollToken
    b.state = 'streaming'
    b.text = ''
    // 续跑期间会话闸被占着：置 busy 让队列里的新消息等它收口再投（POST 会撞 409）。
    busy.value = true
    try {
      try {
        await postResume(runId)
      } catch (e) {
        if (token !== pollToken) return
        // 三种 409 按业务码分支（doc/40 §4.2），不再比对文案。
        const code = e instanceof ApiError ? e.code : 0
        if (code === CODE_RUN_ALIVE) {
          // 别人已经在续：跟着重建帧面，不报错。
          await pollFrames(runId, b, 0, token)
          return
        }
        if (code === CODE_NOT_RESUMABLE) {
          await closeOut(runId, b, null)
          return
        }
        if (code === CODE_RESUME_EXHAUSTED) {
          b.state = 'interrupted'
          b.resumable = false
          return
        }
        b.state = 'error'
        b.text = e instanceof ApiError ? `续跑失败：${e.message}` : '续跑失败'
        return
      }
      if (token !== pollToken) return
      await pollFrames(runId, b, 0, token)
    } finally {
      if (token === pollToken) busy.value = false
    }
  }

  // runSettled：19v6-01 §4 的 `liveRunId` 变空或易主 = 那场 run 已结束（放闸与定稿同笔 PG 事务写入，19v6-03）。
  // 查询失败算「未结束」——收口时机由 frames 自己的错误分支负责，不在这里猜。
  async function runSettled(runId: RunID, token: number): Promise<boolean> {
    const sid = sessionId.value
    if (!sid) return false
    try {
      const status = await getSession(sid)
      return token === pollToken && status.liveRunId !== runId
    } catch {
      return false
    }
  }

  // 断线期间的续命路径：闸不放就一直等（与 frames 轮询同寿命），半截气泡不动。
  async function waitSettled(runId: RunID, token: number) {
    while (token === pollToken && !(await runSettled(runId, token))) {
      await sleep(POLL_MS)
    }
  }

  // 收口（doc/19-v6 index 概念表「收口」）：定稿后权威内容以 runlog 为准，按 run 整条替换半截气泡；
  // 拉 log 经过的别人的定稿行顺带补渲染（drainLog）。
  //
  // error / stopped 终态（失败留痕，2026-10-22）：这一轮可能已经说了话、干了活 → 服务端把
  // **已产出的内容**落进了 runlog（半截回答 + 工具行）。所以这里也要拉一次 log：
  //  1) 刷新页面后从历史面重建时，那半截还在（不再"刷新即消失"）；
  //  2) 客户端的 runlogOffset 与服务端对齐，不留游标空洞。
  // 屏上流出来的半截**不替换**（它可能是本轮最后一段、还没进 turn 的流）——只有气泡空着
  // 时才用 log 里的行补上。终态帧是 PG 事务提交后才推的（EndRun → runliveFinish），
  // 所以看到终态帧时 log 里的行已经在库里。
  async function closeOut(runId: RunID, assistant: Bubble, terminalStatus: string | null) {
    if (terminalStatus === 'error' || terminalStatus === 'stopped') {
      const row = await drainLog(runId, assistant)
      if (row && !assistant.text) assistant.text = row.text
      assistant.state = terminalStatus === 'stopped' ? 'stopped' : 'error'
      return
    }
    // 本 tab 亲历的收尾：全程没见过 usage 帧 = 厂商没报账单，标出来（error 态不标——失败气泡本就有说明行）。
    if (!assistant.usage) assistant.usageUnknown = true
    const row = await drainLog(runId, assistant)
    if (row) {
      assistant.text = row.text
      // 历史真源里也带账单（doc/27 §4b）：有就覆盖场内最后一帧，没有则维持 usageUnknown 判定。
      if (row.usage) {
        assistant.usage = row.usage
        assistant.usageUnknown = false
      }
      assistant.state = 'done'
      return
    }
    // 没有定稿行。三种收尾在 log 里长得一样（都没有 assistant 行），靠 run 的状态分辨：
    //   interrupted = 崩了、但账本还在 → 这场能被接管（doc/30 §8），**不是失败**；
    //   stopped    = 用户停的 → 保留半截；
    //   别的        = 真失败。
    // 先问单场 run：它给的是派生状态（running + 租约过期 → interrupted），比
    // getSession 的 lastRunStatus 更准（后者说的是「最新一场」，未必是本场）。
    const st = await getRun(runId).catch(() => null)
    if (st && st.status === 'interrupted') {
      assistant.state = 'interrupted'
      assistant.resumable = st.resumable
      return
    }
    // error 收尾与 stopped 收尾在 log 里长得一样（都没有 assistant 行）——
    // 查 getSession 的 lastRunStatus 分辨「已停止」与「失败」，猜不出来才转错误。
    const sid = sessionId.value
    if (sid) {
      try {
        if ((await getSession(sid)).lastRunStatus === 'stopped') {
          assistant.state = 'stopped'
          return
        }
      } catch {
        // 引擎不可达：按失败收口（原语义），不为此加重试
      }
    }
    assistant.state = 'error'
  }

  // 从对齐游标续拉 log 直到追平（空批 = 追平），这正是 19v6-02 第 4 步的「回第 2 步拉一次 log」：
  // 游标经过的**别人的**定稿行不能白过——不是本 tab 的就当场补渲染（多 tab/他端投递的整轮
  // 借此进列表）；本 tab 乐观渲染过的 user 行按 messageId 认出跳过（气泡早就在）；本次要替换的
  // 那条 assistant 行只记下、不渲染，交给 closeOut 整条替换半截气泡。
  async function drainLog(runId: RunID, assistant: Bubble): Promise<Row | null> {
    const token = pollToken
    const sid = sessionId.value
    if (!sid) return null
    let hit: Row | null = null
    let ownUserOffset = 0
    const foreign: Row[] = []
    for (;;) {
      if (token !== pollToken) return null
      const resp = await getLog(sid, runlogOffset)
      if (token !== pollToken) return null
      for (const r of resp.rows) {
        if (r.role === 'assistant' && r.runId === runId) {
          hit = r
        } else if (r.role === 'user' && r.messageId && renderedMessageIds.has(r.messageId)) {
          // 自己的 user 行：气泡早渲染过，但「最老已渲染」的记账仍要含它——否则 atStart 撒谎，
          // 翻页会把这两行当没渲染过的再拉一遍、画成两份。带图投递在这里按服务端真源
          // 回填媒体（乐观气泡当时只有文字，path/sha 由服务端铸出）。
          ownUserOffset = r.runlogOffset ?? 0
          noteRendered(r)
          backfillOwnUserMedia(r)
        } else {
          foreign.push(r)
        }
      }
      const advanced = resp.nextOffset > runlogOffset
      runlogOffset = resp.nextOffset
      if (!advanced) break
    }
    if (token !== pollToken) return null
    renderForeign(foreign, assistant, ownUserOffset)
    return hit
  }

  // 切换工作区后调用：清掉屏幕上的气泡，再按新工作区自己的会话键重拉。不删其他工作区记下的会话。
  async function showTenantSession() {
    pollToken++
    sessionId.value = null
    pending.value = []
    bubbles.value = []
    draft.value = ''
    attachments.value = []
    busy.value = false
    loadingOlder.value = false
    renderedMessageIds.clear()
    runlogOffset = 0
    oldestOffset = 0
    atStart.value = true
    await reconcile()
  }

  // reconcile（19v6-02 四步）：查状态 → 窗口化补历史 → 有活 run 则 frames(0) 重建半截气泡。
  // 返回是否真的打开了一场会话（项目页「打开」据此判断，失败时对话页保持空）。
  async function reconcile(): Promise<boolean> {
    sessionGone.value = ''
    // 连点防护：openSession/showTenantSession/newSession 都会 pollToken++ 后清空气泡，
    // 中途进来的那次 reconcile 必须在每个 await 之后认令牌失效就退场，否则历史会重复渲染。
    const token = pollToken
    // 当前项目按当前租户的存储重读：登录/切租户后不能留着上一个租户的项目 id。
    activeWorkspaceId.value = readStoredWorkspace()
    const sid = readStoredSession()
    if (!sid) return false
    let status
    try {
      status = await getSession(sid)
    } catch (e) {
      if (token !== pollToken) return false
      if (isNotFound(e)) dropSession()
      return false
    }
    if (token !== pollToken) return false
    sessionId.value = sid
    // 打开已有会话时当前项目跟着它走：之后「新对话」自然落在同一个项目。
    if (status.workspaceId) setActiveWorkspace(status.workspaceId)
    runlogOffset = Math.max(0, status.runlogOffset - HISTORY_WINDOW)
    for (;;) {
      const resp = await getLog(sid, runlogOffset)
      if (token !== pollToken) return false
      if (resp.rows.length === 0) break
      appendRows(resp.rows)
      runlogOffset = resp.nextOffset
    }
    if (status.liveRunId) {
      const assistant = push({ key: `${status.liveRunId}:live`, runId: status.liveRunId, role: 'assistant', text: '', state: 'streaming' })
      busy.value = true
      const token = pollToken
      pollFrames(status.liveRunId, assistant, 0, token).finally(() => {
        if (token === pollToken) busy.value = false
      })
    } else if (status.lastRunStatus === 'interrupted' && status.lastRunId) {
      // 崩了、但账本还在：这场 run 可以被接管（doc/30 §8）。气泡正文先空着——
      // 崩溃前已产出的正文在账本里，点「继续」后随 resume 帧一起回来（doc/30 §8.3）。
      // 这里不占闸：闸空（liveRunId 为空）正是「执行者没了」的样子。
      const assistant = push({
        key: `${status.lastRunId}:live`,
        runId: status.lastRunId,
        role: 'assistant',
        text: '',
        state: 'interrupted',
      })
      void markResumable(assistant)
    }
    return true
  }

  // 还能不能续：问单场 run 的派生状态。次数用尽时 resumable=false——
  // 那时不该给一个点了必然 409 的按钮，而是提示重新发一条（doc/30 §8.2 的 409 resume_exhausted）。
  async function markResumable(b: Bubble) {
    if (!b.runId) return
    try {
      b.resumable = (await getRun(b.runId)).resumable
    } catch {
      b.resumable = false
    }
  }

  // 一条 log 行 → 气泡，并记账「最老已渲染 offset」（loadOlder 的翻页基准）。
  // 占位行在这里解析成媒体引用、从正文剥掉（doc/32 A5）。
  // runlog 同时是工具执行账本（tool_call / tool_result），气泡只收对话两角色——
  // 账本行留在库里和模型输入里，不上对话页面。
  type ChatRow = Row & { role: 'user' | 'assistant' }

  function isChatRow(r: Row): r is ChatRow {
    return r.role === 'user' || r.role === 'assistant'
  }

  function rowBubble(r: ChatRow): Bubble {
    noteRendered(r)
    const { media, display } = parseMedia(r.text)
    return {
      key: `log:${r.runlogOffset}`,
      runId: r.runId,
      role: r.role,
      text: display,
      media: media.length ? media : undefined,
      state: 'done',
      usage: r.usage,
    }
  }

  // 收口时认出自己的 user 行，按服务端真源回填乐观气泡的媒体与正文。
  function backfillOwnUserMedia(r: Row) {
    const { media, display } = parseMedia(r.text)
    if (!media.length) return
    const b = bubbles.value.find((x) => x.key === `${r.messageId}:user`)
    if (!b) return
    b.media = media
    b.text = display
  }

  function noteRendered(r: Row) {
    const off = r.runlogOffset ?? 0
    if (off > 0 && (oldestOffset === 0 || off < oldestOffset)) oldestOffset = off
    atStart.value = oldestOffset <= 1
  }

  // 别人的定稿行按日志顺序落位：早于本 tab 本轮 user 行的插到本轮那对气泡之前——与刷新后
  // reconcile 重放出的全量顺序一致，不让「刚补上的整轮」排在自己新消息后面；晚于它的
  // （收口检测与拉 log 之间别人的投递，竞态窗口）紧跟本轮气泡之后、排队气泡之前。
  function renderForeign(rows: Row[], assistant: Bubble, ownUserOffset: number) {
    for (const r of rows) {
      if (!isChatRow(r)) continue
      const idx = bubbles.value.indexOf(assistant)
      if (idx < 0) return // 列表已被 newSession 清空，整批作废
      const before = ownUserOffset > 0 && (r.runlogOffset ?? 0) < ownUserOffset
      bubbles.value.splice(Math.max(0, before ? idx - 1 : idx + 1), 0, rowBubble(r))
    }
  }

  function appendRows(rows: Row[]) {
    for (const r of rows) if (isChatRow(r)) push(rowBubble(r))
  }

  // 翻页：临时游标只用于这一次请求，拉完 prepend，**不回退对齐游标**（19v6-02「两种游标别混」）。
  // 批被 W 截断时按 nextOffset 续拉补齐（19v6-01 §3：「客户端从它继续拉即补齐」）。
  // 窗口里可能整块都是工具账本行（气泡不画它们）；这一页没捞到任何对话行时，
  // 把窗口继续往更早移，直到捞到一页或到达会话开头——否则「上滚」会卡在工具块前面。
  async function loadOlder() {
    const sid = sessionId.value
    if (!sid || loadingOlder.value || atStart.value) return
    loadingOlder.value = true
    try {
      let upper = oldestOffset
      let floor = Math.max(0, upper - 1 - PAGE)
      const collected: ChatRow[] = []
      for (;;) {
        const windowRows: ChatRow[] = []
        for (let from = floor; ; ) {
          const resp = await getLog(sid, from)
          windowRows.push(...resp.rows.filter((r): r is ChatRow => isChatRow(r) && (r.runlogOffset ?? 0) < upper))
          const advanced = resp.nextOffset > from
          from = resp.nextOffset
          if (!advanced || from >= upper) break
        }
        // 窗口是从新到旧依次处理的：更早的一窗要放在已收集的前面。
        collected.unshift(...windowRows)
        if (collected.length || floor === 0) break
        upper = floor
        floor = Math.max(0, upper - 1 - PAGE)
      }
      if (collected.length) {
        bubbles.value.unshift(
          ...collected.map((r) => {
            const { media, display } = parseMedia(r.text)
            return {
              key: `log:${r.runlogOffset}`,
              runId: r.runId,
              role: r.role,
              text: display,
              media: media.length ? media : undefined,
              state: 'done' as const,
              usage: r.usage,
            }
          }),
        )
        oldestOffset = collected[0].runlogOffset ?? oldestOffset
      }
      atStart.value = oldestOffset <= 1
    } finally {
      loadingOlder.value = false
    }
  }

  function newSession() {
    pollToken++
    dropSession()
    pending.value = []
    bubbles.value = []
    draft.value = ''
    attachments.value = []
    busy.value = false
    renderedMessageIds.clear()
    sessionGone.value = ''
  }

  return {
    sessionId, bubbles, draft, attachments, busy, pending, loadingOlder, atStart,
    activeWorkspaceId, sessionGone,
    send, reconcile, loadOlder, newSession, showTenantSession, resumeRun,
    setActiveWorkspace, openSession, closeOnDeleted,
  }
}

// 409 = 闸被占（19v6-01 §1），处置是排队；404 找不到这场会话（112001）= 本地 sid 已成僵尸，处置是懒建重投。
// 只认业务码不认 HTTP 状态：500 与 404 都可能是别的原因（doc/40 §4）。
function isBusy(e: unknown): boolean {
  return e instanceof ApiError && e.code === CODE_SESSION_RUNNING
}

function isNotFound(e: unknown): boolean {
  return e instanceof ApiError && e.code === CODE_SESSION_MISSING
}

// 落点项目已被删（114001）：postRuns 带 workspaceId 的预检挡下的就是这个。
function isWorkspaceMissing(e: unknown): boolean {
  return e instanceof ApiError && e.code === CODE_WORKSPACE_MISSING
}

function sleep(ms: number) {
  return new Promise((r) => setTimeout(r, ms))
}
