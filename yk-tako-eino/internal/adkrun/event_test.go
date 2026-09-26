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

// TestConsumeEmitsEvents 用脚本事件流验证 consume 的产出顺序与载荷：
// 工具调用 → 看板回执 → 正文 → 子代理正文（T1.5 的事件序列断言）；
// 同时确认只有 root 消息进历史——子代理那条只呈现、不攒。
func TestConsumeEmitsEvents(t *testing.T) {
	callMsg := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: taskboard.ToolList}}})
	boardMsg := schema.ToolMessage("📋 看板", "c1", schema.WithToolName(taskboard.ToolList))
	textMsg := schema.AssistantMessage("收尾", nil)

	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		for _, m := range []*schema.Message{callMsg, boardMsg, textMsg} {
			gen.Send(&adk.AgentEvent{
				AgentName: "yk-tako",
				Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{
					Role:     m.Role,
					ToolName: m.ToolName,
					Message:  m,
				}},
			})
		}
		gen.Send(&adk.AgentEvent{
			AgentName: "worker",
			Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{
				Role:    schema.Assistant,
				Message: schema.AssistantMessage("子代理说", nil),
			}},
		})
	}()

	var got []Event
	msgs, interrupts, err := consume(iter, "yk-tako", func(ev Event) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(interrupts) != 0 {
		t.Fatalf("interrupts 应为空，实际 %v", interrupts)
	}
	if len(got) != 4 || got[0].Kind != EventToolCall || got[1].Kind != EventBoard ||
		got[2].Kind != EventMessage || got[3].Kind != EventMessage {
		t.Fatalf("事件序列 = %+v，want [tool_call board message message]", got)
	}
	// 载荷逐字段钉死；got[3] 的 Agent 断言锁 eventFrom(event.AgentName, ...)：
	// 若误传 root，子代理名字会变成 "yk-tako"，透传就断了。
	if got[0].Agent != "yk-tako" || strings.Join(got[0].ToolNames, ",") != taskboard.ToolList || got[0].Text != "" {
		t.Fatalf("got[0] = %+v，want agent=yk-tako toolNames=[%s] text 为空", got[0], taskboard.ToolList)
	}
	if got[1].Agent != "yk-tako" || got[1].ToolName != taskboard.ToolList || got[1].Text != "📋 看板" {
		t.Fatalf("got[1] = %+v，want agent=yk-tako toolName=%s text=📋 看板", got[1], taskboard.ToolList)
	}
	if got[2].Agent != "yk-tako" || got[2].Text != "收尾" {
		t.Fatalf("got[2] = %+v，want agent=yk-tako text=收尾", got[2])
	}
	if got[3].Agent != "worker" || got[3].Text != "子代理说" {
		t.Fatalf("got[3] = %+v，want agent=worker text=子代理说", got[3])
	}
	if len(msgs) != 3 {
		t.Fatalf("history 应只收 3 条 root 消息（子代理正文被过滤），实际 %d", len(msgs))
	}
}

// TestConsumeEmitsMessageBeforeInterrupt 锁定「同帧消息照常呈现、interrupt 随后收集」：
// 一个 AgentEvent 可以同时带 MessageOutput 与 Interrupted——模型在触发审批前已经吐出的
// 正文（如「要暂停了」）必须照常进 sink，否则用户只看到审批框、看不到为什么暂停。
// 顺序也钉死：presentation 在收 interrupt 之前完成，未来重构若在 interrupt 帧上跳过
// 产出，这个测试立刻红。
func TestConsumeEmitsMessageBeforeInterrupt(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		gen.Send(&adk.AgentEvent{
			AgentName: "yk-tako",
			Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{
				Role:    schema.Assistant,
				Message: schema.AssistantMessage("要暂停了", nil),
			}},
			Action: &adk.AgentAction{Interrupted: &adk.InterruptInfo{
				InterruptContexts: []*adk.InterruptCtx{{ID: "i1", IsRootCause: true}},
			}},
		})
	}()

	var got []Event
	msgs, interrupts, err := consume(iter, "yk-tako", func(ev Event) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 1 || got[0].Kind != EventMessage || got[0].Text != "要暂停了" {
		t.Fatalf("事件 = %+v，want 一条 text=要暂停了的 message", got)
	}
	if len(interrupts) != 1 || interrupts[0].ID != "i1" {
		t.Fatalf("interrupts = %+v，want 一条 id=i1 的根因中断", interrupts)
	}
	if len(msgs) != 1 {
		t.Fatalf("history 应收到 1 条 root 消息，实际 %d", len(msgs))
	}
}
