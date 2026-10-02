// 本文件：Handler 这一层的测试——钩子改写 state.Messages，且失败不挡住这一轮。
package mediaoffload

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// TestBeforeModelRewriteState 走两遍钩子：第一遍留原图，第二遍换占位符。
func TestBeforeModelRewriteState(t *testing.T) {
	dir := t.TempDir()
	handler := NewHandler(Config{Store: NewStore(filepath.Join(dir, DefaultDir), 0)})
	state := &adk.ChatModelAgentState{
		Messages: []*schema.Message{schema.UserMessage("看这张图"), toolImageMsg(png)},
	}
	ctx := context.Background()

	if _, out, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil || out.Messages[1] != state.Messages[1] {
		t.Fatalf("第一遍：err=%v，本轮第一次看到图不该改 state", err)
	}
	if _, out, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
		t.Fatalf("第二遍出错: %v", err)
	} else if hasImage(out.Messages[1]) {
		t.Fatal("第二遍该把图换成占位符")
	} else if !strings.Contains(firstText(t, out.Messages[1]), "已落盘") {
		t.Fatalf("占位符文案不对: %q", firstText(t, out.Messages[1]))
	}
}

// TestBeforeModelRewriteStateNilSafe 是空指针不该 panic：没装 Store 时 Handler 本身就是 nil。
func TestBeforeModelRewriteStateNilSafe(t *testing.T) {
	var handler *Handler
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("你好")}}
	ctx, out, err := handler.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil || out != state || ctx == nil {
		t.Fatalf("nil Handler 应该原样放过：err=%v", err)
	}
}
