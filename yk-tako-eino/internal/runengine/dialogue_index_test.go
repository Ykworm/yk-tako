package runengine

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// B2：passage 组装（doc/29 §5.1）——只收 user / root 终稿 / 子代理终稿，工具行不进；
// 子代理单条截 1000，总帽 4000 从尾部截并记 truncated。
func TestBuildDialoguePassage(t *testing.T) {
	sub := strings.Repeat("研", dialogueSubFinalCap+200)
	entries := []RunlogEntry{
		{Role: "user", Text: "迁移窗口那天旧实例还能写吗？"},
		{Role: "tool_call", Text: "不该出现在 passage"},
		{Role: "tool_result", Text: "也不该出现"},
		{Role: "assistant", Agent: "researcher", Text: sub},
		{Role: "assistant", Text: "不能，5432 只读到月底。"},
	}
	passage, truncated := buildDialoguePassage("s_1", "r_1", entries)
	if truncated {
		t.Fatal("普通 run 不应 truncated")
	}
	for _, want := range []string{
		"[会话 s_1 · 场 r_1]",
		"用户：迁移窗口那天旧实例还能写吗？",
		"助手：不能，5432 只读到月底。",
		"子代理(researcher)：" + strings.Repeat("研", dialogueSubFinalCap),
	} {
		if !strings.Contains(passage, want) {
			t.Fatalf("passage 缺 %q:\n%s", want, passage)
		}
	}
	if strings.Contains(passage, "不该出现") {
		t.Fatalf("工具行进了 passage:\n%s", passage)
	}

	long := append(entries, RunlogEntry{Role: "assistant", Text: strings.Repeat("尾", dialogueChunkCap)})
	cut, truncated := buildDialoguePassage("s_1", "r_2", long)
	if !truncated || utf8.RuneCountInString(cut) != dialogueChunkCap {
		t.Fatalf("超帽 passage: truncated=%v runes=%d, want true/%d",
			truncated, utf8.RuneCountInString(cut), dialogueChunkCap)
	}
	if !strings.HasPrefix(cut, "[会话 s_1 · 场 r_2]") {
		t.Fatalf("截断应保头部: %q", cut[:30])
	}
}

// B2：反查索引 + 删行自愈 + 只索引 complete（doc/29 §5.2/§5.3）。
func TestDialogueIndexerSelfHeal(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	s.completeRun(t, "s_1", "r_1", "第一场的问题")
	s.completeRun(t, "s_2", "r_2", "第二场的问题")

	s.mustAcquire(t, "s_3", "r_3", "m_3", "失败场")
	if _, _, ok, err := s.EndRun(ctx, "s_3", "r_3", 1, "error", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun(error): ok=%v err=%v", ok, err)
	}

	n, err := s.indexDialogueBatch(ctx)
	if err != nil || n != 2 {
		t.Fatalf("首批 = %d err=%v, want 2（error run 不索引）", n, err)
	}
	chunks, err := s.ListDialogueChunks(ctx, "r_1")
	if err != nil || len(chunks) != 1 || !strings.Contains(chunks[0].Passage, "用户：第一场的问题") {
		t.Fatalf("r_1 chunk = %+v err=%v", chunks, err)
	}
	// 已索引的不会被重复扫到。
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 0 {
		t.Fatalf("二次反查 = %d err=%v, want 0", n, err)
	}

	// 删行 = 丢索引；下一轮反查自愈重建。
	if err := s.db.Exec(`DELETE FROM dialogue_chunks WHERE run_id = 'r_1'`).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 1 {
		t.Fatalf("删行后反查 = %d err=%v, want 1", n, err)
	}
	if got := s.count(t, &DialogueChunk{}, "run_id = ?", "r_1"); got != 1 {
		t.Fatalf("自愈后 r_1 chunk 数 = %d, want 1", got)
	}

	// upsert 幂等：同一 run 再写一次仍是 1 行（不撞主键、不翻倍）。
	if err := s.indexDialogueRun(ctx, unindexedRun{ID: "r_1", SessionID: "s_1"}); err != nil {
		t.Fatal(err)
	}
	if got := s.count(t, &DialogueChunk{}, "run_id = ?", "r_1"); got != 1 {
		t.Fatalf("upsert 后 r_1 chunk 数 = %d, want 1", got)
	}
}

// B4：重建入口——清空全部 chunk 后由反查重建，行数还原（doc/29 §11 D3）。
func TestResetDialogueChunks(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	s.completeRun(t, "s_1", "r_1", "第一场")
	s.completeRun(t, "s_2", "r_2", "第二场")
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 2 {
		t.Fatalf("首批 = %d err=%v, want 2", n, err)
	}
	removed, err := s.ResetDialogueChunks(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("ResetDialogueChunks = %d err=%v, want 2", removed, err)
	}
	if got := s.count(t, &DialogueChunk{}, "1 = 1"); got != 0 {
		t.Fatalf("清空后 chunk 数 = %d, want 0", got)
	}
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 2 {
		t.Fatalf("重建批 = %d err=%v, want 2", n, err)
	}
	if got := s.count(t, &DialogueChunk{}, "1 = 1"); got != 2 {
		t.Fatalf("重建后 chunk 数 = %d, want 2", got)
	}
}

// B2：每轮批量帽 20——反查把历史一次全判待索引，必须分批发酵。
func TestDialogueIndexerBatchCap(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	for i := 0; i < dialogueIndexBatch+2; i++ {
		rid := RunID("r_b" + string(rune('a'+i)))
		s.completeRun(t, SessionID("s_b"+string(rune('a'+i))), rid, "批量")
	}
	n, err := s.indexDialogueBatch(ctx)
	if err != nil || n != dialogueIndexBatch {
		t.Fatalf("首批 = %d err=%v, want %d", n, err, dialogueIndexBatch)
	}
	n, err = s.indexDialogueBatch(ctx)
	if err != nil || n != 2 {
		t.Fatalf("第二批 = %d err=%v, want 2", n, err)
	}
}
