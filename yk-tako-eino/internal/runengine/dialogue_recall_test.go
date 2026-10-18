package runengine

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// newRagStore 是 B3 的测试装配：建扩展（依赖 vector）+ 建 pg_search 索引 + 开两线。
func newRagStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t, testLease, testTTL)
	if err := s.db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_search CASCADE`).Error; err != nil {
		t.Fatalf("CREATE EXTENSION pg_search: %v", err)
	}
	s.ConfigureMemory(MemoryConfig{
		TenantID: "default", Workspace: "/tmp/ws-test", Extract: true, Recall: true, RAG: true,
	})
	if err := s.EnsureDialogueIndex(context.Background()); err != nil {
		t.Fatalf("EnsureDialogueIndex: %v", err)
	}
	return s
}

func (s *Store) addChunk(t *testing.T, rid RunID, sid SessionID, workspace, passage string) {
	t.Helper()
	ch := DialogueChunk{
		RunID: rid, SessionID: sid, TenantID: "default", Workspace: workspace,
		Passage: passage, Runes: utf8.RuneCountInString(passage), IndexedAt: time.Now(),
	}
	if err := s.db.Create(&ch).Error; err != nil {
		t.Fatalf("插入 chunk %s: %v", rid, err)
	}
}

// B3：@@@ 召回的排序、scope 过滤、条数帽与特殊字符安全（doc/29 §6.2）。
func TestRecallDialogueItems(t *testing.T) {
	ctx := context.Background()
	s := newRagStore(t)
	s.addChunk(t, "r_a", "s_1", "/tmp/ws-test", "dev 库是 PostgreSQL 5433 端口，5433 是本机 dev 库")
	s.addChunk(t, "r_b", "s_1", "/tmp/ws-test", "迁移窗口那天旧实例只读到月底")
	s.addChunk(t, "r_c", "s_2", "/tmp/ws-test", "5433 端口已经确认")
	s.addChunk(t, "r_d", "s_3", "/tmp/ws-test", "这场对话和端口无关")
	s.addChunk(t, "r_out", "s_4", "/tmp/ws-other", "5433 端口在别的工作区")

	items, err := s.recallDialogueItems(ctx, "5433")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("召回条数 = %d, want 2（scope 过滤 + 无关块不命中）", len(items))
	}
	for _, it := range items {
		if it.RunID == "r_out" {
			t.Fatalf("别的工作区块进了召回: %+v", items)
		}
	}
	// 命中块按 BM25 分数降序（不押具体名次：BM25 有长度归一，短块可能压过长块）。
	if items[0].Score <= items[1].Score {
		t.Fatalf("score 未降序: %v", items)
	}

	// 特殊字符不报错（paradedb.match 是词面匹配，不解析查询语法）。
	if _, err := s.recallDialogueItems(ctx, `: * " 5433`); err != nil {
		t.Fatalf("特殊字符查询: %v", err)
	}

	// 条数帽：4 条命中最多回 3 条。
	s.addChunk(t, "r_e", "s_5", "/tmp/ws-test", "5433 5433 又一次")
	s.addChunk(t, "r_f", "s_6", "/tmp/ws-test", "5433 再来一条")
	items, err = s.recallDialogueItems(ctx, "5433")
	if err != nil || len(items) != dialogueRecallItemsMax {
		t.Fatalf("条数帽 = %d err=%v, want %d", len(items), err, dialogueRecallItemsMax)
	}
}

// B3：词面局限契约——库里有「周六」，用「周末」查 0 命中；生造词同样 0（doc/29 §6.3）。
func TestRecallLexicalGap(t *testing.T) {
	ctx := context.Background()
	s := newRagStore(t)
	s.addChunk(t, "r_sat", "s_1", "/tmp/ws-test", "旧实例只读到月底，周六凌晨切到 5433。")

	items, err := s.recallDialogueItems(ctx, "周末")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("「周末」应 0 命中，得 %+v", items)
	}
	items, err = s.recallDialogueItems(ctx, "周六")
	if err != nil || len(items) != 1 {
		t.Fatalf("「周六」应 1 命中，得 %d err=%v", len(items), err)
	}
	items, err = s.recallDialogueItems(ctx, "zzzqqqxyz")
	if err != nil || len(items) != 0 {
		t.Fatalf("生造词应 0 命中，得 %d err=%v", len(items), err)
	}
}

// B3：两线合并块（顺序 memory → dialogue）、每条 300 帽、HTML 转义、开关联动。
func TestRecallMergedBlock(t *testing.T) {
	ctx := context.Background()
	s := newRagStore(t)
	s.addMemory(t, "fact", "project.dev-pg-port", "dev 库是 PostgreSQL 5433 端口", time.Now())
	long := "5433 <b>" + strings.Repeat("汉", dialogueRecallRuneCap+50)
	s.addChunk(t, "r_long", "s_9", "/tmp/ws-test", long)

	res, err := s.Recall(ctx, "5433")
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoryHits != 1 || res.DialogueHits != 1 {
		t.Fatalf("Recall = %+v, want memory=1 dialogue=1", res)
	}
	if !strings.Contains(res.Block, "<memory-context>") || !strings.Contains(res.Block, "<dialogue-context>") {
		t.Fatalf("合并块缺节:\n%s", res.Block)
	}
	if strings.Index(res.Block, "<memory-context>") > strings.Index(res.Block, "<dialogue-context>") {
		t.Fatalf("块顺序应为 memory 在前:\n%s", res.Block)
	}
	lineStart := strings.Index(res.Block, "- (")
	if lineStart < 0 {
		t.Fatalf("dialogue 行缺失:\n%s", res.Block)
	}
	line := res.Block[lineStart:]
	if strings.Contains(line, "<b>") || !strings.Contains(line, "&lt;b&gt;") {
		t.Fatalf("dialogue 转义缺失:\n%s", line)
	}
	// 300 帽：行内「汉」的数量 ≤300。
	if got := strings.Count(line, "汉"); got > dialogueRecallRuneCap {
		t.Fatalf("dialogue 行未截断：%d 个汉字", got)
	}
	if res.InjectedRunes != utf8.RuneCountInString(res.Block) {
		t.Fatalf("InjectedRunes = %d, want %d", res.InjectedRunes, utf8.RuneCountInString(res.Block))
	}

	// 开关：RAG off 时线二不召回（块只剩 memory）；Recall off 时线一不召回。
	cfg := s.MemoryConfig()
	cfg.RAG = false
	s.ConfigureMemory(cfg)
	res, err = s.Recall(ctx, "5433")
	if err != nil || res.DialogueHits != 0 || strings.Contains(res.Block, "<dialogue-context>") {
		t.Fatalf("RAG=off Recall = %+v err=%v", res, err)
	}
	cfg.RAG = true
	cfg.Recall = false
	s.ConfigureMemory(cfg)
	res, err = s.Recall(ctx, "5433")
	if err != nil || res.MemoryHits != 0 || strings.Contains(res.Block, "<memory-context>") {
		t.Fatalf("Recall=off Recall = %+v err=%v", res, err)
	}
}
