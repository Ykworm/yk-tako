# maf/ —— L2 任务规划的 Microsoft Agent Framework 对照

> 状态：**冻结存档**（2026-09-24 起不再推进）。主线已定为 Go/Eino（`yk-tako-eino/`），
> 原 TypeScript 实现已删除；下文中"TS 版"均指这份已删除的 LangGraph.js 实现，
> 机制对照描述仍然有效。
>
> 2026-09-26 限定解冻：追加 L1 最小审批回路（`src/Guard.cs` + `Program.cs` 的
> approval 管线接线），用于与 Go/Eino 的 interrupt/resume 做机制对照（见 `maf/doc/10-MAF审批回路与Eino对照.md`）。
> 主线仍在 Go/Eino；本目录除该回路外不再演进。

用微软 [MAF](https://github.com/microsoft/agent-framework)（.NET，`Microsoft.Agents.AI` 1.22）
复刻 L2 任务规划层（与 `yk-tako-eino/cmd/l2planning` 同源）。只覆盖 L2（L1 守卫不在内），
复刻对象与手写版相同：
deepagents / langchain 的 TodoListMiddleware 四零件 + 本仓库外扩的"半途而废提醒"。

## 运行

需要 .NET 8 SDK。不用另外配环境：程序启动时向上找 `yk-tako-eino` 目录，
直接复用其中的 `.env`（`TAKO_MODEL`、厂商 Key）和 `internal/config/provider-catalog.json`，
和 Go 主线用同一份模型配置。

```bash
cd maf
dotnet run
```

斜杠命令：`/todos` 看清单，`quit` 退出。试这两句感受差异：

```text
现在几点                                  → 简单任务，不列清单
帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结   → 先列清单再逐项执行
```

## 机制映射：LangGraph → MAF

MAF 没有"图"和"状态频道"，它的定制点是三层中间件（chat / function / agent run）。
四零件 + 外扩的落点：

| LangGraph（TS 版） | MAF（本目录） | 落点 |
| --- | --- | --- |
| `stateSchema` 的 `todos` 频道 | `TodoStore` 实例（工具与各层中间件共享） | `src/Todo.cs` |
| `write_todos` 工具返回 `Command({update})` | `write_todos` 工具直接写 `TodoStore`，返回紧凑回执文本 | `src/Planning.cs` |
| `wrapModelCall` 拼 systemMessage | **chat 中间件**：`ChatOptions.Instructions` 注入 | `PlanningMiddleware.ChatMiddleware` |
| `afterModel` 并行防守 | **chat 中间件**数调用（发现）+ **function 中间件**按 CallId 拦截（执行） | `ChatMiddleware` + `FunctionMiddleware` |
| `afterModel` + `jumpTo:"model"` 半途而废提醒 | **agent run 中间件**：`RunAsync` 返回后追加提醒消息，再调一次 `innerAgent.RunAsync` | `NudgeMiddleware` |
| `checkpointer: MemorySaver` | `AgentSession`（进程内存会话） | `Program.cs` |
| L1 工具守卫的审批回路（Go/Eino：interrupt + resume） | 规则引擎 `GuardEngine` 判「自动批 / 不批」+ MAF `ToolApprovalAgent` 与 FICC 的 approval 管线承担暂停/回执/执行 | `src/Guard.cs` + `Program.cs` |

几处语义对应值得说明：

- **"清单是状态，不是消息"依然成立**。`/todos` 命令绕过对话直接读 `TodoStore`，
  对齐 TS 版 `agent.getState().values.todos`。差别是 TS 版的状态挂在图上、
  checkpoint 会记住它；MAF 版是普通进程内对象，会话结束即失——
  要持久化得自己序列化 `TodoStore`（L6 断点恢复时再处理）。
- **"一轮"的边界天然清晰**。TS 版判定"本轮用户发言之后写过清单"要从后往前扫消息；
  MAF 版由 run 中间件在每次 `RunAsync` 进入时 `BeginTurn()` 复位标记，边界就是 API 边界。
- **"模型想收工"不用显式判定**。MAF 的函数调用循环在 agent 内部收口，
  `RunAsync` 能正常返回就保证了没有待执行的 tool_calls——这正是 TS 版
  检查 `jumpTo` 前提（无 tool_calls）想要的不变量，这里由框架免费提供，
  所以 `jumpTo:"model"` 的风险（跳过待执行工具留下烂摊子）在这个结构上不存在。
- **半途而废提醒的四条约束全部保留**：只催 in_progress（纯 pending 旧账不催）、
  只催本轮写过的清单（`WroteTodosThisTurn`）、每轮最多催一次（`NudgedThisTurn`，保证终止）。

## 与 TS 版的有意偏离

1. **并行防守拆成两半**。LangGraph 的 `afterModel` 一处完成"发现 + 替换"；
   MAF 里发现发生在 chat 层（数一轮响应里的 `FunctionCallContent`），
   拦截发生在 function 层（按 `CallId` 查防守名单，命中就回错误文本）。
   语义与官方版一致：并行的一组 `write_todos` **一个都不执行**，全部收到错误。
   附带一个行为差异：MAF 的 `FunctionInvokingChatClient` 会把错误结果直接喂回模型、
   **同一轮内**重试合并调用；官方 LangGraph 版若整轮只有 write_todos 则错误留在历史里、
   下轮用户输入时才被看到。MAF 版提醒更即时，算是免费的小改进。
2. **提示词注入是幂等的**。同一份 `ChatOptions` 可能在工具循环的多次模型调用间复用，
   注入前先查重，避免提示词被反复追加。
3. **`calculate` 不用 eval**。TS 版用 `Function()` 求值，.NET 没有等价物，
   换成一个 ~60 行的递归下降解析器（数字 + `+ - * / ( ) %` + 一元负号），
   安全边界不变，且整数除法不会丢精度（`7/2 = 3.5`）。
4. **模型配置只接 openai 协议**。anthropic 协议在 MAF 版未接；
   单 Key 多厂商时 `<STEM>_BASE_URL` 的兄弟检查做了简化（DeepSeek 等单厂商场景无影响）。

## 文件

```text
Program.cs          REPL：组装三层中间件 + 审批回路 + /todos /mode 命令
src/Planning.cs     规划中间件三件套（chat / function / agent run）+ write_todos 工具
src/Todo.cs         TodoItem + TodoStore（todos 频道的 MAF 等价物）
src/BasicTools.cs   get_current_time / calculate（递归下降解析器）/ execute（假 shell，审批回路演示载体）
src/Guard.cs        L1 规则引擎（移植自 Go/Eino 的 toolguard.go：10 条危险命令规则 + 三模式）+ 命中格式化
src/ModelConfig.cs  TAKO_MODEL + provider-catalog.json 解析（resolve.ts 的 C# 移植）
src/EnvFile.cs      向上找仓库根、加载 .env
tests/              GuardEngine 规则引擎单测（xunit，不打模型）
```

## 实测记录

| 输入 | 行为 |
| --- | --- |
| `现在几点` | 直接回答，不列清单 ✅ |
| `帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结` | 先列 3 项清单（首项 in_progress）→ 更新 2/3 → 更新 3/3 → 总结；`/todos` 读到全 ✅ |
| `请调用 execute 工具执行命令：echo hi` | 无审批打扰，假 shell 直接回执（2026-09-26 实测）✅ |
| `请调用 execute 工具执行命令：rm -rf /tmp/x` | 命中危险规则 → ⏸️ 人工审批；拒绝→模型收到拒绝理由并继续，批准→假 shell 回执（同上）✅ |
| `/mode warn` 后同上 | 打印 ⚠️ 命中清单后直接回执（同上）✅ |
| `/mode block` 后同上 | 不进人，直接拦截并把拦截文案回给模型（同上）✅ |

> 冒烟注意：`deepseek-flash`（thinking 模型）下 approval 路径会 400
> （FICC 重建 tool-call 消息时丢掉 `reasoning_content`，DeepSeek 要求带 tools 的
> 请求原样回传）。用非 thinking 别名 `TAKO_MODEL=deepseek/deepseek-chat` 可跑通；
> 根因与生态现状见 `maf/doc/10-MAF审批回路与Eino对照.md`。
