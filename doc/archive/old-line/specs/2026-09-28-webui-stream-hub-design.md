# Web UI 流式枢纽设计（v3：落盘地基 + seq 续传）

> 状态：**草案 v3**（2026-09-29 审阅后修订），可按此实现。
> 缘起：T2（最小 Web UI）验收收尾期间的架构讨论。结论：以「落盘事件日志 +
> seq 续传」为地基，会话拥有 run、日志承接事件、连接只是读者。结构学 WeKnora
> 的五个决策，落盘与 completed 表字段比 WeKnora 更进一步（它靠 Redis TTL +
> 消息行，我们两张表从第一天起落盘）。见第 3 节。
> 关联：T2 定案《doc/10》、台账《doc/11》；对照实现 `ref-agent/WeKnora`；
> 现状事实锚点在第 2 节，均有文件:行号。
> 修订史：v1 曾按 V1/V1.5/V2/V3 分版（内存日志起步）。用户四点拍板后，v2 改为
> 任务制 + 落盘先行，原四个未决问题转入第 3 节。v3 钉死续传游标、seq 范围、
> 契约并入落盘、档案行生命周期、停止路径；REPL 不写库。
> 2026-09-29 续：页面改读进程内事件列表，列表不因完成而删除；重启后从事件表装回。
> 模型下一轮读档案里带产出的已完成轮次，不再使用 `session.history`。
> 助手正文逐字进入这份列表：同一条消息、同一个序号，字变长就再推一次；写完才进事件表。
> 没写完的半截不落库。
> 2026-09-30 续：已结束的对话按会话从 `event_log` 懒加载（`GET /api/session/history`），
> 游标用 `(created_at, run_id)`。Chrome 只保留 `yktakoSession`，不保存助手正文。
> 正在跑的轮次仍用 `from=0` 接上。计划见 `doc/15-页面从event_log读对话的实施计划.md`。
> Redis、运行中插话仍不做。

## 0. 修正记录（先把一处口误钉死）

**当前实现的事件信封里没有 Seq。** 现有 SSE 载荷是 `sseEvent`
（`yk-tako-eino/internal/webui/sse.go:24-32`），字段只有
`kind / agent / role / text / toolName / toolNames / approval`，**无 seq**。
此前对话里说的「我们的事件信封里已经有 Seq」是把本方案的设计说成了现状，属于
口误。Seq 是本方案引入的新字段（第 4 节环 2），引入前请把它当成「未存在的事实」。

## 1. 目标与范围

**目标**：以落盘事件日志为地基，把 Web UI 运行时从「连接即 run」改为
「会话拥有 run、日志承接事件、连接只是读者」。断线续传、多读者、崩溃可查
都从这张表上长出来。REPL 的终端输出与退出行为不变。多用户方向不做。

**地基定义（用户拍板：先做，否则上层白做）**：续传、多读者、回放、审计
的全部前提只有一句：**日志有一个比进程命长的家**。事件表 + 档案表就是 T1
（第 7 节），不是可后补的选项。

**范围冻结**（照《10》第 5 节的纪律，超出的记第 10 节）：

- 不做：steer 运行中插话、多用户鉴权、WS 传输、另立一种 token delta 事件、
  标题/索引/记忆等 WeKnora 增值层、断线后的空闲超时。
  2026-09-29 起，已完成轮次的消息会在下一轮从档案装回。在途轮次仍然不续跑。
  助手正文逐字更新同一条消息，不另加事件种类。
- 只做：事件契约补参数、落盘两张表（事件 + 档案）、seq 续传、运行侧与连接侧解耦、
  按 run 的追加日志轮询、live-run 闸、显式停止、completed 双标记、
  按会话读取当前 live run。
  2026-09-29 起另做了：助手正文在内存里逐字变长，写完才落事件表；半截不落库。
  2026-09-30 起：已结束的对话按会话从 `event_log` 懒加载，Chrome 不保存助手正文。

崩溃之后能回看已落盘的事件，能知道哪一轮没写完终局。在途轮次不续跑
（第 2 节 F5）。下一轮模型读的是档案里已经写上产出的轮次。

## 2. 现状事实（可检验）

**F1 — 连接即 run。** `server.go:99` 把 `r.Context()` 传给 `sess.runTurn`：
浏览器一断，run 被取消，这一轮作废（`server.go:107-110` 注释明写此语义）。
这是《10》第 4.4 节冻结的语义，且前端「停止」按钮就是靠 abort 触发这条链的
（`index.html:1088-1090`）。

**F2 — 事件只进请求级管道。** `server.go:96` 的 `events` chan（容量 16）是
本次 HTTP 响应私有。Web sink（`session.go:111`）与审批 ask
（`session.go:115-126`）两道门各把事件塞进去。连接之外无人可读，进程一退全无。

**F3 — 事件信封有损且无序号。** `eventFrom`（`event.go:56-61`）只抽
`Function.Name`，`arguments` 被丢弃。SSE 载荷无 seq（见第 0 节）。日志一旦成为
档案，回放时连「调了什么」都展示不出来。

**F4 — 单 run 闸存在但只是布尔。** `session.active`（`session.go:29,51-54`）
+ 409（`server.go:82-86`），粒度正确（每会话一轮），但回答不了「当前活跃
run 是哪一轮」，无法被 stop / 续传按 runID 寻址。

**F5 — checkpoint 救不了在途轮次。** Eino checkpoint 只在审批中断时打点，
正常执行路径无存点。Store 是内存 map（`adkrun/turn.go:21-24`，webui 每会话
一份 `session.go:42`），进程崩溃后无点可查。本方案不追求在途轮次崩溃续跑
（那是路线 #7 完整版）。事件侧因落盘而可回放（第 4 节环 6）。

**F6 — 档案只在内存。** 会话历史是 `session.history []*schema.Message`
（`session.go:29`），run 结束追加（`session.go:66-67`），进程退即失。completed
目前无处可标。这正是 T1 要建档案表的原因。成功才落历史；`err != nil` 不落，
这个口径继续有效。

## 3. 已拍板决策

**D1 — 续传游标是「最后收到的 seq」（仅限正在跑的轮次）。** 客户端每帧记下 `lastSeq`。重连请求
`GET /api/chat/stream?run=<runID>&from=<lastSeq>`，服务端返回 `seq > from` 的行。
`>` 保证不重，`ORDER BY seq` 保证不漏。`from` 缺省与 `from=0` 都表示从头
（`seq > 0`）。`from` 超过当前最大 seq 时返回空，不是错误。

例：客户端已经渲染 seq 17，重连带 `from=17`，服务端从 seq 18 起补。
客户端若改传 `lastSeq+1`，会把下一帧跳掉，实现时禁止这种配对。

从 0 全量回放只在游标丢失时使用。跳号自检见第 4 节环 5，它是客户端的断言，
主路径仍是这条游标。

**已结束的对话不用这条游标。** 历史按会话从 `event_log` 懒加载（`GET /api/session/history`），
游标是 `(created_at, run_id)`，滚到顶才取更早的一页。`lastSeq` 只活在同一页内存里，
供正在跑的那一轮断线后续传，不写入 Chrome。

**D2 — 溢出问题随落盘消失，容量界不设。** 溢出是内存日志的两难：不设界
会泄漏（WeKnora `memory_manager.go:26` 的 map 只进不出），设界成环形缓冲
又会把回放截断。落盘后一场 run 的事件在语义级粒度只有几十到几百行（KB 级），
磁盘不是瓶颈。旧 run 删不删归保留策略（第 9 节 N2），不进本方案的实现范围。

**D3 — completed 从第一天就是表格字段。** 档案表带 `is_completed` 与 `status`，
与事件表同属 T1。`is_completed = 1` 当且仅当 `status = completed`。
出错和用户停止也算这一轮已经结束（见 D7）。进程崩溃、没写上终局信封的行保持
`is_completed = 0`，启动对账把 `status` 改成 `failed`。

**D4 — 契约与落盘同属 T1，契约在写入之前落地。** 事件表是续传、多读者、
回放、审计的地址来源。先写旧信封再补参数，已经入库的行就是残的。
`tool_call` 带上原始参数、终局与审批走同一信封，都是 T1 的第一刀，不再单列后期任务。

**D5 — seq 属于一场 run，从 1 计。** 约束是 `PRIMARY KEY (run_id, seq)`。
分配发生在该 run 的一条 `BEGIN IMMEDIATE` 事务里：`SELECT MAX(seq)` 后
`INSERT` 同一整数，列上的 seq 与 payload 里的 seq 是这一个值。
`UNIQUE` 冲突时整笔事务重试一次。SQLite 用纯 Go 驱动 `modernc.org/sqlite`，
Web 进程 `MaxOpenConns = 1`。不同会话的 run 会并发写同一库，事务负责把
`MAX+1` 串起来。

同一场 run 的信封只由该 run 的 goroutine 写入，包括审批请求、审批回执和终局。
审批 HTTP 只把决定送进现有通道。停止 HTTP 只取消上下文，不写行。

**D6 — 库只挂在 Web 入口。** REPL 不打开这个库，磁盘失败不影响终端一轮。
两套入口共用的是 `adkrun.Event` 的结构（`tool_call` 带参数）。
Web 侧写失败则这一轮快速失败，不先把事件推给已经连着的读者。

**D7 — 档案行开跑时插入，终局时与信封同一事务更新。** 开跑、调用模型之前：

1. 插入档案行：`status = running`，`is_completed = 0`，记下本轮用户消息；
2. 写入 `kind = start` 的信封（payload 带 run、seq、用户消息文本）。

跑完时，终局信封与档案更新在同一个事务里：

| 结局 | 终局 kind | 档案 | 下一轮模型 |
|---|---|---|---|
| 成功（`err == nil`） | `complete` | `status=completed`，`is_completed=1`，写入本轮产出 | 读到用户消息与产出 |
| 运行失败 | `error` | `status=completed`，`is_completed=1`，不写产出 | 不读这轮 |
| 用户停止 | `stop` | `status=completed`，`is_completed=1`，不写产出 | 不读这轮 |
| 进程死在终局之前 | 无终局信封 | 启动对账改为 `status=failed`，`is_completed` 仍为 0 | 不读这轮 |

一场 run 只有一条终局信封。`status=completed` 表示「我们知道它怎么结束的」，
成功、失败、停止都算。`failed` 只留给没能写下终局的崩溃。
回看事件读 `event_log`，回看这一轮的用户消息与成功时的产出读档案表。
页面平时读进程内列表；列表没有这场 run 时从 `event_log` 装入。下一轮模型从档案装入带产出的轮次。

**D8 — 停止是进程内取消，日志只作记录。** `POST /api/stop` 核对
`liveRun.runID` 与请求体里的 run 一致后，取消这个 run 的上下文，返回 200。
不一致或会话没有 live run，返回 404。不存在独立的 watcher，也不经事件总线。

T3 之后，这个上下文只会被这条停止请求取消，因此 run 退出时：

- `ctx.Err() != nil` → 终局 `stop`；
- 其它 `err != nil` → 终局 `error`；
- `err == nil` → 终局 `complete`。

取消会打断挂起的审批（ask 仍选择 `ctx.Done()`）。读者从终局信封知道停止，
取消本身不靠轮询日志。

**D9 — 第二块屏幕用会话查询拿到 run。** `GET /api/session?id=<sessionID>`
返回 `{"sessionId","liveRun"}`。有在跑的轮次时 `liveRun` 是 run id，否则为空字符串。
未知会话也返回空 `liveRun`，并且不因此创建会话。`sessionId` 为空则 400。
第一块屏幕从 `start` 信封得知 run id；后打开的页面先查这个接口，再接
`stream?run&from=0`（或自己的 `lastSeq`）。

**结构决策（学 WeKnora 的五个，拆哪条都散架）**：

1. 每次 run 一条追加日志，读者带 `from` 按 seq 顺序读；
2. run 的 ctx 与请求脱钩（T3；此前 T1/T2 仍用请求 ctx）；
3. SSE handler 轮询日志拉增量，不订阅内存扇出（T2 起，T1 仍可沿用请求内通道，但必须先落盘）；
4. 会话级 live-run 标记做 409（检查先于写 SSE 响应头）；
5. stop 在进程内取消 run；终局信封由 run 的退出路径写入，读者据此收流。

**不抄**：Redis/TTL 后端（我们 SQLite 全落盘）、多租户、steer、资源 URL 改写、
前端工程规模。内存日志那一版不存在，所以也没有「只进不出」的 map 要清理。

**已知后果，本次不处理**：断线之后，挂起的审批继续占着 live run，直到另一台设备
回执或用户停止。没有空闲超时。这是多读者要的行为（第 9 节 N4 已拍板）。

## 4. 续传机制链路（一条 message 的一生）

源头消息（模型要调工具，Eino 交回的原始形态）：

```json
{ "role": "assistant",
  "tool_calls": [{"id": "call_1",
    "function": {"name": "bash", "arguments": "{\"command\":\"date\"}"}}] }
```

**环 1 — 分类（`event.go` 的 `eventFrom`）。** 把 `schema.Message` 翻成
`Event{Kind:"tool_call"}`。载荷改为 `ToolCalls:[{Name, Args}]`。
`Args` 是 `Function.Arguments` 的原始字符串，产生侧不截断、不改写。
工具调用 id 仍不进入契约：现有 `tool_result` 也只按工具名配对，本次不扩大范围。

**环 2 — 封信封。** Web sink 是 adkrun 与外界的缝。信封：

```json
{ "run": "r_7f3a", "seq": 17, "ts": "2026-09-28T15:00:15Z", "kind": "tool_call",
  "agent": "yk-tako", "role": "assistant",
  "toolCalls": [{"name": "bash", "args": "{\"command\":\"date\"}"}] }
```

`run` 是这场 run 的 id，写入库之后全局唯一。用不可预测的短 id。
不复用 `session.turn`：那个计数在进程重启后归零，会和库里的旧行撞车。
`seq` 是这场 run 内从 1 起的单调序号（D5）。`ts` 是 RFC3339 UTC。

**环 3 — 落地。** `INSERT INTO event_log(run_id, seq, payload)`。
先写 SQLite，写成功之后才谈推送。写失败则 run 快速失败（D6）。
库文件由 `webui.Config.DBPath` 指定；命令行在 `-web` 之外增加 `-web-db`，
默认相对工作目录的 `yktako-webui.db`。测试用临时文件。

```sql
CREATE TABLE event_log (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  payload TEXT    NOT NULL,
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE run_archive (
  run_id        TEXT    PRIMARY KEY,
  session_id    TEXT    NOT NULL,
  status        TEXT    NOT NULL, -- running | completed | failed
  is_completed  INTEGER NOT NULL, -- 1 当且仅当 status = completed
  user_message  TEXT    NOT NULL,
  produced_json TEXT,             -- 仅成功时写入，内容是本轮产出的消息
  created_at    TEXT    NOT NULL,
  finished_at   TEXT
);
```

启动时，在对外监听之前执行：

```sql
UPDATE run_archive SET status = 'failed'
 WHERE status = 'running';
```

已落盘的 `event_log` 不动。

**环 4 — 轮询读取。** 一个读者持有自己的游标 `from`：

```sql
SELECT seq, payload FROM event_log
 WHERE run_id = ? AND seq > ?
 ORDER BY seq
 LIMIT 50
```

一次醒来先把批次读完：本批满 50 行就立刻再查，不足 50 行才睡眠 100ms。
命中行的 payload 原样 `writeSSE`。游标推进到本批最大 seq。
慢读者只是自己落后。N 个读者是 N 个 `(run, from)`，各查各的。

**环 5 — 客户端续传。** 客户端每帧更新 `lastSeq`。断线、刷新或换设备：

`GET /api/chat/stream?run=r_7f3a&from=17`

服务端返回 `seq > 17` 的全部行。客户端若看到相邻两帧的 seq 相差大于 1
（例如 17 之后直接是 19），视为写入或库有洞：正常的 `MAX+1` 不会跳号。
处理是从最后连续的 seq 再请求一次，并在界面上标出来。这不是第二条恢复协议。

**环 6 — 收尾。** 终局与档案更新同一事务（D7）。读者见到
`complete` / `error` / `stop` 就收流。此后查这一轮的结论看档案表，
看过程看事件表。

```
Server（进程 ctx）
 └─ Session(id)                         ← 活过单连接
     ├─ runner / agent                  ← 下一轮从档案装入已完成的消息
     ├─ liveRun: {runID, cancel}        ← 替 session.active；T3 起
     └─ run goroutine
           开跑：INSERT 档案行 + start 信封（在调用模型之前）
           事件：信封 → event_log → 读者可见
           退出：唯一一条终局信封 + 档案更新，同一事务

POST /api/chat
  已有 liveRun → 409，此时还没有 text/event-stream 响应头
  没有         → 插入档案、写 start、跑起来；本连接从 from=0 轮询
GET /api/session?id=... → {sessionId, liveRun}    ← 第二块屏幕靠它
GET /api/chat/stream?run&from=N → 续传 / 旁观
POST /api/stop {session, run}
  liveRun 一致 → cancel()，200；终局 stop 由 run 退出路径写下
  否则         → 404
启动对账 → status=running 的档案行改为 failed；事件保持可回放
```

## 5. 事件信封契约（对《10》第 4.2 节的修订）

T1 同步改 `event.go`、REPL sink、`sse.go`、两侧测试。Web 静态页在 T1 改为读取
`toolCalls`。停止按钮改走 `POST /api/stop` 属于 T3：T3 之前它仍 abort 当前请求。

1. **地址**：`run` + `seq`。读取、去重、续传都靠这一对。SSE 载荷带上它们。
2. **`tool_call` 带参数**：`ToolNames []string` 改为 `ToolCalls []ToolCall`，
   `ToolCall` 为 `{Name, Args string}`。REPL 打印前用现有 `repl.Clip`，上限 120，
   与 `tool_result` 正文同一条规则。Web 前端折叠显示，产生侧不裁。
3. **同一信封、同一种写入路径**，用 `kind` 区分：
   - `start`：模型调用之前，文本为本轮用户消息；
   - `message` / `tool_call` / `tool_result` / `board`：原呈现四类；
   - `approval`：待批请求（id、tool、args、message），落盘后才挂起；
   - `approval_result`：回执（id、decision），run goroutine 收到决定后、继续执行前落盘；
   - `complete` / `error` / `stop`：终局，一场 run 只有一条。
4. REPL 格式锁定测试按新的参数截断规则更新。未知 kind 在终端仍不打印。
   REPL 不写 `start` / 终局 / seq：那些是 Web 信封上的字段，`adkrun.Event` 只增加
   工具参数，不增加序号。序号在 Web 落盘时分配（D5）。

## 6. 行为变化（对《10》第 4.4 节冻结条目的正式修订）

下表是 T3 完成之后的语义。T1 只保证「写进去了」；断线仍会取消 run，终局为 `error`。
T2 能把已经落盘的行按游标读回来，包括这场已经结束的 run。T3 才让断线不再取消 run。

| 原语义（冻结） | 新语义（T3 起） |
|---|---|
| 断线即这一轮作废 | 断线只结束这个读者；run 继续。重连带最后收到的 seq |
| 停止 = abort fetch | 停止 = `POST /api/stop` 取消 run 上下文；退出路径写一条 `stop` |
| 一个会话一个观众 | 一个会话 N 个读者，各自一份 `(run, from)` |
| 409 靠 `session.active` 布尔 | 409 靠 `liveRun`（带 runID，检查先于 SSE 响应头） |
| 档案在内存、completed 无处标 | 开跑插入档案行；终局与 `is_completed` 同一事务 |

流侧的「结束」是那条终局信封。档案侧的「结束」是 `is_completed = 1`。
两者在同一个事务里出现，崩溃窗口里不会只看到一半。

## 7. 任务拆分

v2 把契约修订单列为最后一刀。v3 把它并进 T1，避免先写下残信封。

- **T1 契约与落盘**：`ToolCalls` 带原始参数；REPL 打印截断与格式锁定测试；
  Web 信封种类按第 5 节补齐；静态页改读 `toolCalls`。
  `event_log` + `run_archive`；`modernc.org/sqlite`，单连接；
  `-web-db` 默认 `yktako-webui.db`。开跑插入档案行并写 `start`（在模型调用之前）；
  事件先落盘；终局与 `is_completed` 同一事务；启动对账把残留的 `running` 标 `failed`。
  此阶段 run 仍使用请求 ctx，断线仍取消 run。REPL 不打开数据库。
- **T2 按游标读**：轮询器替换请求私有通道；`GET /api/chat/stream?run&from=`；
  客户端记 `lastSeq`；`from` 的语义按 D1；跳号只做客户端断言。
  此阶段重连读到的是已经落盘的行（run 可能已因断线结束）。
- **T3 连接解耦**：run 的 ctx 改为会话级，只受 `POST /api/stop` 取消；
  `liveRun` 替换布尔闸；409 先于 SSE 响应头；`GET /api/session` 返回 `liveRun`；
  断线只结束读者；停止按钮改为 POST，不再 abort。
  挂起的审批在断线后继续等，直到回执或停止。
- **T4 验收**：第 8 节的机器用例，外加仿 T2 密度另起草的人工用例。
- **T5 文档回填**：第 10 节清单。
- **后续再排**：鉴权、绑 `0.0.0.0`、多用户、redis/nats、WS、steer、保留策略、
  审批空闲超时。已完成轮次装回模型上下文、助手正文逐字流出，已在 2026-09-29 的读法里做了。

## 8. 验收（T4）

机器验收（全部 `-race`）：

1. 事件表：同一 run 的 seq 从 1 单调且唯一；并发的是不同 run 同时写同一库。
   列 seq 与 payload 内 seq 相等。写失败不产生读者可见的事件。
   `from` 的边界：空表、`from=0`、`from` 等于最后一帧、`from` 超过最大 seq。
2. 断线不杀 run（T3）：httptest 起流、中途断开、断言 run 跑完且事件表完整，终局一条 `complete`。
3. 续传：断开后以最后收到的 seq 作为 `from` 重连，无重复、无缺失。
4. 重启回放：终局写完之前进程消失（测试里直接关库再新开），已落盘事件仍可按 `from` 读，
   档案行 `status=failed` 且 `is_completed=0`。成功结束的行是 `completed` / `1`，
   且与终局信封同在。用户停止与运行失败也是 `completed` / `1`，终局分别是 `stop` 与 `error`，
   模型历史不增加。
5. 双读者：两个连接各带自己的 `(run, from)`，序列一致。后一个连接通过
   `GET /api/session` 拿到 run id，不靠第一个连接转发。
6. stop：`POST /api/stop` 在 liveRun 匹配时返回 200 并取消 run；日志里出现一条 `stop`，
   没有第二条终局。不匹配返回 404。REPL 回归不受影响（不打开 `-web-db`）。
7. 409：liveRun 存在时 `POST /api/chat` 返回 409，响应头没有 `text/event-stream`。
8. 契约：`tool_call` 的 `args` 等于模型给出的原始参数字符串。
   REPL 格式锁定测试按 `repl.Clip(..., 120)` 全绿。`start` 的落盘早于模型被调用。

人工验收（浏览器）：断网或刷新后从最后一帧接上、停止按钮、两个标签页同看一轮、REPL 回归。

## 9. 未决问题

第 3 节已吃掉的项不再挂在这里：续传游标（D1）、档案写入时点（D7）、
审批入流（第 5 节，请求与回执各一种 kind）。

| # | 问题 | 决定 |
|---|---|---|
| N1 | 事件粒度：语义级，还是另加一种 token delta | 仍是语义级事件。助手正文在同一条消息上逐字变长，不另加 delta 种类 |
| N2 | 旧 run 的 `event_log` 与档案删不删 | 先不设限。有真实体积后再按 run 清理 |

## 10. 文档同步清单（T5 回填）

- 《10》第 4.2 节事件契约表：`tool_call` 载荷、`approval` / `approval_result` / 终局入流、增加 `seq` / `run` 列；
- 《10》第 4.4 节：断线语义改写（本篇第 6 节的表）；
- 《11》T2 行与第 4 节：指向本篇，并按第 7 节的任务编号改台账；
- 《05》第 4 节路线表 #7 / #8 行：状态更新；
- 两份 README 的 Web 分支描述：停止、续传、多读者。
