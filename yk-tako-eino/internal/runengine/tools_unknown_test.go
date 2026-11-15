package runengine

import (
	"context"
	"strings"
	"testing"
)

// unknownToolReplier 是幻觉工具调用的兜底（tools.go）：未装配工具的调用不再炸整场 run
// （eino tool_node.go:928 上抛 not found in toolsNode indexes），改按「错误：」文本回注。
func TestUnknownToolReplier(t *testing.T) {
	out, err := unknownToolReplier(context.Background(), "bash", `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("handler 不该返回错误（错误也要变文本回注）: %v", err)
	}
	if !strings.HasPrefix(out, "错误：") {
		t.Fatalf("回执必须「错误：」开头（19v6-08 §3），得到 %q", out)
	}
	if !strings.Contains(out, "bash") {
		t.Fatalf("回执要点名被幻觉的工具名，得到 %q", out)
	}
}
