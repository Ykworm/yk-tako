import { ref } from 'vue'
import {
  ApiError,
  getFrames,
  getLog,
  getSession,
  parseMedia,
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
  state: 'queued' | 'streaming' | 'offline' | 'done' | 'error' | 'stopped'
  usage?: Usage
  // 本 tab 亲历收尾却全程无 usage 帧（厂商不报账单）→ 显示「用量 ?」，不静默。
  usageUnknown?: boolean
}

// 附件（composer 选用）：name 供 chip 显示，img 随 postRuns 上行。
export type Attachment = { name: string; img: PostImage }

// 一条待投投递 = 已渲染的 user 气泡 + 它那条 assistant 气泡 + 请求前就定下的 messageId
// （19v6-01 §1 情形 3：重发不换 id）。
type Pending = { messageId: string; text: string; assistant: Bubble; images?: PostImage[] }

const LS_SESSION = 'tako.sessionId'
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
    pending.value.push({ messageId, text, assistant, images: imgs.length ? imgs : undefined })
    void pump()
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
    sessionId.value = null
    localStorage.removeItem(LS_SESSION)
    runlogOffset = 0
    oldestOffset = 0
    atStart.value = true
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
    assistant.state = 'streaming'
    assistant.text = ''
    busy.value = true
    try {
      const resp = await postWithFreshSession(p)
      sessionId.value = resp.sessionId
      localStorage.setItem(LS_SESSION, resp.sessionId)
      assistant.runId = resp.runId
      await pollFrames(resp.runId, assistant, 0, token)
    } catch (e) {
      if (isBusy(e)) {
        // 别的客户端抢到了闸：什么都不写（19v6-01 §1），所以原样回队首等补投，不是投递失败。
        // 但「新对话」之后的补投属于另一个会话，不能把它塞回队列（幽灵消息）。
        assistant.state = 'queued'
        if (token === pollToken) pending.value.unshift(p)
      } else {
        assistant.state = 'error'
        assistant.text = e instanceof ApiError ? `投递失败：${e.body?.error ?? e.status}` : '投递失败'
      }
    } finally {
      busy.value = false
    }
  }

  // 僵尸 sessionId 自愈：本地存的 sid 被 dev reset / 空重启清走之后，带它投递只会在 19v6-01 §1 的准入
  // 检查上稳定 404（「带 = 只认登记过的，不隐式创建」）。客户端的补救是认它不存在——
  // 丢掉本地值、不带 sid 重投一次走懒建。只重投这一次，不把 500 之类拖进循环。
  async function postWithFreshSession(p: Pending) {
    const sid = sessionId.value
    try {
      return await postRuns(p.messageId, p.text, sid ?? undefined, p.images)
    } catch (e) {
      if (sid && isNotFound(e)) {
        dropSession()
        return await postRuns(p.messageId, p.text, undefined, p.images)
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
        if (f.kind === 'delta' && f.text) assistant.text += f.text
        // usage 帧（doc/27 §4a）：每枪一帧，多枪取最后一帧（= 下一枪的基线）；
        // 子代理帧（agent 非空）第一版不渲染，跳过。
        if (f.kind === 'usage' && !f.agent) {
          assistant.usage = {
            promptTokens: f.promptTokens ?? 0,
            completionTokens: f.completionTokens ?? 0,
            window: f.window ?? 0,
          }
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
  // error 终态不进 runlog（新 A6 已裁）——不拉 log，气泡转错误态。
  // stopped 终态（doc/17 §4）同样不进 runlog——保留已流出的字、标停止、不拉 log 不转错误。
  async function closeOut(runId: RunID, assistant: Bubble, terminalStatus: string | null) {
    if (terminalStatus === 'error') {
      assistant.state = 'error'
      return
    }
    // 本 tab 亲历的收尾：全程没见过 usage 帧 = 厂商没报账单，标出来（error 态不标——失败气泡本就有说明行）。
    if (!assistant.usage) assistant.usageUnknown = true
    if (terminalStatus === 'stopped') {
      // 半截文本原样保留（不替换）：没有定稿行可拉，人看见的就是停那一刻已流出的内容。
      assistant.state = 'stopped'
      return
    }
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
    // 没有定稿行：error 收尾与 stopped 收尾在 log 里长得一样（都没有 assistant 行）——
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
    const sid = sessionId.value
    if (!sid) return null
    let hit: Row | null = null
    let ownUserOffset = 0
    const foreign: Row[] = []
    for (;;) {
      const resp = await getLog(sid, runlogOffset)
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
    renderForeign(foreign, assistant, ownUserOffset)
    return hit
  }

  // reconcile（19v6-02 四步）：查状态 → 窗口化补历史 → 有活 run 则 frames(0) 重建半截气泡。
  async function reconcile() {
    const sid = localStorage.getItem(LS_SESSION) as SessionID | null
    if (!sid) return
    let status
    try {
      status = await getSession(sid)
    } catch (e) {
      if (isNotFound(e)) dropSession()
      return
    }
    sessionId.value = sid
    runlogOffset = Math.max(0, status.runlogOffset - HISTORY_WINDOW)
    for (;;) {
      const resp = await getLog(sid, runlogOffset)
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
  }

  return { sessionId, bubbles, draft, attachments, busy, pending, loadingOlder, atStart, send, reconcile, loadOlder, newSession }
}

// 409 = 闸被占（19v6-01 §1），处置是排队；404 session_not_found = 本地 sid 已成僵尸，处置是懒建重投。
// 只认 error 码不认 HTTP 状态：500 与 404 都可能是别的原因。
function isBusy(e: unknown): boolean {
  return e instanceof ApiError && e.body?.error === 'session_busy'
}

function isNotFound(e: unknown): boolean {
  return e instanceof ApiError && e.body?.error === 'session_not_found'
}

function sleep(ms: number) {
  return new Promise((r) => setTimeout(r, ms))
}
