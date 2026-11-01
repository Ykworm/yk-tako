// 多会话并发的实例池上限（doc/60 A4）。
// 实例会一直保留整段历史，长开页面不能无限涨；但淘汰必须是「看不见的损失最小」：
// 只动已经持久化、不在跑、没草稿的实例——切回去重新 reconcile 就能恢复。

export const MAX_CHATS = 8

export type PoolEntry = {
  sessionId: string | null
  busy: boolean
  // 有草稿或附件：切走再切回也没法从服务端恢复，不能淘汰。
  dirty: boolean
  lastActiveAt: number
}

// 返回该淘汰的下标；没有可淘汰的返回 -1。
// 激活实例不动；有 sid 才可重建；busy/dirty 不动；同条件取 lastActiveAt 最小（最久没看）。
export function pickEviction(entries: PoolEntry[], cap: number, activeIndex: number): number {
  if (entries.length <= cap) return -1
  let hit = -1
  for (let i = 0; i < entries.length; i++) {
    if (i === activeIndex) continue
    const e = entries[i]
    if (!e.sessionId || e.busy || e.dirty) continue
    if (hit < 0 || e.lastActiveAt < entries[hit].lastActiveAt) hit = i
  }
  return hit
}
