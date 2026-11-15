// 本文件钉住 doc/76 §4/§6.2 的 shared 契约（施工包 §10.1 第 3 条）：
// shared 异步组整组共用 work/shared，不直接写用户内容根；isolated 每件事一个 work/<taskId>；
// 中间结果只进 results/<taskId>/<revision>；最终结果经接受与发布准入后才写进 content。
//
// Provider 层用纯文件系统/Git 夹具（不打 PG/Redis/模型）；Store 层一条完整链
// （领取→提交→对账→发布）证明 content 与 runtime 的装配也走同一条边界。
package runengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// sharedGroupHandle 造一个指定模式的纯文件系统组句柄：project 就是商店装配时传入的 content 根。
func sharedGroupHandle(t *testing.T, mode string) (*GroupHandle, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "content")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	o := AgentOrchestration{ID: "or_shared", TenantID: "t1", WorkspaceID: "ws_1", WorkspaceMode: mode}
	g, err := newGroupHandle(root, o, project, newFlockFileLocker(root))
	if err != nil {
		t.Fatalf("newGroupHandle: %v", err)
	}
	return g, project
}

// TestDirectorySharedWorkSiteAndPublicationBoundary 钉住 Directory shared 组：
// 两件事拿到同一棵 work/shared（写此见彼）；接受中间结果只落 results，content 不动；
// 最终结果接受并发布之后 content 才出现变更。
func TestDirectorySharedWorkSiteAndPublicationBoundary(t *testing.T) {
	ctx := context.Background()
	g, project := sharedGroupHandle(t, workspaceModeShared)
	writeProj(t, project, "report.md", "原始", 0o644)
	p := &directoryProvider{}
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatalf("PrepareGroup: %v", err)
	}
	st, err := readWorkspaceGroupState(g.Dir)
	if err != nil || st == nil || st.WorkspaceMode != workspaceModeShared || st.Provider != workspaceProviderDirectory {
		t.Fatalf("组状态不符：%+v / %v", st, err)
	}
	taskA := AgentTask{ID: "at_a", TaskKey: "A"}
	taskB := AgentTask{ID: "at_b", TaskKey: "B"}
	workA, err := p.PrepareTask(ctx, g, taskA, nil)
	if err != nil {
		t.Fatalf("PrepareTask(A): %v", err)
	}
	workB, err := p.PrepareTask(ctx, g, taskB, nil)
	if err != nil {
		t.Fatalf("PrepareTask(B): %v", err)
	}
	wantWork := filepath.Join(g.Dir, "work", "shared")
	if workA.Root != wantWork || workB.Root != wantWork {
		t.Fatalf("shared 现场应同组共用 work/shared：A=%s B=%s want=%s", workA.Root, workB.Root, wantWork)
	}
	if workA.Root == filepath.Join(g.Dir, "work", taskA.ID) {
		t.Fatal("shared 组不能落成每 Task 独立现场")
	}
	// 写此见彼：同一棵树。
	writeProj(t, workA.Root, "a.txt", "共享中间产物", 0o644)
	if b, err := os.ReadFile(filepath.Join(workB.Root, "a.txt")); err != nil || string(b) != "共享中间产物" {
		t.Fatalf("B 应看见 A 写入的共享现场：%q / %v", b, err)
	}
	// 中间结果不进 content。
	if _, err := os.Stat(filepath.Join(project, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("工作现场的写入不应提前进 content")
	}
	resA := dirAccept(t, p, g, taskA, 1,
		TaskResult{Summary: "A 结果", Artifacts: []TaskArtifact{{Path: "a.txt", Kind: "file"}}}, workA)
	if _, err := os.Stat(filepath.Join(g.resultDir(taskA.ID, resA.ResultRevision), "artifacts", "a.txt")); err != nil {
		t.Fatalf("中间结果应固化在 results 下：%v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("接受中间结果后 content 仍不应出现文件")
	}
	// 最终任务 C 仍在同一棵现场上继续；它把 report.md 改掉并新增 final.md。
	taskC := AgentTask{ID: "at_c", TaskKey: "C"}
	workC, err := p.PrepareTask(ctx, g, taskC, []ResultRef{{TaskID: taskA.ID, ResultRevision: resA.ResultRevision}})
	if err != nil {
		t.Fatalf("PrepareTask(C): %v", err)
	}
	if workC.Root != wantWork {
		t.Fatalf("最终任务应复用同一棵 shared 现场：%s", workC.Root)
	}
	writeProj(t, workC.Root, "report.md", "最终", 0o644)
	writeProj(t, workC.Root, "final.md", "汇总", 0o644)
	resC := dirAccept(t, p, g, taskC, 1, TaskResult{Summary: "最终结果", Artifacts: []TaskArtifact{
		{Path: "report.md", Kind: "file"},
		{Path: "final.md", Kind: "file"},
	}}, workC)
	// 接受之后、发布之前：content 仍是原始字节。
	if b, err := os.ReadFile(filepath.Join(project, "report.md")); err != nil || string(b) != "原始" {
		t.Fatalf("接受最终结果不应直接改 content：%q / %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(project, "final.md")); !os.IsNotExist(err) {
		t.Fatal("接受最终结果不应把 final.md 直接写进 content")
	}
	if _, err := p.PreparePublication(ctx, g, resC); err != nil {
		t.Fatalf("PreparePublication: %v", err)
	}
	out, err := p.Publish(ctx, g, resC, 1)
	if err != nil || out.Status != publishOutcomePublished {
		t.Fatalf("发布应 published：%+v / %v", out, err)
	}
	// 发布之后 content 才是最终事实：A 的产物、C 的修改与新增都在。
	for path, want := range map[string]string{
		"a.txt": "共享中间产物", "report.md": "最终", "final.md": "汇总",
	} {
		if b, err := os.ReadFile(filepath.Join(project, path)); err != nil || string(b) != want {
			t.Fatalf("content/%s = %q / %v，want %q", path, b, err, want)
		}
	}
}

// TestDirectoryIsolatedWorkSitesPerTask 钉住 isolated 组：每件事一个 work/<taskId>，
// 互不可见；中间结果同样只进 results，content 不动。
func TestDirectoryIsolatedWorkSitesPerTask(t *testing.T) {
	ctx := context.Background()
	g, project := sharedGroupHandle(t, workspaceModeIsolated)
	writeProj(t, project, "base.txt", "基线", 0o644)
	p := &directoryProvider{}
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatalf("PrepareGroup: %v", err)
	}
	taskA := AgentTask{ID: "at_a", TaskKey: "A"}
	taskB := AgentTask{ID: "at_b", TaskKey: "B"}
	workA, err := p.PrepareTask(ctx, g, taskA, nil)
	if err != nil {
		t.Fatalf("PrepareTask(A): %v", err)
	}
	workB, err := p.PrepareTask(ctx, g, taskB, nil)
	if err != nil {
		t.Fatalf("PrepareTask(B): %v", err)
	}
	if workA.Root == workB.Root {
		t.Fatalf("isolated 现场必须每件事独立：%s", workA.Root)
	}
	if workA.Root != filepath.Join(g.Dir, "work", taskA.ID) || workB.Root != filepath.Join(g.Dir, "work", taskB.ID) {
		t.Fatalf("isolated 现场布局不符：A=%s B=%s", workA.Root, workB.Root)
	}
	// 两件都从基线复制。
	for _, work := range []TaskWork{workA, workB} {
		if b, err := os.ReadFile(filepath.Join(work.Root, "base.txt")); err != nil || string(b) != "基线" {
			t.Fatalf("工作现场应来自基线：%q / %v", b, err)
		}
	}
	writeProj(t, workA.Root, "only-a.txt", "A 的", 0o644)
	if _, err := os.Stat(filepath.Join(workB.Root, "only-a.txt")); !os.IsNotExist(err) {
		t.Fatal("isolated 的 A 写入不应对 B 可见")
	}
	dirAccept(t, p, g, taskA, 1,
		TaskResult{Summary: "A 结果", Artifacts: []TaskArtifact{{Path: "only-a.txt", Kind: "file"}}}, workA)
	if _, err := os.Stat(filepath.Join(project, "only-a.txt")); !os.IsNotExist(err) {
		t.Fatal("中间结果不应提前进 content")
	}
}

// TestGitSharedGroupSingleBranchAndPublication 钉住 Git shared 组：整组一条内部支线
// tako/<orchID>/shared，不按父结果逐条 merge；发布前原项目不变，发布后快进写入。
func TestGitSharedGroupSingleBranchAndPublication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "content")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitMust(t, project, "init", "-b", "main")
	gitMust(t, project, "config", "user.name", "test")
	gitMust(t, project, "config", "user.email", "test@localhost")
	writeProj(t, project, "README.md", "init", 0o644)
	writeProj(t, project, "shared.txt", "原", 0o644)
	gitMust(t, project, "add", "-A")
	gitMust(t, project, "commit", "-m", "init")
	baseHead := gitMust(t, project, "rev-parse", "HEAD")

	o := AgentOrchestration{ID: "or_shared", TenantID: "t1", WorkspaceID: "ws_1", WorkspaceMode: workspaceModeShared}
	g, err := newGroupHandle(root, o, project, newFlockFileLocker(root))
	if err != nil {
		t.Fatalf("newGroupHandle: %v", err)
	}
	p := &gitProvider{}
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatalf("PrepareGroup: %v", err)
	}
	taskA := AgentTask{ID: "at_a", TaskKey: "A"}
	taskB := AgentTask{ID: "at_b", TaskKey: "B"}
	workA, err := p.PrepareTask(ctx, g, taskA, nil)
	if err != nil {
		t.Fatalf("PrepareTask(A): %v", err)
	}
	wantWork := filepath.Join(g.Dir, "work", "shared")
	if workA.Root != wantWork {
		t.Fatalf("Git shared 现场应为 work/shared：%s", workA.Root)
	}
	wantBranch := "tako/or_shared/shared"
	if got := gitMust(t, workA.Root, "rev-parse", "--abbrev-ref", "HEAD"); got != wantBranch {
		t.Fatalf("shared 组应只有一条内部支线：%q != %q", got, wantBranch)
	}
	writeProj(t, workA.Root, "feature.txt", "A 的功能", 0o644)
	gitCommitAll(t, workA.Root, "feature A")
	// B 与 A 同一 worktree：现场已存在时直接复用（幂等），不重建、不 merge 父结果。
	workB, err := p.PrepareTask(ctx, g, taskB, []ResultRef{{TaskID: taskA.ID, ResultRevision: 1}})
	if err != nil {
		t.Fatalf("PrepareTask(B): %v", err)
	}
	if workB.Root != wantWork {
		t.Fatalf("B 应复用同一棵 shared 现场：%s", workB.Root)
	}
	if got := gitMust(t, workB.Root, "rev-parse", "--abbrev-ref", "HEAD"); got != wantBranch {
		t.Fatalf("B 应仍在 shared 支线上：%q", got)
	}
	if _, err := os.Stat(filepath.Join(workB.Root, "feature.txt")); err != nil {
		t.Fatalf("B 应看见 A 在同一支线上的提交：%v", err)
	}
	writeProj(t, workB.Root, "second.txt", "B 的", 0o644)
	gitCommitAll(t, workB.Root, "second B")
	resB := gitAccept(t, p, g, taskB, 1, TaskResult{Summary: "B 结果", Artifacts: []TaskArtifact{
		{Path: "feature.txt", Kind: "file"},
		{Path: "second.txt", Kind: "file"},
	}}, workB)
	// 接受（中间结果固化）之后原项目主分支仍未前进。
	if now := gitMust(t, project, "rev-parse", "HEAD"); now != baseHead {
		t.Fatalf("接受结果不应改原项目主分支：%s != %s", now, baseHead)
	}
	if _, err := p.PreparePublication(ctx, g, resB); err != nil {
		t.Fatalf("PreparePublication: %v", err)
	}
	out, err := p.Publish(ctx, g, resB, 1)
	if err != nil || out.Status != publishOutcomePublished {
		t.Fatalf("发布应 published：%+v / %v", out, err)
	}
	if now := gitMust(t, project, "rev-parse", "HEAD"); now != resB.Reference {
		t.Fatalf("原项目主分支应前进到结果提交：%s != %s", now, resB.Reference)
	}
	for path, want := range map[string]string{"feature.txt": "A 的功能", "second.txt": "B 的"} {
		if b, err := os.ReadFile(filepath.Join(project, path)); err != nil || string(b) != want {
			t.Fatalf("原项目 %s = %q / %v，want %q", path, b, err, want)
		}
	}
}

// TestStoreSharedFinalResultPublishesIntoContent 是 Store 层的同一条边界：
// 组行（shared）经真实领取链准备工作现场；领取时的 Root 在 runtime/orchestrations 下；
// 提交与对账都碰不到 content；最终 Task 对账（= 发布快路径）之后 content 才出现结果。
func TestStoreSharedFinalResultPublishesIntoContent(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	content, runtimeDir, err := ensureProjectLayout(s.workspaceRoot, testOwner.TenantID, wsID)
	if err != nil {
		t.Fatalf("ensureProjectLayout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(content, "report.md"), []byte("原始"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := mkTwoLayerCmd(wsID, sessID, "shared-pub")
	cmd.WorkspaceMode = workspaceModeShared
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, cmd, planEnv{})
	if err != nil {
		t.Fatalf("CreateAgentOrchestration: %v", err)
	}
	// shared 组串行（doc/82 §9.2）：一次只领一件，A 收尾后才领 B；两件仍共用同一棵 work/shared 现场。
	claimA := claimOne(t, s, orch.OrchestrationID)
	workA, kind, err := s.prepareTaskWork(ctx, claimA.TaskID)
	if err != nil {
		t.Fatalf("prepareTaskWork(A): %v", err)
	}
	if kind != workspaceProviderDirectory {
		t.Fatalf("Provider = %q, want directory", kind)
	}
	wantWork := filepath.Join(runtimeDir, "orchestrations", orch.OrchestrationID, "workspace", "work", "shared")
	if workA.Root != wantWork {
		t.Fatalf("shared 工作现场应落在 runtime/orchestrations 下：%s != %s", workA.Root, wantWork)
	}
	if err := os.WriteFile(filepath.Join(workA.Root, "a.txt"), []byte("共享中间产物"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(content, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("工作现场写入不应提前进 content")
	}
	revA, err := s.SubmitTaskResult(ctx, claimA.TaskID, claimA.RunID, claimA.Epoch,
		TaskResult{Summary: "A 结果", Artifacts: []TaskArtifact{{Path: "a.txt", Kind: "file"}}}, workA.Root)
	if err != nil {
		t.Fatalf("SubmitTaskResult(A): %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "orchestrations", orch.OrchestrationID, "workspace",
		"results", claimA.TaskID, "1", "artifacts", "a.txt")); err != nil {
		t.Fatalf("中间结果应固化在 results 下（rev %d）：%v", revA, err)
	}
	if _, err := os.Stat(filepath.Join(content, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("接受中间结果后 content 仍不应出现文件")
	}
	setRunStatus(t, s, claimA.RunID, "complete")
	if err := s.ReconcileRun(ctx, claimA.RunID); err != nil {
		t.Fatal(err)
	}
	// A 收尾后领 B：同一 shared 组共用一棵现场。
	claimB := claimOne(t, s, orch.OrchestrationID)
	workB, _, err := s.prepareTaskWork(ctx, claimB.TaskID)
	if err != nil {
		t.Fatalf("prepareTaskWork(B): %v", err)
	}
	if workB.Root != wantWork {
		t.Fatalf("同组应共用一棵现场：%s != %s", workB.Root, wantWork)
	}
	submitOK(t, s, claimB.TaskID, claimB.RunID, claimB.Epoch)
	setRunStatus(t, s, claimB.RunID, "complete")
	if err := s.ReconcileRun(ctx, claimB.RunID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PromoteReadyTasks(ctx); err != nil || n != 1 {
		t.Fatalf("promote n=%d err=%v, want 1（C）", n, err)
	}
	claimC := claimOne(t, s, orch.OrchestrationID)
	workC, _, err := s.prepareTaskWork(ctx, claimC.TaskID)
	if err != nil {
		t.Fatalf("prepareTaskWork(C): %v", err)
	}
	if workC.Root != wantWork {
		t.Fatalf("最终任务应复用同一棵 shared 现场：%s", workC.Root)
	}
	if err := os.WriteFile(filepath.Join(workC.Root, "report.md"), []byte("最终"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workC.Root, "final.md"), []byte("汇总"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitTaskResult(ctx, claimC.TaskID, claimC.RunID, claimC.Epoch,
		TaskResult{Summary: "最终结果", Artifacts: []TaskArtifact{
			{Path: "report.md", Kind: "file"},
			{Path: "final.md", Kind: "file"},
		}}, workC.Root); err != nil {
		t.Fatalf("SubmitTaskResult(C): %v", err)
	}
	// 提交后、对账前：content 仍是原始字节（发布由 ReconcileRun 的快路径触发）。
	if b, err := os.ReadFile(filepath.Join(content, "report.md")); err != nil || string(b) != "原始" {
		t.Fatalf("提交不应直接改 content：%q / %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(content, "final.md")); !os.IsNotExist(err) {
		t.Fatal("提交不应把 final.md 直接写进 content")
	}
	setRunStatus(t, s, claimC.RunID, "complete")
	if err := s.ReconcileRun(ctx, claimC.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := taskStatus(t, s, claimC.TaskID); st != agentTaskStatusSucceeded {
		t.Fatalf("最终 Task 对账后 = %s, want succeeded", st)
	}
	for path, want := range map[string]string{
		"a.txt": "共享中间产物", "report.md": "最终", "final.md": "汇总",
	} {
		b, err := os.ReadFile(filepath.Join(content, path))
		if err != nil || string(b) != want {
			t.Fatalf("content/%s = %q / %v，want %q", path, b, err, want)
		}
	}
}
