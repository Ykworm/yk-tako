package runengine

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 线一召回的单测（doc/65 §4.4）：**C3 起 BM25 整体替换了旧 ILIKE**——
// query 整句交给 pg_search（chinese_lindera 分词在扩展内做），Go 侧零切词；
// 排序 = score、证据时间、id。旧「切词规则」测试随旧实现退役，由 BM25 正负路径
// 测试（memory_store_test.go 的 TestMemoryBM25IndexWorks 与本文件）覆盖同等意图：
// 词面有交集才命中、词面不同不命中。
//
// 可见性按新结构（doc/65 §3.1）：user 行跨项目可见；workspace 行只在本项目可见。
// 没有身份的调用退回启动配置的租户、放开 user 维度（bm25MemoryRows 的兜底，
// 与旧 memoryRecallFilter 的无身份行为同形）。
func TestRecallBM25Query(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)
	now := time.Now().Add(-time.Minute)
	old := now.Add(-2 * time.Hour)

	s.addMemory(t, "dev 库是 PostgreSQL 5433 端口", old)
	s.addMemory(t, "dev 库改到 PostgreSQL 5434 端口", now)
	s.addMemory(t, "回答先给结论，再展开项目细节", now)
	s.addMemory(t, "项目选 Go + Gin，理由是单二进制部署", now)
	s.addMemory(t, "调用 read_media_file 读取附件", now)
	s.addMemory(t, "readXmediaYfile 不是工具名", now)

	// **不做正文去重**（正文去重索引已随旧结构取消，doc/65 §4.2）：
	// 5433 与 5434 两行都命中「dev 数据库端口」。
	items, err := s.recallMemoryItems(ctx, "dev 数据库端口")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		var contents []string
		for _, it := range items {
			contents = append(contents, it.Content)
		}
		t.Fatalf("dev 端口召回 = %v, want 两条（5433 与 5434 都在）", contents)
	}
	// 不押两行先后：BM25 有长度归一，「库是」比「库改到」短一分、分就高一点——
	// 同分按证据时间/id 的 tie-break 由 TestRecallEvidenceTimeOrder 钉。
	var contents []string
	for _, it := range items {
		contents = append(contents, it.Content)
	}
	if !contains(contents, "dev 库是 PostgreSQL 5433 端口") || !contains(contents, "dev 库改到 PostgreSQL 5434 端口") {
		t.Fatalf("dev 端口召回 = %v, want 5433 与 5434 两行都在", contents)
	}

	// 特殊字符安全：查询里的下划线按词法走，不把 readXmediaYfile 当通配命中。
	items, err = s.recallMemoryItems(ctx, "read_media_file")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "调用 read_media_file 读取附件" {
		t.Fatalf("下划线查询 = %+v, want 仅真工具名一条", items)
	}

	// 无命中 = 空（词面没有交集就不命中；「周末」不命中「周六」类语义相似同理）。
	if items, err := s.recallMemoryItems(ctx, "紫貂不存在的词"); err != nil || len(items) != 0 {
		t.Fatalf("无命中 = %+v err=%v", items, err)
	}
}

// TestRecallVisibilityTwoBranches：召回的可见性 = 统一两分支（doc/65 §3.1）。
// user 行**跨项目可见**（旧行为是按 workspace 列挡掉——新语义下那不是缺陷而是设计）；
// workspace 行只在本项目可见；别的租户永不可见。
func TestRecallVisibilityTwoBranches(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)
	now := time.Now()

	// 当前调用者（default/u1）的 user 行。
	s.addMemory(t, "用户自己的记忆偏好", now)
	// 同租户同 user 但 workspace 列不同项目 —— user 行**仍然可见**（跨项目）。
	cross := MemoryItem{
		ID: "mem_cross_proj", TenantID: "default", UserID: "u1", Workspace: "另一个项目",
		Layer: MemoryLayerRecall, Scope: MemoryScopeUser, State: MemoryStateActive,
		Source: MemorySourceExtracted, Content: "跨项目的用户记忆", CreatedAt: now,
	}
	if err := s.db.Create(&cross).Error; err != nil {
		t.Fatal(err)
	}
	// 同租户**别的项目**的 workspace 行 —— 不可见。
	otherWs := MemoryItem{
		ID: "mem_other_ws", TenantID: "default", UserID: "u1", Workspace: "别家的项目",
		Layer: MemoryLayerRecall, Scope: MemoryScopeWorkspace, State: MemoryStateActive,
		Source: MemorySourceExtracted, Content: "别家项目的记忆", CreatedAt: now,
	}
	if err := s.db.Create(&otherWs).Error; err != nil {
		t.Fatal(err)
	}
	// **别的租户**的 user 行 —— 不可见。
	otherTenant := MemoryItem{
		ID: "mem_other_tenant", TenantID: "别人家", UserID: "u1", Workspace: "p1",
		Layer: MemoryLayerRecall, Scope: MemoryScopeUser, State: MemoryStateActive,
		Source: MemorySourceExtracted, Content: "别家租户的记忆", CreatedAt: now,
	}
	if err := s.db.Create(&otherTenant).Error; err != nil {
		t.Fatal(err)
	}

	items, err := s.recallMemoryItems(ctx, "记忆")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Content)
	}
	if len(got) != 2 || !contains(got, "用户自己的记忆偏好") || !contains(got, "跨项目的用户记忆") {
		t.Fatalf("召回 = %v, want 只有两行 user 行（跨项目可见；别项目 workspace 行与别租户不可见）", got)
	}

	// 带身份且挂着项目：workspace 分支生效——该项目的 workspace 行可见；
	// user 行仍按 user 分支命中（跨项目，设计如此）。
	wsCtx := withPrincipal(withWorkspace(context.Background(),
		WorkspaceRef{TenantID: "default", SessionID: "s_1", Root: "/tmp/ws/p1", WorkspaceID: "别家的项目"}),
		principal{TenantID: "default", UserID: "u1", Role: "owner"})
	items, err = s.recallMemoryItems(wsCtx, "记忆")
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, it := range items {
		got = append(got, it.Content)
	}
	if !contains(got, "别家项目的记忆") || !contains(got, "用户自己的记忆偏好") || !contains(got, "跨项目的用户记忆") {
		t.Fatalf("带身份后 = %v, want 三行（本项目 workspace 行 + 两行 user 行）", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestRecallEvidenceTimeOrder：同分时按**证据时间**降序（doc/65 §4.4）——
// 证据时间来自 runlog_entries.created_at（source_run_id+source_seq），不是提交时间。
func TestRecallEvidenceTimeOrder(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)

	// 同内容两行（同分）：提交时间新的那行，证据时间反而更早——排序应按证据时间。
	// 证据行的 (session_id, seq) 有唯一约束：两行用不同 seq。
	newSubmitted := s.addMemoryWithEvidence(t, "同内容的两行记忆", time.Now(), "r_evid1", 5, time.Now().Add(-48*time.Hour))
	oldSubmitted := s.addMemoryWithEvidence(t, "同内容的两行记忆", time.Now().Add(-time.Hour), "r_evid2", 6, time.Now().Add(-time.Minute))

	items, err := s.recallMemoryItems(ctx, "同内容的两行记忆")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("召回 = %d 行, want 2", len(items))
	}
	if items[0].ID != oldSubmitted {
		t.Fatalf("第一行 = %s, want 证据时间新的 %s（不是提交时间新的 %s）", items[0].ID, oldSubmitted, newSubmitted)
	}
}

// addMemory 往记忆库插一行（line-1 召回路径的形状：scope=user、tenant=default）。
func (s *Store) addMemory(t *testing.T, content string, at time.Time) {
	t.Helper()
	s.addMemoryWithEvidence(t, content, at, RunID("r_"+at.Format("150405.000000000")), 1, time.Time{})
}

// addMemoryWithEvidence 插一行并可指定证据行与证据时间（证据时间零值 = 不造 runlog 行）。
func (s *Store) addMemoryWithEvidence(t *testing.T, content string, at time.Time, evidRun RunID, evidSeq int64, evidAt time.Time) string {
	t.Helper()
	id, err := newRowID("mem_test_")
	if err != nil {
		t.Fatal(err)
	}
	it := MemoryItem{
		ID:              id,
		TenantID:        "default",
		UserID:          "u1",
		Workspace:       "/tmp/ws-test",
		Layer:           MemoryLayerRecall,
		Scope:           MemoryScopeUser,
		State:           MemoryStateActive,
		Source:          MemorySourceExtracted,
		Content:         content,
		SourceSessionID: "s_seed",
		SourceRunID:     evidRun,
		SourceSeq:       evidSeq,
		CreatedAt:       at,
	}
	if err := s.db.Create(&it).Error; err != nil {
		t.Fatalf("插入记忆 %s: %v", content, err)
	}
	if evidRun != "" && !evidAt.IsZero() {
		en := RunlogEntry{
			ID: "rl_" + id, SessionID: "s_seed", Seq: evidSeq, RunID: evidRun,
			Role: "user", Text: content, CreatedAt: evidAt,
		}
		if err := s.db.Create(&en).Error; err != nil {
			t.Fatalf("插入 runlog 证据行: %v", err)
		}
	}
	return id
}

func TestRecallTopFiveAndRuneCap(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)
	now := time.Now()
	// 7 条行都命中同一个词，注入只取前 5；内容 200 rune 时 600 上限只放得下两条。
	for i := 0; i < 7; i++ {
		head := "zeta" + string(rune('0'+i))
		s.addMemory(t, head+strings.Repeat("长", 200-utf8.RuneCountInString(head)),
			now.Add(time.Duration(i)*time.Second))
	}
	items, err := s.recallMemoryItems(ctx, "zeta")
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
	if strings.Count(block, "\n- ") != hits {
		t.Fatalf("块内条数 != hits（hits=%d）:\n%s", hits, block)
	}

	// HTML 转义：& < > 不原样出现。
	esc, n := renderMemoryBlock([]memoryCandidate{{Content: "<b>&hack</b>"}})
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
