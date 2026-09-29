# 10 · 最小 Web UI（方案 A 定案）

> 性质：**定案**。新系列第十篇，记 2026-09-26 的一次路线讨论：在「先接 UI」与「按《07》§7
> 顺序先做上下文管理」之间，选了**方案 A——最小 Web UI 先行**。本篇按推理链记
> 「为什么这么定、范围冻在哪、事件契约怎么落」，实现计划另起《11》执行台账（T1 已完成）。
> 关联：能力路线表在《05》§4（本篇改变了它的顺序）、审批回路缺口在《06》§8、
> 事件产出与呈现现状在《07》§6、`internal/adkrun/event.go` 与 `cmd/yktako/sink.go`、
> `//go:embed` 机制在《09》§10。

## 0. 问题

摆在桌上的问题只有一个：**现在要不要先把 UI 接上？**

表面上这不是个路线问题——路线表（《05》§4）里根本没有「UI」这一行。但正因为没有，
才要先回答它到底算哪一层、会挤掉谁。

## 1. 前提：当前代码离「能接 UI」的真实距离

先核对事实，不凭感觉。当前版本（工作区未提交状态，入口 `cmd/yktako/main.go`）
有四个**结构性缺口**，不是「加个前端就能连」：

| 缺口 | 现状 | 位置 |
|---|---|---|
| 无 HTTP 层 | `internal/` 下没有任何 server 包，唯一 I/O 是 REPL 的 stdin/stdout | `cmd/yktako/main.go` 全文 |
| 无流式 | `EnableStreaming: false`，整轮跑完才一次性拿到结果 | `main.go:77` |
| 会话在进程内存 | `history` 是 main 的局部变量，重启即丢；checkpoint 只在 interrupt 时用 | `main.go:85` |
| 审批阻塞在 stdin | 审批回调 = `console.ReadLine("批准执行？[y/N]")`，同步阻塞 | `main.go:109-128` |

外加一个隐性的第五缺口：**规划时 `adkrun` 直接往 stdout 打印**（规划时在 `internal/adkrun/turn.go`
的打印分支与看板投影；T1 重构后分类在 `internal/adkrun/event.go` 的 `eventFrom`，
格式化在 `cmd/yktako/sink.go` 的 `newREPLSink`）——事件的「产生」和「呈现」焊死在一起，
UI 需要的「同一份事件换个消费者」当前做不到。

**推导**：这五个缺口里，流式对应路线 **#7（断点恢复与流式）**，HTTP 层 + 审批结构 +
会话存储对应 **#8（多入口统一路由）**。也就是说，「先接 UI」在路线表上不是插队到
一个无关的新层，而是**把 #7 和 #8 的主体提前做**。

**边界检查**：#4（上下文管理）的入场理由是「看板让任务变长，上下文膨胀随后就到」
（《07》§7）——这个理由**不依赖 UI 做不做**，REPL 里任务照样变长。所以 UI 提前与 #4
是**正交通道**，不是互相阻塞：#4 被顺延，但没有被取消，也没有失去入场条件。

## 2. 三个候选方案与取舍

### 2.1 方案清单

| 方案 | 内容 | 一句话评价 |
|---|---|---|
| **A. 最小 Web UI 先行** | 事件 sink 重构 → HTTP + SSE 单页 UI + HITL 审批结构，之后回 #4 | 选它 |
| B. 按原路线先做 #4 上下文管理 | UI 缓后，纪律成本最低 | 放弃，理由见 2.2 |
| C. 直接上 Wails 桌面壳 | WeKnora `cmd/desktop` 同款（Go 单二进制 + webview） | 放弃时机，理由见 2.3 |

### 2.2 为什么不选 B（先 #4）

B 的唯一优势是**不改动已定案的顺序**（《07》§7 白纸黑字「下一层 = 上下文管理」）。

但生长式复刻的每层节奏是「实现 → **对比** → 反思」（《05》§3），对比的对象决定了
反思的质量。当前对比对象是控制台日志：看板投影、审批卡片、越界访问提醒全部埋在
终端输出里。而《06》§7.1 已经观察到：ask 态的真正收益**不在拦截率，在于让越界行为
变得可见**——UI 是这个收益的放大器，把「控制台里划过的一行字」变成「用户盯着的卡片」。

**结论**：B 解决的是「任务变长」，A 解决的是「用户能看见和操作的东西太少」。
两个都是真问题，但 A 还额外把 #7/#8 一起带了，B 只带 #4。选 A。

**反例（主动找的）**：A 会不会让 #4 的问题恶化——UI 让任务更容易跑长，上下文膨胀
来得更快？会。但这是「问题更早暴露」而非「问题被制造」，REPL 下同样存在；
且 #4 的解法（compaction）不因入口形态改变而改变。风险可接受，代价是 #4 顺延一个身位。

### 2.3 为什么不选 C（Wails 先行）

C 的吸引力是贴合「Go 单二进制本地数字员工」的终局形态（《04》§3.2），
WeKnora 的 `cmd/desktop`（wails v2）是现成参照。

但壳是容器，**容器里装的事件流协议还没定**：没有 sink 抽象、没有 SSE 契约、
审批还堵在 stdin。先套壳等于让 webview 去适配一个不存在的接口，返工概率高。
正确顺序是 A 先把协议定下来，壳随时能套（A → C，不是跳过 A 直接 C）。

### 2.4 定案

**选方案 A**，分四步走：

0. **提交基线**：当前工作区那批「单入口收敛」改动（删 7 个旧入口、doc 新系列）
   尚未 commit（`git log` 最新停在 09-24 的 `28fcdeb`）。先落基线，
   否则 UI 改动会和「删旧入口」混进同一个 diff，日后无法回看接 UI 前的状态。
1. **事件 sink 重构**（无悔改动）：把 `adkrun` 的打印拆成「产生事件 / 消费事件」，
   REPL 退化成第一个 sink。
2. **HTTP + SSE 最小 UI**：单页面（聊天流 + 审批卡片 + 看板面板），
   `go:embed` 内嵌（机制见《09》§10），补齐 HITL 审批结构。
3. **回到 #4 上下文管理**：UI 让长任务更常见，#4 的入场条件反而更充足。

## 3. 从 WeKnora 取什么、不取什么

按《05》§5 翻译原则——参考实现只回答「要什么」。翻了 `ref-agent/WeKnora` 之后的清单：

### 3.1 取（三样）

1. **SSE 事件契约**：`internal/handler/session/qa.go:658` `setupSSEStream`——
   一个 run 的生命周期怎么切成事件发给前端。特别注意它**在写 `text/event-stream`
   header 之前先做冲突检查**（409/503，`qa.go` 注释原文 "SetLiveRun happens before
   SSE headers"）——流一旦开了就没法回头报错，这个顺序是踩坑换来的，照学。
2. **传输层与运行解耦**：`internal/stream/` 有 `memory_manager` 与 `redis_manager`
   两套实现，同一份事件流可本地消费也可跨实例。印证「先做 sink 抽象」的方向：
   REPL 和 HTTP 各是一个 sink，将来 IM 入口再加一个。
3. **HITL 审批的 API 形态**：`mcp_tool_approval` 的 `PUT /mcp-services/:id/tool-approvals/:tool`
   加上会话内审批——正好补《06》§8 留下的缺口（「审批是 REPL 的 y/n……
   等多入口那一层再对齐」）。UI 提前 = 这个缺口提前补。

### 3.2 不取（一样，但要写清楚为什么）

**WeKnora 的前端工程规模不取**：Vue3 + Vite + TDesign + i18n + mermaid + 虚拟滚动，
`frontend/src` 下 views/api/stores/hooks 一整套，后端 `internal/handler/` 一百多个文件。
照抄它违反《05》§5「功能翻译，不是设计移植」——它的规模服务的是知识库/RBAC/多租户，
我们当前形态是单用户本地 agent，**一个页面够用**。前端第一期就是
`go:embed` 内嵌的一个静态页，不引构建链。

（顺带标记下一站：WeKnora `steer.go` 的运行中插话——队列深度 10、`inject`/`after`
两种投递时机——是 UI 之后顺路的好东西，但**明确不在本期范围**。）

## 4. 事件契约草案（方案 A 的核心技术决策）

### 4.1 事件 sink 抽象

`adkrun.Run` 规划时把事件直接 `fmt.Printf` 出去（原 `turn.go` 的打印分支，当时 157-193 行）。重构目标：

- `adkrun` 只**产生**结构化事件（channel 或回调，实现时定）；
- 消费方各取所需：REPL sink 接现在的打印逻辑（分类留 `adkrun.eventFrom`，
  格式化搬到 `cmd/yktako/sink.go` 的 `newREPLSink`），HTTP sink 把事件转 SSE；
- 判定标准：**做完后 `adkrun` 不再自己决定输出到哪**，输出格式归 sink 所有。

这一步不引入任何 UI，单独可验收（现有 `turn_test.go` 回归 + sink 单测），
所以标为「无悔改动」——即使方案 A 后续叫停，这个重构对 #7/#8 依然有效。

### 4.2 事件类型（第一期：整消息级）

按 `consume`（`turn.go:117-159`；分类在 `event.go:52` 的 `eventFrom`）现有的事件分类推导，
第一期事件类型：

| 事件 | 载荷 | 来源 |
|---|---|---|
| `start` | run、seq、用户消息文本 | Web 在调用模型之前写入事件库。不在 `adkrun.Event` 上 |
| `message` | 文本、agent 名（子代理带前缀）、role | `MessageOutput` 完整消息 |
| `tool_call` | `toolCalls: [{name, args}]`，args 是模型给出的原始参数字符串 | `msg.ToolCalls`（`event.go` 的 `eventFrom`） |
| `tool_result` | 工具名 + 内容（截断归消费方） | `Role=Tool` 消息 |
| `board` | 看板回执原文 | `taskboard.IsBoardTool` 命中 |
| `approval` | id、tool、args、message | Web 的 ask，落盘后才挂起。不经 adkrun sink |
| `approval_result` | id、decision | ask 收到决定后、继续执行前落盘 |
| `complete` / `error` / `stop` | 终局。一场 run 只有一条 | run 退出路径与档案 `is_completed` 同一事务 |

`seq` 与 `run` 是 Web 信封上的地址，在写入 SQLite 时分配。`adkrun.Event` 只有呈现字段，REPL 不写库、也不分配序号。

**明确分期**：第一期**不做 token 级 delta**。理由：当前 `consume` 消费的是
`AgentEvent` 里 `GetMessage()` 拿到的完整消息（`turn.go:131-146`），
delta 需要开 `EnableStreaming` 并改消费方式，牵动 `Runner` 配置与历史攒取逻辑。
SSE 推整消息级事件**已经是流式**（逐事件推送，不是跑完才回），
用户可见的最小收益（过程实时可见、审批卡片实时弹出）第一期就拿得到；
token delta 列为第二期，跟着 #7 的 streaming 一起做。

### 4.3 审批回路：从「阻塞 stdin」到「挂起等另一个请求」

这是方案 A 最核心的技术难点，先说清结构再动手：

- 现状：`Run` 的循环里 `ask(info)` 同步阻塞在 stdin（`turn.go:103`），
  整个 run 挂在 REPL 这一个 goroutine 上。
- Web 下：`ask` 不能再等 stdin，但 run 也不能退出。解法是**通道挂起**——
  run 在自己的 goroutine 里跑，`ask` 内部阻塞在一个 channel 上；
  前端收到 `approval` 事件后 POST 决定，HTTP handler 把 `"approve"` / `"reject"`
  塞进 channel，`ask` 返回，`Run` 走既有的 `ResumeWithParams`（`turn.go:109`）继续。
- **不变量**：resume 的值仍然是字符串 `"approve"` / `"reject"`
  （`guard/middleware.go:41-42`），`ApprovalInfo` 结构不动——
  只换「谁来回答」，不换「回答什么」。REPL 的 stdin 回答方式保留为 sink 之一，
  两条入口共用同一套 interrupt/resume 语义。

### 4.4 会话与持久化

模型下一轮读档案里已经写上产出的轮次（用户消息加上 `produced_json`）。失败、停止、没写上终局的轮次没有产出，不进入下一轮。checkpoint 仍是
`adkrun` 的内存 Store，进程崩溃后不能把在途轮次续跑。

事件和档案另有 SQLite（`-web-db`，默认 `yktako-webui.db`）。开跑时插入档案行并写下
`start`；终局信封（`complete` / `error` / `stop`）与 `is_completed=1` 在同一事务。
没写上终局的行，下次启动对账标 `failed`，已落盘的事件仍可按 seq 回放。

页面读进程内的事件列表，按序号取副本，读走不删。列表里还没有这场 run 时，从事件表装回来再读。进程重启后内存是空的，第一次读取完成这次装载。
助手正文逐字进这份列表，同一序号随着字变长再推一次；写完才进事件表。没写完的半截不落库，模型出错则读者看到失败后从内存删掉。
已结束的对话按会话从 `event_log` 懒加载：滚到顶才取更早的一页，游标用 `(created_at, run_id)`。Chrome 只保留 `yktakoSession`，不保存助手正文。
连接只是读者：断线不取消 run，重连带上最后收到的 seq（`GET /api/chat/stream?run&from=`）。
停止是 `POST /api/stop`，由 run 自己的退出路径写一条 `stop`。同一会话同时只跑一轮，
冲突在写 SSE 响应头之前返回 409。后来打开的页面用 `GET /api/session?id=` 拿当前 run。
定案与任务拆分见 `docs/superpowers/specs/2026-09-28-webui-stream-hub-design.md`。

## 5. 明确不做什么（范围冻结）

写下来是为了防止实现时被 WeKnora 带跑：

- ❌ 多用户 / 鉴权 / RBAC——单用户，服务只绑 `127.0.0.1`；
- ❌ token 级流式 delta（见 4.2，第二期）；
- ❌ 跨实例、Redis。同一进程内的断线续传与事件回放已由流式枢纽落地（第 4.4 节）；
- ❌ steer 运行中插话；
- ❌ 前端构建链（Vue/Vite/TDesign）——一个 embed 静态页；
- ❌ Wails 桌面壳（协议定型后再套，A → C）；
- ❌ 顺手做 #4 的任何内容（compaction 等）——本期只回填 #7/#8 的主体。

## 6. 测试计划

仿《02》的验收密度，按四步各设关口：

1. **基线**：`git status` 干净，`go build ./... && go vet ./... && go test ./...` 全绿。
2. **sink 重构**：现有 `turn_test.go` 回归；新增 sink 单测——
   假模型事件序列 → 断言 sink 收到的事件类型与顺序；REPL sink 打印格式不破。
3. **HTTP + SSE**：`httptest` 起服务，假模型走真实图，断言 SSE 事件序列
   （message → tool_call → board → 收尾）；冲突检查在写 SSE header 之前返回 409。
4. **审批回路**：端到端——假模型触发 interrupt → SSE 收到 `approval` 事件 →
   POST approve → run 恢复跑完；POST reject 对称验证；REPL 入口的 y/n 回归不受影响。

全部 `-race` 通过（run goroutine 与 HTTP handler 跨 goroutine 传审批，竞态是真实风险）。

## 7. 对既有文档的影响

- 《05》§4 能力路线表：#7、#8 标「进行中（Web UI 提前拉动）」，#4 注「顺延一位」；
- 《07》§7「下一层 = 上下文管理」：改为「下一层 = 最小 Web UI（《10》），
  之后回上下文管理」——原判断不删，注明顺延原因；
- 《01》§9 索引补一行本篇；
- 本篇只记定案与契约，**逐任务实现计划仿《02》另起一篇**（当时留待 sink 重构的实现细节
  定型后再写，避免计划先行于已知事实）。
- **更新（2026-09-26）**：计划已另起——执行台账《11-最小Web-UI实施计划》与细粒度计划
  `docs/superpowers/plans/2026-09-26-web-ui-sink.md`；T1（事件 sink 重构）已完成，见《11》§1/§6。

## 8. 风险与反例（主动找的）

| 风险 | 检验 | 处置 |
|---|---|---|
| UI 范围膨胀成 WeKnora 全家桶 | §5 范围冻结是硬约束，每加一个功能先对照 §5 | 超出冻结范围的需求记到后续篇，不进本期 |
| #4 顺延导致长任务先翻车 | 反例成立的前提是「UI 下任务显著变长」——第一期用户量为零，任务长度分布不变 | 可接受；#4 入场条件（长任务出现）不受影响 |
| sink 重构牵动面大、回归风险 | §6 第 2 关单独验收，不动语义只搬输出 | 搬完跑全量测试，输出格式锁住 |
| 审批跨 goroutine 竞态 | 通道是唯一传递路径，无共享可变状态 | `-race` 是合入门槛 |

## 9. 参考来源

- WeKnora（`ref-agent/WeKnora`）：`internal/handler/session/qa.go`（SSE 契约与
  header 顺序）、`internal/stream/`（传输解耦）、`mcp_tool_approval`（审批 API 形态）、
  `cmd/desktop`（Wails 壳，后置参照）；
- 本仓库：`cmd/yktako/main.go`（四个缺口的出处）、`internal/adkrun/event.go`（事件分类契约）、
  `internal/adkrun/turn.go`（审批回路现状）、`internal/guard/middleware.go`（ApprovalInfo 契约）；
- Eino：`Runner` 的 streaming 配置与 `ResumeWithParams`（《09》§12 拆过 resume 中段）。

