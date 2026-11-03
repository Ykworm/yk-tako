# MAF 最小审批回路（L1 tool guard 的 MAF 等价物）设计

> 状态：设计已批准（2026-09-26），待实施。
> 缘起：`doc/09-设计了解.md` §12 讨论的问题——Eino 的 resume 中段里，写入端
> （`adkrun` 的 `Targets`）与读取端（guard 的 `GetResumeContext`）分居两文件、靠
> checkpoint 的 ID↔地址映射与 ctx 公告板在运行时相遇。用户要求核对 MAF 是否有
> 对应功能；MAF 没有，故本设计在 `maf/` 里补上这个功能，并按 MAF 自己的最地道
> 实践实现（不迁就与 Eino 的形状一致），实现后再做机制对比（落在
> `doc/10-MAF审批回路与Eino对照.md`）。

## 1. 背景与前提（事实，可检验）

**F1 — 本仓库 MAF 现状：连功能都没有。** `maf/` 是 L2 任务规划的 MAF 对照，README
明说「只覆盖 L2（L1 守卫不在内）」（`maf/README.md:8`）。全目录 grep
`Resume|Interrupt|Checkpoint|Approval` 零命中：没有守卫、没有中断、没有恢复。
`README.md:49` 自述「要持久化得自己序列化 `TodoStore`（L6 断点恢复时再处理）」。
所以 §12 的问题在 MAF 不是「解法不同」，而是「回路尚未存在」。前提确认：本设计
需要先给 MAF 补上 L1 的最小审批回路。

**F2 — MAF 框架自带这条回路（API 已用反射探针逐一钉死，包
`Microsoft.Agents.AI` 1.22.0 / `Microsoft.Extensions.AI` 10.10.0）：**

- `ApprovalRequiredAIFunction(AIFunction)`：把一个工具标成「执行前需批准」。
- `FunctionInvokingChatClient`（FICC，ChatClientAgent 内部的函数调用循环）：
  响应里含需批准的工具调用时，把 `FunctionCallContent` 换成
  `ToolApprovalRequestContent{RequestId = 由 tool_call_id 拼出}` 返回调用方；
  入站的 `ToolApprovalResponseContent` 一律按 RequestId 绑定到「框架真正 surfaced
  过的请求」，批准的重建函数调用并执行，拒绝的生成失败工具结果。
- `ToolApprovalAgent(innerAgent, options)`：审批中间件，负责排队（多个请求一次只露
  一个）、standing rule（"always approve"）、自动批准规则
  （`AutoApprovalRules: IEnumerable<Func<ToolAutoApprovalRuleContext, ValueTask<bool>>>`，
  上下文含 `FunctionCallContent`、`Session`、`RequestMessages` 等），状态存
  `AgentSession.StateBag`。
- 调用方协议：从 `AgentResponse.Messages` 里取 `ToolApprovalRequestContent`，
  `req.CreateResponse(approved, reason)` 得到 `ToolApprovalResponseContent`，
  装进 `new ChatMessage(ChatRole.User, IList<AIContent>)` 再 `RunAsync`。

**F3 — 框架的审批不是 Eino 式 interrupt/resume。** 没有执行图、没有 checkpoint，
是**消息级请求-回执协议**：跑完的响应把「待批请求」作为内容交给调用方，调用方
下一条消息把「回执」带回来，FICC 重建并执行被批的那次调用。「重入」由 FICC 的
重建机制完成，历史一致性（tool_calls 与结果配对）由框架保证。

**F4 — Eino 侧的对照物（`doc/09` §12 已述）：** 写入端
`yk-tako-eino/internal/adkrun/turn.go:98-110`（`targets[point.ID]=decision`），
读取端 `yk-tako-eino/internal/guard/middleware.go:98-107`（`ownInterrupt` +
`resumeDecision`），钥匙是 `point.ID` + checkpoint 的 ID↔地址映射，公告板是 ctx。

**F5 — 规则表是框架无关的，可移植。** `yk-tako-eino/internal/guard/toolguard.go`
的文件头自述「不含 Eino 依赖，规则本身与框架无关」，`DefaultRules` 10 条危险命令
规则瞄 shell 工具（`bash`/`execute` 的 `command` 参数），带 Severity、Excludes、
三模式（block / warn / require_approval）。Eino 入口默认 `ModeApprove`
（`cmd/yktako/main.go:49`），`/mode` 可切。

## 2. 目标功能语义（与 Eino 对齐的三态）

前提：guard 规则表先在**工具执行前**扫参数，扫出命中清单（Finding）。按模式：

| 模式 | 命中 | 无命中 |
| --- | --- | --- |
| `warn` | 命中任意规则：打印 ⚠️ 告警后放行 | 放行 |
| `block` | 命中达拦截级别（Critical/High）：不执行，拦截文案作为工具结果回给模型（不打扰人）；命中未达级别：放行 | 放行 |
| `require_approval`（默认） | 命中达拦截级别（Critical/High/Medium）：暂停问人，批准→执行、拒绝→拒绝文案作为工具结果回给模型；命中未达级别：放行 | 放行 |

级别映射沿用 Eino `blocking`（`toolguard.go:149-153`）：block 拦 Critical/High，
require_approval 连 Medium 也拦，warn 不拦。

REPL 交互补两条（对齐 `cmd/yktako/main.go:85-103`）：默认 `require_approval`；
`/mode block|warn|require_approval` 切换。审批文案对齐
`cmd/yktako/main.go:107-124`（工具、参数、命中说明、`批准执行？[y/N]`）。

## 3. 设计（已选定：全用 MAF 自带 approval 管线）

### 3.1 为什么是这条路（取舍回顾）

- 方案 B（自写 chat 中间件按参数动态把命中调用转成审批请求）与方案 C（抛异常手写
  中断 + 自建 decision board）都被否：C 在 MAF 没有可恢复执行图的前提下，必须自己
  重建 tool-call/tool-result 消息对才能让重入成立——而这正是 FICC 替你做的事；
  B 要自己处理框架的 all-or-nothing（一个响应里任一工具需审批则全部转请求，
  见 `ApprovalRequirement` 文档）并重造自动回执。两者都是重造轮子。
- 方案 A（本设计）用足框架：静态标记让调用进入审批管线，规则引擎只决定
  「自动批/不批」，暂停、绑定、执行、拒绝回执全部交给框架。与 Eino 的语义差异只
  剩「未命中的 execute 调用也多走一次管线内部循环」，对用户不可见。

### 3.2 组件与文件改动

| 文件 | 改动 | 内容 |
| --- | --- | --- |
| `maf/src/Guard.cs` | 新增 | `Severity`/`Rule`/`Mode`/`Finding`/`Engine`（`Guard`、`ShouldBlock`、`Configure`、`Mode`）+ `DefaultRules` 10 条移植（含 `commentLine`/Excludes 语义）+ `FormatFindings`（对齐 Eino `formatFindings`，≤5 条，带 snippet/remediation） |
| `maf/src/BasicTools.cs` | 修改 | 加假 shell 工具 `execute(string command)`：不真执行，返回「（演示用假 shell）将执行：<命令>」类回执 |
| `maf/Program.cs` | 修改 | ① `execute` 注册时用 `ApprovalRequiredAIFunction` 包一层；② builder 链追加 `UseToolApproval(new ToolApprovalAgentOptions { AutoApprovalRules = [guardRule] })`，追加位置在最内层（贴近 `ChatClientAgent`），保持 Nudge 最外不破坏「一轮」语义；③ REPL 增审批回路与 `/mode` 命令 |
| `maf/tests/YkTako.MafL2.Tests.csproj` + `maf/tests/GuardEngineTests.cs` | 新增 | xunit 2.4.2（本地缓存，离线可还原）单测规则引擎：命中/排除/三模式/片段截取，不打模型 |
| `maf/README.md` | 修改 | 状态行标注限定解冻；机制映射表、文件列表、实测记录各补一条 |
| `doc/10-MAF审批回路与Eino对照.md` | 新增 | 实现记录 + MAF vs Eino 逐点对比（最终版） |

### 3.3 自动批准规则（规则引擎的落点）

```
guardRule(ctx):
    call      = ctx.FunctionCallContent            // execute 调用
    findings  = engine.Guard(call.Name, ParseArgs(call.Arguments))
    if engine.Mode() == warn:
        if findings 非空: 打印 ⚠️ FormatFindings(...)
        return true                                 // warn：一律自动批，放行
    return !engine.ShouldBlock(findings)            // 无阻断命中：自动批；
                                                    // 有阻断命中：不批，留给 REPL
```

推导：`AutoApprovalRules` 只能「批或跳过」，不能拒绝——所以**放行路径全部在这里
消化**（用户不可见）；需要拦或问人的调用由规则返回 false，冒泡到 REPL。

### 3.4 REPL 审批回路（写入端）

```
response = agent.RunAsync(input, session)
loop:
    pending = response.Messages 里 RequiresConfirmation 为真的 ToolApprovalRequestContent
    if pending 为空: break
    replies = []
    for req in pending:
        call = req.ToolCall as FunctionCallContent
        findings = engine.Guard(call.Name, ParseArgs(call.Arguments))
        if engine.Mode() == block && engine.ShouldBlock(findings):
            (approved, reason) = (false, FormatFindings(findings, "工具调用被安全策略拦截（command guard）"))
        else if !engine.ShouldBlock(findings):
            (approved, reason) = (true, "")     // 兜底：自动批准上限路径也可能 surfaced 非阻断请求，
                                                // 此时不打扰人（与 Eino「未达级别直接放行」一致）
        else:
            (approved, reason) = 问人（⏸️ 人工审批：工具/参数/命中说明；y/N）
        replies.Add(req.CreateResponse(approved, reason))
    response = agent.RunAsync(new ChatMessage(ChatRole.User, replies), session)   // 只回执，不回放用户输入
print response.Text
```

说明：TAA 一次只 surfaced 一个请求（其余排队），loop 自然逐个走完；FICC 对批准
的重建调用执行、对拒绝的生成失败工具结果，模型在同一个 `RunAsync` 内继续。

### 3.5 数据流与 §12 对照（初步；实现后 doc/10 给实测版）

```
模型输出 FunctionCallContent(execute, call_id)
  → FICC：检出 ApprovalRequiredAIFunction → 换成 ToolApprovalRequestContent
           {RequestId = ComposeApprovalRequestId(call_id)}
  → TAA：查 guardRule
       ├─ 无阻断命中 → collected response 注入 → 重入 inner → FICC 重建调用并执行 → 模型继续
       └─ 阻断命中   → 写 session.StateBag 的 SurfacedApprovalRequests → 返回 REPL
REPL：问人或按 block 直接拒 → CreateResponse → 作为新 user 消息回传
  → TAA：BindApprovalResponses 按 RequestId 绑定（快照比对，防伪造）
  → FICC：批准→重建执行；拒绝→失败工具结果给模型
```

- 钥匙：`RequestId`（由 tool_call_id 派生）。Eino 用 `point.ID` + ID↔地址映射。
- 公告板：随消息走的 Request/Response 内容（in-band）+ `AgentSession.StateBag`。
  Eino 用 checkpoint + `addrCtx`（out-of-band）。
- 写入端：REPL 回执循环 ≈ `turn.go` 的 `targets[point.ID]=decision` 循环。
- 读取端：TAA 绑定 + FICC 执行 ≈ guard wrapper 重入时 `GetResumeContext`。
- 结构同构（ID 钥匙 + 公告板 + 两端不直接互引），介质相反（消息协议 vs ctx）。

## 4. 偏离与风险（反例优先）

- **假 shell**：不真执行命令（Eino 的 `bash` 是真执行）。安全考虑，明示偏离；
  规则里 `bash` 工具不存在，实际只有 `execute` 会命中，README 标注。
- **all-or-nothing**：框架规定一个响应里任一工具需审批则全部转请求；因只标
  `execute`，只影响同批的 execute 调用（TAA 排队逐个问）。
- **R1 链序**：TAA 追加在最内层（Nudge 最外）。审批往返会重新进入 Nudge
  （`BeginTurn` 复位）——若实测出现「审批回合中催更/重置」异常，调整链序并在
  doc/10 记录。验证手段：冒烟第 1 步带 write_todos 的场景。
- **R2 自动批准上限**：`MaxAutoApprovalIterations` 默认值未核实；REPL 兜底循环
  对 surfaced 请求（含上限触发路径）一视同仁，不依赖该上限。
- **R3 拒绝路径**：拒绝生成「失败工具结果」的确切形状（错误文本）未实测；以冒烟
  第 2 步确认模型能收到拒绝理由并继续。
- **R4 模式评估重复**：规则在 TAA（自动批判定）与 REPL（拦截/问人判定）各评一次；
  引擎纯函数、同参数同结果，无一致性风险。

## 5. 测试与验证

- `dotnet build maf`（编译过）。
- `dotnet test maf/tests`：规则引擎单测（不打模型）：
  1. `execute` + `rm -rf /tmp/x` → High 命中；
  2. `execute` + 注释行 `# rm` → 被 commentLine 排除；
  3. Unicode 空白命令 → 命中；
  4. `sudo apt install` → Medium：在 require_approval 拦、在 block 不拦；
  5. 无命中 → findings 空；
  6. `FormatFindings` 截断到 5 条。
- 手动冒烟（**会真实调模型，需用户同意后跑**）：管道输入
  `执行 rm -rf /tmp/x` → 期望出现 ⏸️ 审批块；`y` → 假工具回执；`N` → 拒绝文案；
  `执行 echo hi` → 不打扰直接执行；`/mode warn` 同上打印 ⚠️；`/mode block` 不问直拒。
- 验收标准（可检验）：
  1. 命中命令出现人工审批块（工具/参数/命中说明/`[y/N]`）；
  2. 批准后模型收到工具结果并继续；
  3. 拒绝后模型收到拒绝理由、工具未执行；
  4. 未命中命令无审批打扰；
  5. 三模式行为与 §2 表一致；
  6. `dotnet test` 全绿。

## 6. 范围外（本设计不做）

- 文件系统守卫与 PII 打码（Eino L1 三件套的另外两件）。
- 真实 shell 执行（假工具仅为演示审批回路）。
- checkpoint 持久化（进程内 session 与会话同生共死）。
- MAF 的 `ApprovalNotRequiredFunctionBypassing` / standing rule（"always approve"）
  等增强能力的显式开关（默认行为即可，不额外配置）。
