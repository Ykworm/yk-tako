# 06 · Eino 规划机制调研 与 路线定案（2026-09-24）

> 性质：**路线定案文档**。本文是"废弃 TypeScript 路线、只保留 Eino"的直接依据，
> 推翻了《02-技术路线讨论》第 7 节的"全栈 TypeScript"定案（反转记录见《02》第 8 节）。
>
> 触发问题（L2 遗留）：**"先干活再写清单"怎么防？** 即 `write_todos` 要不要和干活工具同轮互斥、
> "干活工具跑完才能标 completed"该由谁保证。

## 0. 核心结论

Eino 把这个问题拆成了**三套东西**，而不是在 `write_todos` 上加一条同轮互斥。
官方自己把分界写得很硬：**Graph 管确定性，Agent 管自主性，不要用静态编排去假装动态决策。**

| 你要的约束 | Eino 放哪 | 硬不硬 |
|---|---|---|
| 复杂任务先列清单 | DeepAgent `write_todos` / `PlanTask` | 软，prompt + 工具 |
| 同一轮不要写两份全量清单 | 未做成 LangChain 那种 afterModel 并行防守；`PlanTask` 只是写文件互斥 | 半硬，防的是文件竞争 |
| 干活工具跑完再标 completed | **不在 WriteTodos 里做** | — |
| Planner 不能同时当 Executor | `planexecute.New` + Sequential/Loop | **硬**，节点顺序 |
| 子任务上下文别污染主计划 | DeepAgent `TaskTool` 隔离子 Agent | 硬隔离，软调度 |

一句话：Eino 没有在 middleware 里补"`write_todos` 不得与干活工具同轮"；它承认这是 Agent 层的自觉问题。
字节自己要确定性时，走的是 **Plan-Execute 节点图** 和 **Workflow Agents**，
和 Google ADK / Microsoft Workflows 同一层，而不是把 TodoListMiddleware 升级成状态机。

对照之下，LangGraph + deepagents 只有 middleware 一层可做文章，同类约束只能靠 `afterModel`
钩子防守——用软机制模拟硬约束。这就是"Eino 比 LangGraph + deepagent 更好"的具体含义：
**对"确定性"有一等公民的正式答案（编图），而不是在 middleware 层打补丁。**

## 1. DeepAgent 的 `write_todos`：和 LangChain / Octop 同一层

最接近 yk-tako / Octop 的那条路：

```
DeepAgents = ChatModelAgent
           + WriteTodos
           + TaskTool（子 Agent 委派）
           + 可选 FileSystem
           + 预置系统提示词
```

`write_todos` 是挂在 ReAct 循环上的工具：把 `[]TODO{content, activeForm, status}` 写进 session
（`deep_agent_session_key_todos`），提示词教模型"复杂任务先拆、做完再更新"。官方工作流：

1. 模型收用户输入
2. 调 `write_todos` 拆任务
3. 按项执行（自己调工具，或 `task` 派子 Agent）
4. 再调 `write_todos` 更新进度

没有"本轮不许和干活工具一起出"的硬防守。官方甚至写了反例：简单任务每次都写清单会适得其反。
关掉的方式是 `WithoutWriteTodos: true`。

所以：**Eino 的 DeepAgent 和 langchain `TodoListMiddleware` 是同一设计——清单是认知脚手架 + UX 状态，
时序靠模型自觉。** 主 Agent 多了一手：真正耗上下文的执行可以丢给隔离的子 Agent，
主循环只聚合结果再记账。这是在减污染，不是在锁时序。

## 2. `PlanTask`：清单更重，时序仍然靠自觉

v0.8.0 起另有一套更像工单系统的 middleware，注入四个工具：
`TaskCreate` / `TaskGet` / `TaskUpdate` / `TaskList`，带 `blockedBy` 依赖和环检测。
文档给的"典型流程"：建任务 → 设依赖 → `in_progress` → 干活 → `completed` → 循环。

代码里真正硬的只有两件：

- 同一 middleware 实例下工具调用用 mutex 串行（防并发写坏文件）
- 依赖成环就报错；全部 `completed` 后清文件

**没有**"必须先跑完业务工具才能 `TaskUpdate(completed)`"。状态机画在提示词和约定流程里，
执行权还在 ChatModelAgent 的 ReAct 循环。这是加强版 todo，不是编排器。

## 3. 真要确定性：Workflow Agent + Plan-Execute

这才是 Eino 对"先执行再更新计划"的正式答案。

**Workflow Agents**（对标 Google ADK）：`SequentialAgent` / `ParallelAgent` / `LoopAgent`。
顺序由 `SubAgents` 数组和循环条件决定，不咨询 LLM。Excel Agent 官方写法：

- `Sequential`：Planner 只跑一次，然后进入循环
- `Loop`：每一轮先 Executor，再 Replanner

Planner 出结构化计划，Executor **只执行当前计划的第一步**，Replanner 看结果决定改计划还是收工。
阶段切换是图/流水线保证的：Executor 跑完才会进 Replanner，Planner 不会在执行中途自己改边。
这就是"干活节点跑完 → 再进更新清单节点"。

底层 compose 分三档：

| API | 环 | 边的含义 | 适合 |
|---|---|---|---|
| Chain | 无 | 顺序 | 简单管道 |
| Graph | 可有环 | 边既是控制流也是数据流 | ReAct 那种 think→act→observe |
| Workflow | **无环**（`AllPredecessor`） | 控制流和数据流可分开，字段映射 | 业务阶段锁死 |

官方定位：需要精确控制执行顺序时用 `compose` 的 Graph/Workflow；需要模型自己决定下一步时用
`ChatModelAgent`。两者可以嵌：把编好的 Graph 封成 `GraphTool` 给 Agent 调，
或把 Agent 放进 Sequential/Loop 当一个节点。

## 4. 对 yk-tako 的指导意义

如果在 Eino 里复刻 L2 及以后的层级：

- 用 DeepAgent 就接受和 Octop 一样的"自觉"——清单是软约束，别试图把它改硬；
- 要工程保证，**不要加 afterModel 式防守**，直接 `planexecute.New` 或自己
  `Sequential(Planner, Loop(Executor, Replanner))`（已落地：`cmd/l2planexec`，见 §6）；
- L4（子代理）优先用 DeepAgent 的 `TaskTool`（隔离上下文，硬隔离软调度）；
- L6（断点恢复）落点是 compose 的 checkpoint / interrupt，不是 middleware。

## 5. 定案

- 技术栈终版：**Go + Eino**，主线 `yk-tako-eino/`，后续层级全部在这里生长；
- TypeScript 实现（`src/`、`package.json` 等）全部删除；
- `maf/` 保留为冻结存档，不再推进。

## 6. 落地记录：`cmd/l2planexec`（2026-09-24）

§4 的第二条建议当天落地为 `yk-tako-eino/cmd/l2planexec`：`planexecute.NewPlanner`（强制
`tool_choice` 出 `{"steps":[...]}`）+ `NewExecutor`（ChatModelAgent，输入由 session 里
的计划和步骤结果现拼，每轮只跑剩余计划的第一步）+ `NewReplanner`（强制二选一：
`plan` 改计划 / `respond` 收工），`planexecute.New` 编成
`Sequential(Planner, Loop(Executor, Replanner))`。实测（deepseek/deepseek-flash）：

- 复杂句（查时间 → 算乘法 → 写总结）严格走 📋 计划 → 逐步执行 → 📋 更新计划 → 🐙 答复，
  更新计划只含剩余步骤，时序完全由图保证——和手写 planning 的"全靠模型自觉"对照鲜明；
- **Replanner 可以把最后一步并进 `respond`**：第二步算完乘法后，它判断目标已可满足，
  直接带着完整总结收工，跳过了原计划"写总结"那一步 Executor。硬编排管的是时序，
  不管步数；
- **Planner 看不见 Executor 的工具清单**："现在几点"被过度规划成 5 步（它以为没有时钟
  工具，还规划了"向用户说明无法获取时间"），靠 Executor 真调到 `get_current_time` 才
  纠偏。简单问题走 plan-execute 是纯开销，实际产品里前面该有个"要不要规划"的闸门
  （当天已落地，见 §7）；
- **DeepSeek 的 thinking 模式拒绝强制 `tool_choice`**（400: Thinking mode does not
  support this tool_choice）。planner/replanner 改用 `config.OpenForForcedToolChoice`
  建的模型（对 deepseek 注入 `thinking: {"type":"disabled"}`——输出反正只是工具参数
  JSON，思考是纯开销）；executor 仍用默认模型；
- 事件流里 planner/replanner 的"消息"是工具参数 JSON 当文本发出来的，REPL 端按 JSON
  形状分类展示（`internal/planexec.Classify`）；对话历史只保留"用户输入 + 最终答复"，
  各阶段的工作记忆（现拼的 prompt）不进历史。

## 7. 闸门落地：Gate 治过度规划（2026-09-24）

§6 的两个痛点（Planner 看不见工具清单、简单问题白走全流程）用 Eino 的方式治：
**"要不要规划"是判断题交给模型（强制 `tool_choice` 结构化收束），"怎么执行"仍是
编排题交给图**。`planexecute.New` 被拆开，Planner 换成自写的 Gate 代理
（`internal/planexec/gate.go`），组成 `Sequential(Gate, Loop(Executor, Replanner))`：

- Gate 复用 replanner 的 `respond`/`plan` 工具对做强制二选一：纯知识问答直接
  `respond` 并发 `adk.NewExitAction()`——Sequential 见到子代理最后的动作是 Exit
  即终止，Loop 不执行；需要工具/多步才 `plan`，计划写入 session（顶替官方 Planner
  的职责）。官方 `defaultPlan` 未导出，自写 `planexec.Plan` 满足接口即可
  （FirstStep + JSON 序列化，Executor/Replanner 只断言接口）。
- Gate 的 prompt 注入了 Executor 的工具清单（`planexec.ToolsSummary`），计划不再幻觉
  "没有时钟工具"——"现在几点"从 5 步注水变成 1~2 步（直接点名 `get_current_time`）。
- **实测坑：闸门决策不能喂对话历史**。deepseek-flash 见到历史里"user→assistant
  直接答复"的样本会模式锁定：后续所有请求都模仿历史走 respond 并复读旧答复
  （加强 prompt 规则无效）。解法贴合 planexecute 自己的哲学（各阶段 GenModelInput
  现拼工作记忆，无共享对话）：Gate 只看当前请求，完整历史照常写入
  `UserInputSessionKey`，Executor/Replanner 读得到——多轮指代（"乘积再加 100"）
  由下游结合历史正确执行（实测得到 7,006,752）。代价是闸门看不到历史，
  指代类请求必须一律 plan（prompt 规则兜底），respond 只用于无上下文的纯知识问答。
- 工具描述是决策边界的一部分：Gate 复制官方 ToolInfo 改写了 Desc（值类型可直接
  复制），明确"需要实时信息/精确计算必须 plan"。
- 调试开关：`GATE_DEBUG=1` 时闸门把实际工具调用打到 stderr。

实测（deepseek/deepseek-flash）：

| 输入 | 行为 |
|---|---|
| `1+1 等于几` | Gate respond 直接答复，Loop 不执行 ✅ |
| `现在几点` | 2 步计划（点名真实工具）→ 执行 → 答复 ✅（原为 5 步注水） |
| 复杂句（查时间→算乘法→写总结） | 3 步计划 → 逐步执行 → 更新计划 → 答复 ✅ |
| `乘积再加 100`（多轮指代） | plan → 下游结合历史算出 7,006,752 ✅ |
