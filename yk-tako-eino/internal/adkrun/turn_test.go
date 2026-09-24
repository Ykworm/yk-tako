package adkrun

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/planning"
)

// TestEnsureNudgeCall 验证提醒回执的 tool call 会被补回助手消息里。
func TestEnsureNudgeCall(t *testing.T) {
	assistant := schema.AssistantMessage("先总结到这", nil)
	nudge := schema.ToolMessage(planning.NudgePrefix+" 还有 1 项", "call_nudge_1", schema.WithToolName(planning.NudgeTool))
	history := []*schema.Message{assistant}
	ensureNudgeCall(history, nudge)
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_nudge_1" {
		t.Fatalf("calls: %+v", assistant.ToolCalls)
	}
	ensureNudgeCall(history, nudge)
	if len(assistant.ToolCalls) != 1 {
		t.Fatal("同一个回执不该补两次")
	}

	other := schema.ToolMessage("2026-09-23", "time", schema.WithToolName("get_current_time"))
	before := len(assistant.ToolCalls)
	ensureNudgeCall(history, other)
	if len(assistant.ToolCalls) != before {
		t.Fatal("普通工具回执不该改助手消息")
	}
}
