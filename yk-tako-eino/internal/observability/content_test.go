package observability

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestMessagesJSONTruncatesLongPart(t *testing.T) {
	long := strings.Repeat("甲", maxTextBytes) // 3 bytes per rune
	body, truncated := MessagesJSON([]*schema.Message{
		{Role: schema.User, Content: long},
		{Role: schema.Assistant, Content: "ok"},
	})
	if !truncated {
		t.Fatal("expected truncation flag for a part over 8 KiB")
	}
	if len(body) > maxAttrBytes+16 {
		t.Fatalf("serialized attribute too large: %d bytes", len(body))
	}
}

func TestMessagesJSONToolParts(t *testing.T) {
	body, truncated := MessagesJSON([]*schema.Message{
		{Role: schema.Assistant, Content: "call", ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "get_time", Arguments: `{"x":1}`}}}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "12:00"},
	})
	if truncated {
		t.Fatal("small messages should not truncate")
	}
	for _, want := range []string{`"tool_call"`, `"get_time"`, `"tool_call_response"`, `12:00`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestParseCapture(t *testing.T) {
	on := []string{"SPAN_ONLY", "span_and_event", " SPAN_ONLY "}
	for _, v := range on {
		if !parseCapture(v) {
			t.Fatalf("%q should enable content capture", v)
		}
	}
	off := []string{"", "NO_CONTENT", "EVENT_ONLY", "nonsense"}
	for _, v := range off {
		if parseCapture(v) {
			t.Fatalf("%q should disable content capture", v)
		}
	}
}
