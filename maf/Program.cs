/**
 * L2 演示 —— 任务规划（Microsoft Agent Framework 版）
 *
 * 对照 TS 版 src/l2/01-planning.ts（不含 L1 守卫栈，按需求只复刻 L2）：
 *
 *   用户输入 ──→ 模型 ←── [chat 中间件：注入"什么时候列计划"提示词]
 *                 │
 *        ┌────────┴─────────┐
 *        ▼                  ▼
 *   普通工具执行       write_todos → TodoStore（todos 频道的 MAF 等价物）
 *        │                  │
 *   [function 中间件：并行 write_todos 按 CallId 拦截]
 *        │
 *   [agent run 中间件：本轮清单没走完就想收工 → 追加提醒再跑一次]
 *
 * 2026-09-26 追加 L1 最小审批回路（对照 yk-tako-eino 的 interrupt/resume，见 doc/10）：
 *   execute 工具被 ApprovalRequiredAIFunction 标记 → FICC 把调用换成「待批准请求」
 *   返回 REPL（ToolApprovalAgent 驱动）；guardRule 无阻断命中时自动批准（用户不可见），
 *   有阻断命中时留给 REPL 问人或直拒，回执经 CreateResponse 作为新 user 消息回传。
 *
 * 试试这些输入：
 *   现在几点                                  → 简单任务，不该列计划
 *   帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结
 *                                             → 复杂任务，先列清单再逐项执行
 *   执行 echo hi                              → execute 无命中，直接执行（假 shell）
 *   执行 rm -rf /tmp/x                        → 命中危险规则，暂停问人
 * 斜杠命令：
 *   /todos   查看当前任务清单（直接读 TodoStore —— 清单是状态，不是消息）
 *   /mode    切换守卫模式 block|warn|require_approval
 */
using Microsoft.Agents.AI;
using Microsoft.Extensions.AI;
using YkTako.MafL2;

// ── 模型：复用 yk-tako-eino 的 .env 和 provider-catalog.json，与 Go 主线同一条 TAKO_MODEL ──
var repoRoot = EnvFile.FindRepoRoot()
    ?? throw new InvalidOperationException("向上找不到 yk-tako-eino 目录（yk-tako-eino/internal/config/provider-catalog.json）");
EnvFile.Load(repoRoot);
var modelConfig = ModelResolver.Resolve(repoRoot);

// ── 组装：IChatClient →（chat 中间件）→ ChatClientAgent →（function + run + approval 中间件）──
var store = new TodoStore();
var planning = new PlanningMiddleware(store);
var guardEngine = new GuardEngine(GuardMode.RequireApproval); // 对齐 Eino 入口默认；/mode 可切

var chatClient = modelConfig.BuildChatClient()
    .AsBuilder()
    .Use(getResponseFunc: planning.ChatMiddleware, getStreamingResponseFunc: null)
    .Build();

var baseAgent = new ChatClientAgent(
    chatClient,
    instructions: "你是 yk-tako 的 MAF 移植版，一个命令行 AI 工作伙伴。",
    name: "yk-tako-maf-l2",
    tools:
    [
        AIFunctionFactory.Create(BasicTools.GetCurrentTime, name: "get_current_time"),
        AIFunctionFactory.Create(BasicTools.Calculate, name: "calculate"),
        planning.CreateWriteTodosTool(),
        // execute：规则表唯一能命中的工具；ApprovalRequiredAIFunction 让 FICC 把它的调用
        // 换成「待批准请求」返回调用方（这是 MAF 原生的 interrupt 等价物，见 doc/10）。
        new ApprovalRequiredAIFunction(AIFunctionFactory.Create(BasicTools.Execute, name: "execute")),
    ]);

// 自动批准规则：无阻断命中 → 批（用户不可见）；有阻断命中 → 不批，冒泡给 REPL。
// warn 模式一律批，只把告警打印出来（对齐 Eino ModeWarn）。
var guardRule = new Func<ToolAutoApprovalRuleContext, ValueTask<bool>>(ctx =>
{
    var call = ctx.FunctionCallContent;
    var findings = guardEngine.Guard(call.Name, call.Arguments);
    if (guardEngine.Mode == GuardMode.Warn)
    {
        if (findings.Count > 0)
        {
            Console.WriteLine($"\n⚠️  [tool_guard] {GuardEngine.FormatFindings(findings, "检测到风险参数（warn 模式，放行）")}\n");
        }
        return ValueTask.FromResult(true);
    }
    return ValueTask.FromResult(!guardEngine.ShouldBlock(findings));
});

var agent = baseAgent
    .AsBuilder()
    .Use(runFunc: planning.NudgeMiddleware, runStreamingFunc: null) // 半途而废提醒（最外层）
    .Use(planning.FunctionMiddleware)                               // 并行 write_todos 拦截
    .UseToolApproval(new ToolApprovalAgentOptions { AutoApprovalRules = [guardRule] }) // 审批回路（最内层）
    .Build();

var session = await agent.CreateSessionAsync();

Console.WriteLine($"L2 任务规划 + L1 最小审批回路（MAF approval 管线）。模型 {modelConfig.Ref}。");
Console.WriteLine($"ToolGuard 模式: {GuardEngine.ModeName(guardEngine.Mode)}（/mode 可切换）。/todos 看清单，quit 退出。\n");

while (true)
{
    Console.Write("你: ");
    var input = Console.ReadLine()?.Trim();
    if (input is null or "quit" or "exit") break;
    if (input.Length == 0) continue;

    // /todos：直接读 TodoStore——清单是状态，不是消息，所以能绕过对话这样查
    if (input == "/todos")
    {
        Console.WriteLine($"📋 当前任务清单:\n{store.FormatAll()}\n");
        continue;
    }

    if (input.StartsWith("/mode ", StringComparison.Ordinal))
    {
        var value = input[6..].Trim();
        if (GuardEngine.TryParseMode(value, out var mode))
        {
            guardEngine.Configure(mode);
            Console.WriteLine($"ToolGuard 模式已切换为: {GuardEngine.ModeName(mode)}\n");
        }
        else
        {
            Console.WriteLine("用法: /mode block|warn|require_approval\n");
        }
        continue;
    }

    try
    {
        var response = await agent.RunAsync(input, session);
        response = await ResolveApprovalsAsync(agent, session, response, guardEngine);
        Console.WriteLine($"\n🐙 {response.Text}\n");
    }
    catch (Exception ex)
    {
        Console.Error.WriteLine($"出错了: {ex.Message}");
    }
}

// 审批回路（写入端）：RunAsync 返回的响应里可能有待批准的工具调用
// （ToolApprovalRequestContent，钥匙是这个请求的 RequestId）。逐个判定：
//   block 模式 + 阻断级命中 → 直接拒绝（不进人）；
//   未达阻断级（自动批准上限路径的兜底）→ 静默批准；
//   其余 → 问人。回执装成一条新 user 消息回传，ToolApprovalAgent 按 RequestId
//   绑定到它 surfaced 过的请求，FICC 重建调用执行或生成拒绝结果。
static async Task<AgentResponse> ResolveApprovalsAsync(
    AIAgent agent, AgentSession session, AgentResponse response, GuardEngine engine)
{
    while (true)
    {
        var pending = response.Messages
            .SelectMany(m => m.Contents)
            .OfType<ToolApprovalRequestContent>()
#pragma warning disable MEAI001 // RequiresConfirmation 是实验 API；这里需要区分「需人工确认」的请求
            .Where(r => r.RequiresConfirmation)
#pragma warning restore MEAI001
            .ToList();
        if (pending.Count == 0)
        {
            return response;
        }

        var replies = new List<AIContent>();
        foreach (var request in pending)
        {
            var call = request.ToolCall as FunctionCallContent;
            var findings = call is null
                ? (IReadOnlyList<Finding>)Array.Empty<Finding>()
                : engine.Guard(call.Name, call.Arguments);

            bool approved;
            string reason;
            if (engine.Mode == GuardMode.Block && engine.ShouldBlock(findings))
            {
                approved = false;
                reason = GuardEngine.FormatFindings(findings, "工具调用被安全策略拦截（command guard）");
            }
            else if (!engine.ShouldBlock(findings))
            {
                approved = true;
                reason = "";
            }
            else
            {
                Console.WriteLine("\n⏸️  ═══════════════ 人工审批（tool approval）═══════════════");
                Console.WriteLine($"工具: {call!.Name}   参数: {FormatArgs(call)}");
                Console.WriteLine(GuardEngine.FormatFindings(findings, "工具调用需要人工批准（命中风险规则）"));
                Console.Write("批准执行？[y/N] ");
                var answer = Console.ReadLine()?.Trim().ToLowerInvariant();
                approved = answer is "y" or "yes";
                reason = approved ? "用户批准" : "用户拒绝";
                Console.WriteLine(approved ? "✅ 已批准，继续执行\n" : "🚫 已拒绝\n");
            }
            replies.Add(request.CreateResponse(approved, reason));
        }

        response = await agent.RunAsync(new ChatMessage(ChatRole.User, replies), session);
    }
}

// 审批提示里的参数展示：函数调用的参数表拍平成一行的 key=value。
static string FormatArgs(FunctionCallContent call) =>
    call.Arguments is null
        ? "(无)"
        : string.Join(", ", call.Arguments.Select(kv => $"{kv.Key}={kv.Value}"));
