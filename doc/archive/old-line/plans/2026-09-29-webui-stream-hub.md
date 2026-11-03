# Web UI 流式枢纽 Implementation Plan

> **For agentic workers:** 按本计划逐任务实现。步骤用 checkbox（`- [x]`）跟踪。
> 定案：`docs/superpowers/specs/2026-09-28-webui-stream-hub-design.md`（v3）。本计划不重新打开已拍板项。

**Goal:** 把 Web UI 从「连接即 run」改成「会话拥有 run、日志承接事件、连接只是读者」：事件先落 SQLite，按 `(run, from)` 续传，断线不取消 run，停止走 `POST /api/stop`。

**Architecture:** `adkrun.Event` 只补工具参数，序号在 Web 落盘时分配。`internal/webui` 持有 SQLite（`event_log` + `run_archive`）。T1 仍用请求上下文跑一轮，但事件先写库，开跑先写 `start`，退出写唯一终局并与 `is_completed` 同一事务。T2 用轮询换掉请求私有通道，并给出 `GET /api/chat/stream`。T3 把 run 的上下文从请求上摘下来，`liveRun` 做 409 与停止。REPL 不打开这个库。

**Tech Stack:** Go 1.25、标准库 `database/sql` + `net/http`、`modernc.org/sqlite`（纯 Go，无 cgo）、eino v0.9.21。上游：设计稿 v3 第 3–8 节。

**执行说明：**

1. 不新建 worktree。提交沿用仓库习惯：中文短句、不加 `feat:` 前缀，一个任务一笔。用户没点名要求提交时，代码先留在工作区。
2. 注释按 `AGENTS.md`：中文，文件头与函数头齐全；行为变了，注释一起改。
3. 范围以设计稿第 1 节为准：不做鉴权、WS、token delta、steer、保留策略、重启后恢复模型上下文、审批空闲超时。
4. 验收命令在 `yk-tako-eino/`：`gofmt -l .` 为空，`go test -race ./...` 全绿。

---

## File Structure

| 文件 | 动作 | 职责 |
|---|---|---|
| `yk-tako-eino/internal/adkrun/event.go` | 修改 | `ToolCall{Name,Args}` 替换 `ToolNames`；`eventFrom` 保留原始参数字符串 |
| `yk-tako-eino/internal/adkrun/event_test.go` | 修改 | 参数断言 |
| `yk-tako-eino/cmd/yktako/sink.go` | 修改 | 终端打印前 `repl.Clip(args, 120)`；无参数时仍只打名字 |
| `yk-tako-eino/cmd/yktako/sink_test.go` | 修改 | 格式锁定：无参数的旧句子保持，有参数与超长另加用例 |
| `yk-tako-eino/internal/webui/logdb.go` | 新建 | 打开库、建表、启动对账、`BeginRun` / `Append` / `Finish` / `ReadAfter` |
| `yk-tako-eino/internal/webui/logdb_test.go` | 新建 | seq、并发 run、`from` 边界、终局事务、对账 |
| `yk-tako-eino/internal/webui/sse.go` | 修改 | 信封带 `run` / `seq` / `ts` / `toolCalls`；映射只填呈现字段，序号留给落盘 |
| `yk-tako-eino/internal/webui/session.go` | 修改 | `active` 换成 `liveRun`；先落盘再送通道；终局与历史规则按设计稿 D7 |
| `yk-tako-eino/internal/webui/server.go` | 修改 | `New` 打开库并对账；T2 轮询与 `stream`；T3 `stop` 与 `GET /api/session` |
| `yk-tako-eino/internal/webui/server_test.go` | 修改 | 序列含 `start` / 终局；参数断言；续传、断线、停止、409 |
| `yk-tako-eino/internal/webui/static/index.html` | 修改 | `toolCalls` 展示并折叠参数；T2 记 `lastSeq`；T3 停止改 POST |
| `yk-tako-eino/cmd/yktako/main.go` | 修改 | `-web-db`，默认 `yktako-webui.db`；只在 `-web` 时打开 |
| `.gitignore` | 修改 | 忽略 `yktako-webui.db` 及其 `-wal` / `-shm` |
| `doc/10`、`doc/11`、`doc/05`、两份 README | 修改 | 任务 5，设计稿第 10 节 |

不改：`internal/repl` 的 `Clip` 实现、`internal/guard` 的 resume 值、`internal/taskboard`、REPL 的审批问答。`adkrun.Event` 不加 seq。

---

## 任务与设计稿的对应

| 本计划 | 设计稿 | 做完时用户能看见的行为 |
|---|---|---|
| 任务 1 | T1 契约 + 落盘 | 刷新仍会取消这一轮，但库里已有 `start`、过程与一条 `error` 终局；`tool_call` 带原始参数 |
| 任务 2 | T2 按游标读 | 断线后用最后一帧的 seq 重连，行不重不漏；run 仍可能已被断线取消 |
| 任务 3 | T3 连接解耦 | 断线不取消 run；停止按钮 POST；第二标签页用 `GET /api/session` 跟上 |
| 任务 4 | T4 验收里尚未被前三任务盖住的缺口 | 机器验收 8 条有归宿 |
| 任务 5 | T5 文档 | 《10》《11》《05》与 README 跟新语义一致 |

---

## 任务 1：契约与落盘（设计稿 T1）

**Files：** 上表里除「任务 2/3 才碰的路由与前端续传」之外的代码。

### 契约

- [x] **Step 1: `ToolNames` 改为 `ToolCalls`**

`adkrun.ToolCall` 两个字段：`Name`、`Args`。`eventFrom` 在 `len(msg.ToolCalls) > 0` 时逐条拷贝 `Function.Name` 与 `Function.Arguments`，不截断、不解析 JSON。调用 id 不进入事件。

`event_test.go` 里带参数的用例要断言 `Args` 与构造消息时的字符串相同。`consume` 的序列断言改读 `ToolCalls`。

- [x] **Step 2: REPL 格式**

一条调用的终端片段：

- `Args` 为空：只写名字；
- 否则：`名字 + 空格 + repl.Clip(Args, 120)`。

多个调用仍用 `", "` 连接，整行模板不动：`  → 🔧 %s调用: %s\n`。无参数的现有锁定句保持逐字符不变。另加一条带参数、一条参数超过 120 个 rune 的用例。

### 库

- [x] **Step 3: `logdb.go`**

驱动 `modernc.org/sqlite`。`sql.Open` 的 DSN 带 `_txlock=immediate` 与 `busy_timeout`。`MaxOpenConns = 1`。空 `DBPath` 用内存库，只给测试；生产路径由 `-web-db` 传入。

表结构用设计稿第 4 节环 3 的 DDL。`Open` 之后 `CREATE TABLE IF NOT EXISTS`。`Reconcile` 把 `status='running'` 改成 `failed`，`is_completed` 保持 0，事件行不动。

写入全走 `LogDB` 上的互斥锁加一条事务，`UNIQUE` 冲突整笔重试一次：

- `BeginRun(runID, sessionID, userMessage)`：插入档案行（`running` / `is_completed=0`）并写入 `kind=start` 的信封，`seq=1`，`text` 为用户消息。同一事务。
- `Append(ev)`：`ev.Run` 必须指向仍为 `running` 的行；`seq` 由 `MAX(seq)+1` 分配；列与 payload 里的 `seq` 是同一个整数。`ts` 空则填 RFC3339 UTC。
- `Finish(runID, kind, text, producedJSON)`：再分配一帧终局，并把档案更新为 `completed` / `is_completed=1`。`kind` 只允许 `complete`、`error`、`stop`。只有 `complete` 写入 `produced_json`。`WHERE status='running'` 影响行数不是 1 则回滚。一场 run 因此只有一条终局。
- `ReadAfter(runID, from, limit)`：`seq > from ORDER BY seq LIMIT limit`。`from<=0` 视为从头。超过最大 seq 返回空切片，不是错误。未知 run 同样是空切片。

`run_id` 用 `crypto/rand` 的 4 字节，格式 `r_` 加 8 位十六进制。不用 `session.turn`。

- [x] **Step 4: `logdb_test.go` 先写后实现，覆盖设计稿第 8 节第 1、4 条里属于库的部分**

1. 同一 run：`start` 的 seq 是 1，其后单调、无洞；列 seq 与 payload 内 seq 相等。
2. 多个 run 并发 `Append`（`-race`）：各 run 自己的序号空间不串。
3. `ReadAfter`：空表、`from=0`、`from` 等于最后一帧、`from` 大于最大 seq。
4. `Finish(complete)` 后档案为 `completed` / `1`，且终局帧与更新同时可见（中途杀进程的窗口用「事务回滚后两边都不在」来测：构造第二次 `Finish` 必须失败，库里仍只有一条终局）。
5. `Finish(error)` 与 `Finish(stop)` 也是 `completed` / `1`，`produced_json` 为空。
6. 未 `Finish` 就 `Reconcile`：`failed` / `0`，已写入的事件仍能按 `from` 读出。

### 接到 Web（此阶段仍用请求 ctx）

- [x] **Step 5: 信封与会话**

`sseEvent` 增加 `Run`、`Seq`、`Ts`，`ToolNames` 改为 `ToolCalls`。`approval_result` 复用 `sseApproval`，只填 `id` 与 `decision`。

`session.active` 改为 `liveRun *liveRun`（本任务只放 `id`）。`beginTurn` 在已有 `liveRun` 时失败，供 409 使用。

`handleChat` 顺序：

1. 校验请求体；
2. `beginTurn` 失败则 409，此时还没有 `text/event-stream`；
3. `BeginRun`。失败则清掉 `liveRun`（不追加历史），500，同样还没有 SSE 响应头；
4. 写 SSE 响应头，起 goroutine：先把已经落盘的 `start` 送进通道，再 `runTurn(r.Context(), ...)`；
5. sink 与 ask 里每条呈现事件、每条 `approval`：先 `Append`（用进程 ctx，不用请求 ctx），成功才送进通道。写失败则取消本次 run 并让 `runTurn` 返回该错误，不把没落盘的帧送出去；
6. ask 收到决定后、返回给 `adkrun` 之前，`Append` 一帧 `approval_result`；
7. goroutine 收尾调用 `Finish`：`err == nil` 为 `complete` 并把产出 JSON 写入档案，同时 `finishTurn` 追加历史；任何 `err != nil`（含请求取消）为 `error`，不追加历史。终局帧落盘后再送进通道。`Finish` 失败则只清 `liveRun`，不另造一帧未落盘的 error。

模型调用必须发生在 `BeginRun` 提交之后。用临时库文件做一条测试：假模型在 `Generate` 里打开同一文件，断言已有 `kind=start` 且 `text` 等于本轮用户消息。

现有 SSE 测试改为期望：`start`、过程事件、`complete`。审批用例在 `approval` 与后续 `tool_result` 之间多一帧 `approval_result`，流以 `complete` 结束。`tool_call` 的 `args` 等于剧本里的原始参数字符串。

- [x] **Step 6: 静态页只改呈现**

`renderEvent` 的 `tool_call` 读 `ev.toolCalls`。每个调用：名字用 chip，参数非空时用现有 `.out` 折叠。`start`、`complete`、`approval_result` 本任务不另画一行（用户消息本地已经画过；批准/拒绝的说明仍由 POST 成功路径写）。停止按钮此任务仍是 `abort()`。

- [x] **Step 7: 入口与忽略规则**

`-web-db` 默认 `yktako-webui.db`，仅 `-web` 非空时传给 `webui.New`。`New` 返回 `(*Server, error)`，打开失败或对账失败则返回错误。`.gitignore` 加上库文件与 WAL 伴随文件。

- [x] **Step 8: 跑通**

```bash
cd yk-tako-eino
gofmt -l .
go test -race ./internal/adkrun ./internal/webui ./cmd/yktako
```

预期：无 gofmt 差异，包测试通过。

---

## 任务 2：按游标读（设计稿 T2）

**Files：** `server.go`、`server_test.go`、`static/index.html`。

- [x] **Step 1: 轮询替换请求私有通道**

`POST /api/chat` 仍负责 `beginTurn` + `BeginRun` + 起 run goroutine。goroutine 只写库，不再持有 `chan sseEvent`。HTTP 处理函数从 `from=0` 轮询 `ReadAfter`：一批满 `limit`（50）就立刻再查，不足才睡眠 100ms。写出失败（客户端断开）只结束这个读者。此任务 run 仍绑 `r.Context()`，所以断开仍会取消 run，终局仍是 `error`。见到终局 kind 后读者正常收流。

run goroutine 与轮询之间不需要内存订阅。`start` 已在轮询开始前提交。

- [x] **Step 2: `GET /api/chat/stream?run=&from=`**

`from` 缺省与 `0` 都是从头（`seq > 0`）。run 在档案里不存在则 404。存在则按同一轮询写 SSE，直到终局或客户端断开。不在这里创建会话，也不取消 run。

- [x] **Step 3: 前端游标**

每帧记下 `ev.seq` 与 `start` 里的 `run`。非 `AbortError` 的读中断：若已有 run id，再请求一次 `GET /api/chat/stream?run&from=lastSeq`。相邻 seq 相差大于 1 时在日志里插一行说明，并从最后连续的 seq 再请求一次。用户点停止仍走 abort，不触发这次续传。

- [x] **Step 4: 测试**

`httptest`：读到至少一帧后断开请求体，再 `GET .../stream?run=&from=<最后 seq>`，拼起来的 seq 严格递增、无重复、无空洞，且最后一帧是终局。`from` 大于最大 seq 时响应体没有 `data:` 行且连接在见到「没有更多且已终局」后结束（终局已在 `from` 之前，立即结束）。

```bash
cd yk-tako-eino && go test -race ./internal/webui
```

---

## 任务 3：连接解耦（设计稿 T3）

**Files：** `session.go`、`server.go`、`server_test.go`、`static/index.html`。

- [x] **Step 1: run 上下文脱离请求**

`liveRun` 增加 `cancel context.CancelFunc`。`beginTurn` 成功时 `context.WithCancel(进程 ctx)`，run 使用这个上下文，不使用 `r.Context()`。读者用请求 ctx 轮询；请求结束只离开 `ReadAfter` 循环。`finishTurn` 在 run goroutine 里调用，并 `cancel` 防泄漏，然后把 `liveRun` 置空。

退出分类（进程 ctx 只有停止会取消它）：

- `err == nil` → `complete`，追加历史；
- `errors.Is(err, context.Canceled)` → `stop`，不追加历史；
- 其它错误，或落盘失败 → `error`，不追加历史。

落盘失败若通过 `cancel` 打断 run，必须先认出落盘错误，不能把它写成 `stop`。

- [x] **Step 2: 停止与查询**

`POST /api/stop`，JSON `{sessionId, run}`。`liveRun.id` 一致则 `cancel()` 并 200；否则 404。处理函数不写事件。

`GET /api/session?id=`：`id` 空则 400；否则 200 `{"sessionId","liveRun"}`。没有会话或没有在跑的轮次时 `liveRun` 为 `""`，不创建会话。

409 仍在写 SSE 响应头之前，判定改看 `liveRun != nil`。

- [x] **Step 3: 前端停止**

停止按钮 `POST /api/stop`，带上当前 `run`。不再 `abort()` 当前聊天请求当作停止。abort 只留给「换新会话」这条明确要丢掉本地读者的路径；换会话时若有 live run，同样 POST stop。断线续传逻辑保留。审批卡片在断线后仍可 POST `/api/approval`（服务端 ask 还挂在 run 的 ctx 上）。

- [x] **Step 4: 测试**

1. 起流后取消 HTTP 请求，假模型仍能跑完；档案 `completed`，终局恰好一条 `complete`。
2. `POST /api/stop` 匹配时 200，终局恰好一条 `stop`，历史长度不变。不匹配 404。
3. 两个 `httptest` 连接：第二个先 `GET /api/session` 拿到 run，再 `stream?from=0`，两边 seq 序列一致。
4. liveRun 存在时第二个 `POST /api/chat` 为 409，且 `Content-Type` 不是 `text/event-stream`。
5. REPL 包测试不打开数据库（`cmd/yktako` 的 sink 测试不依赖 `webui`）。

```bash
cd yk-tako-eino && go test -race ./...
```

---

## 任务 4：验收缺口

设计稿第 8 节 8 条应分别落在任务 1–3 的测试里。本任务只补还红的那几条，不新开行为。人工用例（浏览器里断网、停止、双标签、REPL）记在 `docs/superpowers/plans/` 下另起一页，仿 `2026-09-27-t2-人工验收用例.md` 的密度，不在本任务写自动化。

- [x] 对照第 8 节逐条点名现有 `Test*`；缺的补上。

| 设计稿第 8 节 | 测试 |
|---|---|
| 1 seq / from / 终局事务 / 对账 | `TestLogDBSeqMatchesPayload`、`TestLogDBReadAfterBounds`、`TestLogDBFinishOnce`、`TestLogDBReconcileFailed`、`TestLogDBConcurrentRuns` |
| 2 断线不杀 run | `TestDisconnectDoesNotCancelRun` |
| 3 续传不重不漏 | 同上，`from` 为最后一帧 |
| 4 重启对账 | `TestLogDBReconcileFailed`（关库再开由 `Reconcile` 覆盖；不拉起第二个进程） |
| 5 双读者 | `TestTwoReaders` |
| 6 stop | `TestStopOneTerminal`；REPL 不引用 `webui`，`go test ./cmd/yktako` 不打开库 |
| 7 409 | `TestChatConflict409` |
| 8 契约与 start 先于模型 | `TestChatSSESequence`、`TestStartPersistedBeforeModel`、`TestREPLSinkFormat` |

人工用例在 `2026-09-29-webui-stream-hub-manual.md`，留给人手勾。

- [x] `go test -race ./...` 全绿。

---

## 任务 5：文档回填（设计稿 T5）

- [x] 《10》第 4.2 节：`tool_call` 载荷改为名字加原始参数；补 `start` / `approval_result` / 终局；写明 seq 与 run 是 Web 信封，不在 `adkrun.Event` 上。
- [x] 《10》第 4.4 节：按设计稿第 6 节改断线、停止、多读者、409、档案。写明 T1 曾短暂保留「断线取消」，T3 起不再取消。
- [x] 《11》状态表：为流式枢纽加一行，指向本计划与设计稿，避免和当年的 sink T1、HTTP T2 编号混淆。
- [x] 《05》第 4 节路线 #7 / #8：事件可回放已落盘；在途轮次崩溃续跑仍未做。
- [x] 两份 README：`-web` 与 `-web-db`、停止、续传、第二标签页。单用户仍只绑本机。

---

## 自检（写完计划时）

- 设计稿 D1：客户端传最后收到的 seq，服务端 `seq > from`。任务 2 的重连测试按这个配对，不使用 `lastSeq+1`。
- 设计稿 D5：seq 每场 run 从 1 计。任务 1 的并发测试是不同 run，不是同一 run 的三个写入者。
- 设计稿 D7：开跑插入档案行；终局与 `is_completed` 同一事务。崩溃只来自对账，成功 / 失败 / 停止都是 `completed`。
- 设计稿 D8：停止 HTTP 不写行。终局由 run 退出路径写。
- REPL 不 import `webui`，也不打开 `-web-db`。
