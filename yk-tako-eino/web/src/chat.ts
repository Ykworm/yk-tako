import { ref } from 'vue'
import {
  ApiError,
  getFrames,
  getLog,
  getSession,
  postRuns,
  type Row,
  type RunID,
  type SessionID,
} from './api'

export type Bubble = {
  key: string
  runId: RunID | null
  role: 'user' | 'assistant'
  text: string
  state: 'queued' | 'streaming' | 'offline' | 'done' | 'error'
}

// 一条待投投递 = 已渲染的 user 气泡 + 它那条 assistant 气泡 + 请求前就定下的 messageId
// （doc/19 §1 情形 3：重发不换 id）。
type Pending = { messageId: string; text: string; assistant: Bubble }

const LS_SESSION = 'tako.sessionId'
const POLL_MS = 400
// 冗余信号核对节奏：每 STATUS_EVERY 拍 frames 顺带查一次 session（≈2s 一次 GET）。
// 闸释放与 terminal 压入是同一笔原子写（doc/19 §7），所以「闸已空」必然意味着「terminal 已在」，
// 这一拍只会让收口提前，不会误收。
const STATUS_EVERY = 5
// 窗口化初始游标 = max(0, 高水位 - HISTORY_WINDOW)（doc/19 §3：初始值不是 0；窗口大小客户端定）。
// 10 = 刷新后先见最近五轮，更早的上滚翻页拿（U5）。
const HISTORY_WINDOW = 10
const PAGE = 10

export function useChat() {
  const sessionId = ref<SessionID | null>(null)
  const bubbles = ref<Bubble[]>([])
  const draft = ref('')
  const busy = ref(false)
  const pending = ref<Pending[]>([])
  const loadingOlder = ref(false)
  const atStart = ref(true)

  // 对齐游标：只在拉 log 时前进（doc/19 §5 不变量）。翻页游标是 loadOlder 里的临时值，不碰它。
  let runlogOffset = 0
  // 已渲染最老一行的 offset；0 = 还没渲染任何历史。
  let oldestOffset = 0
  let pollToken = 0
  let pumping = false

  function push(b: Bubble): Bubble {
    bubbles.value.push(b)
    return b
  }

  function newMessageId(): string {
    return `m_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`
  }

  // 投递 = 乐观渲染两条气泡，真正发不发交给 pump（忙时留在队里，不变成错误）。
  function send() {
    const text = draft.value.trim()
    if (!text) return
    draft.value = ''
    const messageId = newMessageId()
    push({ key: `${messageId}:user`, runId: null, role: 'user', text, state: 'done' })
    const assistant = push({ key: `${messageId}:assistant`, runId: null, role: 'assistant', text: '', state: 'queued' })
    pending.value.push({ messageId, text, assistant })
    void pump()
  }

  // 队列泵（doc/19 §1「409 = 客户端本地排队、本轮收口后补投」）：本 tab 有 run 在跑、
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

  // gateIdle：闸空 = 会话没有活 run（doc/19 §4）。404 = 本地 sid 已被 reset 清成僵尸，
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
        // 别的客户端抢到了闸：什么都不写（doc/19 §1），所以原样回队首等补投，不是投递失败。
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

  // 僵尸 sessionId 自愈：本地存的 sid 被 dev reset / 空重启清走之后，带它投递只会在 §1 的准入
  // 检查上稳定 404（「带 = 只认登记过的，不隐式创建」）。客户端的补救是认它不存在——
  // 丢掉本地值、不带 sid 重投一次走懒建。只重投这一次，不把 500 之类拖进循环。
  async function postWithFreshSession(p: Pending) {
    const sid = sessionId.value
    try {
      return await postRuns(p.messageId, p.text, sid ?? undefined)
    } catch (e) {
      if (sid && isNotFound(e)) {
        dropSession()
        return await postRuns(p.messageId, p.text)
      }
      throw e
    }
  }

  // doc/19 §2：delta 按 eventOffset 顺序追加，本地游标 = nextOffset。
  // terminal 帧 / status:gone / liveRunId 易主，三者任一即停轮询并收口（§5 第 4 步），不押注单一信号。
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

  // runSettled：doc/19 §4 的 `liveRunId` 变空或易主 = 那场 run 已结束（terminal 与放闸同笔写入）。
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

  // 收口（doc/19 概念表「收口」）：定稿后权威内容以 runlog 为准，按 run 整条替换半截气泡。
  // error 终态不进 runlog（新 A6 已裁）——不拉 log，气泡转错误态。
  async function closeOut(runId: RunID, assistant: Bubble, terminalStatus: string | null) {
    if (terminalStatus === 'error') {
      assistant.state = 'error'
      return
    }
    const row = await fetchAssistantRow(runId)
    if (row) {
      assistant.text = row.text
      assistant.state = 'done'
    } else {
      // 闸已空 / 帧已 gone 却没有定稿行 = 那场早已 error 清走（或 run 被 reset 打断）：
      // 半截文本留着，状态转错误——权威历史里没有它。
      assistant.state = 'error'
    }
  }

  // 从对齐游标续拉 log 直到追平（空批 = 追平），只找该 run 的 assistant 行、不渲染
  // （本 tab 的 user 气泡是投递时乐观渲染的，再渲染就重复）。
  async function fetchAssistantRow(runId: RunID): Promise<Row | null> {
    const sid = sessionId.value
    if (!sid) return null
    for (;;) {
      const resp = await getLog(sid, runlogOffset)
      const hit = resp.rows.find((r) => r.role === 'assistant' && r.runId === runId) ?? null
      const advanced = resp.nextOffset > runlogOffset
      runlogOffset = resp.nextOffset
      if (hit) return hit
      if (!advanced) return null
    }
  }

  // reconcile（doc/19 §5 四步）：查状态 → 窗口化补历史 → 有活 run 则 frames(0) 重建半截气泡。
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

  function appendRows(rows: Row[]) {
    for (const r of rows) {
      push({ key: `log:${r.runlogOffset}`, runId: r.runId, role: r.role, text: r.text, state: 'done' })
      const off = r.runlogOffset ?? 0
      if (off > 0 && (oldestOffset === 0 || off < oldestOffset)) oldestOffset = off
    }
    atStart.value = oldestOffset <= 1
  }

  // 翻页：临时游标只用于这一次请求，拉完 prepend，**不回退对齐游标**（doc/19「两种游标别混」）。
  // 批被 W 截断时按 nextOffset 续拉补齐（doc/19 §3：「客户端从它继续拉即补齐」）。
  async function loadOlder() {
    const sid = sessionId.value
    if (!sid || loadingOlder.value || atStart.value) return
    loadingOlder.value = true
    try {
      let from = Math.max(0, oldestOffset - 1 - PAGE)
      const collected: Row[] = []
      for (;;) {
        const resp = await getLog(sid, from)
        collected.push(...resp.rows.filter((r) => (r.runlogOffset ?? 0) < oldestOffset))
        if (resp.nextOffset >= oldestOffset || resp.nextOffset === from) break
        from = resp.nextOffset
      }
      if (collected.length) {
        bubbles.value.unshift(
          ...collected.map((r) => ({
            key: `log:${r.runlogOffset}`,
            runId: r.runId,
            role: r.role,
            text: r.text,
            state: 'done' as const,
          })),
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
    busy.value = false
  }

  return { sessionId, bubbles, draft, busy, pending, loadingOlder, atStart, send, reconcile, loadOlder, newSession }
}

// 409 = 闸被占（doc/19 §1），处置是排队；404 session_not_found = 本地 sid 已成僵尸，处置是懒建重投。
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
