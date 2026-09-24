using Microsoft.Agents.AI;
using Microsoft.Extensions.AI;

namespace YkTako.MafL2;

/// <summary>
/// 任务规划中间件（L2）—— Microsoft Agent Framework 版。
/// 复刻对象与 TS 手写版相同：deepagents / langchain 的 TodoListMiddleware 四零件，
/// 加上本仓库外扩的"半途而废提醒"。零件到 MAF 机制的映射：
///
///  LangGraph（TS 版）                     MAF（本文件）
///  ─────────────────────────────         ──────────────────────────────────
///  stateSchema 的 todos 频道        →    TodoStore（工具与各层中间件共享的实例）
///  write_todos 工具返回 Command      →    write_todos 工具直接写 TodoStore，返回紧凑回执文本
///  wrapModelCall 拼 systemMessage    →    chat 中间件（IChatClient 层）注入 ChatOptions.Instructions
///  afterModel 并行防守               →    chat 中间件数调用 + function 中间件按 CallId 拦截
///  afterModel + jumpTo:"model" 提醒  →    agent run 中间件：RunAsync 返回后追加提醒消息再跑一次
///
/// 与 TS 版保持一致的语义：紧凑中文提示词、紧凑回执摘要、全量替换、每轮最多催一次、
/// 只催本轮写过清单的 in_progress（上轮遗留不劫持新话题）。
/// </summary>
public sealed class PlanningMiddleware
{
    // ─────────── 提示词（与 TS 版逐字一致：紧凑中文版 ~15 行） ───────────
    public const string PlanningSystemPrompt = """
        ## `write_todos` 任务清单

        你可以用 `write_todos` 工具维护一份任务清单，把复杂目标拆成可跟踪的步骤，让用户看到你的进度。

        什么时候用：
        - 需要 3 步以上的复杂任务、用户一次给了多件事、或用户明确要求列计划
        - 简单任务（一两步能完成）直接做，不要用——写清单本身也花 token

        怎么用：
        - 每次调用提交完整清单（全量替换语义，不是追加）
        - 开始某项前先把它标成 in_progress；完成一项立刻标 completed，不要攒着批量改
        - 计划可以随进展修订：过时的删掉，新发现的加上
        - 每轮最多调用一次，绝不并行调用（全量替换下并行会产生歧义）
        """;

    private const string ParallelWriteError =
        "错误：write_todos 每轮只能调用一次。它是全量替换语义，并行调用会产生歧义。请把改动合并成一次调用重试。";

    private const string NudgePrefix = "[系统提醒·任务清单]";

    private readonly TodoStore _store;
    private readonly bool _nudgeOnIncomplete;

    public PlanningMiddleware(TodoStore store, bool nudgeOnIncomplete = true)
    {
        _store = store;
        _nudgeOnIncomplete = nudgeOnIncomplete;
    }


    // ─────────── 零件 2：write_todos 工具（全量替换 + 紧凑回执） ───────────

    public AIFunction CreateWriteTodosTool() => AIFunctionFactory.Create(
        (TodoItem[] todos) =>
        {
            var error = _store.ReplaceAll(todos);
            if (error is not null) return error;
            var done = todos.Count(t => t.Status == "completed");
            var receipt = $"任务清单已更新（{done}/{todos.Length} 已完成）：\n{TodoStore.Format(todos)}";
            // 同步打印到控制台，让 REPL 能看到 📋 清单的状态流转（对齐 TS 版 printChunk 里的 todos 分支）
            Console.WriteLine($"  📋 任务清单:\n     {TodoStore.Format(todos).Replace("\n", "\n     ")}");
            return receipt;
        },
        name: "write_todos",
        description: "创建或更新任务清单（全量替换）。用于 3 步以上的复杂任务：先列计划，执行中实时更新各项状态（pending/in_progress/completed）。简单任务不要用。");

    // ─────────── 零件 3：系统提示词注入（chat 中间件，对应 wrapModelCall） ───────────
    // ─────────── 零件 4：并行防守的"发现"半（对应 afterModel 检查一轮内调用数） ───────────

    /// <summary>
    /// IChatClient 层中间件：每次调模型前注入规划提示词；拿到响应后数 write_todos 的
    /// FunctionCallContent，一轮超过 1 个就按 CallId 登记进防守名单（由 function
    /// 中间件执行拦截）。注册方式：chatClient.AsBuilder().Use(getResponseFunc: ..., null)。
    /// </summary>
    public async Task<ChatResponse> ChatMiddleware(
        IEnumerable<ChatMessage> messages, ChatOptions? options, IChatClient innerChatClient, CancellationToken cancellationToken)
    {
        // 注入提示词。幂等：同一份 ChatOptions 可能在工具循环的多次模型调用间复用，重复追加会刷屏。
        options ??= new ChatOptions();
        if (options.Instructions?.Contains(PlanningSystemPrompt, StringComparison.Ordinal) != true)
            options.Instructions = string.IsNullOrEmpty(options.Instructions)
                ? PlanningSystemPrompt
                : options.Instructions + "\n\n" + PlanningSystemPrompt;

        var response = await innerChatClient.GetResponseAsync(messages, options, cancellationToken);

        // 并行防守（发现半）：一轮模型输出里 write_todos 出现多次 → 全量替换语义下是歧义。
        // 与官方版一致：这些调用全部拦截（一个都不执行），由 function 中间件回错误。
        var writeTodosCalls = response.Messages
            .SelectMany(m => m.Contents)
            .OfType<FunctionCallContent>()
            .Where(c => c.Name == "write_todos")
            .ToList();
        if (writeTodosCalls.Count > 1)
            _store.BlockCalls(writeTodosCalls.Select(c => c.CallId));

        return response;
    }

    // ─────────── 零件 4：并行防守的"拦截"半（function 中间件） ───────────

    /// <summary>
    /// 函数调用中间件：write_todos 调用的 CallId 在防守名单里 → 不执行，直接回错误文本。
    /// 名单外的调用（包括其他工具）原样放行。注册方式：agent.AsBuilder().Use(...)。
    /// </summary>
    public async ValueTask<object?> FunctionMiddleware(
        AIAgent agent, FunctionInvocationContext context,
        Func<FunctionInvocationContext, CancellationToken, ValueTask<object?>> next, CancellationToken cancellationToken)
    {
        if (context.Function.Name == "write_todos" && _store.IsBlocked(context.CallContent?.CallId))
        {
            Console.WriteLine("  🚫 并行 write_todos 已拦截（全量替换语义下并行 = 歧义）");
            return ParallelWriteError;
        }
        return await next(context, cancellationToken);
    }

    // ─────────── 外扩：半途而废提醒（对应 afterModel + jumpTo:"model"） ───────────

    /// <summary>
    /// agent run 中间件：包住整轮 RunAsync。
    ///   进入时 BeginTurn() 复位本轮标记 —— "一轮"的边界在 run 层天然清晰，
    ///   这正是 LangGraph 版需要扫消息判定的"本轮用户发言之后"的 MAF 等价物。
    ///   返回后检查：本轮成功写过清单 + 清单还有 in_progress + 本轮还没催过
    ///   → 追加一条提醒消息再跑一次 agent（jumpTo "model" 的等价物）。
    /// RunAsync 能正常返回本身就保证了"模型想收工"（无待执行 tool_calls）——
    /// TS 版需要显式检查的条件在这里由框架的函数调用循环天然满足。
    /// </summary>
    public async Task<AgentResponse> NudgeMiddleware(
        IEnumerable<ChatMessage> messages, AgentSession? session, AgentRunOptions? options,
        AIAgent innerAgent, CancellationToken cancellationToken)
    {
        _store.BeginTurn();
        var response = await innerAgent.RunAsync(messages, session, options, cancellationToken);

        if (!_nudgeOnIncomplete || _store.NudgedThisTurn || !_store.WroteTodosThisTurn || !_store.HasInProgress)
            return response;

        // 只催 in_progress（纯 pending 旧账不催）；本轮没写过清单不催（WroteTodosThisTurn 已挡）；
        // 每轮最多催一次（NudgedThisTurn 保证终止，不会死循环）。
        _store.NudgedThisTurn = true;
        var incomplete = _store.Incomplete;
        var nudgeText =
            $"{NudgePrefix} 还有 {incomplete.Count} 项任务未完成（含进行中的）：\n{TodoStore.Format(incomplete)}\n" +
            "如果确实无法继续，请向用户说明卡在哪里；否则请继续执行，并在完成后更新清单。";
        Console.WriteLine($"  ↩️  {nudgeText.Split('\n')[0]}（已跳回模型继续）");

        // session 里已有本轮全部历史，只把提醒作为新的用户消息传进去即可继续。
        return await innerAgent.RunAsync([new ChatMessage(ChatRole.User, nudgeText)], session, options, cancellationToken);
    }
}
