package runengine

import (
	"context"
	"testing"
	"time"

	"yk-tako-eino/internal/observability"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSumRunUsageConversationLedger(t *testing.T) {
	turn := []TurnEntry{
		{Role: "assistant", Agent: "", Text: "a", Usage: &UsageBill{PromptTokens: 10, CompletionTokens: 5}, ToolCalls: []schema.ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", ToolName: "get_time"},
		{Role: "assistant", Agent: "coder", Text: "b", Usage: &UsageBill{PromptTokens: 7, CompletionTokens: 3}},
	}
	got := sumRunUsage(turn)
	if got.Input != 17 || got.Output != 8 {
		t.Fatalf("usage = %+v, want input 17 output 8", got)
	}
	if got.Turns != 2 || got.ToolCalls != 1 {
		t.Fatalf("turns/toolcalls = %d/%d, want 2/1", got.Turns, got.ToolCalls)
	}
}

func TestTurnMessagesRootOnly(t *testing.T) {
	turn := []TurnEntry{
		{Role: "assistant", Agent: "", Text: "root"},
		{Role: "assistant", Agent: "coder", Text: "sub"},
		{Role: "tool", Text: "ignored"},
	}
	root := turnMessages(turn, true)
	if len(root) != 1 || root[0].Content != "root" {
		t.Fatalf("rootOnly = %+v", root)
	}
	all := turnMessages(turn, false)
	if len(all) != 2 {
		t.Fatalf("all = %d messages, want 2", len(all))
	}
}

func TestSplitModelRef(t *testing.T) {
	cases := []struct{ in, provider, model string }{
		{"deepseek/deepseek-chat", "deepseek", "deepseek-chat"},
		{"tencent-coding-plan", "", "tencent-coding-plan"},
		{"", "", ""},
	}
	for _, c := range cases {
		p, m := splitModelRef(c.in)
		if p != c.provider || m != c.model {
			t.Fatalf("splitModelRef(%q) = %q/%q, want %q/%q", c.in, p, m, c.provider, c.model)
		}
	}
}

type stubChat struct{}

func (stubChat) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "ok", ResponseMeta: &schema.ResponseMeta{
		FinishReason: "stop",
		Usage:        &schema.TokenUsage{PromptTokens: 5, CompletionTokens: 2},
	}}, nil
}

func (stubChat) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](2)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "ok", ResponseMeta: &schema.ResponseMeta{
			FinishReason: "stop",
			Usage:        &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 2},
		}}, nil)
	}()
	return sr, nil
}

// TestTraceSpanTree 用内存导出器验证 doc/54 A4/A5/A8 的父子关系与关键属性：
// chat / 步骤 span 挂在 run 根 span 下，chat 带 usage 与 TTFT。
func TestTraceSpanTree(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	}()

	ctx, runSpan := observability.Tracer().Start(context.Background(), "invoke_agent yktako")
	_, step := startStep(ctx, "tako.step.history")
	finishSpan(step, nil)

	m := wrapChatModel(stubChat{}, "deepseek/deepseek-flash")
	sr, err := m.Stream(ctx, []*schema.Message{
		{Role: schema.System, Content: "sys"},
		{Role: schema.User, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		if _, rerr := sr.Recv(); rerr != nil {
			break
		}
	}
	sr.Close()
	runSpan.End()

	deadline := time.Now().Add(2 * time.Second)
	for len(exp.GetSpans()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var chat, hist tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "chat deepseek-flash":
			chat = s
		case "tako.step.history":
			hist = s
		}
	}
	if chat.Name == "" || hist.Name == "" {
		t.Fatalf("missing spans: %+v", exp.GetSpans())
	}
	runID := runSpan.SpanContext().SpanID()
	if chat.Parent.SpanID() != runID || hist.Parent.SpanID() != runID {
		t.Fatalf("parent mismatch: chat=%s history=%s run=%s", chat.Parent.SpanID(), hist.Parent.SpanID(), runID)
	}
	attrs := map[string]attribute.Value{}
	for _, kv := range chat.Attributes {
		attrs[string(kv.Key)] = kv.Value
	}
	if attrs[attrOperation].AsString() != "chat" {
		t.Fatalf("operation = %q", attrs[attrOperation].AsString())
	}
	if attrs[attrInputTok].AsInt64() != 3 || attrs[attrOutputTok].AsInt64() != 2 {
		t.Fatalf("usage = %v/%v", attrs[attrInputTok], attrs[attrOutputTok])
	}
	if _, ok := attrs[attrTTFT]; !ok {
		t.Fatalf("missing TTFT: %+v", chat.Attributes)
	}
}
