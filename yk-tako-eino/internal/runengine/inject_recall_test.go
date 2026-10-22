package runengine

import (
	"context"
	"testing"
)

// B4：两线都无命中时注入器不动消息（与无记忆功能逐字节相同，doc/29 §6.4/§11 D5）。
//
// 43 之后注入走 recallIntoCurrentInput（块拼进「本轮输入」那一行，见 worker.go）：
// 无命中 = 那一行逐字节不变 = 整份输入与没开记忆时逐字节相同。
func TestInjectRecallNoHitLeavesMessagesIntact(t *testing.T) {
	s := newMemoryStore(t) // Recall=true；两表皆空 = 无命中
	w := &Worker{store: s}
	history := []ModelMessage{
		{RunID: "r_1", Seq: 1, Role: "user", Text: "zzzqqqxyz 无命中探针"},
	}
	before := history[0].Text

	got := w.recallIntoCurrentInput(context.Background(), "s_1", "r_1", history)
	if len(got) != 1 {
		t.Fatalf("行数 = %d, want 1", len(got))
	}
	if got[0].Text != before {
		t.Fatalf("无命中改动了消息:\nbefore=%q\nafter =%q", before, got[0].Text)
	}
}
