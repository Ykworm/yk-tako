// 拍4 确定性测试（施工包 §12.1 第 4 组）：Git bare clone／worktree 绑定、父提交合并、冲突、
// 结果引用导出、主分支前进、脏目录与幂等发布。用宿主 git 在 t.TempDir() 的隔离仓库里真跑，
// 不打 PG/Redis/模型；锁用本机 flock。验收夹具内的本地 commit 只发生在隔离测试仓库。
package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitMust 在 dir 执行 git（受控环境、关 hooks），失败即终止测试。
func gitMust(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v：%v", args, err)
	}
	return strings.TrimSpace(out)
}

// gitTestSetup 造一个 gitProvider + 组句柄 + 已初始化（main 分支、一个提交）的原项目。
func gitTestSetup(t *testing.T) (*gitProvider, *GroupHandle, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
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
	o := AgentOrchestration{ID: "or_1", TenantID: "t1", WorkspaceID: "ws_1"}
	g, err := newGroupHandle(root, o, project, newFlockFileLocker(root))
	if err != nil {
		t.Fatal(err)
	}
	return &gitProvider{}, g, project
}

// gitAccept 在 Task 锁内接受结果（模拟引擎提交链持锁）。
func gitAccept(t *testing.T, p *gitProvider, g *GroupHandle, task AgentTask, rev int, result TaskResult, work TaskWork) ImmutableResult {
	t.Helper()
	rel, err := g.Locker.Lock(context.Background(), []string{g.taskLockKey(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	res, err := p.AcceptResult(context.Background(), g, task, rev, result, work)
	if err != nil {
		t.Fatalf("AcceptResult: %v", err)
	}
	return res
}

// gitCommitAll 把工作现场的全部改动提交（测试夹具用）。
func gitCommitAll(t *testing.T, workRoot, msg string) {
	t.Helper()
	gitMust(t, workRoot, "add", "-A")
	gitMust(t, workRoot, "commit", "-m", msg)
}

// gitMakeParent 造一个父结果：领取 → 改动 → 提交 → 接受，返回结果引用。
func gitMakeParent(t *testing.T, p *gitProvider, g *GroupHandle, taskID, key string, mutate func(workRoot string)) ResultRef {
	t.Helper()
	ctx := context.Background()
	task := AgentTask{ID: taskID, TaskKey: key}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	mutate(work.Root)
	gitCommitAll(t, work.Root, "parent "+key)
	res := gitAccept(t, p, g, task, 1, TaskResult{Summary: "父结果"}, work)
	return ResultRef{TaskID: res.TaskID, ResultRevision: res.ResultRevision}
}

func TestGitPrepareGroupClone(t *testing.T) {
	ctx := context.Background()
	p, g, project := gitTestSetup(t)
	origHead := gitMust(t, project, "rev-parse", "HEAD")
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatalf("PrepareGroup: %v", err)
	}
	st, err := readGitState(g)
	if err != nil || st == nil {
		t.Fatalf("读 Git 组状态：%v / %v", st, err)
	}
	if st.BaseBranch != "main" || st.BaseCommit != origHead {
		t.Fatalf("基线不符：%+v，origHead=%s", st, origHead)
	}
	if bare := gitMust(t, "", g.gitDirArg(), "rev-parse", "--is-bare-repository"); bare != "true" {
		t.Fatalf("中间仓库应为 bare：%s", bare)
	}
	if remotes := gitMust(t, "", g.gitDirArg(), "remote"); remotes != "" {
		t.Fatalf("origin 应已删除：%s", remotes)
	}
	// 幂等：再调不改 createdAt。
	time.Sleep(2 * time.Millisecond)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	st2, _ := readGitState(g)
	if !st2.CreatedAt.Equal(st.CreatedAt) {
		t.Fatal("幂等 PrepareGroup 改了状态时间")
	}
	// 原始主分支不变。
	if now := gitMust(t, project, "rev-parse", "HEAD"); now != origHead {
		t.Fatalf("PrepareGroup 不应改原始 HEAD：%s → %s", origHead, now)
	}
}

func TestGitPrepareGroupRejectsEmptyAndDetached(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitMust(t, project, "init", "-b", "main")
	gitMust(t, project, "config", "user.name", "test")
	gitMust(t, project, "config", "user.email", "test@localhost")
	o := AgentOrchestration{ID: "or_1", TenantID: "t1", WorkspaceID: "ws_1"}
	g, err := newGroupHandle(root, o, project, newFlockFileLocker(root))
	if err != nil {
		t.Fatal(err)
	}
	p := &gitProvider{}
	// 空仓库（无提交）拒绝。
	if err := p.PrepareGroup(ctx, g); err == nil {
		t.Fatal("空仓库应拒绝")
	}
	// 造一个提交后转 Detached HEAD，拒绝。
	writeProj(t, project, "a.txt", "x", 0o644)
	gitMust(t, project, "add", "-A")
	gitMust(t, project, "commit", "-m", "c1")
	head := gitMust(t, project, "rev-parse", "HEAD")
	gitMust(t, project, "checkout", "--detach", head)
	if err := p.PrepareGroup(ctx, g); err == nil {
		t.Fatal("Detached HEAD 应拒绝")
	}
}

func TestGitPrepareTaskWorktreeBinding(t *testing.T) {
	ctx := context.Background()
	p, g, project := gitTestSetup(t)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatalf("PrepareTask: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work.Root, ".git")); err != nil {
		t.Fatalf("worktree 应有 .git：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(work.Root, "README.md")); err != nil || string(b) != "init" {
		t.Fatalf("worktree 应含基线文件：%q / %v", b, err)
	}
	if len(work.ExtraWriteRoots) != 1 || work.ExtraWriteRoots[0] != g.gitRepoDir() {
		t.Fatalf("附加写根应只有本组 repo.git：%+v", work.ExtraWriteRoots)
	}
	if branch := gitMust(t, work.Root, "symbolic-ref", "--short", "HEAD"); branch != gitBranchName("or_1", "t1") {
		t.Fatalf("分支绑定不符：%s", branch)
	}
	// 幂等：再领取返回同一现场。
	work2, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil || work2.Root != work.Root {
		t.Fatalf("幂等 PrepareTask：%+v / %v", work2, err)
	}
	// 恢复。
	work3, err := p.RecoverTaskWork(ctx, g, task, 1)
	if err != nil || work3.Root != work.Root {
		t.Fatalf("RecoverTaskWork：%+v / %v", work3, err)
	}
	_ = project
}

func TestGitAcceptResultAndExport(t *testing.T) {
	ctx := context.Background()
	p, g, project := gitTestSetup(t)
	origHead := gitMust(t, project, "rev-parse", "HEAD")
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "final.md", "# 报告\n正文", 0o644)
	gitCommitAll(t, work.Root, "add report")
	res := gitAccept(t, p, g, task, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "final.md", Kind: "file"}}}, work)
	head := gitMust(t, work.Root, "rev-parse", "HEAD")
	if res.Reference != head {
		t.Fatalf("reference 应为确切 HEAD：%s != %s", res.Reference, head)
	}
	refName := gitResultRefName("at_1", 1)
	// 本组与原始仓库都有结果引用，且指向同一提交。
	if c := gitMust(t, "", g.gitDirArg(), "rev-parse", "--verify", refName); c != head {
		t.Fatalf("本组结果引用不符：%s", c)
	}
	if c := gitMust(t, project, "rev-parse", "--verify", refName); c != head {
		t.Fatalf("原始仓库结果引用不符：%s", c)
	}
	// 原始仓库已有提交对象（可从内部引用重建）。
	gitMust(t, project, "cat-file", "-e", head+"^{commit}")
	// 原始主分支与工作树不因导出改变。
	if now := gitMust(t, project, "rev-parse", "HEAD"); now != origHead {
		t.Fatalf("导出不应改原始主分支：%s → %s", origHead, now)
	}
	if _, err := os.Stat(filepath.Join(project, "final.md")); !os.IsNotExist(err) {
		t.Fatal("导出不应改原始工作树")
	}
	// artifact 逐字读回。
	b, err := p.ReadArtifact(ctx, g, ResultRef{TaskID: "at_1", ResultRevision: 1}, "final.md")
	if err != nil || string(b) != "# 报告\n正文" {
		t.Fatalf("ReadArtifact：%q / %v", b, err)
	}
	// TaskResult 落库字节不含 Provider 内部字段。
	bj, _ := json.Marshal(TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "final.md", Kind: "file"}}})
	for _, forbidden := range []string{"commit", "reference", "repo", "worktree", "branch"} {
		if strings.Contains(string(bj), forbidden) {
			t.Fatalf("TaskResult 不应含 %s：%s", forbidden, bj)
		}
	}
}

func TestGitAcceptResultRejectsDirty(t *testing.T) {
	ctx := context.Background()
	p, g, _ := gitTestSetup(t)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := g.Locker.Lock(ctx, []string{g.taskLockKey(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	// 未提交的已跟踪修改 → 拒绝。
	writeProj(t, work.Root, "README.md", "改了没提交", 0o644)
	if _, err := p.AcceptResult(ctx, g, task, 1, TaskResult{Summary: "x"}, work); err == nil {
		t.Fatal("未提交修改应拒绝")
	}
	gitMust(t, work.Root, "checkout", "--", "README.md")
	// 未跟踪非运行时文件 → 拒绝。
	writeProj(t, work.Root, "stray.txt", "x", 0o644)
	if _, err := p.AcceptResult(ctx, g, task, 1, TaskResult{Summary: "x"}, work); err == nil {
		t.Fatal("未跟踪非运行时文件应拒绝")
	}
	if err := os.Remove(filepath.Join(work.Root, "stray.txt")); err != nil {
		t.Fatal(err)
	}
	// .tako 运行时文件不算脏：只剩未跟踪 .tako 时接受成功（无已跟踪改动、无声明产物）。
	writeProj(t, work.Root, ".tako/runtime.json", "{}", 0o644)
	if _, err := p.AcceptResult(ctx, g, task, 1, TaskResult{Summary: "x"}, work); err != nil {
		t.Fatalf(".tako 运行时文件不应判脏：%v", err)
	}
}

func TestGitAcceptResultParentAncestry(t *testing.T) {
	ctx := context.Background()
	p, g, _ := gitTestSetup(t)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	refA := gitMakeParent(t, p, g, "at_a", "a", func(w string) {
		writeProj(t, w, "a.txt", "A", 0o644)
	})
	st, _ := readGitState(g)
	child := AgentTask{ID: "at_c", TaskKey: "c"}
	work, err := p.PrepareTask(ctx, g, child, []ResultRef{refA})
	if err != nil {
		t.Fatal(err)
	}
	// 子故意 reset 回基线，丢掉父提交 → 祖先校验拒绝。
	gitMust(t, work.Root, "reset", "--hard", st.BaseCommit)
	rel, err := g.Locker.Lock(ctx, []string{g.taskLockKey(child.ID)})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if _, err := p.AcceptResult(ctx, g, child, 1, TaskResult{Summary: "x"}, work); err == nil {
		t.Fatal("父提交不是 HEAD 祖先应拒绝")
	}
}

func TestGitParentMergeAndConflict(t *testing.T) {
	ctx := context.Background()
	p, g, _ := gitTestSetup(t)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	refA := gitMakeParent(t, p, g, "at_a", "a", func(w string) {
		writeProj(t, w, "shared.txt", "A", 0o644)
	})
	refB := gitMakeParent(t, p, g, "at_b", "b", func(w string) {
		writeProj(t, w, "shared.txt", "B", 0o644)
	})
	child := AgentTask{ID: "at_c", TaskKey: "c"}
	work, err := p.PrepareTask(ctx, g, child, []ResultRef{refA, refB})
	if err != nil {
		t.Fatalf("PrepareTask child: %v", err)
	}
	// 冲突留在子 worktree，记录清单，overlay 提示。
	conflicts, err := p.loadConflicts(g, child.ID)
	if err != nil || len(conflicts) == 0 {
		t.Fatalf("应记录合并冲突：%+v / %v", conflicts, err)
	}
	if !strings.Contains(work.PromptOverlay, "冲突") {
		t.Fatalf("overlay 应提示冲突：%s", work.PromptOverlay)
	}
	if unmerged := gitMust(t, work.Root, "diff", "--name-only", "--diff-filter=U"); !strings.Contains(unmerged, "shared.txt") {
		t.Fatalf("shared.txt 应处于未合并状态：%s", unmerged)
	}
	// 解决冲突并提交 → 两个父都是祖先，接受成功。
	writeProj(t, work.Root, "shared.txt", "merged", 0o644)
	gitMust(t, work.Root, "add", "shared.txt")
	gitMust(t, work.Root, "commit", "-m", "resolve")
	res := gitAccept(t, p, g, child, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "shared.txt", Kind: "file"}}}, work)
	for _, ref := range []ResultRef{refA, refB} {
		c := gitMust(t, "", g.gitDirArg(), "rev-parse", gitResultRefName(ref.TaskID, ref.ResultRevision))
		gitMust(t, work.Root, "merge-base", "--is-ancestor", c, res.Reference)
	}
}

func TestGitAcceptResultVerifyCommand(t *testing.T) {
	ctx := context.Background()
	p, g, _ := gitTestSetup(t)
	g.VerifyCommand = "test -f marker.txt"
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "marker.txt", "ok", 0o644)
	gitCommitAll(t, work.Root, "marker")
	res := gitAccept(t, p, g, task, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "marker.txt", Kind: "file"}}}, work)
	if res.Reference == "" {
		t.Fatal("验证通过应接受结果")
	}
	// 验证证据与输出文件落盘（静默命令也有 .out）。
	evb, err := os.ReadFile(g.verificationPath(task.ID))
	if err != nil || !strings.Contains(string(evb), "passed") {
		t.Fatalf("验证证据应 passed：%s / %v", evb, err)
	}
	if _, err := os.Stat(filepath.Join(g.Dir, "verification", task.ID+".out")); err != nil {
		t.Fatalf("验证输出应落盘：%v", err)
	}
	// 验证失败 → 拒绝接受。
	g.VerifyCommand = "false"
	task2 := AgentTask{ID: "at_2", TaskKey: "t2"}
	work2, err := p.PrepareTask(ctx, g, task2, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work2.Root, "m2.txt", "ok", 0o644)
	gitCommitAll(t, work2.Root, "m2")
	rel, err := g.Locker.Lock(ctx, []string{g.taskLockKey(task2.ID)})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if _, err := p.AcceptResult(ctx, g, task2, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "m2.txt", Kind: "file"}}}, work2); err == nil {
		t.Fatal("验证失败应拒绝接受")
	}
}

// gitFullResult 走完整链到「结果已接受、发布计划已落」，返回最终结果。
func gitFullResult(t *testing.T, p *gitProvider, g *GroupHandle) ImmutableResult {
	t.Helper()
	ctx := context.Background()
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "feature.txt", "新功能", 0o644)
	gitCommitAll(t, work.Root, "feature")
	res := gitAccept(t, p, g, task, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "feature.txt", Kind: "file"}}}, work)
	if _, err := p.PreparePublication(ctx, g, res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGitPublishFastForward(t *testing.T) {
	ctx := context.Background()
	p, g, project := gitTestSetup(t)
	res := gitFullResult(t, p, g)
	out, err := p.Publish(ctx, g, res, 1)
	if err != nil || out.Status != publishOutcomePublished {
		t.Fatalf("应 fast-forward 发布：%+v / %v", out, err)
	}
	if now := gitMust(t, project, "rev-parse", "HEAD"); now != res.Reference {
		t.Fatalf("原始主分支应前进到结果提交：%s != %s", now, res.Reference)
	}
	if b, err := os.ReadFile(filepath.Join(project, "feature.txt")); err != nil || string(b) != "新功能" {
		t.Fatalf("原始工作树应更新：%q / %v", b, err)
	}
	// 幂等：再发布一次仍 published（origHead==commit）。
	out2, err := p.Publish(ctx, g, res, 1)
	if err != nil || out2.Status != publishOutcomePublished {
		t.Fatalf("幂等发布：%+v / %v", out2, err)
	}
	// 对账：已发布成功补收尾，不再 merge。
	out3, err := p.ReconcilePublication(ctx, g, 1)
	if err != nil || out3.Status != publishOutcomePublished {
		t.Fatalf("对账应补 published：%+v / %v", out3, err)
	}
}

func TestGitPublishChanged(t *testing.T) {
	ctx := context.Background()
	p, g, project := gitTestSetup(t)
	res := gitFullResult(t, p, g)
	// 原始主分支前进（用户又提交了别的）。
	writeProj(t, project, "other.txt", "别人改的", 0o644)
	gitMust(t, project, "add", "-A")
	gitMust(t, project, "commit", "-m", "advance")
	newHead := gitMust(t, project, "rev-parse", "HEAD")
	out, err := p.Publish(ctx, g, res, 1)
	if err != nil || out.Status != publishOutcomeBlocked || out.ErrorCode != taskErrWorkspaceChanged {
		t.Fatalf("主分支前进应 workspace_changed：%+v / %v", out, err)
	}
	// 新基线已带入本组 refs/tako/base。
	if base := gitMust(t, "", g.gitDirArg(), "rev-parse", "--verify", gitBaseRef); base != newHead {
		t.Fatalf("refs/tako/base 应为新基线：%s != %s", base, newHead)
	}
	// 原始主分支不被覆盖。
	if now := gitMust(t, project, "rev-parse", "HEAD"); now != newHead {
		t.Fatalf("不应覆盖原始主分支：%s", now)
	}
}

func TestGitPublishDirty(t *testing.T) {
	ctx := context.Background()
	p, g, project := gitTestSetup(t)
	res := gitFullResult(t, p, g)
	// 原始工作树有未提交已跟踪修改。
	writeProj(t, project, "README.md", "脏", 0o644)
	out, err := p.Publish(ctx, g, res, 1)
	if err != nil || out.Status != publishOutcomeBlocked || out.ErrorCode != taskErrWorkspaceDirty {
		t.Fatalf("脏目录应 workspace_dirty：%+v / %v", out, err)
	}
	if now := gitMust(t, project, "rev-parse", "HEAD"); now == res.Reference {
		t.Fatal("脏目录不应被发布")
	}
}

func TestGitCleanup(t *testing.T) {
	ctx := context.Background()
	p, g, _ := gitTestSetup(t)
	res := gitFullResult(t, p, g)
	// 未过窗 → 保留。
	rep, err := p.Cleanup(ctx, g, time.Now().UTC(), time.Hour)
	if err != nil || len(rep.Removed) != 0 {
		t.Fatalf("未过窗应保留：%+v / %v", rep, err)
	}
	// 建立 epoch 记录后把它置回 pending（模拟发布未收口）→ 过窗也保留。
	if _, err := p.Publish(ctx, g, res, 1); err != nil {
		t.Fatal(err)
	}
	pub, err := readGitPublication(g, 1)
	if err != nil || pub == nil {
		t.Fatalf("读发布记录：%v / %v", pub, err)
	}
	pub.Status = publicationStatusPending
	if err := writeGitPublication(g, g.publicationPath(1), pub); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(2 * time.Hour)
	rep, err = p.Cleanup(ctx, g, future, time.Hour)
	if err != nil || len(rep.Removed) != 0 {
		t.Fatalf("发布未收口应保留：%+v / %v", rep, err)
	}
	// 收口（published）+ 过窗 → 删 worktree 与中间仓库，保留结果。
	pub.Status = publicationStatusPublished
	if err := writeGitPublication(g, g.publicationPath(1), pub); err != nil {
		t.Fatal(err)
	}
	rep, err = p.Cleanup(ctx, g, future, time.Hour)
	if err != nil || len(rep.Removed) == 0 {
		t.Fatalf("收口过窗应清理：%+v / %v", rep, err)
	}
	if _, err := os.Stat(g.gitRepoDir()); !os.IsNotExist(err) {
		t.Fatal("中间仓库应被清理")
	}
	if _, err := os.Stat(filepath.Join(g.Dir, "work")); !os.IsNotExist(err) {
		t.Fatal("worktree 应被清理")
	}
	if _, err := readImmutableResult(g, ResultRef{TaskID: "at_1", ResultRevision: 1}); err != nil {
		t.Fatalf("不可变结果应保留：%v", err)
	}
}