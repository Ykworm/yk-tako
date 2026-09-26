# 11 · 最小 Web-UI 实施计划（方案 A 执行台账）

> 性质：**实施计划 + 进度追踪**。定案在《10》§2.4（四步走），本篇是《10》§7 说的
> 「仿《02》另起的实现计划」，但比《02》多一个职责：**它是唯一执行台账**——每推进一步
> 就更新状态列与 §6 进度日志，未完成的写清卡在哪。后来者（包括未来的自己）
> 先读本篇状态表，再决定从哪开工。
> 编号说明：maf 冻结线的《10-MAF审批回路与Eino对照》已移入 `maf/doc/`（2026-09-26，
> P1 关闭），doc/ 下编号归新系列独有；本篇顺延为 11。

## 0. 使用方式（持续更新的规矩）

- **状态记号**：⬜ 未开始 / 🔶 进行中 / ✅ 完成 / ⛔ 阻塞。
- **完成判定**：该任务「产出」列全部满足 + 验收命令全绿 + §3.4 文档同步清单无遗漏
  （AGENTS.md：行为改了必须同步改注释/文档，漏了不算完成）。
- **每次推进**：① 改 §1 状态表该行；② 在 §6 进度日志**倒序追加一行**
  （日期 | 任务号 | commit | 变更摘要）；③ 被阻塞的写进 §5。
- **范围纪律**：范围冻结清单以《10》§5 为准，超出冻结范围的需求不进本表，
  记到《10》之后的后续篇目，防止被 WeKnora 带跑。

## 1. 任务总览（状态表）

| 任务 | 内容 | 产出 | 验收命令 | 状态 |
|---|---|---|---|---|
| **T0** | 提交基线 | 2 笔 commit + .gitignore 补编译产物 | `git status` 干净；`go build/vet/test ./...` 全绿 | ✅ 2026-09-26（`1d0d1eb` + `076d6d3`） |
| **T1** | 事件 sink 重构（《10》§2.4 步 1） | `adkrun` 只产事件；REPL sink 落消费方；sink 单测 | 《10》§6 关口 2：`turn_test` 回归 + sink 事件序列断言 + REPL 打印格式不破 | ✅ 2026-09-26（`31e88a6`+`eaa8e41`、`12e08c0`+`6950f85`、`c0e4c7b`+`d609d3d`+`6ecd330`） |
| **T2** | HTTP + SSE 最小 UI（步 2） | SSE handler、会话 map、审批 channel 化、embed 单页 | 《10》§6 关口 3/4：SSE 事件序列断言、审批端到端、`-race` 全绿 | ⬜（细化时机见 §4） |
| **T3** | 回 #4 上下文管理（步 3） | 能力路线 #4 落地 | 该层自己的计划另起（不在本台账展开） | ⬜ 占位 |

前置关系：T0 → T1 → T2 → T3，严格串行；T1 不依赖 T2 的任何东西（所以 T1 是
「无悔改动」，中途叫停也不浪费，《10》§4.1）。

## 2. T0 提交基线 ✅（已完成，留档）

**为什么先提交**：《10》§2.4 步 0——不先落基线，UI 改动会和「删旧入口」混进同一个 diff，
日后无法回看接 UI 前的状态。

**做了什么**：

1. 全绿校验：`gofmt -l`（空）+ `go build ./...` + `go vet ./...` + `go test ./...`（7 包 ok）。
2. `.gitignore` 补 `yk-tako-eino/yktako`（`go build` 的 Mach-O 产物，原先会混进提交）。
3. 两笔提交：
   - `1d0d1eb` 单入口收敛：45 files（删 7 个旧入口 + planexec/planning、新增 taskboard 7 文件、
     README×2、AGENTS.md）；
   - `076d6d3` doc 新系列 01–10 + 删旧 01–06 + doc/blog（17 files，+4187/−1029）。
4. 提交前确认：stash 为空、无丢失风险；`maf/` 工作区干净。

**顺带发现（不是本任务的事，但记下）**：HEAD 基线（`63e5efe`）之前已落了 9 笔 MAF 限定解冻
实验提交（`f4543b0`→`63e5efe`），其中包含 `10-MAF审批回路与Eino对照.md`——它带来了
编号冲突，见 §5。

## 3. T1 事件 sink 重构 ✅（2026-09-26 完成）

**目标**（《10》§4.1）：`adkrun` 只**产生**结构化事件，呈现归 sink；判定标准——
重构后 `adkrun` 不再自己决定输出到哪，REPL 退化成第一个 sink。

### 3.1 侦察结论（已核实的事实，实现时直接用，不必重查）

1. **`adkrun.Run` 调用方只有 2 处**：`cmd/yktako/main.go:106`、`internal/adkrun/turn_test.go:61`。
   taskboard 集成测试直接用 `runner.Run`（`integration_test.go:178/215/244`），**不受签名影响**。
   → 加 `sink` 参数只改这 2 处。
2. **REPL sink 不能进 `internal/repl`**：该包头注释承诺「与 Eino 无关」（`repl.go:1`），
   而 sink 实现要 import `adkrun`（进而拉入 Eino），会破坏承诺。→ 落点候选：
   `cmd/yktako`（消费方，最贴合「呈现归消费方」）——**已定：`cmd/yktako`（P2 关闭，
   理由记在 `sink.go` 注释）**。
3. **Sink 用函数类型**：包内已有先例 `type Ask func(info guard.ApprovalInfo) (string, error)`
   （`turn.go:66`），单方法无状态，函数类型足够，不必定义 interface（也就不需要编译期断言）。
4. **eino 类型事实**（`go doc` 核实，v0.9.21）：
   - `adk.MessageVariant = TypedMessageVariant[*schema.Message]`，关键字段 `Role` / `ToolName`
     （`Role==schema.Tool` 且 `ToolName` 非空 = 工具回执）；
   - `schema.ToolCall.Function` 是 `FunctionCall{Name, Arguments}`；
   - `schema.Message` 自带 `ToolName` 字段，构工具消息用 `schema.ToolMessage(content, id, schema.WithToolName(name))`。
5. **重构前分类逻辑的位置**（拆分依据）：`printMessage`（`turn.go:157-193`）实际 3 个分支——
   ① `ToolCalls` → 调用；② `Role==Tool` → 回执（`IsBoardTool` 再分看板）；③ `Content` → 正文。
   「子代理前缀」（`agentName != root`）只是修饰，不是独立分支。3 支映射《10》§4.2 的
   `message / tool_call / tool_result / board` 四类（看板是回执的子类）。
   **§4.2 另外两类不在 `printMessage`**：`approval` 来自 interrupt（`turn.go:142-151`）、
   `error` 来自 `event.Err`（`turn.go:129-131`）——产出点归属见 T1.1 的口径。
6. **测试素材可复用**：`taskboard/integration_test.go` 的 `scriptModel` + `toolCallMsg` 模式
   能构造「调工具 → 回执 → 收尾」的真实事件序列；`turn_test.go` 的 `fakeAgent` 走的是
   `Run` 入口，直接可用。


### 3.2 实现子步骤（未开始的打 ⬜，做一步勾一步）

> 细粒度执行计划（TDD 步骤 + 完整代码 + 提交切分）在
> `docs/superpowers/plans/2026-09-26-web-ui-sink.md`；本节是概要台账，两边状态同步。
>
> 行号说明：§3.1/§3.2 的行号与 `printMessage` 是 T1 动手前的侦察/计划快照，`printMessage`
> 已删除；实际落点：分类 `internal/adkrun/event.go`（`eventFrom`）、格式化
> `cmd/yktako/sink.go`（`newREPLSink`）。当前行号以代码为准，完成情况见 §3.4。

- ✅ **T1.1** 定义事件类型与 Sink：新文件（候选 `internal/adkrun/event.go`）——
  `Event{Kind, Agent, Text, ToolNames...}` + `Kind` 常量 + `type Sink func(Event)`，全部中文注释。
  **口径（本步先定死，防 T2 返工）**：T1 只落 4 类呈现事件 `message / tool_call /
  tool_result / board`；`approval` **不经 Sink**——由消费方的 ask 实现产出（REPL=现有
  审批问答，HTTP=T2.3 推 SSE），`error` 维持 `Run` 返回给调用方。理由：T1 要求零行为
  变化，而这两类今天就不经过 `printMessage`；此边界须写进 `Event` 的注释。
- ✅ **T1.2** `consume` 改产事件：`turn.go:118-154` 里 `printMessage` 调用点改为
  「分类构 `Event` → `sink(event)`」；**分类函数留在 `adkrun`**（含 `IsBoardTool` 判定，
  它属于「产生事件」而非「呈现」；HTTP sink 不能再做一遍）；**history 攒取与
  interrupt 收集两段逻辑一行不动**（它们不属于呈现，动了就是范围膨胀）。
- ✅ **T1.3** REPL sink 落点 + 打印格式化搬迁：搬的是 `printMessage` 的**格式化部分**
  （emoji、缩进、`repl.Clip`），输出格式逐字符不变；分类已在 T1.2 留在 `adkrun`，不搬。
  搬完 `adkrun` 应可删 `repl` import（现在仅为 `Clip`，`turn.go:13`/`:187`）。决定落点
  并在注释里写清理由（对照 §3.1 第 2 条的候选），P2 随本步关闭。
- ✅ **T1.4** `Run` 签名加 sink 参数，接线 `main.go:106` 与 `turn_test.go:61`
  （传 nil 的语义：不呈现——需在 `Run` 注释里写明 nil 合法）。
- ✅ **T1.5** 测试：① sink 单测——假模型事件序列 → 断言收到的事件 Kind 顺序；
  ② REPL sink 格式锁定测试（打印字节级断言，防格式漂移；捕获方式随 P2 落点定——
  `cmd/yktako` 走 `os.Stdout` 管道、`internal/sink` 注入 `io.Writer`）；③ 既有 `turn_test` 回归。
  全部 `-race`。
- ✅ **T1.6** 文档同步（§3.4 清单逐项勾）+ 本台账更新 + commit。

### 3.3 验收（《10》§6 关口 2）

```bash
cd yk-tako-eino && gofmt -l . && go build ./... && go vet ./... && go test ./... -race
```

外加两条人工判定：REPL 跑一次多步任务，终端输出与重构前**肉眼无差异**；
`grep -rn 'fmt.Printf' internal/adkrun/` 为空（呈现彻底出包）。

### 3.4 文档同步清单（T1 的完成条件之一，漏一项不算完成）

| 文件:位置 | 现状 | 要改成 | 状态 |
|---|---|---|---|
| `yk-tako-eino/README.md:73` | 「internal/adkrun 事件打印（含看板投影）」 | 事件产出归 `adkrun`、打印归 sink（补 sink 落点）；:15 树图顺带复查（现只提 CheckPointStore，预计不改） | ✅ 已改 |
| `README.md`（根） | 能力段未直接提打印 | T1.6 时 grep「打印/事件」复查一遍 | ✅ 已复查（无涉及，不改） |
| `doc/01:85` | 「`printMessage` 认四个账本工具名」 | 改述为「事件分类认账本工具名，投影由 sink 打印」 | ✅ 已改 |
| `doc/10` §4 全部 `turn.go` 锚点（:28、:120、:123、:132、:137-140、:144、:154、:159、:169…） | 引 `printMessage` 与 `turn.go` 行号 | 逐处复查、刷新为 sink 后的行号/文件（consume 改动 + printMessage 删除会让多数锚点漂移，不能只改枚举的几处） | ✅ 已逐处刷新（含枚举外的 :7、:221） |
| `doc/10:140` | `approval` 行的来源只写 interrupt | 补一句「产出点在消费方 ask，不经 sink」（T1.1 口径） | ✅ 已改 |
| `maf/doc/10-MAF审批回路与Eino对照`（原 doc/；:83、:161、:162） | 引 `turn.go:98-110` 等行号 | **不改**——P1 已移入 `maf/doc/` 作冻结存档（同 `doc/02`） | ✅ 确认不改 |
| `doc/09` §5/§11 约 15 处 `turn.go:NN` | 其中 `:157-160`（前缀）、`:134`（筛历史）会挪位 | 机制篇按《01》§9 约定**必须**跟当前代码刷新 | ✅ 已逐处刷新（前缀改指 `sink.go`） |
| `doc/07:160`（§6 落点表） | 「流式投影（`adkrun` 认看板工具，原样打印 📋 看板）\| `internal/adkrun/turn.go`」 | 改为「分类归 `adkrun`（`IsBoardTool`）、打印归 sink」 | ✅ 已改 |
| `internal/adkrun/turn.go:1-3` 包头 | 「事件消费（打印过程…）」 | 改为「事件产出（呈现归 sink）」 | ✅ 已改（T1.1：包文档移入 `event.go`） |
| `doc/02:515、:812、:836` | 引 printMessage 行号 | **不改**——历史计划记录当时状态（《01》§9:160「被删掉的实现不抹掉记录」） | ✅ 确认不改 |

## 4. T2 HTTP + SSE 最小 UI（⬜，细化时机有约定）

《10》§7 已声明：逐任务计划**等 sink 重构定型后再写**，避免计划先行于已知事实。
所以本节只立骨架，T1 完成后展开成 §3 那样的子步骤：

- ⬜ T2.1 会话存储：进程内 `map[sessionID][]*schema.Message`（《10》§4.4 边界）；
- ⬜ T2.2 SSE handler：事件转 `text/event-stream`；**冲突检查先于写 header**
  （WeKnora `qa.go:658` 的 409/503 顺序，见《10》§3.1）；
- ⬜ T2.3 审批回路 channel 化：`ask` 从阻塞 stdin 改为挂起等 POST（《10》§4.3），
  resume 值 `"approve"` / `"reject"` 不变；
- ⬜ T2.4 embed 单页：聊天流 + 审批卡片 + 看板面板，`go:embed`（《09》§10）；
- ⬜ T2.5 测试：《10》§6 关口 3/4（SSE 序列、审批端到端、`-race`）。

## 5. 待定问题（需要决策，不属于任何任务）

| # | 问题 | 事实 | 建议 | 状态 |
|---|---|---|---|---|
| P1 | **doc 编号冲突**：两个 `10-` | `10-MAF审批回路与Eino对照.md`（maf 冻结线）与 `10-最小Web-UI.md`（新系列）并存 | 移入 `maf/doc/` 冻结存档，新系列编号归位；《01》§9 加说明 | ✅ 2026-09-26 已执行（已随 `e1aafd9` 提交） |
| P2 | T1.3 sink 具体落点 | 候选：`cmd/yktako`（贴「呈现归消费方」）vs 新建 `internal/sink`（若 REPL 与 HTTP 要共享打印逻辑） | 已定：`cmd/yktako`——HTTP sink 输出 SSE JSON，与终端文本没有可共享的格式化，不必新开包；理由记在 `sink.go` 注释（T1.5 捕获方式随之定：注入 `io.Writer` + `bytes.Buffer`） | ✅ 2026-09-26（`12e08c0`） |

## 6. 进度日志（倒序追加，格式：日期 | 任务 | commit | 摘要）

- 2026-09-26 | T1 | `cff708c` | T1 完成：事件 sink 重构落地——adkrun 只产事件，REPL sink 在 cmd/yktako；README/doc/01/doc/07/doc/09/doc/10 锚点刷新；计划偏差补记入执行计划文件
- 2026-09-26 | T1 | —（验收） | 关口 2 通过：全量 `-race` 绿、`fmt.Printf` 出 adkrun 归零；人工跑 REPL 多步任务（查时间→计算→总结）：`→ 🔧`×6、`← 🔧`×2、`🐙`×1、看板 📋 投影正常，格式与重构前一致
- 2026-09-26 | T1 | `c0e4c7b`+`d609d3d`+`6ecd330` | 驱动层接线：Run/consume 加 sink、删 printMessage、main/turn_test 接线；consume 序列载荷测试 + interrupt 同帧测试（审查加固）
- 2026-09-26 | T1 | `12e08c0`+`6950f85` | REPL sink（cmd/yktako）+ 格式锁定 11 用例；P2 落点定在 cmd/yktako
- 2026-09-26 | T1 | `31e88a6`+`eaa8e41` | 事件契约：Event/Sink/eventFrom + 分类 8 用例；包文档移入 event.go
- 2026-09-26 | 计划 | 工作区（未提交） | P1 关闭：10-MAF审批回路与Eino对照 移入 maf/doc/ 冻结存档，doc/ 编号唯一；《01》§9 加说明；§3.4 对应行改「不改」；maf/README.md、maf/Program.cs 的 `doc/10` 引用同步修正
- 2026-09-26 | 计划 | 本文档 | 审阅修订：T1 口径收窄为 4 类呈现事件（approval 走消费方 ask、error 走 Run 返回）；分类留 adkrun、只搬格式化；§3.4 补 doc/07:160、doc/10 全锚点与 doc/10-MAF（随 P1），修正 README:15 现状与《01》§9 出处
- 2026-09-26 | 计划 | 本文档 | 建立执行台账：T0 ✅、T1 侦察结论入档（6 条事实 + 文档同步清单 7 项）、T2 骨架、P1 编号冲突待定
- 2026-09-26 | T0 | `1d0d1eb` + `076d6d3` | 基线两笔提交：代码收敛（45 files）+ doc 新系列（17 files）；.gitignore 补编译产物；提交前后全绿
