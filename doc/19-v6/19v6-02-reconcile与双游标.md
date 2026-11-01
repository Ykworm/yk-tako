# 19v6-02 · reconcile 与双游标

> 本篇是 doc/19-v6 文档集的**客户端流程篇**：refresh / reconnect 共用的纯轮询总流程，以及窗口化引入的两种游标之分。
> 端点字节真源在 `19v6-01`；概念表见 index（`19-运行引擎架构-v6.md`）。

## reconcile —— 客户端纯轮询总流程（refresh / reconnect 共用）

本步只要求语义、不要求实现：

1. **查状态** `GET /api/sessions/{sessionId}` → `{liveRunId, runlogOffset=高水位}`；
2. **补历史** `GET /api/sessions/{sessionId}/log?runlogOffset=本地值`（新 tab 本地值 = `max(0, 第 1 步的高水位 - 窗口)`，**不是 0**）→ 渲染这批定稿记录；本地 `runlogOffset` 推进到返回的 `nextOffset`；**若 `nextOffset` 仍小于第 1 步的高水位，说明本批被 W 截断，重复拉直到追平**；用户往上滚要更早的历史时，用更小的 offset **临时**拉一批（见下「两种游标」）；**窗口起点可能正好落在一次工具调用与它的回执之间**（游标排他）：取回的行里若有回执找不到同批的调用行，把起点再往前挪一页重拉，直到配对完整或到 0（卡片口径见 [doc/59](../59-工具调用可见性.md) §5.2）；
3. **观测生成** 若 `liveRunId` 非空 → `GET /api/runs/{liveRunId}/frames?eventOffset=本地值`（新 tab = 0）→ 按 `eventOffset` 追加 delta 成「正在生成」气泡；按轮询间隔重复拉 `nextOffset` 之后；
4. **收口** 见 frames `status:done`（terminal）或 `status:gone`，或状态查询发现 `liveRunId` 变 `""` / 变了 → 回第 2 步拉一次 log，把那条 `assistant` 全文按 `runId` **替换**半截气泡；停止轮询该 run 的 frames。

关键不变量：**两个对齐游标各自只在自己的端点前进**——本地 `runlogOffset` 只在拉 log 时 +，本地 `eventOffset` 只在拉 frames 时 +。所以在线的、断线重连的、run 结束后才开的新 tab，都靠「拉 log 补齐 + 有活 run 就拉 frames」这一条路径对齐，不需要「当时在场」。terminal 帧、`liveRunId` 变化、frames `gone` 三者任一都能触发收口，互为冗余，不押注单一信号。

## 两种游标别混（窗口化引入的区分）

- **对齐游标**（本地 `runlogOffset` / `eventOffset`）：表示「我已经对齐到哪」，**只前进**，就是上面那条不变量说的游标。
- **翻页游标**（往上滚翻旧历史时用的 offset）：**临时值，只用于那一次请求**，拉完渲染即可，**不回退对齐游标**。

之所以能这么拆，是因为 `runlogOffset` 永久有效：往回拨拉旧数据不破坏任何东西，服务端也不需要知道客户端翻到哪。窗口化因此**不削弱**「不需要当时在场」——恢复只要求「拿得到高水位 + 一段尾巴 + 之后能续」，从来不要求「拿到全部」。
