using System.Text.Json;
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
}
