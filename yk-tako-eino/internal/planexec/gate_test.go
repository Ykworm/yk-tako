package planexec

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeGateModel 返回预设的工具调用，模拟支持强制 tool_choice 的模型。
type fakeGateModel struct {
	toolName string
	args     string
}

func (f *fakeGateModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			Function: schema.FunctionCall{Name: f.toolName, Arguments: f.args},
		}},
	}, nil
}

func (f *fakeGateModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not implemented")
}

func (f *fakeGateModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

func runGate(t *testing.T, g *Gate, userText string) []*adk.AgentEvent {
	t.Helper()
	iter := g.Run(context.Background(), &adk.AgentInput{
		Messages: []*schema.Message{schema.UserMessage(userText)},
	})
	var events []*adk.AgentEvent
	for {
		ev, ok := iter.Next()
		if !ok {
			return events
		}
		events = append(events, ev)
	}
}

func TestPlanFirstStep(t *testing.T) {
	if got := (&Plan{}).FirstStep(); got != "" {
		t.Fatalf("空计划 FirstStep 应为空串，实际 %q", got)
	}
	p := &Plan{Steps: []string{"第一步", "第二步"}}
	if got := p.FirstStep(); got != "第一步" {
		t.Fatalf("FirstStep = %q", got)
	}
}

func TestPlanJSONRoundTrip(t *testing.T) {
	p := &Plan{Steps: []string{"查时间", "算乘法"}}
	data, err := p.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var q Plan
	if err := q.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	if len(q.Steps) != 2 || q.Steps[0] != "查时间" || q.FirstStep() != "查时间" {
		t.Fatalf("往返后计划不符: %+v", q)
	}
}

func TestGateRespondExits(t *testing.T) {
	g := NewGate(&fakeGateModel{toolName: "respond", args: `{"response":"四"}`}, "- t: d\n")
	events := runGate(t, g, "1+3 等于几")

	if len(events) != 2 {
		t.Fatalf("respond 应产出 2 个事件（答复 + Exit），实际 %d", len(events))
	}
	if events[0].Err != nil {
		t.Fatal(events[0].Err)
	}
	msg, err := events[0].Output.MessageOutput.GetMessage()
	if err != nil || msg == nil || msg.Content != "四" {
		t.Fatalf("答复事件内容不符: msg=%v err=%v", msg, err)
	}
	if events[1].Action == nil || !events[1].Action.Exit {
		t.Fatalf("第二个事件应为 Exit 动作: %+v", events[1].Action)
	}
}

func TestGatePlan(t *testing.T) {
	g := NewGate(&fakeGateModel{toolName: "plan", args: `{"steps":["查时间","算乘法"]}`}, "- t: d\n")
	events := runGate(t, g, "复杂请求")

	if len(events) != 1 {
		t.Fatalf("plan 应产出 1 个事件，实际 %d", len(events))
	}
	if events[0].Err != nil {
		t.Fatal(events[0].Err)
	}
	if events[0].Action != nil && events[0].Action.Exit {
		t.Fatal("plan 不应带 Exit 动作（Loop 需要继续执行）")
	}
	msg, err := events[0].Output.MessageOutput.GetMessage()
	if err != nil || msg == nil {
		t.Fatalf("plan 事件应携带消息: %v", err)
	}
	// 与官方 planner 一致：计划 JSON 放在 content 里，Classify 应识别为计划
	if kind, _, _ := Classify(msg.Content); kind != KindPlan {
		t.Fatalf("计划事件应被 Classify 识别为 KindPlan，实际内容: %s", msg.Content)
	}
}

func TestGateNoToolCall(t *testing.T) {
	g := NewGate(&fakeGateModel{toolName: "", args: ""}, "")
	events := runGate(t, g, "x")
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("无工具调用应报错: %+v", events)
	}
}

func TestGateBuildMessagesInjectsTools(t *testing.T) {
	g := NewGate(&fakeGateModel{}, "- get_current_time: 获取当前时间\n")
	msgs := g.buildMessages([]*schema.Message{schema.UserMessage("现在几点")})
	if len(msgs) != 2 || msgs[0].Role != schema.System {
		t.Fatalf("应为 system + user: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "get_current_time") || !strings.Contains(msgs[0].Content, "分流闸门") {
		t.Fatalf("system 应包含工具清单与分流职责: %s", msgs[0].Content)
	}
}

// 闸门决策只看当前请求，不喂对话历史（防模式锁定，见 buildMessages 注释）。
func TestGateBuildMessagesDropsHistory(t *testing.T) {
	g := NewGate(&fakeGateModel{}, "")
	msgs := g.buildMessages([]*schema.Message{
		schema.UserMessage("1+1 等于几"),
		schema.AssistantMessage("2", nil),
		schema.UserMessage("现在几点"),
	})
	if len(msgs) != 2 {
		t.Fatalf("历史不应进入闸门输入，实际 %d 条: %+v", len(msgs), msgs)
	}
	if msgs[1].Content != "现在几点" {
		t.Fatalf("最后一条应为当前请求，实际 %q", msgs[1].Content)
	}
}
