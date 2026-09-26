# MAF 最小审批回路 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 `maf/` 补上「规则命中 → 暂停问人 → resume 执行/拒绝」的最小审批回路（L1 tool guard 的 MAF 等价物），全部走 MAF 自带 approval 管线。

**Architecture:** 规则引擎（`GuardEngine`，从 Eino `toolguard.go` 逐条移植，框架无关）决定「自动批 / 不批」；`execute` 假 shell 工具用 `ApprovalRequiredAIFunction` 标记；`ToolApprovalAgent` + `FunctionInvokingChatClient` 承担暂停、绑定、执行、拒绝回执；REPL 负责问人与 block 模式直拒。

**Tech Stack:** .NET 8 / C#、Microsoft.Agents.AI 1.22.0、Microsoft.Extensions.AI 10.10.0、xunit 2.4.2（已在本机 NuGet 缓存，离线可还原）。设计文档：`docs/superpowers/specs/2026-09-26-maf-tool-approval-design.md`。

**执行说明（与默认流程的两处有意偏离，原因可检验）：**
1. 不新建 worktree/分支：本计划只触碰 `maf/`、`doc/`、`docs/`，与工作区现有未提交改动（`yk-tako-eino/`、`README.md` 等）无交集；沿用仓库既有的「直接提交 master」习惯（见 `git log`）。
2. 提交按仓库风格写中文短句，不加 `feat:` 前缀。

---

### Task 1: 测试项目骨架 + 规则引擎最小实现（第一条命中测试）

**Files:**
- Create: `maf/tests/YkTako.MafL2.Tests.csproj`
- Create: `maf/tests/GuardEngineTests.cs`
- Create: `maf/src/Guard.cs`

- [ ] **Step 1: 建测试项目文件**

`maf/tests/YkTako.MafL2.Tests.csproj`：

```xml
<Project Sdk="Microsoft.NET.Sdk">

  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
    <ImplicitUsings>enable</ImplicitUsings>
    <Nullable>enable</Nullable>
    <IsPackable>false</IsPackable>
  </PropertyGroup>

  <ItemGroup>
    <PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.5.0" />
    <PackageReference Include="xunit" Version="2.4.2" />
    <PackageReference Include="xunit.runner.visualstudio" Version="2.4.5" />
  </ItemGroup>

  <ItemGroup>
    <ProjectReference Include="../YkTako.MafL2.csproj" />
  </ItemGroup>

</Project>
```

- [ ] **Step 2: 写第一条失败测试（rm 命中 High）**

`maf/tests/GuardEngineTests.cs`：

```csharp
using Xunit;
using YkTako.MafL2;

namespace YkTako.MafL2.Tests;

/// <summary>
/// 规则引擎单测：只验纯逻辑（工具名 + 参数 → 命中清单 / 拦截判断），不打模型、不碰 MAF。
/// 语义对齐 yk-tako-eino/internal/guard/toolguard.go 与 guard_test.go。
/// </summary>
public sealed class GuardEngineTests
{
    /// <summary>默认规则 + require_approval 模式，覆盖最常用的判定路径。</summary>
    private static GuardEngine NewApproveEngine() => new(GuardMode.RequireApproval);

    /// <summary>把一条 shell 命令装成工具参数表（参数名对齐 DefaultRules 的 commandArg）。</summary>
    private static Dictionary<string, object?> Args(string command) => new() { ["command"] = command };

    [Fact]
    public void RmCommand_IsCaught_AsHigh()
    {
        var findings = NewApproveEngine().Guard("execute", Args("rm -rf /tmp/x"));

        var finding = Assert.Single(findings);
        Assert.Equal("TOOL_CMD_DANGEROUS_RM", finding.RuleId);
        Assert.Equal(GuardSeverity.High, finding.Severity);
        Assert.Equal("command", finding.ParamName);
    }
}
```

- [ ] **Step 3: 跑测试，确认失败**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: 编译失败——`GuardMode`/`GuardEngine`/`GuardSeverity` 不存在。

- [ ] **Step 4: 写 Guard.cs 最小实现**

`maf/src/Guard.cs`：

```csharp
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
```

- [ ] **Step 5: 跑测试，确认通过**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: 1 passed。

- [ ] **Step 6: 提交**

```bash
git add maf/src/Guard.cs maf/tests/YkTako.MafL2.Tests.csproj maf/tests/GuardEngineTests.cs
git commit -m "MAF：规则引擎最小实现与测试骨架（rm 命中用例）"
```

---

### Task 2: 补齐 DefaultRules 10 条 + 模式/排除语义测试

**Files:**
- Modify: `maf/src/Guard.cs`（`DefaultRules` 数组）
- Modify: `maf/tests/GuardEngineTests.cs`

- [ ] **Step 1: 先写失败测试（追加到 GuardEngineTests）**

```csharp
    [Fact]
    public void CommentLines_AreStripped_BeforeMatching()
    {
        var findings = NewApproveEngine().Guard("execute", Args("# rm -rf /\nls"));
        Assert.Empty(findings);
    }

    [Fact]
    public void UnicodeWhitespace_IsCaught()
    {
        var findings = NewApproveEngine().Guard("execute", Args("rm\u00a0-rf /tmp/x"));
        Assert.Contains(findings, f => f.RuleId == "TOOL_CMD_UNICODE_WHITESPACE");
    }

    [Fact]
    public void Sudo_Medium_IsBlockedOnlyInApproveMode()
    {
        var approve = new GuardEngine(GuardMode.RequireApproval);
        var block = new GuardEngine(GuardMode.Block);
        var args = Args("sudo apt update");

        Assert.True(approve.ShouldBlock(approve.Guard("execute", args)));
        Assert.False(block.ShouldBlock(block.Guard("execute", args)));
    }

    [Fact]
    public void WarnMode_NeverBlocks()
    {
        var warn = new GuardEngine(GuardMode.Warn);
        Assert.False(warn.ShouldBlock(warn.Guard("execute", Args("rm -rf /tmp/x"))));
    }

    [Fact]
    public void BenignCommand_HasNoFindings()
    {
        Assert.Empty(NewApproveEngine().Guard("execute", Args("echo hi")));
    }

    [Fact]
    public void NonShellTool_IsNotScanned()
    {
        Assert.Empty(NewApproveEngine().Guard("calculate", Args("rm -rf /tmp/x")));
    }

    [Fact]
    public void WholeValueExclude_SkipsRule()
    {
        var rule = new GuardRule("T_LS", ["execute"], ["command"], GuardSeverity.High,
            [@"\bls\b"], [@"\bsafe\b"], "d", "r");
        var engine = new GuardEngine(GuardMode.Block, [rule]);

        Assert.Empty(engine.Guard("execute", Args("ls safe")));
        Assert.Single(engine.Guard("execute", Args("ls x")));
    }
```

- [ ] **Step 2: 跑测试，确认失败**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: `UnicodeWhitespace_IsCaught`、`Sudo_Medium_IsBlockedOnlyInApproveMode` 失败（规则还没补）。

- [ ] **Step 3: 补齐 DefaultRules**

把 `maf/src/Guard.cs` 里 `DefaultRules` 的数组替换为完整 10 条（保留注释与新加项，顺序与 Eino 一致）：

```csharp
    /// <summary>
    /// DefaultRules：从 harness 的危险命令表里抽出的 10 条（与 Eino DefaultRules 逐条一致，
    /// 安全知识与语言、框架无关）。
    /// </summary>
    public static readonly GuardRule[] DefaultRules =
    [
        new("TOOL_CMD_DANGEROUS_RM", ShellTools, CommandArg, GuardSeverity.High,
            [@"\brm\b", @"\bdel\b", @"(?i)\bRemove-Item\b"],
            [CommentLine],
            "命令包含 rm/del，可能造成数据丢失",
            "执行删除前与用户确认"),
        new("TOOL_CMD_DANGEROUS_MV", ShellTools, CommandArg, GuardSeverity.High,
            [@"\bmv\b"], [],
            "命令包含 mv，可能意外移动或覆盖文件",
            "执行移动/重命名前与用户确认"),
        new("TOOL_CMD_FS_DESTRUCTION", ShellTools, CommandArg, GuardSeverity.Critical,
            [@"(?i)\bmkfs(\.[a-z0-9_]+)?\b", @"(?i)\bmke2fs\b", @"(?i)\bdd\s+.*of=/dev/",
             @">\s*/dev/(sd[a-z]\d*|vd[a-z]\d*|nvme\d+n\d+(p\d+)?)"],
            [],
            "检测到磁盘格式化/擦除等底层破坏性命令",
            "直接拦截。agent 不应操作裸块设备。"),
        new("TOOL_CMD_DOS_FORK_BOMB", ShellTools, CommandArg, GuardSeverity.Critical,
            [@":\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:", @"\bkill\s+-9\s+(-1\b|1\b)"],
            [],
            "检测到 fork 炸弹或批量杀进程命令",
            "立即拦截。这类命令会搞垮宿主机。"),
        new("TOOL_CMD_PIPE_TO_SHELL", ShellTools, CommandArg, GuardSeverity.Critical,
            [@"(?i)\b(curl|wget)\b\s+.*\|.*\b(bash|sh|zsh|ash|dash)\b"],
            [],
            "检测到 curl|bash 模式：下载并立即执行远程脚本",
            "与用户确认。agent 应先审查脚本内容再执行。"),
        new("TOOL_CMD_REVERSE_SHELL", ShellTools, CommandArg, GuardSeverity.Critical,
            [@"(?i)/dev/(tcp|udp)/", @"(?i)\bnc\s+.*-e\s*\S+", @"(?i)\bncat\s+.*-e\s*\S+", @"(?i)\bsocat\s+.*EXEC:"],
            [],
            "检测到反弹 shell / 未授权网络隧道",
            "直接拦截。agent 无需绑定交互式 shell 或隧道。"),
        new("TOOL_CMD_UNICODE_WHITESPACE", ShellTools, CommandArg, GuardSeverity.High,
            ["[\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]"],
            [],
            "命令含 Unicode 空白字符，可能造成解析歧义绕过检查",
            "拦截含非 ASCII 空白的命令，使用普通空格。"),
        new("TOOL_CMD_PROC_ENVIRON", ShellTools, CommandArg, GuardSeverity.High,
            [@"/proc/(?:self|\d+)/environ(?:\b|$)"],
            [CommentLine],
            "访问 /proc/*/environ，可能泄露 API Key 等环境变量",
            "拦截对进程环境变量文件的访问。"),
        new("TOOL_CMD_JQ_SYSTEM", ShellTools, CommandArg, GuardSeverity.High,
            [@"(?i)\bjq\b.*\bsystem\s*\("],
            [CommentLine],
            "jq 的 system() 可执行任意命令，把数据处理工具变成执行通道",
            "拦截使用 system() 的 jq 命令。"),
        new("TOOL_CMD_SUDO", ShellTools, CommandArg, GuardSeverity.Medium,
            [@"(?i)\bsudo\b", @"(?i)\bdoas\b"],
            [CommentLine],
            "命令试图提升权限（sudo/doas）",
            "agent 原则上不应需要提权，与用户确认。"),
    ];
```

- [ ] **Step 4: 跑测试，确认通过**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: 8 passed。

- [ ] **Step 5: 提交**

```bash
git add maf/src/Guard.cs maf/tests/GuardEngineTests.cs
git commit -m "MAF：补齐危险命令规则表 10 条与模式/排除语义测试"
```

---

### Task 3: FormatFindings + JsonElement 参数测试

**Files:**
- Modify: `maf/src/Guard.cs`（加 `FormatFindings` 静态方法）
- Modify: `maf/tests/GuardEngineTests.cs`

- [ ] **Step 1: 先写失败测试**

```csharp
    [Fact]
    public void JsonElementArguments_AreRead()
    {
        using var doc = JsonDocument.Parse("\"rm -rf /tmp/x\"");
        var findings = NewApproveEngine().Guard("execute", new Dictionary<string, object?> { ["command"] = doc.RootElement });
        Assert.Single(findings);
    }

    [Fact]
    public void FormatFindings_CapsAtFive_AndCountsRest()
    {
        var findings = Enumerable.Range(0, 7)
            .Select(i => new Finding($"R{i}", GuardSeverity.High, "execute", "command", $"snippet{i}", $"desc{i}", $"fix{i}"))
            .ToList();

        var text = GuardEngine.FormatFindings(findings, "表头");

        Assert.Contains("表头", text);
        Assert.Equal(5, text.Split('\n').Count(line => line.StartsWith("- [")));
        Assert.Contains("... 以及另外 2 条告警", text);
    }
```

（文件顶部补 `using System.Text.Json;`。）

- [ ] **Step 2: 跑测试，确认失败**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: `FormatFindings` 不存在，编译失败。

- [ ] **Step 3: 实现 FormatFindings**

加到 `GuardEngine` 类内（`IsCommentLine` 之前）：

```csharp
    /// <summary>把命中清单排成人读的几行，最多列 5 条（对齐 Eino formatFindings）。</summary>
    public static string FormatFindings(IReadOnlyList<Finding> findings, string header)
    {
        var toolName = findings.Count > 0 ? findings[0].ToolName : "?";
        var lines = new List<string> { header, "Tool: " + toolName, "" };
        var limit = Math.Min(findings.Count, 5);
        for (var i = 0; i < limit; i++)
        {
            var finding = findings[i];
            lines.Add($"- [{finding.Severity}] {finding.Description}");
            lines.Add($"  snippet: {finding.Snippet}");
            lines.Add($"  remediation: {finding.Remediation}");
        }
        if (findings.Count > 5)
        {
            lines.Add($"... 以及另外 {findings.Count - 5} 条告警");
        }
        return string.Join("\n", lines);
    }
```

- [ ] **Step 4: 跑测试，确认通过**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: 10 passed。

- [ ] **Step 5: 提交**

```bash
git add maf/src/Guard.cs maf/tests/GuardEngineTests.cs
git commit -m "MAF：命中清单格式化与 JsonElement 参数解析测试"
```

---

### Task 4: 假 shell 工具 + Program.cs 接入审批管线与 REPL 回路

**Files:**
- Modify: `maf/src/BasicTools.cs`
- Modify: `maf/Program.cs`

- [ ] **Step 1: BasicTools 加假 shell 工具**

在 `BasicTools` 类内、`Calculate` 之后加：

```csharp
    /// <summary>
    /// 假 shell：审批回路的演示载体，不真正执行任何命令，只回显。
    /// 它是规则表唯一能命中的工具（DefaultRules 瞄 shellTools = bash/execute；
    /// MAF 演示里没有 bash，故不注册）。
    /// </summary>
    [Description("执行一条 shell 命令（演示用假 shell：不会真正执行，只回显命令文本）")]
    public static string Execute([Description("要执行的命令行")] string command)
    {
        Console.WriteLine($"  ⚙️ （假 shell）将执行: {command}");
        return $"（演示用假 shell，未真正执行）命令已受理：{command}";
    }
```

- [ ] **Step 2: Program.cs 全量替换为接线版**

`maf/Program.cs` 新内容（保留原文件头注释，只在其后追加一段审批回路说明）：

```csharp
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
            .Where(r => r.RequiresConfirmation)
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
```

- [ ] **Step 3: 编译**

Run: `dotnet build maf/YkTako.MafL2.csproj --nologo`
Expected: Build succeeded（0 error；若有 warning 先记录不阻塞）。

- [ ] **Step 4: 回归单测（确认没把工程结构改坏）**

Run: `dotnet test maf/tests/YkTako.MafL2.Tests.csproj --nologo`
Expected: 10 passed。

- [ ] **Step 5: 真模型冒烟（需要用户同意；会消耗 API 配额）**

先问用户是否同意跑，同意后交互式执行：

```bash
dotnet run --project maf/YkTako.MafL2.csproj
```

Expected（逐条核对并记录到 doc/10）：
1. `执行 echo hi` → 无审批块，直接出现「⚙️ （假 shell）」；
2. `执行 rm -rf /tmp/x` → 出现 `⏸️ 人工审批` 块；输入 `N` → 模型收到拒绝理由并继续；再试一次输入 `y` → 假 shell 回执；
3. `/mode warn` 后重复第 2 条 → 打印 ⚠️ 且直接执行；
4. `/mode block` 后重复第 2 条 → 不问直接拒（模型收到拦截文案）；
5. 若模型不肯调用 `execute`（安全对齐拒答），改用更中性话术或在输入里明确「请调用 execute 工具」；仍不行则记录为冒烟受限并标注。

- [ ] **Step 6: 提交**

```bash
git add maf/src/BasicTools.cs maf/Program.cs
git commit -m "MAF：假 shell + 审批回路接线（ApprovalRequiredAIFunction/ToolApprovalAgent/REPL 回执）"
```

---

### Task 5: README 更新

**Files:**
- Modify: `maf/README.md`

- [ ] **Step 1: 状态行加限定解冻说明**

把 `maf/README.md:3-5` 的状态段落改为：

```markdown
> 状态：**冻结存档**（2026-09-24 起不再推进）。主线已定为 Go/Eino（`yk-tako-eino/`），
> 原 TypeScript 实现已删除；下文中"TS 版"均指这份已删除的 LangGraph.js 实现，
> 机制对照描述仍然有效。
>
> 2026-09-26 限定解冻：追加 L1 最小审批回路（`src/Guard.cs` + `Program.cs` 的
> approval 管线接线），用于与 Go/Eino 的 interrupt/resume 做机制对照（见 `doc/10`）。
> 主线仍在 Go/Eino；本目录除该回路外不再演进。
```

- [ ] **Step 2: 机制映射表加一行**

在「机制映射：LangGraph → MAF」表格末尾追加：

```markdown
| L1 工具守卫的审批回路（Go/Eino：interrupt + resume） | 规则引擎 `GuardEngine` 判「自动批/不批」+ MAF `ToolApprovalAgent` 与 FICC 的 approval 管线承担暂停/回执/执行 | `src/Guard.cs` + `Program.cs` |
```

- [ ] **Step 3: 文件列表与实测记录**

文件列表加一行：

```text
src/Guard.cs        L1 规则引擎（移植自 Go/Eino 的 toolguard.go）+ 命中格式化
```

实测记录表格追加（冒烟实跑后按实际补文案，先留行）：

```markdown
| `执行 rm -rf /tmp/x` | execute 命中危险规则 → ⏸️ 人工审批 → 拒绝则模型收到拒绝理由、批准则假 shell 回执 ✅ |
```

- [ ] **Step 4: 提交**

```bash
git add maf/README.md
git commit -m "MAF：README 记录限定解冻与审批回路"
```

---

### Task 6: doc/10 对比文档（MAF vs Eino）

**Files:**
- Create: `doc/10-MAF审批回路与Eino对照.md`

- [ ] **Step 1: 写文档**

结构（内容按冒烟实测补全；设计层结论已定，见 spec）：

```markdown
# 10 - MAF 审批回路与 Eino 对照

> 承接 doc/09 §12：Eino 的 resume 中段（Targets ↔ GetResumeContext）如何相遇。
> 本文回答：MAF 有没有这个问题、补上对应功能后两者差在哪。实现记录在
> maf/src/Guard.cs、maf/Program.cs；设计见 docs/superpowers/specs/2026-09-26-maf-tool-approval-design.md。

## 1. 结论先行
（改前 MAF 无此功能；改后走 MAF 自带 approval 管线；两边的「钥匙+公告板」同构、介质相反）

## 2. MAF 改前为什么没有这个问题
（无 L1 → 无中断 → 无从相遇；框架其实自带 approval 管线）

## 3. 实现记录
（文件清单、三个模式的终端行为、冒烟实测输出摘要）

## 4. 逐点对照
| 维度 | Eino | MAF |
| 钥匙 | point.ID + checkpoint 的 ID↔地址映射 | RequestId（tool_call_id 派生） |
| 公告板 | ctx（addrCtx）| 消息内 Request/Response 内容 + AgentSession.StateBag |
| 写入端 | adkrun.Run 的 targets 循环 | REPL 的 ResolveApprovalsAsync 回执循环 |
| 读取端 | guard wrapper 重入读 GetResumeContext | ToolApprovalAgent 绑定 + FICC 重建执行 |
| 重入 | 执行链原样重跑（无 resume 分支） | FICC 重建 FunctionCall 并执行（框架统一处理） |
| 持久化 | checkpoint 可落盘、跨进程 | 会话内存，进程内 |
| 防伪造 | 无（回执按 ID 直接取） | 只认 surfaced 请求 + 快照比对 |
| 并行多中断 | 每个 wrapper 独立决定 | 同响应全部转请求，TAA 排队逐问 |
| 两端是否同文件 | 否（turn.go / middleware.go） | 是（都在框架内部；我们只写规则与回执循环） |

## 5. §12 的「隐性耦合」在 MAF 的形态
（数据耦合仍在：request id 是隐性契约；但框架用「只认 surfaced request」把契约显式化，
调用方不允许自行构造钥匙——对照 Eino 的 ID 注释误导问题，谁更防呆）

## 6. 本仓库口径
（maf 只做对照实验；主线在 Go/Eino）
```

- [ ] **Step 2: 提交**

```bash
git add doc/10-MAF审批回路与Eino对照.md
git commit -m "doc 10：MAF 审批回路与 Eino interrupt/resume 对照"
```

---

## Self-Review 记录

- **Spec coverage**：三模式语义 → Task 2/4；规则表移植 → Task 2；假 shell → Task 4；automatic-approval 分流 → Task 4；REPL 回执循环 → Task 4；单测 → Task 1-3；README 解冻标注 → Task 5；doc/10 → Task 6。全覆盖。
- **Placeholder scan**：Task 5 的实测记录行与 Task 6 的段落标注了「按实测补全」，这是对运行时观测的显式依赖（冒烟需用户同意），不是未决设计；其余步骤均为完整代码/命令。
- **Type consistency**：`GuardEngine.Guard(string, IDictionary<string, object?>?)`、`ShouldBlock(IReadOnlyList<Finding>)`、`FormatFindings(IReadOnlyList<Finding>, string)`、`TryParseMode(string, out GuardMode)`、`ModeName(GuardMode)` 在 Task 1-4 中签名一致；`Finding` 记录的位置参数顺序在 Task 1 定义、Task 3 测试与 Task 4 使用中一致。
