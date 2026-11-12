// workspace_publish_test.go 钉发布互斥的根因：同一把组锁在所有调用路径上是同一个键。
//
// 病灶（2026-11-12 活体）：发布快路径带 run ctx（有租户）、调度慢路径带循环 ctx（无租户），
// 锁键若按 ctx 推导就落成两把不同的锁 → 两路同时进 Provider 半，一路 merge 失败落 blocked，
// 项目却已被另一路写成。修复 = 租户身份钉在编排行上（groupTenantLocker）。
// 这里用真 PG advisory lock + 真 flock 断言：跨 ctx、跨副本都必须排队。
package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// publicationAllDone 断言 Directory 发布记录的每条路径都已 done（记录形状 = 逐路径进度）。
func publicationAllDone(t *testing.T, g *GroupHandle, epoch int64) {
	t.Helper()
	b, err := os.ReadFile(g.publicationPath(epoch))
	if err != nil {
		t.Fatalf("读发布记录: %v", err)
	}
	var rec directoryPublication
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("发布记录损坏: %v", err)
	}
	if len(rec.Entries) == 0 {
		t.Fatal("发布记录没有路径条目")
	}
	for _, e := range rec.Entries {
		if e.Status != dirPubDone {
			t.Fatalf("发布记录路径 %s = %s, want done", e.Path, e.Status)
		}
	}
}

// 同一把组锁：无租户 ctx（调度慢路径）持锁时，带租户 ctx（run 快路径）的另一个副本必须等。
// 修复前两路落成不同的 PG 键与 flock 目录，第二个会直接拿到锁。
func TestGroupLockSameKeyAcrossCtxPaths(t *testing.T) {
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	s.SetWorkspaceRoot(root)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k-lock-ctx", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "锁键一致性", Agent: "researcher", WorkspaceMode: "isolated",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", resp.OrchestrationID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}

	gA, err := s.workspaceGroupHandle(o)
	if err != nil {
		t.Fatal(err)
	}
	relA, err := gA.Locker.Lock(ctx, []string{gA.groupLockKey()}) // 调度路径：裸 ctx
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	// flock 锁目录必须落在租户目录下（身份来自组行，不是 ctx）。
	if _, err := os.Stat(filepath.Join(tenantStorageRoot(root, o.TenantID), ".tako", "locks")); err != nil {
		t.Fatalf("组锁的 flock 目录应在租户目录下: %v", err)
	}

	s2 := secondStoreOn(t, dsn) // 另一个副本
	s2.SetWorkspaceRoot(root)
	gB, err := s2.workspaceGroupHandle(o)
	if err != nil {
		t.Fatal(err)
	}
	runCtx := withWorkspace(ctx, WorkspaceRef{
		TenantID:   o.TenantID,
		Root:       gB.ProjectRoot,
		TenantRoot: tenantStorageRoot(root, o.TenantID),
	})
	got := make(chan error, 1)
	go func() {
		relB, err := gB.Locker.Lock(runCtx, []string{gB.groupLockKey()})
		if err == nil {
			relB()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("A 持锁时 B 不该拿到同一把组锁（B: err=%v）——锁键随 ctx 分裂了", err)
	case <-time.After(700 * time.Millisecond):
	}
	relA()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("A 放锁后 B 应拿到锁: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("A 放锁后 B 一直没拿到锁")
	}
}

// 发布收尾必须等组锁：持锁期间，快路径（ReconcileRun，裸 ctx）与慢路径
// （reconcileOrchestrationPublication，带租户 ctx）都不得进 Provider 半。
// 修复前带租户 ctx 的一路直接穿过别人持有的锁把项目写成；修复后两路排队，放锁才发布，终态唯一。
func TestPublicationWaitsForGroupLockAcrossCtxPaths(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	projDir, c, _ := seedIsolatedSubmitted(t, s, "researcher", "k-pub-lock")
	setRunStatus(t, s, c.RunID, "complete")
	var o AgentOrchestration
	if err := s.db.Where("id = ?", c.OrchestrationID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()}) // 第三个写者持锁
	if err != nil {
		t.Fatal(err)
	}

	runCtx := withWorkspace(ctx, WorkspaceRef{
		TenantID:   o.TenantID,
		Root:       projDir,
		TenantRoot: tenantStorageRoot(s.workspaceRoot, o.TenantID),
	})
	fast := make(chan error, 1)
	go func() { fast <- s.ReconcileRun(ctx, c.RunID) }() // 快路径：run 对账后当场发布
	// 等准入事务落 publishing 再放慢路径进来（否则慢路径读到 '' 直接 no-op，钉不住竞争）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := taskStatus(t, s, c.TaskID); st == agentTaskStatusPublishing {
			break
		}
		if time.Now().After(deadline) {
			rel()
			t.Fatal("快路径一直没把 task 落 publishing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	slow := make(chan error, 1)
	go func() { slow <- s.reconcileOrchestrationPublication(runCtx, o.ID) }() // 慢路径：带租户 ctx

	// 持锁窗口内：准入可以落 publishing（PG 事务不取文件锁），但 Provider 半不得动项目。
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(g.publicationPath(1)); err == nil {
		rel()
		t.Fatal("组锁被别的写者持有，发布却已落进度记录——两路锁键不一致")
	}
	if b, err := os.ReadFile(filepath.Join(projDir, "report.md")); err != nil || string(b) != "原始" {
		rel()
		t.Fatalf("持锁窗口内项目不该被写：%q, %v", b, err)
	}
	if st, _ := taskStatus(t, s, c.TaskID); st != agentTaskStatusPublishing {
		rel()
		t.Fatalf("持锁窗口内 task 应已落 publishing，got %s", st)
	}
	rel()
	for _, ch := range []chan error{fast, slow} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("放锁后发布收尾失败: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("放锁后发布一直没完成")
		}
	}

	if st, code := taskStatus(t, s, c.TaskID); st != agentTaskStatusSucceeded || code != "" {
		t.Fatalf("发布后 task = %s/%s, want succeeded/空", st, code)
	}
	var after AgentOrchestration
	if err := s.db.Where("id = ?", o.ID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.PublicationStatus != publicationStatusPublished {
		t.Fatalf("组发布状态 = %s, want published", after.PublicationStatus)
	}
	if b, err := os.ReadFile(filepath.Join(projDir, "report.md")); err != nil || string(b) != "结果v1" {
		t.Fatalf("项目应为结果字节：%q, %v", b, err)
	}
	publicationAllDone(t, g, 1)
}