package runengine

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A4 召回的单测（doc/29 §6.1）：切词规则是词法召回最容易踩空的一步，逐条钉死；
// SQL 侧用真 PG（newTestStore）验转义、排序与 scope 过滤。
func TestRecallTerms(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// 英文/数字整词 + 中文 2-gram，按原文出现先后。
		{"dev 数据库端口是多少？", []string{"dev", "数据", "据库", "库端", "端口", "口是", "是多", "多少"}},
		{"PostgreSQL 5433 端口", []string{"postgresql", "5433", "端口"}},
		// 单字串保留单字；单字符英文 token 丢弃。
		{"猫", []string{"猫"}},
		{"a x", nil},
		// 去重（大小写归一）与工具名下划线整词。
		{"DEV dev Dev read_media_file", []string{"dev", "read_media_file"}},
		// 标点/空白不产生词。
		{"，。！？ ;:、", nil},
	}
	for _, c := range cases {
		got := recallTerms(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Fatalf("recallTerms(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// 上限 12：长中文串只取前 12 个 2-gram，且按出现先后。
	long := "天地玄黄宇宙洪荒日月盈昃辰宿列张"
	got := recallTerms(long)
	if len(got) != memoryRecallTermsMax {
		t.Fatalf("长串词数 = %d, want %d", len(got), memoryRecallTermsMax)
	}
	if got[0] != "天地" || got[1] != "地玄" {
		t.Fatalf("长串前两词 = %v", got[:2])
	}
}

func (s *Store) addMemory(t *testing.T, kind, topic, content string, at time.Time) {
	t.Helper()
	it := MemoryItem{
		ID:              "mem_test_" + strings.ReplaceAll(topic, ".", "_") + "_" + kind + "_" + at.Format("150405.000000000"),
		TenantID:        "default",
		Workspace:       "/tmp/ws-test",
		Kind:            kind,
		Content:         content,
		Topic:           topic,
		SourceSessionID: "s_seed",
		SourceRunID:     RunID("r_" + at.Format("150405.000000000")),
		SourceSeq:       1,
		CreatedAt:       at,
	}
	if err := s.db.Create(&it).Error; err != nil {
		t.Fatalf("插入记忆 %s: %v", topic, err)
	}
}

func TestRecallMemoryQuery(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	now := time.Now().Add(-time.Minute)
	old := now.Add(-2 * time.Hour)

	s.addMemory(t, "fact", "project.dev-pg-port", "dev 库是 PostgreSQL 5433 端口", old)
	s.addMemory(t, "fact", "project.dev-pg-port", "dev 库改到 PostgreSQL 5434 端口", now)
	s.addMemory(t, "preference", "pref.reply", "回答先给结论，再展开项目细节", now)
	s.addMemory(t, "decision", "decision.stack", "项目选 Go + Gin，理由是单二进制部署", now)
	s.addMemory(t, "episode", "run:r_seed", "这场在查项目 5433 端口", now)
	s.addMemory(t, "fact", "tool.read", "调用 read_media_file 读取附件", now)
	s.addMemory(t, "fact", "tool.other", "readXmediaYfile 不是工具名", now)

	// 其它工作区不可见。
	other := MemoryItem{
		ID: "mem_other_ws", TenantID: "default", Workspace: "/tmp/ws-other",
		Kind: "fact", Content: "dev 库是 PostgreSQL 5433 端口", Topic: "project.dev-pg-port",
		SourceSessionID: "s_seed", SourceRunID: "r_other_ws", SourceSeq: 1, CreatedAt: now,
	}
	if err := s.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}

	// 宽召回 + 同 topic 取最新：旧 5433 行被新 5434 行替换；episode 不参与。
	items, err := s.recallMemoryItems(ctx, recallTerms("dev 数据库端口"))
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, it := range items {
		contents = append(contents, it.Content)
	}
	if len(items) != 1 || !strings.Contains(items[0].Content, "5434") {
		t.Fatalf("dev 端口召回 = %v, want 仅最新 5434 一条", contents)
	}

	// ILIKE 转义：查询词 read_media_file 不能把 readXmediaYfile 当通配命中。
	items, err = s.recallMemoryItems(ctx, recallTerms("read_media_file"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "调用 read_media_file 读取附件" {
		t.Fatalf("下划线转义召回 = %+v, want 仅真工具名一条", items)
	}

	// kind 优先级：同一个词命中 preference/fact/decision 时 preference 排最前。
	items, err = s.recallMemoryItems(ctx, recallTerms("项目"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 2 || items[0].Kind != "preference" {
		t.Fatalf("kind 优先级 = %+v, want preference 第一", items)
	}

	// 无命中 = 空。
	if items, err := s.recallMemoryItems(ctx, recallTerms("完全不存在的词")); err != nil || len(items) != 0 {
		t.Fatalf("无命中 = %+v err=%v", items, err)
	}
}

func TestRecallTopFiveAndRuneCap(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	now := time.Now()
	// 7 条不同 topic 都命中同一个词，注入只取前 5；内容 200 rune 时 600 上限只放得下两条。
	for i := 0; i < 7; i++ {
		head := "zeta" + string(rune('0'+i))
		s.addMemory(t, "fact", "topic.hit"+string(rune('0'+i)),
			head+strings.Repeat("长", memoryContentCap-utf8.RuneCountInString(head)),
			now.Add(time.Duration(i)*time.Second))
	}
	items, err := s.recallMemoryItems(ctx, recallTerms("zeta"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != memoryRecallItemsMax {
		t.Fatalf("候选截取 = %d, want %d", len(items), memoryRecallItemsMax)
	}
	block, hits := renderMemoryBlock(items)
	if hits == 0 || hits > memoryRecallItemsMax {
		t.Fatalf("render hits = %d", hits)
	}
	if n := utf8.RuneCountInString(block); n > memoryRecallRuneCap {
		t.Fatalf("memory 块 %d rune > 上限 %d", n, memoryRecallRuneCap)
	}
	if strings.Count(block, "\n- [") != hits {
		t.Fatalf("块内条数 != hits（hits=%d）:\n%s", hits, block)
	}

	// HTML 转义：& < > 不原样出现。
	esc, n := renderMemoryBlock([]memoryCandidate{{Kind: "fact", Content: "<b>&hack</b>"}})
	if n != 1 || strings.Contains(esc, "<b>") || strings.Contains(esc, "&hack") {
		t.Fatalf("转义块 = %q", esc)
	}
	if !strings.Contains(esc, "&lt;b&gt;&amp;hack&lt;/b&gt;") {
		t.Fatalf("转义结果 = %q", esc)
	}

	// Store.Recall 走同一份开关与渲染；无命中 block 为空串。
	res, err := s.Recall(ctx, "zeta")
	if err != nil {
		t.Fatal(err)
	}
	if res.Block == "" || res.MemoryHits != hits || res.InjectedRunes != utf8.RuneCountInString(res.Block) {
		t.Fatalf("Recall = %+v (render hits=%d)", res, hits)
	}
	if res, err := s.Recall(ctx, "绝对没有的检索词"); err != nil || res.Block != "" || res.MemoryHits != 0 {
		t.Fatalf("无命中 Recall = %+v err=%v", res, err)
	}
}
