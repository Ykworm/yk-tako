// 本文件是存量项目容器布局迁移命令的确定性测试（施工包 §6.3）：
// 旧字节按映射进 content/ 与 runtime/、校验后原子切换、租户级编排组工作现场搬进项目容器、
// 幂等跳过、未知运行时目录拒迁、崩溃产物对账、批量上限、only-id 不存在报错，
// 以及 Git bare 仓与 worktree 一起搬目录后的登记修复。
//
// 迁移入口自己开 PG 连接，所以这些测试用真 PG（隔离临时库）+ 真磁盘；不需要 Redis。
package runengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func migContainer(t *testing.T, root, workspaceID string) string {
	t.Helper()
	dir, err := projectContainerDir(root, testOwner.TenantID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// seedClosedOrchestration 建一场已收口的编排组：创建后把 Task 与组都置终态，
// 迁移准入才不会把它当「未收口」。
func seedClosedOrchestration(t *testing.T, s *Store, wsID string, sessID SessionID, key string) string {
	t.Helper()
	ctx := context.Background()
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, key), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE agent_tasks SET status = ? WHERE orchestration_id = ?`,
		agentTaskStatusSucceeded, orch.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE agent_orchestrations SET status = ? WHERE id = ?`,
		orchStatusSucceeded, orch.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	return orch.OrchestrationID
}

func TestProjectLayoutMigrationMovesLegacyContainer(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := s.workspaceRoot
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	orchID := seedClosedOrchestration(t, s, wsID, sessID, "k_mig")

	container := migContainer(t, root, wsID)
	writeMigFile(t, filepath.Join(container, "report.md"), "报告")
	writeMigFile(t, filepath.Join(container, "notes", "a.txt"), "笔记")
	writeMigFile(t, filepath.Join(container, ".media", "pic.png"), "png")
	writeMigFile(t, filepath.Join(container, ".tako", "bash", "out.txt"), "bash 全文")
	writeMigFile(t, filepath.Join(container, ".tako", "fetch", "page.html"), "<html>")
	writeMigFile(t, filepath.Join(container, ".tako", "search", "s.json"), "{}")
	writeMigFile(t, filepath.Join(container, "MEMORY.md"), "项目记忆")
	tenantRoot := tenantStorageRoot(root, testOwner.TenantID)
	oldGroup := filepath.Join(tenantRoot, ".tako", "orchestrations", orchID, "workspace")
	writeMigFile(t, filepath.Join(oldGroup, "work", "shared", "中间.txt"), "中间产物")

	report, err := RunProjectLayoutMigration(ctx, dsn, root, "")
	if err != nil {
		t.Fatalf("迁移: %v", err)
	}
	if report.Failed() {
		t.Fatalf("迁移报告失败：%+v", report.Results)
	}
	if len(report.Results) != 1 || report.Results[0].WorkspaceID != wsID {
		t.Fatalf("报告 = %+v", report.Results)
	}
	res := report.Results[0]
	if res.Status != "migrated" || res.Files != 8 || res.Orches != 1 {
		t.Fatalf("结果 = %+v（files=%d orches=%d）", res, res.Files, res.Orches)
	}

	content := filepath.Join(container, projectContentDirName)
	runtimeDir := filepath.Join(container, projectRuntimeDirName)
	for _, want := range []string{
		filepath.Join(container, projectLayoutFileName),
		filepath.Join(content, "report.md"),
		filepath.Join(content, "notes", "a.txt"),
		filepath.Join(runtimeDir, "media", "pic.png"),
		filepath.Join(runtimeDir, "tmp", "bash", "out.txt"),
		filepath.Join(runtimeDir, "tmp", "fetch", "page.html"),
		filepath.Join(runtimeDir, "tmp", "search", "s.json"),
		filepath.Join(runtimeDir, "memory", "MEMORY.md"),
		filepath.Join(runtimeDir, "orchestrations", orchID, "workspace", "work", "shared", "中间.txt"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("缺 %s：%v", want, err)
		}
	}
	for _, gone := range []string{
		filepath.Join(container, ".media"),
		filepath.Join(container, ".tako"),
		filepath.Join(container, "MEMORY.md"),
		filepath.Join(container, "report.md"),
		container + projectStagingSuffix,
		container + projectBackupSuffix,
		filepath.Join(tenantRoot, ".tako", "orchestrations", orchID),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("应已不存在：%s（err=%v）", gone, err)
		}
	}
	manifest, err := readProjectMigrationManifest(container)
	if err != nil || manifest == nil {
		t.Fatalf("清单 = %v, %v", manifest, err)
	}
	if manifest.Phase != "switched" || len(manifest.Files) != 8 || manifest.FinishedAt == nil {
		t.Fatalf("清单未收尾：phase=%q files=%d", manifest.Phase, len(manifest.Files))
	}
}

func TestProjectLayoutMigrationSkipsMigratedProject(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := s.workspaceRoot
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "已迁移")
	if err != nil {
		t.Fatal(err)
	}
	content, _, err := ensureProjectLayout(root, testOwner.TenantID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeMigFile(t, filepath.Join(content, "keep.md"), "内容")

	report, err := RunProjectLayoutMigration(ctx, dsn, root, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != "skipped" {
		t.Fatalf("结果 = %+v", report.Results)
	}
	if _, err := os.Stat(filepath.Join(content, "keep.md")); err != nil {
		t.Fatalf("内容被动过：%v", err)
	}
	if _, err := os.Stat(migContainer(t, root, ws.ID) + projectStagingSuffix); !os.IsNotExist(err) {
		t.Fatal("幂等运行不该留下 staging")
	}
}

func TestProjectLayoutMigrationRefusesUnknownRuntimeDir(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := s.workspaceRoot
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "未知")
	if err != nil {
		t.Fatal(err)
	}
	container := migContainer(t, root, ws.ID)
	writeMigFile(t, filepath.Join(container, "keep.md"), "用户内容")
	writeMigFile(t, filepath.Join(container, ".tako", "unknown", "x.bin"), "未知")

	report, err := RunProjectLayoutMigration(ctx, dsn, root, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != "failed" || report.Results[0].Err == nil {
		t.Fatalf("结果 = %+v", report.Results)
	}
	if !strings.Contains(report.Results[0].Err.Error(), ".tako/unknown") {
		t.Fatalf("错误应点名未知目录：%v", report.Results[0].Err)
	}
	// 失败时旧容器原样不动，也不留 staging/backup。
	for _, want := range []string{
		filepath.Join(container, "keep.md"),
		filepath.Join(container, ".tako", "unknown", "x.bin"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("旧容器被动过：%s", want)
		}
	}
	for _, gone := range []string{container + projectStagingSuffix, container + projectBackupSuffix} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("不该留下 %s", gone)
		}
	}
}

// 崩溃对账：只处理认得出身份的 staging／backup；认不出的一律报错、不删。
func TestProjectLayoutMigrationReconcile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	root := s.workspaceRoot
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "对账")
	if err != nil {
		t.Fatal(err)
	}
	container := migContainer(t, root, ws.ID)
	containerRel, err := relToRoot(root, container)
	if err != nil {
		t.Fatal(err)
	}
	staging, backup := container+projectStagingSuffix, container+projectBackupSuffix
	runner := &projectMigrationRunner{root: root}
	row := projectMigrationRow{ID: ws.ID, TenantID: testOwner.TenantID}
	ownMarker := projectMigrationMarker{
		FormatVersion: projectMigrationFormat,
		Kind:          projectMigrationMarkerKind,
		WorkspaceID:   ws.ID,
		TenantID:      testOwner.TenantID,
		Container:     containerRel,
	}

	t.Run("半截 staging（无清单）清掉重来", func(t *testing.T) {
		writeMigFile(t, filepath.Join(container, "old.md"), "旧")
		if err := os.MkdirAll(staging, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMigrationMarker(staging, ownMarker); err != nil {
			t.Fatal(err)
		}
		recovered, err := runner.reconcile(row, container, staging, backup)
		if err != nil || recovered {
			t.Fatalf("reconcile = %v, %v", recovered, err)
		}
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Fatal("半截 staging 应被清掉")
		}
	})

	t.Run("陌生 staging 不删只报错", func(t *testing.T) {
		foreign := ownMarker
		foreign.WorkspaceID = "ws_other"
		if err := os.MkdirAll(staging, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMigrationMarker(staging, foreign); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(staging)
		if _, err := runner.reconcile(row, container, staging, backup); err == nil {
			t.Fatal("陌生 staging 应报错")
		}
		if _, err := os.Stat(staging); err != nil {
			t.Fatal("陌生 staging 不该被删")
		}
	})

	t.Run("清单就绪但正式容器与 backup 都不在", func(t *testing.T) {
		if err := os.MkdirAll(staging, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMigrationMarker(staging, ownMarker); err != nil {
			t.Fatal(err)
		}
		if err := writeProjectMigrationManifest(staging, &projectMigrationManifest{
			FormatVersion: projectMigrationFormat, Phase: "copied",
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(container); err != nil {
			t.Fatal(err)
		}
		defer func() {
			os.RemoveAll(staging)
			os.MkdirAll(container, 0o755)
		}()
		if _, err := runner.reconcile(row, container, staging, backup); err == nil {
			t.Fatal("状态无法解释时应报错")
		}
	})

	t.Run("backup 已改名但正式容器不在：恢复旧容器", func(t *testing.T) {
		writeMigFile(t, filepath.Join(backup, "old.md"), "旧")
		if err := writeMigrationMarker(backup, ownMarker); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(container); err != nil {
			t.Fatal(err)
		}
		recovered, err := runner.reconcile(row, container, staging, backup)
		if err != nil || recovered {
			t.Fatalf("reconcile = %v, %v", recovered, err)
		}
		if _, err := os.Stat(filepath.Join(container, "old.md")); err != nil {
			t.Fatal("旧容器未恢复")
		}
		if _, err := os.Stat(backup); !os.IsNotExist(err) {
			t.Fatal("backup 未清掉")
		}
	})

	t.Run("切换完成后的 backup 按身份清掉", func(t *testing.T) {
		writeMigFile(t, filepath.Join(container, "new.md"), "新")
		writeMigFile(t, filepath.Join(backup, "old.md"), "旧")
		if err := writeMigrationMarker(backup, ownMarker); err != nil {
			t.Fatal(err)
		}
		if _, err := runner.reconcile(row, container, staging, backup); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(backup); !os.IsNotExist(err) {
			t.Fatal("backup 应被清掉")
		}
		if _, err := os.Stat(filepath.Join(container, "new.md")); err != nil {
			t.Fatal("正式容器被动过")
		}
	})
}

func TestProjectLayoutMigrationOnlyIDNotFound(t *testing.T) {
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	if _, err := RunProjectLayoutMigration(context.Background(), dsn, s.workspaceRoot, "ws_missing"); err == nil ||
		!strings.Contains(err.Error(), "项目不存在") {
		t.Fatalf("err = %v，应报项目不存在", err)
	}
}

func TestProjectLayoutMigrationBatchLimit(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	for i := 0; i < projectMigrateBatchLimit+1; i++ {
		if err := s.db.WithContext(ctx).Create(&Workspace{
			ID: fmt.Sprintf("ws_batch_%02d", i), TenantID: testOwner.TenantID,
			UserID: testOwner.UserID, Name: "批量",
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	report, err := RunProjectLayoutMigration(ctx, dsn, s.workspaceRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != projectMigrateBatchLimit {
		t.Fatalf("本批处理 %d 个项目，上限 %d", len(report.Results), projectMigrateBatchLimit)
	}
}

// Git 组：bare 仓与 worktree 一起搬进项目容器后，`git worktree repair` 把两侧的绝对登记
// 从旧租户目录改到新位置（doc/76 §8）。
func TestProjectLayoutMigrationRepairsGitWorktrees(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := s.workspaceRoot
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "Git 迁移")
	if err != nil {
		t.Fatal(err)
	}
	orchID := "or_gitmig"
	if err := s.db.WithContext(ctx).Create(&AgentOrchestration{
		ID: orchID, TenantID: testOwner.TenantID, WorkspaceID: ws.ID,
		SourceSessionID: "s_gitmig", Goal: "g", Status: orchStatusSucceeded,
		FinalTaskID: "at_none", IdempotencyKey: "k_gitmig",
	}).Error; err != nil {
		t.Fatal(err)
	}

	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitMust(t, project, "init", "-b", "main")
	gitMust(t, project, "config", "user.name", "test")
	gitMust(t, project, "config", "user.email", "test@localhost")
	writeMigFile(t, filepath.Join(project, "README.md"), "init")
	gitMust(t, project, "add", "-A")
	gitMust(t, project, "commit", "-m", "init")

	tenantRoot := tenantStorageRoot(root, testOwner.TenantID)
	oldGroup := filepath.Join(tenantRoot, ".tako", "orchestrations", orchID, "workspace")
	oldRepo := filepath.Join(oldGroup, "provider", "repo.git")
	if err := os.MkdirAll(filepath.Join(oldGroup, "provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitMust(t, "", "clone", "--bare", "--no-hardlinks", project, oldRepo)
	gitMust(t, "", "--git-dir="+oldRepo, "remote", "remove", "origin")
	gitMust(t, "", "--git-dir="+oldRepo, "config", "user.name", "tako")
	gitMust(t, "", "--git-dir="+oldRepo, "config", "user.email", "tako@localhost")
	oldWork := filepath.Join(oldGroup, "work", "shared")
	gitMust(t, "", "--git-dir="+oldRepo, "worktree", "add", "-b", "tako/"+orchID+"/shared", oldWork, "HEAD")
	writeMigFile(t, filepath.Join(oldWork, "progress.txt"), "中途")
	gitMust(t, oldWork, "add", "-A")
	gitMust(t, oldWork, "commit", "-m", "progress")

	// 旧容器里同时有用户内容，确保迁移走复制路径而不是空容器初始化。
	container := migContainer(t, root, ws.ID)
	writeMigFile(t, filepath.Join(container, "report.md"), "报告")

	report, err := RunProjectLayoutMigration(ctx, dsn, root, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != "migrated" {
		t.Fatalf("结果 = %+v", report.Results)
	}
	if len(report.Results[0].Warnings) > 0 {
		t.Fatalf("worktree 修复应成功，warnings = %v", report.Results[0].Warnings)
	}

	newGroup := filepath.Join(container, projectRuntimeDirName, "orchestrations", orchID, "workspace")
	newRepo := filepath.Join(newGroup, "provider", "repo.git")
	newWork := filepath.Join(newGroup, "work", "shared")
	top := gitMust(t, newWork, "rev-parse", "--show-toplevel")
	if !samePath(top, newWork) {
		t.Fatalf("worktree top = %q，期望 %q", top, newWork)
	}
	if out := gitMust(t, newWork, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree 不干净：%q", out)
	}
	list := gitMust(t, "", "--git-dir="+newRepo, "worktree", "list", "--porcelain")
	registered := false
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(line, "worktree ") && samePath(strings.TrimPrefix(line, "worktree "), newWork) {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("bare 仓未登记新路径：\n%s", list)
	}
	if _, err := os.Stat(oldGroup); !os.IsNotExist(err) {
		t.Fatalf("旧组目录未清掉：%v", err)
	}
}
