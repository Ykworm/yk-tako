package runengine

import (
	"context"
	"testing"
	"time"
)

// 这一组钉的是 pipeline 记忆**数据层**的不变量（doc/65 §4.1–§4.4）：
//
//   - 视图：resident_notes 只出 resident 行，recall_items 只出 recall 行；
//   - 可见性：scope='user' 的行跨项目可见，scope='workspace' 的行只在本项目可见；
//     **scope='user' 的行不被 workspace 分支过滤掉**（它在 user 分支上命中）；
//   - 状态机：只进不复活（active → superseded | removed | archived）；
//     left_active_at 只写一次；历史行 content/quote 不原地改；
//     removed_at 只在用户明确删除时写；
//   - 证据时间：按 source_run_id + source_seq 查 runlog_entries.created_at；
//     run 还没完成也取得到（不取 runs.finished_at）；
//   - BM25 索引：真建出来且能查（正路径命中、词面负路径不命中）。
//
// 全部走真 PG（newTestStore 建临时库），不打模型、不用 API Key。

// newMemoryStoreWithSearch 是带 pg_search 扩展的装配（BM25 索引要扩展）。
// newRagStore 的同款做法：隔离库要自己装扩展（依赖 vector）。
func newMemoryStoreWithSearch(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t, testLease, testTTL)
	if err := s.db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_search CASCADE`).Error; err != nil {
		t.Fatalf("CREATE EXTENSION pg_search: %v", err)
	}
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: "/tmp/ws-test", Extract: true, Recall: true})
	return s
}

// memCtx 造一个带身份与工作区的 ctx：tenant=t1、user=u1、当前项目 p1。
func memCtx() context.Context {
	ctx := withPrincipal(context.Background(), principal{TenantID: "t1", UserID: "u1", Role: "owner"})
	return withWorkspace(ctx, WorkspaceRef{TenantID: "t1", SessionID: "s_1", Root: "/tmp/ws/p1", WorkspaceID: "p1"})
}

// seedMem 插一行记忆（不设 ID，让 Store 生成）。
func seedMem(t *testing.T, s *Store, m MemoryItem) MemoryItem {
	t.Helper()
	if m.ID == "" {
		id, err := newRowID("mem_")
		if err != nil {
			t.Fatal(err)
		}
		m.ID = id
	}
	if m.TenantID == "" {
		m.TenantID = "t1"
	}
	if m.UserID == "" {
		m.UserID = "u1"
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	if err := s.db.Create(&m).Error; err != nil {
		t.Fatalf("插记忆: %v", err)
	}
	return m
}

// TestMemoryViewsSeparateLayers：两个视图各自只出自己那层的行。
func TestMemoryViewsSeparateLayers(t *testing.T) {
	ctx := memCtx()
	s := newMemoryStore(t)

	res := seedMem(t, s, MemoryItem{Layer: MemoryLayerResident, Scope: MemoryScopeUser,
		Content: "常驻行", State: MemoryStateActive})
	rec := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "召回行", State: MemoryStateActive})

	var resident []MemoryItem
	if err := s.db.Raw(`SELECT * FROM resident_notes`).Scan(&resident).Error; err != nil {
		t.Fatal(err)
	}
	if len(resident) != 1 || resident[0].ID != res.ID {
		t.Fatalf("resident_notes = %+v, want 只有常驻行 %s", resident, res.ID)
	}

	var recall []MemoryItem
	if err := s.db.Raw(`SELECT * FROM recall_items`).Scan(&recall).Error; err != nil {
		t.Fatal(err)
	}
	if len(recall) != 1 || recall[0].ID != rec.ID {
		t.Fatalf("recall_items = %+v, want 只有召回行 %s", recall, rec.ID)
	}

	// 应用读口按层读，层不对就不出。
	rows, err := s.ListVisibleMemoryItems(ctx, MemoryLayerResident, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != res.ID {
		t.Fatalf("读 resident 层 = %+v, want %s", rows, res.ID)
	}
	rows, err = s.ListVisibleMemoryItems(ctx, MemoryLayerRecall, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != rec.ID {
		t.Fatalf("读 recall 层 = %+v, want %s", rows, rec.ID)
	}
}

// TestMemoryVisibilityTwoBranches：可见性两分支。
//
//	scope='user'       → 跟这个人走，跨项目可见（换项目仍看得到）
//	scope='workspace'  → 只在本项目可见（换项目就看不到）
//
// 并且 scope='user' 的行**不被** workspace 分支过滤掉：它在 user 分支上命中。
func TestMemoryVisibilityTwoBranches(t *testing.T) {
	s := newMemoryStore(t)

	userRow := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Workspace: "p1", Content: "跨项目可见的偏好", State: MemoryStateActive})
	wsRow := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeWorkspace,
		Workspace: "p1", Content: "只在本项目可见的事实", State: MemoryStateActive})
	otherUser := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Workspace: "p1", UserID: "u2", Content: "别人的记忆", State: MemoryStateActive})

	// 在项目 p1：两条都可见（user 行走 user 分支，workspace 行走 workspace 分支），别人的不可见。
	rows, err := s.ListVisibleMemoryItems(memCtx(), MemoryLayerRecall, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got[userRow.ID] || !got[wsRow.ID] {
		t.Fatalf("p1 里可见 = %+v, want user 行与 workspace 行都在", got)
	}
	if got[otherUser.ID] {
		t.Fatalf("p1 里看见了别人的记忆 %s", otherUser.ID)
	}

	// 换到项目 p2：user 行仍可见（跨项目），workspace 行不可见（只在 p1）。
	ctx2 := withWorkspace(withPrincipal(context.Background(), principal{TenantID: "t1", UserID: "u1", Role: "owner"}),
		WorkspaceRef{TenantID: "t1", SessionID: "s_2", Root: "/tmp/ws/p2", WorkspaceID: "p2"})
	rows, err = s.ListVisibleMemoryItems(ctx2, MemoryLayerRecall, 10)
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got[userRow.ID] {
		t.Fatalf("p2 里看不到跨项目可见的 user 行（%s 不在 %+v）", userRow.ID, got)
	}
	if got[wsRow.ID] {
		t.Fatalf("p2 里看到了只属于 p1 的 workspace 行 %s", wsRow.ID)
	}
}

// TestMemoryStateMachineOnlyForward：状态机只进不复活。
//
// active → superseded | removed | archived；archived/removed/superseded 都不许回 active。
// left_active_at 只在首次离开 active 时写一次（后来的更新不覆盖它）。
// 历史行的 content/quote 不原地改。removed_at 只在用户明确删除时写。
func TestMemoryStateMachineOnlyForward(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	row := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "原正文", Quote: "原话", State: MemoryStateActive})

	// 用户改口：新行替代旧行。
	wrote, err := s.WriteMemoryItems(ctx, []MemoryItem{{
		Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "新正文", Quote: "新话",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if wrote.Written != 1 || len(wrote.IDs) != 1 {
		t.Fatalf("写入结果 = %+v, want 写入一行", wrote)
	}
	newID := wrote.IDs[0]
	var newRow MemoryItem
	if err := s.db.Where("id = ?", newID).First(&newRow).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.SupersedeMemoryItem(ctx, row.ID, newRow.ID); err != nil {
		t.Fatal(err)
	}
	var after MemoryItem
	if err := s.db.Where("id = ?", row.ID).First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.State != MemoryStateSuperseded {
		t.Fatalf("旧行 state = %s, want superseded", after.State)
	}
	if after.SupersededBy == nil || *after.SupersededBy != newRow.ID {
		t.Fatalf("旧行 superseded_by = %v, want %s", after.SupersededBy, newRow.ID)
	}
	if after.LeftActiveAt == nil {
		t.Fatal("离开 active 时没写 left_active_at")
	}
	// 历史行的 content/quote 不原地改。
	if after.Content != "原正文" || after.Quote != "原话" {
		t.Fatalf("历史行被就地改了：content=%q quote=%q", after.Content, after.Quote)
	}
	// removed_at 只在明确删除时写——superseded 不写。
	if after.RemovedAt != nil {
		t.Fatalf("superseded 行写了 removed_at = %v", after.RemovedAt)
	}
	firstLeft := *after.LeftActiveAt

	// 第二次 supersede 不生效（只进不复活）：已被 superseded 的行不再被改。
	if err := s.SupersedeMemoryItem(ctx, row.ID, newRow.ID); err != nil {
		t.Fatal(err)
	}
	var again MemoryItem
	if err := s.db.Where("id = ?", row.ID).First(&again).Error; err != nil {
		t.Fatal(err)
	}
	if again.State != MemoryStateSuperseded {
		t.Fatalf("二次迁移后 state = %s, want 仍 superseded", again.State)
	}
	if again.LeftActiveAt == nil || !again.LeftActiveAt.Equal(firstLeft) {
		t.Fatalf("left_active_at 被覆盖：%v want %v", again.LeftActiveAt, firstLeft)
	}

	// 用户明确删除 → removed + removed_at 留痕。
	del := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "要删的行", State: MemoryStateActive})
	if err := s.RemoveMemoryItem(ctx, del.ID); err != nil {
		t.Fatal(err)
	}
	var deleted MemoryItem
	if err := s.db.Where("id = ?", del.ID).First(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	if deleted.State != MemoryStateRemoved || deleted.RemovedAt == nil {
		t.Fatalf("删除后 = %+v, want removed/removed_at 非空", deleted)
	}
	if deleted.LeftActiveAt == nil {
		t.Fatal("删除时没写 left_active_at")
	}

	// 归档（程序整理退场）：archived，不写 removed_at。
	arch := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "要归档的行", State: MemoryStateActive})
	if err := s.ArchiveMemoryItem(ctx, arch.ID); err != nil {
		t.Fatal(err)
	}
	var archived MemoryItem
	if err := s.db.Where("id = ?", arch.ID).First(&archived).Error; err != nil {
		t.Fatal(err)
	}
	if archived.State != MemoryStateArchived || archived.RemovedAt != nil {
		t.Fatalf("归档后 = %+v, want archived/removed_at 空", archived)
	}

	// 归档/删除的行不再被默认读口看到（默认只认 active）。
	rows, err := s.ListVisibleMemoryItems(memCtx(), MemoryLayerRecall, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == row.ID || r.ID == del.ID || r.ID == arch.ID {
			t.Fatalf("默认读口看到了非 active 行 %s（state=%s）", r.ID, r.State)
		}
	}
}

// TestMemorySourceUserNotReplaced：source='user' 的行不被程序整理替代/淘汰。
// 用户明说的行只能由用户自己改口，程序不得合并/替代/淘汰它。
func TestMemorySourceUserNotReplaced(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	userRow := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "用户明说的行", Source: MemorySourceUser, State: MemoryStateActive})
	modelRow := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "工具写的行", Source: MemorySourceModel, State: MemoryStateActive})

	if err := s.SupersedeMemoryItem(ctx, userRow.ID, modelRow.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveMemoryItem(ctx, userRow.ID); err != nil {
		t.Fatal(err)
	}
	var got MemoryItem
	if err := s.db.Where("id = ?", userRow.ID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.State != MemoryStateActive {
		t.Fatalf("source=user 的行被程序整理动了：state=%s want 仍 active", got.State)
	}

	// 但用户自己明确删除它是可以的。
	if err := s.RemoveMemoryItem(ctx, userRow.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Where("id = ?", userRow.ID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.State != MemoryStateRemoved {
		t.Fatalf("source=user 的行删不掉：state=%s", got.State)
	}
}

// TestMemoryEvidenceTimeFromRunlog：证据时间取 runlog_entries.created_at。
//
// run 尚未完成也能取到（inrun 写入时来源 run 还没定稿，runs.finished_at 是空的），
// 且**不按提交顺序**判新旧——完成顺序与说话顺序相反时，旧证据不覆盖新证据。
func TestMemoryEvidenceTimeFromRunlog(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	// 两场 run：r_first 先说话（seq 小、created_at 早），r_second 后说话（seq 大、created_at 晚）。
	// 但完成顺序相反：r_second 先完成。两场 run 分属两个会话（一会话同时只跑一场）。
	s.mustAcquire(t, "s_1", "r_first", "m_1", "先说的话")
	s.mustAcquire(t, "s_2", "r_second", "m_2", "后说的话")
	// mustAcquire 已经写过每场 run 的 user 行（seq=1），这里补 seq=2 的行做证据指针。
	if err := s.db.Exec(`INSERT INTO runlog_entries (id, session_id, seq, run_id, role, text, created_at)
		VALUES ('log_first', 's_1', 2, 'r_first', 'user', '先说的话', now() - interval '2 hour')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec(`INSERT INTO runlog_entries (id, session_id, seq, run_id, role, text, created_at)
		VALUES ('log_second', 's_2', 2, 'r_second', 'user', '后说的话', now() - interval '1 hour')`).Error; err != nil {
		t.Fatal(err)
	}
	// r_second 先完成（完成顺序与说话顺序相反）。
	if err := s.db.Exec(`UPDATE runs SET finished_at = now() WHERE id = 'r_second'`).Error; err != nil {
		t.Fatal(err)
	}

	first := MemoryItem{SourceRunID: "r_first", SourceSeq: 2}
	second := MemoryItem{SourceRunID: "r_second", SourceSeq: 2}

	at, ok, err := s.MemoryEvidenceTime(ctx, "r_first", 2)
	if err != nil || !ok {
		t.Fatalf("取证据时间 = %v ok=%v err=%v", at, ok, err)
	}
	bt, ok, err := s.MemoryEvidenceTime(ctx, "r_second", 2)
	if err != nil || !ok {
		t.Fatalf("取证据时间 = %v ok=%v err=%v", bt, ok, err)
	}
	if !bt.After(at) {
		t.Fatalf("证据时间顺序不对：first=%v second=%v", at, bt)
	}

	// run 尚未完成（runs 里没有 finished_at）也能取到证据时间。
	pending := MemoryItem{SourceRunID: "r_first", SourceSeq: 2}
	var finishedCount int64
	if err := s.db.Raw(`SELECT COUNT(*) FROM runs WHERE id = 'r_first' AND finished_at IS NOT NULL`).Scan(&finishedCount).Error; err != nil {
		t.Fatal(err)
	}
	if finishedCount != 0 {
		t.Fatalf("r_first 应该还没完成（已定稿 %d 条），否则测不到「未完成也能取证据」", finishedCount)
	}
	if _, ok, err := s.MemoryEvidenceTime(ctx, pending.SourceRunID, pending.SourceSeq); err != nil || !ok {
		t.Fatalf("未完成的 run 取不到证据时间：ok=%v err=%v", ok, err)
	}

	// 完成顺序与说话顺序相反时，旧证据不覆盖新证据：按证据时间判，second 更新。
	cmp, err := s.CompareMemoryEvidence(ctx, second, first)
	if err != nil {
		t.Fatal(err)
	}
	if cmp != 1 {
		t.Fatalf("证据比较 = %d, want 1（second 的证据更新）", cmp)
	}
	cmp, err = s.CompareMemoryEvidence(ctx, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if cmp != -1 {
		t.Fatalf("证据比较 = %d, want -1", cmp)
	}
}

// TestMemoryBM25IndexWorks：BM25 索引真建出来且能查。
//
// 正路径命中（词面有交集）、词面负路径不命中（BM25 是词面检索，不承诺语义相似）。
// 查询形状：`WHERE id @@@ paradedb.match('content', ?)` + `paradedb.score(id)`
// —— 左边写**键列**（id），不是索引名。
func TestMemoryBM25IndexWorks(t *testing.T) {
	ctx := memCtx()
	s := newMemoryStoreWithSearch(t)

	if err := s.EnsureMemoryIndexes(ctx); err != nil {
		t.Fatalf("EnsureMemoryIndexes: %v", err)
	}

	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "dev 库是 PostgreSQL 5433 端口", State: MemoryStateActive})
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "deck 放在 output/deck.html", State: MemoryStateActive})

	var hits []string
	if err := s.db.Raw(
		`SELECT id FROM recall_items WHERE id @@@ paradedb.match('content', ?) ORDER BY paradedb.score(id) DESC`,
		"5433").Scan(&hits).Error; err != nil {
		t.Fatalf("BM25 查询: %v", err)
	}
	if len(hits) != 1 || hits[0] == "" {
		t.Fatalf("BM25 正路径命中 = %v, want 一条", hits)
	}

	// 词面负路径：库里没有这个词，就不该命中。
	var miss []string
	if err := s.db.Raw(
		`SELECT id FROM recall_items WHERE id @@@ paradedb.match('content', ?)`, "紫貂").Scan(&miss).Error; err != nil {
		t.Fatalf("BM25 负路径: %v", err)
	}
	if len(miss) != 0 {
		t.Fatalf("BM25 负路径命中 = %v, want 空", miss)
	}
}
