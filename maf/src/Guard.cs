using System.Globalization;
using System.Text.Json;
using System.Text.RegularExpressions;

namespace YkTako.MafL2;

/// <summary>规则命中的严重级别，从 Critical 到 Info（对齐 Eino guard.Severity）。</summary>
public enum GuardSeverity { Critical, High, Medium, Low, Info }

/// <summary>守卫运行模式：block 直接拦，warn 只提示，require_approval 暂停问人（对齐 Eino guard.Mode）。</summary>
public enum GuardMode { Block, Warn, RequireApproval }

/// <summary>一条工具调用检查规则：限定工具与参数名，用正则找危险特征，Excludes 用来放过误报。</summary>
public sealed record GuardRule(
    string Id,
    string[] Tools,
    string[] Params,
    GuardSeverity Severity,
    string[] Patterns,
    string[] Excludes,
    string Description,
    string Remediation);

/// <summary>一条命中记录，带命中片段的前后文，方便人判读。</summary>
public sealed record Finding(
    string RuleId,
    GuardSeverity Severity,
    string ToolName,
    string ParamName,
    string Snippet,
    string Description,
    string Remediation);

/// <summary>
/// 命令守卫的规则引擎 —— 从 yk-tako-eino/internal/guard/toolguard.go 逐条移植。
/// 只负责「工具名 + 参数 → 命中清单」与「按模式判断该不该拦」，不依赖 MAF：
/// 自动批准判定（Program.cs 的 guardRule）与 REPL 审批回路共用同一个实例。
/// </summary>
public sealed class GuardEngine
{
    /// <summary>规则默认瞄的 shell 工具名；MAF 演示里只有 execute 真实存在（bash 分支永不命中）。</summary>
    private static readonly string[] ShellTools = ["bash", "execute"];

    /// <summary>shell 工具的命令参数名。</summary>
    private static readonly string[] CommandArg = ["command"];

    /// <summary>注释行特殊排除的源码，要和 IsCommentLine 的判等用同一字面量。</summary>
    private static readonly string CommentLine = @"^\s*#";

    /// <summary>
    /// DefaultRules：从 harness 的危险命令表里抽出的 10 条（与 Eino DefaultRules 逐条一致，
    /// 安全知识与语言、框架无关）。这里只放第一条，后续任务补齐。
    /// </summary>
    public static readonly GuardRule[] DefaultRules =
    [
        new("TOOL_CMD_DANGEROUS_RM", ShellTools, CommandArg, GuardSeverity.High,
            [@"\brm\b", @"\bdel\b", @"(?i)\bRemove-Item\b"],
            [CommentLine],
            "命令包含 rm/del，可能造成数据丢失",
            "执行删除前与用户确认"),
    ];

    private readonly (GuardRule Rule, Regex[] Patterns, Regex[] Excludes)[] _rules;
    private GuardMode _mode;

    /// <summary>构造引擎：mode 决定拦截级别；rules 为 null 用 DefaultRules。正则在这里一次编译。</summary>
    public GuardEngine(GuardMode mode, GuardRule[]? rules = null)
    {
        _mode = mode;
        var source = rules ?? DefaultRules;
        _rules = source
            .Select(rule => (
                rule,
                rule.Patterns.Select(p => new Regex(p, RegexOptions.Compiled)).ToArray(),
                rule.Excludes.Select(p => new Regex(p, RegexOptions.Compiled)).ToArray()))
            .ToArray();
    }

    /// <summary>当前模式。</summary>
    public GuardMode Mode => _mode;

    /// <summary>切换运行时模式（对齐 Eino Engine.Configure）。</summary>
    public void Configure(GuardMode mode) => _mode = mode;

    /// <summary>扫一遍工具调用的参数，返回命中清单；片段前后各留 40 字上下文（对齐 Eino Engine.Guard）。</summary>
    public IReadOnlyList<Finding> Guard(string toolName, IDictionary<string, object?>? args)
    {
        var findings = new List<Finding>();
        if (args is null)
        {
            return findings;
        }

        foreach (var (paramName, raw) in args)
        {
            var value = ToText(raw);
            if (string.IsNullOrWhiteSpace(value))
            {
                continue;
            }

            foreach (var (rule, patterns, excludes) in _rules)
            {
                if (rule.Tools.Length > 0 && !rule.Tools.Contains(toolName))
                {
                    continue;
                }
                if (rule.Params.Length > 0 && !rule.Params.Contains(paramName))
                {
                    continue;
                }

                var (scanned, keep) = TextForRule(value, excludes);
                if (!keep || string.IsNullOrWhiteSpace(scanned))
                {
                    continue;
                }

                Regex? hit = null;
                foreach (var pattern in patterns)
                {
                    if (pattern.IsMatch(scanned))
                    {
                        hit = pattern;
                        break;
                    }
                }
                if (hit is null)
                {
                    continue;
                }

                var match = hit.Match(scanned);
                var start = match.Success ? match.Index : 0;
                var end = match.Success ? match.Index + match.Length : 0;
                var from = Math.Max(0, start - 40);
                var to = Math.Min(scanned.Length, end + 40);
                findings.Add(new Finding(
                    rule.Id, rule.Severity, toolName, paramName,
                    scanned[from..to], rule.Description, rule.Remediation));
            }
        }

        return findings;
    }

    /// <summary>按当前模式判断命中清单里有没有该拦的级别（对齐 Eino Engine.ShouldBlock）。</summary>
    public bool ShouldBlock(IReadOnlyList<Finding> findings)
    {
        foreach (var finding in findings)
        {
            if (IsBlocking(_mode, finding.Severity))
            {
                return true;
            }
        }
        return false;
    }

    /// <summary>模式 → 该拦哪些级别：warn 全放行，require_approval 连 Medium 也拦。</summary>
    private static bool IsBlocking(GuardMode mode, GuardSeverity severity) => mode switch
    {
        GuardMode.Block => severity is GuardSeverity.Critical or GuardSeverity.High,
        GuardMode.RequireApproval => severity is GuardSeverity.Critical or GuardSeverity.High or GuardSeverity.Medium,
        _ => false,
    };

    /// <summary>参数值转文本：string 原样；JsonElement（FICC 反序列化的常见形态）取字符串或原始 JSON。</summary>
    private static string ToText(object? raw) => raw switch
    {
        null => "",
        string text => text,
        JsonElement element => element.ValueKind == JsonValueKind.String
            ? element.GetString() ?? ""
            : element.GetRawText(),
        IFormattable formattable => formattable.ToString(null, CultureInfo.InvariantCulture),
        _ => raw.ToString() ?? "",
    };

    /// <summary>
    /// 对齐 Eino textForRule：`^\s*#` 只剥注释行，剩下的文本继续匹配；
    /// 其它排除模式命中整段则本条规则不报警。
    /// </summary>
    private static (string Text, bool Keep) TextForRule(string value, Regex[] excludes)
    {
        var comments = new List<Regex>();
        foreach (var pattern in excludes)
        {
            if (IsCommentLine(pattern))
            {
                comments.Add(pattern);
                continue;
            }
            if (pattern.IsMatch(value))
            {
                return ("", false);
            }
        }

        if (comments.Count == 0)
        {
            return (value, true);
        }

        var kept = new List<string>();
        foreach (var line in value.Split('\n'))
        {
            var skip = false;
            foreach (var pattern in comments)
            {
                if (pattern.IsMatch(line))
                {
                    skip = true;
                    break;
                }
            }
            if (!skip)
            {
                kept.Add(line);
            }
        }
        return (string.Join("\n", kept), true);
    }

    /// <summary>认出 CommentLine 这条特殊排除（判等用源码字面量，和 Eino isCommentLineExclude 同法）。</summary>
    private static bool IsCommentLine(Regex pattern) => pattern.ToString() == CommentLine;
}
