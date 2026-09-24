using System.ComponentModel;

namespace YkTako.MafL2;

/// <summary>
/// 清单条目。status 用 string + 工具内校验（而不是枚举），
/// 让 JSON schema 对模型直白，也避免 STJ 枚举序列化形态的干扰。
/// </summary>
public sealed class TodoItem
{
    [Description("任务内容，一句话，具体可执行")]
    public string Content { get; set; } = "";

    [Description("pending 待办 / in_progress 进行中 / completed 已完成")]
    public string Status { get; set; } = "pending";
}

/// <summary>
/// 任务清单状态 —— LangGraph 版图状态里 `todos` 频道的 MAF 等价物。
///
/// MAF 没有"图状态频道"概念：状态不挂在图上，而是收在一个由工具和各层中间件
/// 共享的存储对象里（按会话一个实例）。REPL 的 /todos 绕过对话直接读它，
/// 语义与 TS 版 `agent.getState(config).values.todos` 一致：清单是状态，不是消息。
///
/// 同时承载两个跨层协作标记：
///  - 本轮标记（WroteTodosThisTurn / NudgedThisTurn）：半途而废提醒的判定依据，
///    由 agent run 中间件在每轮用户输入时 BeginTurn() 复位 —— "一轮"的边界天然清晰。
///  - 并行防守名单（_blockedCallIds）：chat 中间件发现一轮模型输出里有多个
///    write_todos 调用时按 CallId 登记，function 中间件逐个拦截。
/// </summary>
public sealed class TodoStore
{
    private static readonly HashSet<string> ValidStatuses = new(StringComparer.Ordinal)
        { "pending", "in_progress", "completed" };
    private static readonly Dictionary<string, string> StatusIcon = new()
    {
        ["pending"] = "⬜",
        ["in_progress"] = "🔨",
        ["completed"] = "✅",
    };

    private readonly List<TodoItem> _todos = [];
    private readonly HashSet<string> _blockedCallIds = new(StringComparer.Ordinal);

    public IReadOnlyList<TodoItem> Todos => _todos;
    public bool WroteTodosThisTurn { get; private set; }
    public bool NudgedThisTurn { get; set; }

    /// <summary>每轮用户输入开始时由 agent run 中间件调用，复位本轮标记。</summary>
    public void BeginTurn()
    {
        WroteTodosThisTurn = false;
        NudgedThisTurn = false;
    }

    /// <summary>全量替换清单（write_todos 的唯一写入口）。返回 null 表示成功，否则是错误信息。</summary>
    public string? ReplaceAll(IReadOnlyList<TodoItem> todos)
    {
        foreach (var t in todos)
        {
            if (!ValidStatuses.Contains(t.Status))
                return $"错误：无效的状态 \"{t.Status}\"，只允许 pending / in_progress / completed";
        }
        _todos.Clear();
        _todos.AddRange(todos);
        WroteTodosThisTurn = true;
        return null;
    }

    // ── 并行防守名单 ──
    public void BlockCalls(IEnumerable<string> callIds)
    {
        foreach (var id in callIds) _blockedCallIds.Add(id);
    }

    public bool IsBlocked(string? callId) => callId is not null && _blockedCallIds.Contains(callId);

    // ── 半途而废提醒的判定 ──
    public List<TodoItem> Incomplete => _todos.Where(t => t.Status != "completed").ToList();
    public bool HasInProgress => _todos.Any(t => t.Status == "in_progress");

    /// <summary>紧凑清单摘要：write_todos 回执和 REPL 的 /todos 命令共用（对齐 TS 的 formatTodos）。</summary>
    public static string Format(IReadOnlyList<TodoItem> todos)
    {
        if (todos.Count == 0) return "（清单为空）";
        return string.Join("\n", todos.Select((t, i) =>
            $"{StatusIcon.GetValueOrDefault(t.Status, "⬜")} {i + 1}. {t.Content}"));
    }

    public string FormatAll() => Format(_todos);
}
