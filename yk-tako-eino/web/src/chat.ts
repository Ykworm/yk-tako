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
  state: 'streaming' | 'done' | 'error'
}

const LS_SESSION = 'tako.sessionId'
const POLL_MS = 400
// 窗口化初始游标 = max(0, 高水位 - HISTORY_WINDOW)（doc/19 §3：初始值不是 0；窗口大小客户端定）。
// 10 = 刷新后先见最近五轮，更早的上滚翻页拿（U5）。
const HISTORY_WINDOW = 10
const PAGE = 10

export function useChat() {
  const sessionId = ref<SessionID | null>(null)
  const bubbles = ref<Bubble[]>([])
  const draft = ref('')
  const busy = ref(false)
  const loadingOlder = ref(false)
  const atStart = ref(true)

  // 对齐游标：只在拉 log 时前进（doc/19 §5 不变量）。翻页游标是 loadOlder 里的临时值，不碰它。
  let runlogOffset = 0
  // 已渲染最老一行的 offset；0 = 还没渲染任何历史。
  let oldestOffset = 0
  let pollToken = 0

  function push(b: Bubble): Bubble {
    bubbles.value.push(b)
    return b
  }

  function newMessageId(): string {
    return `m_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`
  }

  async function send() {
    const text = draft.value.trim()
    if (!text || busy.value) return
    draft.value = ''
    const messageId = newMessageId()
    push({ key: `${messageId}:user`, runId: null, role: 'user', text, state: 'done' })
    const assistant = push({ key: `${messageId}:assistant`, runId: null, role: 'assistant', text: '', state: 'streaming' })
    busy.value = true
    try {
      const resp = await postRuns(messageId, text, sessionId.value ?? undefined)
      sessionId.value = resp.sessionId
      localStorage.setItem(LS_SESSION, resp.sessionId)
      assistant.runId = resp.runId
      await pollFrames(resp.runId, assistant, 0, pollToken)
    } catch (e) {
      assistant.state = 'error'
      assistant.text = e instanceof ApiError ? `投递失败：${e.body?.error ?? e.status}` : '投递失败'
    } finally {
      busy.value = false
    }
  }

  // doc/19 §2：delta 按 eventOffset 顺序追加，本地游标 = nextOffset。
  // terminal 帧 / status:gone / 拉取请求失败，三者任一即停轮询并收口（§5 第 4 步），不押注单一信号。
  async function pollFrames(runId: RunID, assistant: Bubble, eventOffset: number, token: number) {
    for (;;) {
      if (token !== pollToken) return
      let resp
      try {
        resp = await getFrames(runId, eventOffset)
      } catch {
        assistant.state = 'error'
        assistant.text = '生成中断线（刷新可恢复）'
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
      // gone 且无定稿行 = 这场早已 error 清走：诚实显示失败。
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
      if (e instanceof ApiError && e.status === 404) localStorage.removeItem(LS_SESSION)
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
    localStorage.removeItem(LS_SESSION)
    sessionId.value = null
    bubbles.value = []
    draft.value = ''
    busy.value = false
    runlogOffset = 0
    oldestOffset = 0
    atStart.value = true
  }

  return { sessionId, bubbles, draft, busy, loadingOlder, atStart, send, reconcile, loadOlder, newSession }
}

function sleep(ms: number) {
  return new Promise((r) => setTimeout(r, ms))
}
