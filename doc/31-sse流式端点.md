# 31 · SSE 流式端点（方案草稿，待点头）

> **施工状态**：2026-10-18 方案成文并当日点头、A1–A3 施工 + 活体验收 + A4 doc 收口**全部完成**（单测 4 件 + 全量 runengine 15.1s 绿 + vet 干净；活体五场实录见 `tmp/31-sse流式端点/todos.md`）。
> 翻案标注已落：`19v6-01` §0 纯拉段 + 新 §5、`19v6-90` 二次修订段、`doc/16` 能力表 7d 行。
> 翻案对象：`19v6-01` §0 纯拉模型的「无 SSE、无长连接、无推送」句 + `19v6-90` 2026-10-02 二次修订（b2「删 SSE 走纯拉」）。翻案标注等施工批随代码一起落，本批不动旧文档。
> 编号跳过 30：`tmp/30-崩溃续跑/` 已占位，30 留给那条 loop。

## 1. 背景与动机：谁想要什么

动机（2026-10-18 用户拍）：**API 面通用流式**。tako 的 HTTP API 是产品面的一部分，外部消费方——curl 脚本、SDK、别的产品进程——要观测「一场 run 怎么跑」：回复怎么一个字一个字长出来、中间调了什么工具、什么时候定稿。web 前端只是众多消费方之一，不是这次的服务对象。

现在的四 API 是纯拉：没有任何一条路能让服务端把「有新帧了」主动送到一个还挂在连接上的消费方手里。

## 2. 病灶：不做的具体后果（一场戏）

一个外部消费方想在终端里打字机式展示 tako 的回复。他必须自己写这个循环：

```
loop:
  sleep 300ms                                  ← 节奏他自己定
  GET /api/runs/r_1/frames?eventOffset=N       ← 他自己攒游标
  status==generating → 继续睡
  status==done      → 收口
  status==gone      → 转拉 log 对账            ← 三态语义他必须全懂
```

要正确跑通，他得先读完 19v6-01 §2 的全部字节规则（五+1 种 kind、nextOffset、单批上限 W、agent/model 字段、gone 转拉），再把这套样板抄进自己的语言。**每个集成方重写一遍**，抄错任何一处就是丢帧或死循环。

轮询本身还有一笔税：300ms 间隔意味着绝大多数请求返回空批——白来的 HTTP 往返；拉长间隔省往返，实时性就掉。服务端明明知道「第 7 号帧刚压进去」，却只能等下一次轮询才说得出口。

OpenAI / Anthropic 的流式 API 证明这类产品的消费方期待是什么形状：**一次 GET，字节持续流出，读到底就完**。传输出行的问题不该摊派给每个消费方。

## 3. 与 10-02 b2 裁定的对质（为什么当时对、现在为什么不对）

当时拍的（19v6-90 二次修订）：每 delta 独占 `eventOffset` 后，它的唯一归宿就是续传游标；与其让流认游标、或维护「挂流 + 拉取双路合并」，不如删掉流，`runlive` 直接当拉取端点。

**当时对在哪**：那时 SSE 的定位是**唯一实时通道**——stream 与 frames 是两路状态源，客户端收流的同时还得拉 frames 补漏，两路合并的复杂度真实存在，删掉是对的。

**现在为什么不对**（用户 10-18 拍「流拉并存」）：流不再承担任何正确性职责——四 API 一字不动，断线恢复、新 tab、补历史、收口对账全部仍走拉。流只是**同一帧面的第二种运输方式**，交付的字节与 frames 端点逐字节同形状（见 §4）。所谓「双路合并」不需要回来，因为消费逻辑本来就只有一份（19v6-02 reconcile）：无论字节从流来还是从拉来，都喂给同一套「delta 追加、按 runId 对 log 收口、gone 转拉」。

**不变量一句话：拉是正确性，流是优化。** 流不承诺可靠送达——连接断了，中间的帧就丢了，服务端不补发（重连补发是消费方拿 Last-Event-ID 自己做的事，见 §4）。所以任何消费方的收口对账（拿 log 定稿行替换碎片气泡）都以拉路为准；流上收到 terminal 可以直接收口，也可以再拉一次 log 验一遍，消费方自己选。

黑话翻译：`eventOffset` 没有长出第二个含义。它还是「runlive LIST 里的序号」，只是现在有两种读法——批量读（frames 端点）和逐条读（stream 端点）。api.ts 里「不存在统一 cursor 抽象」的注释继续成立：游标自始至终只有一个。

## 4. 端点契约：`GET /api/runs/{runId}/stream`（第五个 API）

**响应** `200 Content-Type: text/event-stream`。帧真源就是 runlive Redis LIST（`tk:runlive:<runId>`），流 handler 循环调 `ReadFrames`——与 frames 端点同一个存储原语。**流上没有新帧形状**：kind 集合与 frames §2 契约同一份、随它走（当前 start / delta / tool_call / tool_result / usage / terminal，doc/30 的 resume 也在列），字段逐一相同（含 `agent` / `model`，子代理帧原样带出，消费方自己决定渲染与否）。

SSE 字节格式（每帧一个 event）：

```
id: 2
event: frame
data: {"eventOffset":2,"kind":"delta","agent":"","model":"deepseek/deepseek-flash","text":"我查一下"}

id: 6
event: frame
data: {"eventOffset":6,"kind":"terminal","agent":"","model":"","status":"complete"}

```

- `id:` = 该帧的 `eventOffset`。重连 = 带 `Last-Event-ID: N` 头重新 GET，服务端从 offset > N 继续——**不是流自己另认游标，是同一个游标的第二种读法**。
- `event: gone`：连接建立时 runlive 键已不存在（含「从未存在」与「TTL 过期」，与 frames 的 200 `gone` 同语义、故意不区分）→ 推一条后关闭。
- **terminal 帧推完服务端主动关连接**：run 结束 = 流的自然终点（complete / error / stopped 同）。
- **心跳**：每 15 秒写一行 SSE 注释 `: ping`（无 data，EventSource 自动忽略），防 NAT / 代理掐闲连接。10-02 删心跳是因为纯拉无连接可掐；流模式下没有它，60 秒无字节的连接死在中间层，消费方看到的是「无声断流」。间隔 `-sse-heartbeat` 可配。
- 单批上限 W **不适用**于流：W 是拉路「防超长回复撑爆单次响应」的概念，流逐条推、无批。
- 错误：本端点不回 404（未知 run = 200 + `gone`，同 frames）。建流之后服务端内部错误 = 直接断流——SSE 协议没有错误语义，消费方凭「连接断了且没收到 terminal」知道该重连。

## 5. 服务端机制（每步四问）

handler 就是一个循环（Gin + `http.Flusher`）：

1. **建流**：读 `Last-Event-ID`（缺省 0）→ `ReadFrames(ctx, runID, from, W)`。
2. **推存量**：读到的帧逐条写 `id/event/data` + flush。
3. **判终**：末帧是 terminal → 关连接；键不存在 → `gone` 事件 + 关连接。
4. **tail**：没 terminal → sleep tail 间隔（dev 150ms）→ `from` = 已推到的 offset → 回到第 1 步。

- **做什么**：把 runlive LIST 的增长持续搬运到连接上。
- **不做的后果**：不做 tail = 只推连接建立瞬间已有的帧，之后生成的全丢——流毫无用处；不做判终 = run 结束后连接永久挂着空转。
- **输入从哪来**：runlive Redis LIST——worker 每枪 drain 合并后压帧的那份真源（POST 事务建、收尾 TTL 清）。流不碰 worker 内部、不订任何进程内事件。
- **输出到哪去**：消费方的 SSE 解析器 → 交给与拉路相同的 reconcile 逻辑（§3）。

**tail 用定时 `ReadFrames`，不用事件通知**，理由挂 AGENTS.md 硬规矩：进程内 chan / pubsub 的等待者上线即失效。定时读 Redis 天然多副本安全——任何副本都能服务任何 run 的流，零粘性路由、零每连接状态（offset 就是游标本身）。代价：每连接每 150ms 一次 LRANGE 空转，连接数上去后这笔账实测再算；Redis pub/sub 唤醒登记为**演进方向**（§7），本拍不做。

## 6. 边界情形

| 情形 | 行为 |
|---|---|
| run 不存在 / TTL 已过期 | 200 + `gone` 事件 + 关（与 frames 的 200 `gone` 同语义） |
| 连接建立时 run 已 terminal | 存量推完（含 terminal）即关 |
| 消费方读得慢（回压） | Write 阻塞 → tail 停摆，不丢帧；断开重连从 Last-Event-ID 补 |
| 连接中途断（客户端侧） | 服务端 ctx 取消即退出，无清理义务——每连接零状态 |
| 连接中途断（服务端侧） | 同上断流；消费方重连补发 |
| error / stopped 结局 | terminal 帧照推（status 字段区分），推完关 |
| 子代理委派帧 | 原样推（带 `agent`/`model`），消费方自决渲染 |

## 7. 明确不做（本拍）

- **web 前端切流**：前端拉路已验收，切流是优化不是需求，独立拍（待拍项 1）。
- **会话级流 / 多 run 聚合流 / 历史流**：历史回放走 log 端点，流只管「生成中」这一段。
- **鉴权 / 压缩 / 重放**：照 doc/28 多租户的缝走，不在本拍。
- **Redis pub/sub 唤醒 tail**：演进方向，空转成本实测后再议。

## 8. 验收（契约拍：curl 手打，不是测试运行器）

启动 `go run ./cmd/yktako -engine 127.0.0.1:<port>`，逐条手打：

1. **金路径**：`curl -N http://…/api/runs/r_x/stream`（POST /api/runs 后立刻挂上）→ 期望字节：`start` → `delta…` → `terminal`，terminal 后连接自动关闭。
2. **断线续传**：中途 Ctrl-C，重连 `-H "Last-Event-ID: <N>"` → 期望字节：首帧 `eventOffset` = N+1，直到 terminal。
3. **gone**：对一个已过期 runId stream → 期望字节：一条 `event: gone` 后关闭。
4. **error 结局**：构造 error run（如强制委派探针），stream 全程 → terminal 帧 `status:"error"`，连接关闭。
5. **心跳**：不说话的 run（如长工具执行中）挂 20s → 期望字节：至少一条 `: ping`。

每条验收先打跑着的东西，再谈单测；失败分支（服务端内部错断流）单测盖。

## 9. 待拍清单

1. **web 前端切不切流**——倾向本拍不切（拉是正确性路径且已验收）。
2. tail 间隔初值（150ms）与心跳间隔初值（15s）——dev 校准，写 `-sse-heartbeat` / `-sse-tail` 旗标还是常数，施工拍定。
3. `retry:` 行（重连间隔建议值）要不要——小项，可随契约拍定。
4. event 名就 `frame` / `gone` 两个，够不够——倾向够。

## 10. 闭环：所以这解决了什么

外部消费方从「读懂三态 + 抄轮询样板 + 自管节奏」降为「一次 GET，读到 terminal 为止」；服务端从被动等问变为主动送达，且交付字节与拉路同形状、游标同源、收口仍以拉路对账——流上做的一切优化都不碰正确性。翻案代价被钉在最小：四 API 零改动，第五个端点是纯增量。
