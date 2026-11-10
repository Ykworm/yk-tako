package runengine

// search_memory 与 BM25 读侧的确定性测试（doc/65 §4.4）：默认只查 active 未过期
// 的 recall 行；resident 不进普通检索；limit 上限 20；0 命中是正常结果；as_of 现实回放；
// include_history 双向链 + 防环。模型行为不进单测——直接打工具的调用面。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

// newRecallStore 是带 pg_search 扩展**与记忆索引**的装配（BM25 查询要索引在）。
// 召回路径与 search_memory 的公共 fixture。
func newRecallStore(t *testing.T) *Store {
	t.Helper()
	s := newMemoryStoreWithSearch(t)
	if err := s.EnsureMemoryIndexes(context.Background()); err != nil {
		t.Fatalf("EnsureMemoryIndexes: %v", err)
	}
	return s
}

func searchToolFor(t *testing.T, s *Store) tool.InvokableTool {
	t.Helper()
	raw, err := newSearchMemoryTool(s)
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := raw.(tool.InvokableTool)
	if !ok {
		t.Fatalf("工具 %T 不是可调用的 InvokableTool", raw)
	}
	return inv
}

// TestSearchMemoryDefaults：默认只查 active 未过期 recall 行；resident 不进检索；
// limit 上限 20；0 命中是正常结果（不是错误）。
func TestSearchMemoryDefaults(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)
	now := time.Now()

	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "deck 在 output/deck.html", TenantID: "default", UserID: "u1"})
	// 常驻行：**不进**普通检索（doc/65 §4.4——resident 每场必见，不需要查）。
	seedMem(t, s, MemoryItem{Layer: MemoryLayerResident, Scope: MemoryScopeUser,
		Content: "deck 常驻版", TenantID: "default", UserID: "u1"})
	// 过期的召回行：默认查询看不见。
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "deck 过期版", TenantID: "default", UserID: "u1",
		ExpiresAt: ptrTime(now.Add(-time.Hour))})

	inv := searchToolFor(t, s)
	out, err := inv.InvokableRun(ctx, `{"query":"deck"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "output/deck.html") || strings.Contains(out, "常驻版") || strings.Contains(out, "过期版") {
		t.Fatalf("结果 = %q, want 只有一行 active 召回行（常驻与过期的不进）", out)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 {
		t.Fatalf("结果行数 = %d, want 1: %q", n, out)
	}

	// 0 命中是正常结果：不是错误、不抛异常。
	out, err = inv.InvokableRun(ctx, `{"query":"紫貂查不到的词"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "没有命中") {
		t.Fatalf("0 命中回执 = %q, want 明确说没有命中", out)
	}

	// 空 query / 非法 as_of → 错误文案回注（不是 Go error）。
	out, _ = inv.InvokableRun(ctx, `{"query":"  "}`)
	if !strings.Contains(out, "错误：") {
		t.Fatalf("空 query 回执 = %q", out)
	}
	out, _ = inv.InvokableRun(ctx, `{"query":"deck","as_of":"昨天下午"}`)
	if !strings.Contains(out, "RFC3339") {
		t.Fatalf("非法 as_of 回执 = %q", out)
	}
}

// TestSearchMemoryLimitCap：limit 上限 20——传 100 也只回 20 条。
func TestSearchMemoryLimitCap(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)
	for i := 0; i < 21; i++ {
		seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
			Content: "斑马纹记忆条目", TenantID: "default", UserID: "u1"})
	}
	inv := searchToolFor(t, s)
	out, err := inv.InvokableRun(ctx, `{"query":"斑马纹","limit":100}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 20 {
		t.Fatalf("limit=100 回了 %d 行, want 上限 20", n)
	}
}

// TestEnsureMemoryIndexesRequiresExtension：pg_search 缺失 → 明确报错，
// 且错误文案不得建议「设 TAKO_RAG=off」（那只控制 dialogue_chunks，施工包 §5.1）。
func TestEnsureMemoryIndexesRequiresExtension(t *testing.T) {
	s := newTestStore(t, testLease, testTTL) //  fresh 库没装 pg_search
	err := s.EnsureMemoryIndexes(context.Background())
	if err == nil {
		t.Fatalf("缺扩展时 EnsureMemoryIndexes 应报错")
	}
	if !strings.Contains(err.Error(), "pg_search") {
		t.Fatalf("错误文案没点名 pg_search: %v", err)
	}
	if strings.Contains(err.Error(), "TAKO_RAG") {
		t.Fatalf("错误文案不该建议 TAKO_RAG=off（它不管记忆索引）: %v", err)
	}
}

// TestSearchMemoryAsOfReplay：as_of 现实回放（doc/65 §4.4）——
// 现实区间 = [COALESCE(valid_from, created_at), LEAST(expires_at, left_active_at))。
func TestSearchMemoryAsOfReplay(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)
	now := time.Now()

	// 行 A：三天前建立，昨天被替代（left_active_at=昨天）→ as_of=前天 在现实里、今天不在。
	a := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "deck 在 v1.html", TenantID: "default", UserID: "u1",
		CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour),
		LeftActiveAt: ptrTime(now.Add(-24 * time.Hour)), State: MemoryStateSuperseded})
	// 行 B：昨天建立、active → as_of=前天 **不在**（当时它还没出生）。
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "deck 在 v2.html", TenantID: "default", UserID: "u1",
		CreatedAt: now.Add(-24 * time.Hour)})
	// 行 C：上周建立、昨天被**删除** → as_of=前天 在现实里。
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "临时目录放 tmp/", TenantID: "default", UserID: "u1",
		CreatedAt: now.Add(-7 * 24 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour),
		LeftActiveAt: ptrTime(now.Add(-24 * time.Hour)), RemovedAt: ptrTime(now.Add(-24 * time.Hour)),
		State: MemoryStateRemoved})
	_ = a

	inv := searchToolFor(t, s)
	asOf := now.Add(-48 * time.Hour).Format(time.RFC3339)
	out, err := inv.InvokableRun(ctx, `{"query":"deck","as_of":"`+asOf+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deck 在 v1.html") || strings.Contains(out, "deck 在 v2.html") {
		t.Fatalf("as_of=%s 回放 = %q, want 旧行在、后继行不在", asOf, out)
	}
	out, err = inv.InvokableRun(ctx, `{"query":"deck"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deck 在 v2.html") || strings.Contains(out, "deck 在 v1.html") {
		t.Fatalf("默认查询 = %q, want 只有现行行", out)
	}
	// 现实回放也看得见「当时还没删」的行；默认查询看不见 removed 行。
	out, err = inv.InvokableRun(ctx, `{"query":"tmp","as_of":"`+asOf+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "临时目录放 tmp/") {
		t.Fatalf("as_of 回放应含被删前行 = %q", out)
	}
	out, _ = inv.InvokableRun(ctx, `{"query":"tmp"}`)
	if strings.Contains(out, "临时目录放 tmp/") {
		t.Fatalf("默认查询不该回 removed 行 = %q", out)
	}
}

// TestSearchMemoryHistoryChain：include_history 沿 superseded_by **双向**取链、
// 防环、跨层指针能走到常驻行。
func TestSearchMemoryHistoryChain(t *testing.T) {
	ctx := context.Background()
	s := newRecallStore(t)

	// 链：p1 → p2 → cur（recall），cur 的后继是**常驻**行（跨层指针）；
	// resident 再回指 cur 造一个环——遍历必须靠 visited 终止（doc/65 §4.4）。
	p1 := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "链头旧版", TenantID: "default", UserID: "u1", State: MemoryStateSuperseded})
	p2 := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "链中旧版", TenantID: "default", UserID: "u1", State: MemoryStateSuperseded})
	cur := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "现行版", TenantID: "default", UserID: "u1"})
	resident := seedMem(t, s, MemoryItem{Layer: MemoryLayerResident, Scope: MemoryScopeUser,
		Content: "常驻新版", TenantID: "default", UserID: "u1"})
	for _, e := range []struct{ from, to string }{
		{p1.ID, p2.ID}, {p2.ID, cur.ID}, {cur.ID, resident.ID}, {resident.ID, cur.ID},
	} {
		if err := s.db.Exec(`UPDATE memory_items SET superseded_by = ? WHERE id = ?`, e.to, e.from).Error; err != nil {
			t.Fatal(err)
		}
	}

	inv := searchToolFor(t, s)
	out, err := inv.InvokableRun(ctx, `{"query":"现行版","include_history":true}`)
	if err != nil {
		t.Fatal(err)
	}
	// 双向 + 跨层：前驱链（链头/链中）与常驻后继都要在；环被 visited 终止（能跑完就是没挂）。
	for _, want := range []string{"链头旧版", "链中旧版", "常驻新版"} {
		if !strings.Contains(out, want) {
			t.Fatalf("include_history 缺 %q: %q", want, out)
		}
	}
}

// TestBM25MemoryRowsIdentityChannels：两条身份通道都要认（2026-11-10 活体抓到的回归）。
// HTTP 读口把身份放 dataScope（memoryContext → withScope），run 内 ctx 放 principal——
// 只认 principal 时，多租户引擎上预览端点的线一召回恒 0 命中（租户落空回启动配置）。
func TestBM25MemoryRowsIdentityChannels(t *testing.T) {
	s := newRecallStore(t)
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "频道锚词xyzzy", TenantID: "default", UserID: "u1"})
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "别人的频道锚词xyzzy", TenantID: "default", UserID: "u2"})

	// (a) dataScope 通道（HTTP 读口）：user scope 只见本人行。
	ctx := withScope(context.Background(), dataScope{TenantID: "default", UserID: "u1", Scope: "user"})
	rows, err := s.bm25MemoryRows(ctx, "xyzzy", 10, memoryBM25Active, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != "频道锚词xyzzy" {
		t.Fatalf("dataScope user 命中 = %+v, want 本人 1 行", rows)
	}

	// (b) principal 通道（run 内）：同样命中本人行。
	ctx = withPrincipal(context.Background(), principal{TenantID: "default", UserID: "u1"})
	rows, err = s.bm25MemoryRows(ctx, "xyzzy", 10, memoryBM25Active, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("principal 命中 %d 行, want 1", len(rows))
	}

	// (c) scope=tenant（管理员预览）：放开 user 维度，两个人的行都命中。
	ctx = withScope(context.Background(), dataScope{TenantID: "default", Scope: "tenant"})
	rows, err = s.bm25MemoryRows(ctx, "xyzzy", 10, memoryBM25Active, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("scope=tenant 命中 %d 行, want 2", len(rows))
	}

	// (d) 别的租户看不见。
	ctx = withScope(context.Background(), dataScope{TenantID: "t_other", UserID: "u1", Scope: "user"})
	rows, err = s.bm25MemoryRows(ctx, "xyzzy", 10, memoryBM25Active, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("跨租户命中 %d 行, want 0", len(rows))
	}
}
