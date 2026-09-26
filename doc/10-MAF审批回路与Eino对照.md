# 10 - MAF 审批回路与 Eino interrupt/resume 对照

> 承接 `doc/09-设计了解.md` §12：Eino 的 resume 中段里，写入端（`adkrun` 的 `Targets`）
> 与读取端（guard 的 `GetResumeContext`）如何相遇。本文回答：**MAF 里有没有这个问题**；
> 补上对应功能后，两者的机制差在哪。
> 设计：`docs/superpowers/specs/2026-09-26-maf-tool-approval-design.md`；
> 计划：`docs/superpowers/plans/2026-09-26-maf-tool-approval.md`；
> 实现：`maf/src/Guard.cs`、`maf/Program.cs`。

## 1. 结论先行

1. **改前 MAF 不存在「这个问题」——因为它连产生问题的功能都没有**：`maf/` 是 L2
   任务规划对照，L1 守卫不在内（`maf/README.md`），全目录无 interrupt/resume/checkpoint。
2. **但 MAF 框架（Microsoft.Agents.AI 1.22 + Microsoft.Extensions.AI 10.10）自带
   等价的审批回路**：不是「图中断 + checkpoint 恢复」，而是**消息级请求-回执协议**——
   待批准的工具调用被打包成 `ToolApprovalRequestContent` 交回调用方，调用方用
   `ToolApprovalResponseContent` 回执，FICC 按回执重建调用并执行或拒绝。
3. **本次已按 MAF 最地道的实践补上功能**（规则表移植 + 假 shell + approval 管线），
   五条行为路径真模型实测通过（§4）。
4. **§12 关心的「两端靠什么相遇」在 MAF 同样成立，但介质相反**：Eino 是
   out-of-band（checkpoint 的 ID↔地址映射 + ctx 公告板），MAF 是 in-band
   （RequestId 钥匙 + 随消息走的请求/回执 + 会话状态袋）。详见 §5。
5. **额外发现一个框架缺口**：DeepSeek thinking 模型下，approval 路径的消息重建会丢
   `reasoning_content` → API 400。并非本次实现引入，是 MEAI/FICC 的恢复路径与
   DeepSeek V4 协议要求的冲突；证据与影响见 §6。

## 2. 改前事实：MAF 没有这条回路

- `maf/README.md` 自述「只覆盖 L2（L1 守卫不在内）」。
- grep `Resume|Interrupt|Checkpoint|Approval` 零命中；没有守卫、没有暂停、没有恢复。
- 所以对 §12 的问题，改前答案是：**不是「MAF 用了不同解法」，而是功能缺席**。

## 3. MAF 框架自带的 approval 管线（改后我们接的这条）

机制三段（均已用反射探针核对签名，见 spec §1 F2）：

1. **标记**：工具包一层 `ApprovalRequiredAIFunction`。
2. **暂停**：模型响应里出现需批准的工具调用时，FICC（`FunctionInvokingChatClient`，
   ChatClientAgent 内部的函数调用循环）把 `FunctionCallContent` 换成
   `ToolApprovalRequestContent`，其 `RequestId` 由 tool_call_id 派生
   （框架方法 `ComposeApprovalRequestId`），连同该调用的快照一起返回调用方。
   `ToolApprovalAgent` 负责排队（多请求一次只露一个）、自动批准规则、会话状态。
3. **恢复**：调用方把 `ToolApprovalResponseContent`（`Approved` + `Reason`，
   经 `CreateResponse` 生成）装进一条新 user 消息回传；`ToolApprovalAgent`
   按 RequestId 绑定到「它真正 surfaced 过的请求」（快照比对，防伪造），
   FICC 重建这次函数调用：批准→执行；拒绝→生成失败工具结果。

本仓库的接线（`maf/Program.cs`）：

- 规则引擎 `GuardEngine`（`maf/src/Guard.cs`，从 `toolguard.go` 逐条移植的 10 条
  危险命令规则 + block/warn/require_approval 三模式）只做两件事：扫描参数出命中清单、
  按模式判断该不该拦。
- **自动批准规则**：无阻断命中 → 批（用户不可见）；有阻断命中 → 不批，冒泡给 REPL。
  warn 模式一律批并打印 ⚠️。
- **REPL 回执循环 `ResolveApprovalsAsync`**：block 模式阻断级命中不问直达拒绝；
  未达阻断级静默批准（兜底）；其余问人（`⏸️ 人工审批` + 命中清单 + `[y/N]`）。
- `execute` 假 shell（不真正执行）是规则表唯一能命中的工具。

## 4. 实现记录与实测（2026-09-26，`deepseek/deepseek-chat`）

单测：`dotnet test maf/tests` 10 passed（规则命中/注释行排除/Unicode 空白/三模式/
JsonElement 参数/格式化截断），不打模型。

真模型冒烟（关键输出摘录）：

| 输入 | 实测行为 |
| --- | --- |
| `请调用 execute 工具执行命令：echo hi` | 无审批打扰 → `⚙️ （假 shell）将执行: echo hi` → 模型收到结果并总结 ✅ |
| 同上换 `rm -rf /tmp/x` + 回 `n` | `⏸️ 人工审批` 块（工具/参数/`- [High] 命令包含 rm/del…snippet…remediation`）→ `🚫 已拒绝` → 模型收到拒绝理由并给出替代建议 ✅ |
| 同上 + 回 `y` | `✅ 已批准，继续执行` → 假 shell 回执 → 模型继续 ✅ |
| `/mode warn` 后重复 | `⚠️ [tool_guard] 检测到风险参数（warn 模式，放行）` + 命中清单 → 直接回执 ✅ |
| `/mode block` 后重复 | **不出现审批块** → 模型读到「命令被安全策略拦截，未执行」（拦截文案即拒绝 reason） ✅ |

结论：暂停、问人、回执、批准执行、拒绝回传、三模式分流全部成立。

## 5. 逐点对照（Eino vs MAF）

| 维度 | Eino（Go 主线） | MAF（本目录） |
| --- | --- | --- |
| 中断判定位置 | 工具执行边界（wrapper 内扫参数） | 模型响应之后（FICC 把调用转成请求） |
| 钥匙 | `point.ID` + checkpoint 的 ID↔地址映射 | `RequestId`（tool_call_id 派生） |
| 公告板 | `addrCtx`（ctx，out-of-band，随 checkpoint 恢复） | 消息内携带的 Request/Response 内容（in-band）+ `AgentSession.StateBag` |
| 写入端 | `adkrun.Run` 的 `targets[point.ID]=decision` 循环（`turn.go:98-110`） | REPL `ResolveApprovalsAsync` 回执循环（`Program.cs`） |
| 读取端 | guard wrapper 重入时 `GetResumeContext`（`middleware.go:98-107`） | `ToolApprovalAgent.BindApprovalResponses` 绑定 + FICC 重建执行 |
| 重入机制 | 执行链按地址原样重跑（无 resume 专用分支） | FICC 从回执重建 FunctionCall（框架统一处理） |
| 决策载荷 | `Targets` map（值=approve/reject） | `ToolApprovalResponseContent`（Approved+Reason） |
| 拒绝结果 | 拒绝文案作为工具输出回给模型 | 失败工具结果（含 reason）回给模型 |
| 并行多中断 | 每个 wrapper 独立判断、独立 interrupt | 一个响应任一工具需审批则全部转请求，TAA 排队逐个问 |
| 回执防伪造 | 无（按 ID 取回执即可） | 只认 surfaced 请求 + 快照比对（框架内建） |
| 持久化 | checkpoint 可落盘、可跨进程 | 进程内会话，随进程销毁 |
| 两端所在 | 我们自己的两个文件（跨文件隐性耦合） | 框架内部；我们只写「规则」与「回执循环」 |

## 6. 发现：DeepSeek thinking 模型的 400（框架缺口，非本次实现引入）

**现象**：`TAKO_MODEL=deepseek-flash`（thinking 模式）时，任何走 approval 路径的工具
调用在**执行之后的下一次模型调用**返回 HTTP 400：

```text
The `reasoning_content` in the thinking mode must be passed back to the API.
```

普通工具路径（如 `get_current_time`，不标 approval）同一模型下**不报错**。

**根因链**（前提→推导）：

1. DeepSeek 文档明确：请求带 `tools` 时，assistant 消息的 `reasoning_content` 必须
   在后续所有请求里原样回传，否则 400（thinking_mode 指南的 Tool Calls 一节；
   2026 年生态里 OpenCode/Hermes/OpenClaw 等多个 agent 都踩过同一坑）。
2. 普通路径能过，说明 MEAI 的 OpenAI 客户端在「沿用原 assistant 消息」时保留了
   reasoning（`TextReasoningContent` 类型存在，客户端有 `TryGetReasoningContent`）。
3. approval 路径里，FICC 从「审批请求/回执」**重建** tool-call 消息
   （`ConvertToFunctionCallContentMessages` 等），重建消息丢了原始 reasoning 承载
   → 下一次请求缺 `reasoning_content` → 400。
4. 结论：与本次接线无关，是「FICC 恢复路径 vs DeepSeek thinking 协议」的冲突；
   Eino 主线不受影响（`schema.Message.ReasoningContent` 原生往返）。

**当前对策（本仓库）**：冒烟用非 thinking 别名 `TAKO_MODEL=deepseek/deepseek-chat`；
`maf/README.md` 已标注。生态级修法（供上游参考）：approval 重建时保留 reasoning
（或按 DeepSeek `thinking:{type:disabled}` 显式关闭；OpenCode 的做法是
`interleaved.field=reasoning_content`）。

## 7. 回到 §12：「隐性耦合」在 MAF 的形态

§12 列了三个让人读代码觉得跨度大的隐性耦合：数据耦合非调用耦合、词汇三次换名、
重入隐式。逐条看 MAF：

1. **数据耦合仍在，但缩短了**。钥匙仍是数据（RequestId），写入端与读取端仍无直接
   调用；但两端都在框架内部的一段管线里（FICC/TAA），我们不写这两端，只写「规则」
   （决定批不批）与「回执循环」（搬运人话），跨度从「两个业务文件互不引用」缩成
   「调用方 ↔ 框架的公开协议」。
2. **词汇换名仍有，但外显**。tool_call_id → RequestId 的拼法被框架收口在
   `ComposeApprovalRequestId`，且**调用方被强制走「回执只认 surfaced request」**：
   自己拿 CallId 直接拼一把回执去蒙，会被快照比对拒掉。Eino 里 `point.ID` 的注释
   与真实含义不一致（doc 09 §11）会误导读者；MAF 用绑定把「钥匙契约」显式化成
   安全校验，这一层比 Eino 防呆。
3. **重入仍隐式，但被框架承担**。调用方不需要知道「重入」这件事：回执消息递回去，
   FICC 自己重建并执行。Eino 里重入是「执行链原样重跑」，也隐式；两边对调用方
   都表现为「我递交回执，工具最终跑了」。

一句话：**同构的本质（ID 钥匙 + 共享公告板 + 数据耦合）两边都在；MAF 把公告板
从 ctx 换成消息与会话状态、把钥匙契约从注释换成强制绑定。** 若给 §12 那句话
做一个 MAF 版：中断时 FICC 把「谁要审批」登记成请求随响应出境；恢复时回执按
RequestId 认领，重建调用执行——没有传话员，只有绑定。

## 8. 边界与未覆盖

- 本次只做 L1 的「命令守卫审批回路」最小版；文件系统守卫、PII 打码、checkpoint
  落盘未移植（spec §6 范围外）。
- 并行多中断的排队行为只依框架文档与单请求实测，未专门构造同响应多 execute 场景。
- thinking 模型缺口的修法未在本仓库实现（属 MEAI/MAF 上游），只记录现象与对策。
