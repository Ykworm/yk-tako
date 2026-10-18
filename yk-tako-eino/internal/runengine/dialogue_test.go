package runengine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// B1：启动索引的三种状态（RAG off 不碰扩展 / RAG on 缺扩展 fail fast / 装好后建索引且幂等）。
func TestEnsureDialogueIndex(t *testing.T) {
	ctx := context.Background()

	// RAG=off：测试库没装扩展也必须返回 nil（完全不碰扩展）。
	off := newTestStore(t, testLease, testTTL)
	off.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: "/tmp/ws-test"})
	if err := off.EnsureDialogueIndex(ctx); err != nil {
		t.Fatalf("RAG=off EnsureDialogueIndex: %v", err)
	}

	// RAG=on 且测试库未装扩展：启动即退的错误必须点名「未安装」。
	missing := newTestStore(t, testLease, testTTL)
	missing.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: "/tmp/ws-test", RAG: true})
	if err := missing.EnsureDialogueIndex(ctx); err == nil || !strings.Contains(err.Error(), "未安装") {
		t.Fatalf("RAG=on 缺扩展 err = %v, want 未安装", err)
	}

	// 装上扩展（依赖 vector）：建索引成功，二次调用幂等。
	if err := missing.db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_search CASCADE`).Error; err != nil {
		t.Fatalf("CREATE EXTENSION pg_search: %v", err)
	}
	if err := missing.EnsureDialogueIndex(ctx); err != nil {
		t.Fatalf("EnsureDialogueIndex: %v", err)
	}
	if err := missing.EnsureDialogueIndex(ctx); err != nil {
		t.Fatalf("二次 EnsureDialogueIndex: %v", err)
	}
	var n int64
	if err := missing.db.Raw(
		`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'dialogue_chunks_search_idx'`).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pg_search 索引数 = %d, want 1", n)
	}
}

// B1：dialogue 观测端点的读侧（runId 精确查 / 空参最近 20）。
func TestListDialogueChunks(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	now := time.Now()
	for i, rid := range []RunID{"r_a", "r_b", "r_c"} {
		ch := DialogueChunk{
			RunID: rid, SessionID: "s_1", TenantID: "default", Workspace: "/tmp/ws-test",
			Passage: "passage " + string(rid), Runes: 10, Truncated: false,
			IndexedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := s.db.Create(&ch).Error; err != nil {
			t.Fatal(err)
		}
	}
	one, err := s.ListDialogueChunks(ctx, "r_b")
	if err != nil || len(one) != 1 || one[0].RunID != "r_b" || one[0].Passage != "passage r_b" {
		t.Fatalf("按 runId 查 = %+v err=%v", one, err)
	}
	all, err := s.ListDialogueChunks(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("空参 = %d 条 err=%v, want 3", len(all), err)
	}
	if all[0].RunID != "r_c" {
		t.Fatalf("空参首条 = %s, want 最新 r_c", all[0].RunID)
	}
	if none, err := s.ListDialogueChunks(ctx, "r_missing"); err != nil || len(none) != 0 {
		t.Fatalf("未知 runId = %+v err=%v", none, err)
	}
}
