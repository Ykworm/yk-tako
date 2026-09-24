# yk-tako-eino

yk-tako 的主线实现（Go + Eino）。模型按厂商目录接，OpenAI 兼容接口走 `eino-ext/openai`，Anthropic 走 `eino-ext/claude`。`.env` 就在本目录。

工作原则（doc/03 §5）：复刻 Octop 是**功能翻译**——Octop/harness 源码只回答"要什么"，设计与实现一律按 Eino 的架构思想和最佳实践来（compose 管确定性、ADK 管自主性、prebuilt 优先、middleware 只做横切），不移植 Octop 的设计；Eino 没有的类库参考 WeKnora。

> 历史说明（2026-09-24）：本目录最初是 TypeScript 课（L0–L2）的 Eino 对照实现；
> 当日路线定案只保留 Eino，TS 实现已删除。下表左列的 TS 路径仅作历史对照保留，
> 帮助回忆"同一层能力在 LangGraph 里长什么样"。

| 层 | TypeScript（已删除） | Eino（本目录） | 状态 |
| --- | --- | --- | --- |
| L0 ① | `src/l0/01-raw-loop.ts` | `cmd/l0raw` 手写 for 循环 | ✅ |
| L0 ② | `src/l0/02-langgraph.ts` `createAgent` | `cmd/l0react` `react.NewAgent` | ✅ |
| L0 ③ | `src/l0/03-deepagents.ts` `createDeepAgent` | `cmd/l0deep` `deep.New` | ✅ |
| L1 | `src/l1/01-tool-guard.ts` | `cmd/l1guard` ADK middleware + interrupt | ✅ |
| L2 手写 | `src/l2/01-planning.ts` | `cmd/l2planning` 手写 planning | ✅ |
| L2 官方 | `src/l2/02-builtin.ts` `todoListMiddleware` | `cmd/l2builtin` `deep.New` 自带 `write_todos` | ✅ |
| L2 硬编排 | —（TS 侧没有对应） | `cmd/l2planexec` `Sequential(Gate, Loop(Executor, Replanner))` | ✅ |

## 和 TypeScript 版的对应关系

① 两边都是自己写循环。Eino 只提供 `ChatModel.Generate` 和 `InvokableTool`。

② LangGraph 的 `createAgent` 对应 `flow/agent/react`。图里是 ChatModel 节点和 Tools 节点。这张图的 state 只活在一次 `Generate` 里，跨轮对话仍由 REPL 把历史传回去。checkpoint 和 interrupt 不在这个预构建图的构造参数里。

③ `createDeepAgent` 对应 `adk/prebuilt/deep`。默认带 `write_todos` 和通用子代理。文件工具来自 `Backend`。官方 local backend 会拉 PDF 解析依赖，这里用 `internal/fsbackend` 实现同一个 `filesystem.Backend` 接口，直接读写本机文件。

L1 的三件套语义和 TypeScript 版一致：命令守卫三模式、文件守卫三态（区外默认问人）、API Key 打码。挂载点换成 ADK 的 `WrapInvokableToolCall`。审批用 `compose.StatefulInterrupt`，恢复用 `Runner.ResumeWithParams`。resume 值是 `"approve"` / `"reject"`。

L2 手写版的四个零件和 `src/middleware/planning.ts` 相同：清单、`write_todos` 全量替换、紧凑中文提示词、并行防守，外加半途而废提醒。ChatModelAgent 没有自定义 state 频道，清单放在进程内，`/todos` 读它；checkpoint 不记这份清单。模型节点之后只看最后一条消息有没有 tool call，所以提醒不是追加一条 HumanMessage，而是补一次对模型隐藏的 `planning_nudge` 工具调用，让图走进工具节点再回到模型。`write_todos` 同轮多次、或与其它干活工具同轮混排时不改清单、只回错误（全量替换语义要求清单更新独占一轮，混排会让清单状态和执行结果对不上时序），同轮其它工具照常执行。Eino 执行完工具总会回到模型，错误在同一轮就可见；LangGraph 在这一轮全是 `write_todos` 时会直接出图。

官方对照用 `deep.New` 自带的 `write_todos`。Instruction 留空就是框架那篇长提示词（默认英文），回执是 JSON，没有提醒，也没有 L1 守卫。通用子代理和文件后端都关掉，工具只留时间和计算。

硬编排对照（`cmd/l2planexec`）是第三版，走的正是 `doc/06` 定案的那条路：规划时序从 prompt 自觉升级成图结构。组成是 `Sequential(Gate, Loop(Executor, Replanner))`——Gate 是自写的入口分流闸门（`internal/planexec`，治官方 Planner 的两个实测痛点）：强制 `tool_choice` 二选一，纯知识问答直接 `respond` + Exit 动作终止全流程（`1+1 等于几` 不进循环），需要工具/多步才 `plan`，且 prompt 注入了 Executor 的工具清单（`现在几点` 从 5 步注水幻觉变成点名 `get_current_time` 的 1~2 步计划）；Executor 每轮只执行剩余计划的第一步；Replanner 看完结果决定改计划（`plan`）还是收工（`respond`）。阶段切换由图结构保证，不咨询模型；前两个脚本里"先列计划再干活、干完再更新清单"全靠模型自觉。它没有共享对话：各阶段输入是各自拿 session 现拼的（Gate 决策只看当前请求——喂历史会让 deepseek 模式锁定走 respond，见 doc/06 §7），REPL 历史只保留"用户输入 + 最终答复"。遗留观察：Replanner 可以把计划的最后一步直接并进 `respond` 一次性答复，跳过一轮 Executor——硬编排管时序，不管步数。另外 DeepSeek 的 thinking 模式拒绝强制 `tool_choice`（400），Gate/replanner 用 `config.OpenForForcedToolChoice` 建的模型（对 deepseek 注入 `thinking: disabled`），executor 仍用默认模型。

## 快速开始

需要 Go 1.25+。在这个目录里：

```bash
cd yk-tako-eino
go run ./cmd/l0raw
go run ./cmd/l0react
go run ./cmd/l0deep
go run ./cmd/l1guard
go run ./cmd/l2planning
go run ./cmd/l2builtin
go run ./cmd/l2planexec
```

`.env` 已经放好。改模型时改 `TAKO_MODEL`：`厂商/模型`，只配了一个厂商时可省略前缀；目录外厂商用 `TAKO_PROVIDER_<NAME>_API_KEY` / `_BASE_URL` / `_PROTOCOL`。

L1 可以试：

```text
用 bash 执行 echo hello
用 bash 执行 rm -rf /tmp/l1test
用 read_file 读取 /etc/passwd
用 read_file 读取 .env
/mode block
```

L2 可以试同一句：

```text
现在几点
帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结
/todos
```

`现在几点` 不该列清单。规划那句会先写 3 项，再查时间和计算，最后总结。`/todos` 只在手写版里。
同一句拿去喂 `l2planexec`，能看到硬编排的完整过程：📋 计划 → 逐步执行 → 📋 更新计划 → 🐙 最终答复；再喂 `1+1 等于几`，Gate 直接答复、不进循环。

单元测试不打模型：

```bash
go test ./...
```

## 目录

```text
cmd/l0raw          ① 手写循环
cmd/l0react        ② react.NewAgent
cmd/l0deep         ③ deep.New
cmd/l1guard        L1 三件套 + 人工审批
cmd/l2planning     L2 手写 planning（L1 栈 + /todos）
cmd/l2builtin      L2 官方 write_todos 对照
cmd/l2planexec     L2 planexecute 硬编排（Sequential(Gate, Loop(Executor, Replanner))，Gate 治过度规划）
internal/config    厂商目录和模型工厂（TS 版曾共用同一份目录 JSON）
internal/tools     时间、计算、bash、文件
internal/guard     命令守卫、文件守卫、PII、ADK middleware
internal/planning  手写任务清单、并行防守、半途而废提醒
internal/planexec  planexecute 入口闸门（Gate/Plan/ToolsSummary）+ 事件解析与打印（Classify/Run）
internal/fsbackend DeepAgent 用的本地文件后端
internal/repl      控制台输入 + Clip/Exitf/Indent 打印小工具
internal/adkrun    事件打印、interrupt 审批回路、内存 checkpoint
```
