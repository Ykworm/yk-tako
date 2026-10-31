// 变更同步（doc/46 Loop A）：同机多 tab 的失效通知。
//
// 删除/改名成功后，当事 tab 向同 origin 广播一条变更事件；其它 tab 收到后让本地状态
// **失效**（清落点 / 重拉列表 / 作废气泡），失效永远跟一次重拉——事件只带 {type, id}，
// 不携带状态（doc/46 §3.1：拉是正确性，事件只是让对账早点发生）。Loop B 会把服务端
// SSE 事件流的帧喂进同一个 handler 集合，这里已是它的形状。
import { readTenant, readToken, refreshAccessToken } from './auth'

export type MutationEvent = {
  // skills_changed：租户级技能目录变了（doc/52 C3 / doc/46 §7.6）。id 是技能名。
  // tools_changed：租户工具根变了（doc/52 F1）。与技能分开是让客户端知道重拉哪一页；
  // 两个事件都只通知、不带状态，收到就重拉。
  type:
    | 'session_deleted'
    | 'workspace_deleted'
    | 'workspace_renamed'
    | 'skills_changed'
    | 'tools_changed'
  id: string
}

type Handler = (evt: MutationEvent) => void
const handlers = new Set<Handler>()

// 订阅变更事件；返回取消函数（组件 onUnmounted 用）。
export function onMutation(h: Handler): () => void {
  handlers.add(h)
  return () => handlers.delete(h)
}

function dispatch(evt: MutationEvent) {
  // 单个失效动作出错不拖垮其它 handler——失效是优化，不值得为它炸页面。
  for (const h of [...handlers]) {
    try {
      h(evt)
    } catch {
      /* 忽略 */
    }
  }
}

// sourceId 挡自回声：BroadcastChannel 会送达包括发送者在内的所有 tab，
// 而发送者已在成功回调里更新过自己的状态。
const TAB_ID = Math.random().toString(36).slice(2)
const channel =
  typeof BroadcastChannel !== 'undefined' ? new BroadcastChannel('tako.mutations') : null

if (channel) {
  channel.onmessage = (e: MessageEvent) => {
    const m = e.data as { sourceId?: string; tenantId?: string; evt?: MutationEvent } | undefined
    if (!m || m.sourceId === TAB_ID) return
    if (m.tenantId !== readTenant().id) return // 同浏览器里另一个租户的 tab，不关我的事
    if (m.evt) dispatch(m.evt)
  }
}

// 当事 tab 在变更成功后调用：把事件广播给同机其它 tab（自己不重复处理）。
export function broadcastMutation(evt: MutationEvent) {
  channel?.postMessage({ sourceId: TAB_ID, tenantId: readTenant().id, evt })
}

// —— Loop B：服务端事件流（doc/46 §3.3）——
// 与 BroadcastChannel 并存：同机广播是快路径，SSE 是跨机跨端的正路；
// 两个源都喂同一个 dispatch，失效逻辑只有一份（doc/46 §3.2 的关键设计）。
let es: EventSource | null = null
let esTimer: ReturnType<typeof setTimeout> | null = null

// ensureEventStream 挂上服务端事件流（已挂则空操作；换租户先 closeEventStream 再挂）。
export function ensureEventStream() {
  const tenantId = readTenant().id
  const token = readToken()
  if (!tenantId || !token) return
  if (es) return
  es = new EventSource(`/api/events?access_token=${encodeURIComponent(token)}`)
  es.onmessage = (e) => {
    try {
      const evt = JSON.parse(e.data) as MutationEvent & { seq?: string }
      if (evt.type && evt.id) dispatch(evt)
    } catch {
      /* 单帧坏 JSON 不炸通道 */
    }
  }
  es.onerror = () => {
    // EventSource 原生重连用连接那一刻的 URL——access token 短效，过期后原样重连只会 401。
    // 关掉、退避重建（3s，网络抖动不至于狂刷）。
    es?.close()
    es = null
    if (esTimer) clearTimeout(esTimer)
    esTimer = setTimeout(() => {
      // **重建前自己刷一次 token**（2026-10-23 修，doc/48 之后的复审发现）：
      // 原先这里只「取最新 token 重建」，可**页面静止时没人调用 api 层，也就没人去刷新它**——
      // sessionStorage 里那个 access token 就一直是过期的 → 服务端一律 401 →
      // 3 秒一次无限重连，把引擎日志刷满（实测连续 401 刷屏）。
      // 自己先刷：刷得到就用新 token 重建；刷不到（refresh token 也失效，clearAuth 已清）
      // 就不再重建，等用户重新登录（App.vue 的 authState watch 会重新挂）。
      void refreshAccessToken().finally(() => {
        if (readToken()) ensureEventStream()
      })
    }, 3000)
  }
}

export function closeEventStream() {
  if (esTimer) {
    clearTimeout(esTimer)
    esTimer = null
  }
  es?.close()
  es = null
}
