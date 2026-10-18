package runengine

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// B4：两线都无命中时注入器不动消息（与无记忆功能逐字节相同，doc/29 §6.4/§11 D5）。
func TestInjectRecallNoHitLeavesMessagesIntact(t *testing.T) {
	s := newMemoryStore(t) // Recall=true；两表皆空 = 无命中
	w := &Worker{store: s}
	msgs := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("zzzqqqxyz 无命中探针")}
	before := msgs[1].Content

	w.injectRecall(context.Background(), "s_1", "r_1", msgs)
	if msgs[1].Content != before {
		t.Fatalf("无命中改动了消息:\nbefore=%q\nafter =%q", before, msgs[1].Content)
	}
}
