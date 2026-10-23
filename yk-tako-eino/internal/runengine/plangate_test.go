package runengine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestIsMutatingTool 钉 fail-closed 白名单：读类与三把等人/进模式工具放行，其余（含未知新工具、
// 委派工具、MCP 工具）一律当修改类。新增工具时这里红 = 逼人当场决定它在计划模式里的待遇。
func TestIsMutatingTool(t *testing.T) {
	readOnly := []string{
		"get_time", "read_text_file", "read_file", "read_pdf_text", "read_media_file",
		"list_directory", "directory_tree", "get_file_info", "search_files",
		"search_history", "web_search", "tool_search",
		"TaskList", "TaskGet", "TaskCreate", "TaskUpdate",
		"ask_user", "submit_plan", "enter_plan_mode",
	}
	for _, name := range readOnly {
		if isMutatingTool(name) {
			t.Errorf("%q 应判只读（计划模式放行）", name)
		}
	}
	mutating := []string{
		"write_file", "edit_file", "move_file", "create_directory",
		"bash", "memory",
		"researcher", "coder", "mcp__fs__write_file", "unknown_new_tool", "",
	}
	for _, name := range mutating {
		if !isMutatingTool(name) {
			t.Errorf("%q 应判修改类（计划模式拒绝）", name)
		}
	}
}

// TestPlanGuardToolDeniesBeforeInner 钉运行时守卫的两态：开模式命中修改类 → 拒绝文本、内层
// 一次都没被调；关模式 → 原样透传。拒绝是普通结果（err=nil），模型能读到原因继续。
func TestPlanGuardToolDeniesBeforeInner(t *testing.T) {
	ctx := context.Background()
	gate := &planGate{}
	ctx = withPlanGate(ctx, gate)
	inner := &fakeInvokableTool{info: &schema.ToolInfo{Name: "write_file"}, out: "已写入"}
	guarded := &planGuardTool{inner: inner}

	gate.on.Store(true)
	out, err := guarded.InvokableRun(ctx, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 0 {
		t.Fatalf("计划模式里修改类工具被调用了 %d 次", inner.calls)
	}
	if !strings.Contains(out, "计划模式") || !strings.Contains(out, "write_file") {
		t.Fatalf("拒绝文本没说清原因与工具名: %q", out)
	}

	gate.on.Store(false)
	out, err = guarded.InvokableRun(ctx, "{}")
	if err != nil || out != "已写入" || inner.calls != 1 {
		t.Fatalf("放行后应真执行: out=%q calls=%d err=%v", out, inner.calls, err)
	}
}

// TestPlanGuardAllowsReadTools 钉只读工具在计划模式里照常执行（侦察不被误伤）。
func TestPlanGuardAllowsReadTools(t *testing.T) {
	ctx := withPlanGate(context.Background(), &planGate{})
	g := planGateFrom(ctx)
	g.on.Store(true)
	inner := &fakeInvokableTool{info: &schema.ToolInfo{Name: "read_file"}, out: "内容"}
	guarded := &planGuardTool{inner: inner}
	out, err := guarded.InvokableRun(ctx, "{}")
	if err != nil || out != "内容" || inner.calls != 1 {
		t.Fatalf("只读工具应放行: out=%q calls=%d err=%v", out, inner.calls, err)
	}
}

// TestPlanApprovalText 钉批准/驳回注回的字节，Q4 的 resume 对账复用同一组函数。
func TestPlanApprovalText(t *testing.T) {
	if got := planApprovalText(""); got != "方案已批准，按方案执行。" {
		t.Fatalf("未编辑批准文本: %q", got)
	}
	if got := planRevisionText(""); !strings.Contains(got, "方案被驳回") || !strings.Contains(got, "重新提交") {
		t.Fatalf("驳回文本没说清未执行/重交: %q", got)
	}
	if got := planRevisionText("先备份"); !strings.Contains(got, "用户的意见：\n先备份") {
		t.Fatalf("驳回文本丢了用户意见: %q", got)
	}
	got := planApprovalText("第二步改成先备份")
	if !strings.Contains(got, "方案已批准，按方案执行。") || !strings.Contains(got, "用户的修改：\n第二步改成先备份") {
		t.Fatalf("带修改批准文本: %q", got)
	}
}
