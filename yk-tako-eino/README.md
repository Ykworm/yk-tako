# yk-tako-eino

yk-tako 的主线实现（Go + Eino），**唯一入口 `cmd/yktako`**。模型按厂商目录接，OpenAI 兼容接口走 `eino-ext/openai`，Anthropic 走 `eino-ext/claude`。`.env` 就在本目录。

工作原则：复刻 Octop 是**功能翻译**——参考实现只回答"要什么"，设计与实现一律按 Eino 的架构思想和最佳实践来（compose 管确定性、ADK 管自主性、prebuilt 优先、middleware 只做横切）；Eino 没有的类库参考 WeKnora。细则见 `../doc/05-Octop架构剖析与生长式复刻.md`。

## 架构

```
cmd/yktako（唯一入口）
  └─ ChatModelAgent（裸 ReAct，MaxIterations 20）
       ├─ Instruction = config.SystemPrompt() + taskboard.Instruction（记账约定）
       ├─ Handlers = [taskboard] + guard 三件套（命令守卫 → 文件守卫 → PII）
       ├─ Tools = 时间 / 计算 / bash / 文件读写（internal/tools）
       └─ Runner(CheckPointStore = adkrun 内存实现) + REPL 审批回路
```

**任务看板**（`internal/taskboard`，定案见 `../doc/01-任务看板与单入口.md`，实现记录见 `../doc/07-任务规划与看板.md`）：

- 四个记账工具：`TaskCreate` / `TaskUpdate` / `TaskList` / `TaskGet`，约束写在 tool description（≥3 步才建表、动手前标 in_progress、做完立刻 completed、测试红不准 completed、卡住建 blocker）。
- 存储两层：middleware 自持 Store 是**正本**（跨 REPL 轮次存活）；每次变更后 `adk.AddSessionValue` 写全量**镜像**，interrupt 时随 runContext gob 进 checkpoint——落盘不依赖模型自觉。
- 每个工具输出 = 回执 + 当前看板 + 催更块（有未完成项时）；`ExecuteSequentially: true` 让同批调用串行。
- 不做：Action ↔ Task 双射、漏更新时自动勾行、图拓扑强制顺序（要硬进度需收窄执行模型，不在开放 ReAct 上做语义对齐）。

**工具安全**（`internal/guard`，见 `../doc/06-工具安全层.md`）：三件套语义——命令守卫三模式（block / warn / require_approval，REPL 里 `/mode` 切换）、文件守卫三态（区外默认问人）、API Key 打码。挂载点是 ADK 的 `WrapInvokableToolCall`；审批用 `compose.StatefulInterrupt`，恢复用 `Runner.ResumeWithParams`，resume 值是 `"approve"` / `"reject"`。

## 快速开始

需要 Go 1.25+。在这个目录里：

```bash
go run ./cmd/yktako
```

`.env` 已经放好。改模型时改 `TAKO_MODEL`：`厂商/模型`，只配了一个厂商时可省略前缀；目录外厂商用 `TAKO_PROVIDER_<NAME>_API_KEY` / `_BASE_URL` / `_PROTOCOL`。

可以试：

```text
现在几点                                  # 一步任务：不建看板，直接回答
帮我查一下现在时间，算 1234*5678，再写个总结   # 多步任务：建看板 → 逐步执行 → 逐项勾掉
用 bash 执行 rm -rf /tmp/tako-test         # 命中守卫规则：弹人工审批
/mode block                              # 切换守卫模式
```

单元测试不打模型：

```bash
go test ./...
```

## 注释规范

硬性要求，细则见仓库根目录 [`AGENTS.md`](../AGENTS.md)：

- 每个文件、每个 type / 方法 / 常量组都写中文注释；测试文件与假模型同样适用。
- 凡是实现或嵌入（继承）了接口，必须在类型注释和对应方法注释里写明**哪个包的哪个文件的哪个 interface**，嵌入的基类提供什么、覆盖了哪些钩子也要写（Eino 靠「接口 + 嵌入」组织代码，光看签名认不出钩子归属）。
- 实现接口的类型旁边加编译期断言 `var _ pkg.Interface = (*Type)(nil)`，并用中文注释说明校验对象。
- 行为改动必须同步改注释，禁止留过时注释。

示例可看 `internal/guard/middleware.go`（`adk.ChatModelAgentMiddleware` 的 `WrapInvokableToolCall` / `BeforeModelRewriteState` 钩子）、`internal/taskboard/middleware.go`（同接口的 `BeforeAgent` 钩子）与 `internal/fsbackend/backend.go`（`filesystem.Backend`）。

## 目录

```text
cmd/yktako         唯一入口：裸 ReAct + 任务看板 + L1 三件套
internal/config    厂商目录和模型工厂
internal/tools     时间、计算、bash、文件
internal/guard     命令守卫、文件守卫、PII、ADK middleware
internal/taskboard 任务看板：四个记账工具、Store 正本、session 镜像、middleware
internal/fsbackend 本地文件后端（filesystem.Backend 实现，暂无调用方，保留）
internal/repl      控制台输入 + Clip/Exitf 打印小工具
internal/adkrun    事件打印（含看板投影）、interrupt 审批回路、内存 checkpoint
```
