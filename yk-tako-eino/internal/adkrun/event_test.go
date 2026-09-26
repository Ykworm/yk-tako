// 本文件：事件契约的单元测试——eventFrom 的分类口径（《10》§4.2 呈现四类）
// 与 consume 的事件产出顺序逐条钉死；改口径必须同步改这里。
package adkrun

import (
	"strings"
	"testing"

	"yk-tako-eino/internal/taskboard"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// TestEventFrom 覆盖六类输入：工具调用、账本回执（board）、普通工具回执、
// 工具名从 msg 兜底、正文、子代理事件的名字透传。
func TestEventFrom(t *testing.T) {
	callMsg := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read_file"}}})
	replyMsg := schema.ToolMessage("文件内容", "c1", schema.WithToolName("read_file"))
	boardMsg := schema.ToolMessage("📋 看板", "c2", schema.WithToolName(taskboard.ToolList))
	textMsg := schema.AssistantMessage("你好", nil)

	cases := []struct {
		name  string
		agent string
		mv    *adk.MessageVariant
		msg   *schema.Message
		want  Event
	}{
		{
			name: "工具调用", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Assistant, Message: callMsg},
			msg:  callMsg,
			want: Event{Kind: EventToolCall, Agent: "yk-tako", Role: schema.Assistant, ToolNames: []string{"read_file"}},
		},
		{
			name: "账本回执走看板", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Tool, ToolName: taskboard.ToolList, Message: boardMsg},
			msg:  boardMsg,
			want: Event{Kind: EventBoard, Agent: "yk-tako", Role: schema.Tool, ToolName: taskboard.ToolList, Text: "📋 看板"},
		},
		{
			name: "普通工具回执", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Tool, ToolName: "read_file", Message: replyMsg},
			msg:  replyMsg,
			want: Event{Kind: EventToolResult, Agent: "yk-tako", Role: schema.Tool, ToolName: "read_file", Text: "文件内容"},
		},
		{
			name: "工具名从 msg 兜底", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Tool, Message: replyMsg},
			msg:  replyMsg,
			want: Event{Kind: EventToolResult, Agent: "yk-tako", Role: schema.Tool, ToolName: "read_file", Text: "文件内容"},
		},
		{
			name: "调用与正文并存时弃正文", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Assistant, Message: schema.AssistantMessage("我先查一下", []schema.ToolCall{{ID: "c3", Function: schema.FunctionCall{Name: "read_file"}}})},
			msg:  schema.AssistantMessage("我先查一下", []schema.ToolCall{{ID: "c3", Function: schema.FunctionCall{Name: "read_file"}}}),
			want: Event{Kind: EventToolCall, Agent: "yk-tako", Role: schema.Assistant, ToolNames: []string{"read_file"}},
		},
		{
			name: "回执角色只取 msg（mv.Role 只做判定）", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Tool, ToolName: "read_file", Message: &schema.Message{Content: "回执", ToolName: "read_file"}},
			msg:  &schema.Message{Content: "回执", ToolName: "read_file"},
			want: Event{Kind: EventToolResult, Agent: "yk-tako", ToolName: "read_file", Text: "回执"},
		},
		{
			name: "正文", agent: "yk-tako",
			mv:   &adk.MessageVariant{Role: schema.Assistant, Message: textMsg},
			msg:  textMsg,
			want: Event{Kind: EventMessage, Agent: "yk-tako", Role: schema.Assistant, Text: "你好"},
		},
		{
			name: "子代理名字透传", agent: "worker",
			mv:   &adk.MessageVariant{Role: schema.Assistant, Message: textMsg},
			msg:  textMsg,
			want: Event{Kind: EventMessage, Agent: "worker", Role: schema.Assistant, Text: "你好"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := eventFrom(tc.agent, tc.mv, tc.msg)
			if !ok {
				t.Fatal("eventFrom ok = false，want true")
			}
			if got.Kind != tc.want.Kind || got.Agent != tc.want.Agent || got.Role != tc.want.Role ||
				got.Text != tc.want.Text || got.ToolName != tc.want.ToolName ||
				strings.Join(got.ToolNames, ",") != strings.Join(tc.want.ToolNames, ",") {
				t.Fatalf("eventFrom = %+v，want %+v", got, tc.want)
			}
		})
	}
}

// TestEventFromNoContent 验证没有可呈现内容时静默跳过（旧 printMessage 的空分支语义）：
// nil 消息与空内容都不产出事件。
func TestEventFromNoContent(t *testing.T) {
	if _, ok := eventFrom("yk-tako", &adk.MessageVariant{Role: schema.Assistant}, nil); ok {
		t.Fatal("nil 消息不应产出事件")
	}
	if _, ok := eventFrom("yk-tako", &adk.MessageVariant{Role: schema.Assistant}, &schema.Message{Role: schema.Assistant}); ok {
		t.Fatal("空内容不应产出事件")
	}
}
