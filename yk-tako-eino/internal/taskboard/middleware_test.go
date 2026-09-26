// 本文件：middleware 的 BeforeAgent 注入测试——四个记账工具进 runCtx.Tools，
// 且不改动调用方原有的 runCtx（拷贝语义）。
package taskboard

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
)

// TestBeforeAgentInjectsTools 验证 BeforeAgent 把四个记账工具追加进本轮工具表。
func TestBeforeAgentInjectsTools(t *testing.T) {
	mw, err := New(NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runCtx := &adk.ChatModelAgentContext{}
	_, nRunCtx, err := mw.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent: %v", err)
	}
	if len(nRunCtx.Tools) != 4 {
		t.Fatalf("应注入 4 个工具，实际 %d", len(nRunCtx.Tools))
	}
	if len(runCtx.Tools) != 0 {
		t.Fatal("原 runCtx 不应被改动（拷贝语义）")
	}
}

// TestBeforeAgentNilRunCtx 验证 nil runCtx 原样返回，不炸。
func TestBeforeAgentNilRunCtx(t *testing.T) {
	mw, err := New(NewStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, nRunCtx, err := mw.BeforeAgent(context.Background(), nil)
	if err != nil || nRunCtx != nil {
		t.Fatalf("nil 进应 nil 出: %v %v", nRunCtx, err)
	}
}
