// 本文件：压缩只改内存，失败退回快照，成功收尾才把接过摘要的那句放进 modelMsgs。
package webui

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/compact"
)

// TestSessionStageRevertAndNote 钉住：第二次 Stage 仍按第一次的快照取舍；失败退回全部旧轮；
// 成功时 noteCompleted 接上的是摘要加原话，不是原来的那句。
func TestSessionStageRevertAndNote(t *testing.T) {
	sess := newSession(context.Background(), "s", openTestDB(t))
	first := []*schema.Message{schema.UserMessage("第一句"), schema.AssistantMessage("第一答", nil)}
	second := []*schema.Message{schema.UserMessage("第二句"), schema.AssistantMessage("第二答", nil)}
	sess.modelReady = true
	sess.modelTurns = []archivedTurn{
		{RunID: "r1", Msgs: first},
		{RunID: "r2", Msgs: second},
	}
	sess.modelMsgs = append(append([]*schema.Message{}, first...), second...)
	sess.live = &liveRun{id: "r3", userText: "第三句"}

	if err := sess.Stage(compact.Commit{KeptRunIDs: []string{"r2"}, CombinedUser: "【上下文摘要】\n短\n\n第三句"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Stage(compact.Commit{KeptRunIDs: nil, CombinedUser: "【上下文摘要】\n更短\n\n第三句"}); err != nil {
		t.Fatal(err)
	}
	if len(sess.modelMsgs) != 0 {
		t.Fatalf("两次压缩后 modelMsgs = %d", len(sess.modelMsgs))
	}
	if len(sess.live.dropped) != 2 || sess.live.dropped[0] != "r1" || sess.live.dropped[1] != "r2" {
		t.Fatalf("被换掉的行 = %+v", sess.live.dropped)
	}
	sess.revertStage()
	if len(sess.modelTurns) != 2 || sess.live.modelUserText != "" {
		t.Fatalf("退回后 turns=%d text=%q", len(sess.modelTurns), sess.live.modelUserText)
	}

	if err := sess.Stage(compact.Commit{KeptRunIDs: []string{"r2"}, CombinedUser: "【上下文摘要】\n短\n\n第三句"}); err != nil {
		t.Fatal(err)
	}
	sess.noteCompleted([]*schema.Message{schema.AssistantMessage("第三答", nil)})
	var users []string
	for _, msg := range sess.modelMsgs {
		if msg != nil && msg.Role == schema.User {
			users = append(users, msg.Content)
		}
	}
	if len(users) != 2 || users[0] != "第二句" || !strings.Contains(users[1], "短") || !strings.HasSuffix(users[1], "第三句") {
		t.Fatalf("用户消息 = %+v", users)
	}
	if sess.staged {
		t.Fatal("收尾后不应再留着快照")
	}
}

// TestSessionNotifyCompact 钉住：running 的 run 会多一帧 compact，正文是短句。
func TestSessionNotifyCompact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sess := newSession(ctx, "s", db)
	sess.events = newEventMem()
	if _, err := db.BeginRun(ctx, "r1", "s", "原话", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	sess.live = &liveRun{id: "r1", userText: "原话"}
	sess.Notify("正在压缩")
	evs, err := db.ReadAfter(ctx, "r1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Kind != "compact" || evs[1].Text != "正在压缩" {
		t.Fatalf("事件 = %+v", evs)
	}
	if evs[0].Text != "原话" {
		t.Fatalf("start 被改成 %q", evs[0].Text)
	}
}
