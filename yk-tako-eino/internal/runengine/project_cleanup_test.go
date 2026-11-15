package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 项目删除按整个容器清（doc/76 §6.4）：content／runtime（组工作区、媒体、项目记忆）与挂在该
// 项目上的会话私有目录一起删；租户技能／工具、用户 USER.md、游离会话目录与别的项目不动。
func TestDeleteWorkspaceRemovesWholeContainerAndKeepsTenantAssets(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	root := s.workspaceRoot
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "Q4")
	if err != nil {
		t.Fatal(err)
	}
	content, runtimeDir, err := ensureProjectLayout(root, testOwner.TenantID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(content, "report.md"), "报告")
	writeFile(filepath.Join(runtimeDir, "orchestrations", "or_1", "workspace", "work", "shared", "a.txt"), "中间")
	writeFile(filepath.Join(runtimeDir, "media", "pic.png"), "png")
	writeFile(filepath.Join(runtimeDir, "memory", "MEMORY.md"), "项目记忆")

	sid := SessionID("s_pc_attached")
	sessDir, err := sessionWorkspace(root, testOwner.TenantID, sid)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(sessDir, "deliverable.md"), "产出")
	if err := s.db.WithContext(ctx).Exec(
		`INSERT INTO sessions (id, title, tenant_id, user_id, visibility, agent_id, workspace_id, created_at)
		 VALUES (?, 'p', ?, ?, 'private', '', ?, now())`,
		sid, testOwner.TenantID, testOwner.UserID, ws.ID).Error; err != nil {
		t.Fatal(err)
	}
	looseSID := SessionID("s_pc_loose")
	looseDir, err := sessionWorkspace(root, testOwner.TenantID, looseSID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(looseDir, "keep.md"), "游离")
	if err := s.db.WithContext(ctx).Exec(
		`INSERT INTO sessions (id, title, tenant_id, user_id, visibility, agent_id, created_at)
		 VALUES (?, 'l', ?, ?, 'private', '', now())`,
		looseSID, testOwner.TenantID, testOwner.UserID).Error; err != nil {
		t.Fatal(err)
	}

	tenantRoot := tenantStorageRoot(root, testOwner.TenantID)
	writeFile(filepath.Join(tenantRoot, "skills", "foo", "SKILL.md"), "技能")
	writeFile(filepath.Join(tenantRoot, "tools", "bar.md"), "工具")
	userHome, err := userDir(root, testOwner.TenantID, testOwner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(userHome, "USER.md"), "用户")

	other, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "另一个")
	if err != nil {
		t.Fatal(err)
	}
	otherContent, _, err := ensureProjectLayout(root, testOwner.TenantID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(otherContent, "keep.md"), "别的项目")

	if err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, ws.ID); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, &ProjectCleanup{}, "workspace_id = ? AND status = ?", ws.ID, projectCleanupPending); n != 1 {
		t.Fatalf("删除事务没写 project_cleanups.pending（count=%d）", n)
	}
	if !s.sqlBool(t, `SELECT workspace_id = ? FROM sessions WHERE id = ?`, ws.ID, sid) {
		t.Fatal("会话行软删后丢了 workspace_id，补偿找不到会话目录")
	}
	if err := s.CleanupProjectStorage(ctx, testOwner.TenantID, ws.ID); err != nil {
		t.Fatalf("物理清理: %v", err)
	}
	if n := s.count(t, &ProjectCleanup{}, "workspace_id = ? AND status = ?", ws.ID, projectCleanupDone); n != 1 {
		t.Fatalf("清理完成后记录不是 done（count=%d）", n)
	}

	container, err := projectContainerDir(root, testOwner.TenantID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(container); !os.IsNotExist(err) {
		t.Fatalf("项目容器还在: %v", err)
	}
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Fatalf("关联会话目录还在: %v", err)
	}
	for _, path := range []string{
		filepath.Join(looseDir, "keep.md"),
		filepath.Join(tenantRoot, "skills", "foo", "SKILL.md"),
		filepath.Join(tenantRoot, "tools", "bar.md"),
		filepath.Join(userHome, "USER.md"),
		filepath.Join(otherContent, "keep.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s 不该被删: %v", path, err)
		}
	}
	if s.count(t, &Workspace{}, "id = ?", other.ID) != 1 {
		t.Fatal("别的项目行被删了")
	}
	if s.count(t, &Workspace{}, "id = ?", ws.ID) != 0 {
		t.Fatal("项目行还在")
	}
}

// 物理清理失败留 pending 与错误，业务删除不回滚；调度补偿再删成功后收口 done。
func TestProjectCleanupPendingAndCompensation(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := s.workspaceRoot
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "Q4")
	if err != nil {
		t.Fatal(err)
	}
	content, _, err := ensureProjectLayout(root, testOwner.TenantID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, ws.ID); err != nil {
		t.Fatal(err)
	}

	// 把作业根挂在一个普通文件下面，模拟「删不掉」：RemoveAll 收到 ENOTDIR。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := secondStore(t, dsn, filepath.Join(blocker, "nested"))
	if err := bad.CleanupProjectStorage(ctx, testOwner.TenantID, ws.ID); err == nil {
		t.Fatal("坏作业根上的清理应当失败")
	}
	var row ProjectCleanup
	if err := s.db.WithContext(ctx).Where("workspace_id = ?", ws.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != projectCleanupPending || row.Error == "" {
		t.Fatalf("失败后记录 = %+v, want pending + 错误", row)
	}
	container, err := projectContainerDir(root, testOwner.TenantID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(container); err != nil {
		t.Fatalf("清理失败时容器不该消失: %v", err)
	}

	ts := &TaskScheduler{store: s}
	ts.compensateProjectCleanups(ctx)
	if err := s.db.WithContext(ctx).Where("workspace_id = ?", ws.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != projectCleanupDone || row.Error != "" {
		t.Fatalf("补偿后记录 = %+v, want done", row)
	}
	if _, err := os.Stat(container); !os.IsNotExist(err) {
		t.Fatalf("补偿后容器还在: %v", err)
	}
}

// doc/75 新增的 preparing／waiting_previous 同样是未收口状态，必须挡住项目删除。
func TestDeleteWorkspaceBlocksPreparingSuccessorGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "k_prep"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE agent_tasks SET status = ? WHERE orchestration_id = ?`,
		agentTaskStatusSucceeded, orch.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE agent_orchestrations SET status = 'preparing' WHERE id = ?`,
		orch.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, wsID); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("preparing 组时删项目 err = %v, want ErrWorkspaceBusy", err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE agent_orchestrations SET status = 'waiting_previous' WHERE id = ?`,
		orch.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, wsID); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("waiting_previous 组时删项目 err = %v, want ErrWorkspaceBusy", err)
	}
	if n := s.count(t, &Workspace{}, "id = ?", wsID); n != 1 {
		t.Fatal("拒绝后项目行没了")
	}
}
