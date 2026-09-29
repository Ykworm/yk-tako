// 本文件：REPL 内存的 Stage、失败退回，以及成功后用接过摘要的那句。
package compact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestReplMemoryStageAbortAndFinish 钉住：压缩失败退回全部旧轮；成功收尾只留下保留的轮，并接上摘要。
func TestReplMemoryStageAbortAndFinish(t *testing.T) {
	mem := NewReplMemory(nil)
	mem.BeginTurn("第一句")
	_ = mem.FinishTurn([]*schema.Message{schema.AssistantMessage("第一答", nil)})
	mem.BeginTurn("第二句")
	_ = mem.FinishTurn([]*schema.Message{schema.AssistantMessage("第二答", nil)})

	mem.BeginTurn("第三句")
	if err := mem.Stage(Commit{KeptRunIDs: []string{mem.turns[1].id}, CombinedUser: "【上下文摘要】\n短\n\n第三句"}); err != nil {
		t.Fatal(err)
	}
	if len(mem.turns) != 1 || mem.turns[0].id != "repl-2" {
		t.Fatalf("Stage 后 = %+v", mem.turns)
	}
	mem.AbortTurn()
	if len(mem.CompletedRunIDs()) != 2 {
		t.Fatalf("退回后 = %+v", mem.CompletedRunIDs())
	}

	mem.BeginTurn("第三句")
	kept := mem.CompletedRunIDs()[1]
	if err := mem.Stage(Commit{KeptRunIDs: []string{kept}, CombinedUser: "【上下文摘要】\n短\n\n第三句"}); err != nil {
		t.Fatal(err)
	}
	history := mem.FinishTurn([]*schema.Message{schema.AssistantMessage("第三答", nil)})
	if len(mem.CompletedRunIDs()) != 2 {
		t.Fatalf("收尾后轮次 = %+v", mem.CompletedRunIDs())
	}
	var texts []string
	for _, msg := range history {
		if msg.Role == schema.User {
			texts = append(texts, msg.Content)
		}
	}
	if len(texts) != 2 || !strings.Contains(texts[1], "短") || !strings.Contains(texts[1], "第三句") {
		t.Fatalf("用户消息 = %+v", texts)
	}
	if strings.Contains(texts[0], "第一句") {
		t.Fatalf("被换掉的第一轮还在：%+v", texts)
	}
}
