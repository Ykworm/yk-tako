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
