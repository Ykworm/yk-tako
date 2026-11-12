// 拍3 确定性测试（施工包 §12.1 第 5 组）：Directory 基线/结果清单、同路径冲突、删除、
// 哈希校验、原子写与部分发布恢复。纯文件系统，不打 PG/Redis/模型；锁用本机 flock。
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

// dirTestSetup 造一个 directoryProvider + 组句柄 + 空原项目根。
func dirTestSetup(t *testing.T) (*directoryProvider, *GroupHandle, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	o := AgentOrchestration{ID: "or_1", TenantID: "t1", WorkspaceID: "ws_1"}
	g, err := newGroupHandle(root, o, project, newFlockFileLocker(root))
	if err != nil {
		t.Fatal(err)
	}
	return &directoryProvider{}, g, project
}

func writeProj(t *testing.T, project, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(project, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// dirAccept 在 Task 锁内接受结果（模拟引擎提交链持锁）。
func dirAccept(t *testing.T, p *directoryProvider, g *GroupHandle, task AgentTask, rev int, result TaskResult, work TaskWork) ImmutableResult {
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

func TestDirectoryPrepareGroupBaseline(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "a.txt", "甲", 0o644)
	writeProj(t, project, "sub/b.sh", "#!/bin/sh\necho ok\n", 0o755)
	writeProj(t, project, ".tako/runtime.json", "{}", 0o644) // 运行时目录排除
	// 越界软链排除。
	outside := filepath.Join(filepath.Dir(project), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatalf("PrepareGroup: %v", err)
	}
	bl, err := readBaseline(g)
	if err != nil || bl == nil {
		t.Fatalf("读基线: %v / %v", bl, err)
	}
	paths := map[string]directoryFileEntry{}
	for _, e := range bl.Files {
		paths[e.Path] = e
	}
	if len(bl.Files) != 2 {
		t.Fatalf("基线应只含 2 个普通文件（排除 .tako 与越界软链），得 %d：%+v", len(bl.Files), bl.Files)
	}
	if _, ok := paths[".tako/runtime.json"]; ok {
		t.Fatal(".tako 不应进基线")
	}
	if _, ok := paths["link-out"]; ok {
		t.Fatal("越界软链不应进基线")
	}
	if paths["sub/b.sh"].Mode&0o111 == 0 {
		t.Fatal("脚本执行位应保留")
	}
	// 基线副本字节与原项目一致。
	b, err := os.ReadFile(filepath.Join(g.baselineDir(), "a.txt"))
	if err != nil || string(b) != "甲" {
		t.Fatalf("基线副本字节不符：%q / %v", b, err)
	}
	// 不创建 .git。
	if _, err := os.Stat(filepath.Join(g.providerDir(), ".git")); err == nil {
		t.Fatal("Directory 不应创建 .git")
	}
	// 幂等：再调不改 createdAt。
	time.Sleep(2 * time.Millisecond)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	bl2, _ := readBaseline(g)
	if !bl2.CreatedAt.Equal(bl.CreatedAt) {
		t.Fatal("幂等 PrepareGroup 改了基线时间")
	}
}

func TestDirectoryPrepareTaskCopiesBaseline(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "a.txt", "甲", 0o644)
	writeProj(t, project, "run.sh", "echo hi\n", 0o755)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatalf("PrepareTask: %v", err)
	}
	if work.Root != g.workDir("at_1") {
		t.Fatalf("Root = %q", work.Root)
	}
	b, err := os.ReadFile(filepath.Join(work.Root, "a.txt"))
	if err != nil || string(b) != "甲" {
		t.Fatalf("工作目录副本不符：%q / %v", b, err)
	}
	info, err := os.Stat(filepath.Join(work.Root, "run.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("工作目录脚本执行位丢失：%v", err)
	}
	// 幂等：再调返回同一现场。
	work2, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil || work2.Root != work.Root {
		t.Fatalf("幂等 PrepareTask: %v / %q", err, work2.Root)
	}
	// RecoverTaskWork 返回已存在现场。
	rec, err := p.RecoverTaskWork(ctx, g, task, 0)
	if err != nil || rec.Root != work.Root {
		t.Fatalf("RecoverTaskWork: %v / %q", err, rec.Root)
	}
}

// dirMakeParent 准备一个父 Task、改工作目录、接受结果，返回 ResultRef。
func dirMakeParent(t *testing.T, p *directoryProvider, g *GroupHandle, taskID, key string, mutate func(workRoot string)) ResultRef {
	t.Helper()
	ctx := context.Background()
	task := AgentTask{ID: taskID, TaskKey: key}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	mutate(work.Root)
	res := dirAccept(t, p, g, task, 1, TaskResult{Summary: "父结果"}, work)
	return ResultRef{TaskID: res.TaskID, ResultRevision: res.ResultRevision}
}

func TestDirectoryParentApplicationAndConflict(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "base.txt", "基线", 0o644)
	writeProj(t, project, "shared.txt", "原", 0o644)
	writeProj(t, project, "del.txt", "待删", 0o644)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	// 父 A：新增 added.txt、改 shared.txt→A、删 del.txt。
	refA := dirMakeParent(t, p, g, "at_a", "a", func(w string) {
		writeProj(t, w, "added.txt", "A新增", 0o644)
		writeProj(t, w, "shared.txt", "A", 0o644)
		if err := os.Remove(filepath.Join(w, "del.txt")); err != nil {
			t.Fatal(err)
		}
	})
	// 父 B：改 shared.txt→B（与 A 冲突）。
	refB := dirMakeParent(t, p, g, "at_b", "b", func(w string) {
		writeProj(t, w, "shared.txt", "B", 0o644)
	})
	// 子任务按 taskKey 顺序应用 A、B。
	child := AgentTask{ID: "at_c", TaskKey: "c"}
	work, err := p.PrepareTask(ctx, g, child, []ResultRef{refA, refB})
	if err != nil {
		t.Fatalf("PrepareTask child: %v", err)
	}
	// A 的新增带入。
	if b, err := os.ReadFile(filepath.Join(work.Root, "added.txt")); err != nil || string(b) != "A新增" {
		t.Fatalf("父新增未带入：%q / %v", b, err)
	}
	// A 的删除生效。
	if _, err := os.Stat(filepath.Join(work.Root, "del.txt")); !os.IsNotExist(err) {
		t.Fatal("父删除未生效")
	}
	// shared.txt：A 先应用成 A，B 冲突 → 保留 A，B 版本进 .tako/conflicts。
	if b, _ := os.ReadFile(filepath.Join(work.Root, "shared.txt")); string(b) != "A" {
		t.Fatalf("冲突应保留先应用的 A，得 %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(work.Root, ".tako", "conflicts", "at_b", "shared.txt")); err != nil || string(b) != "B" {
		t.Fatalf("冲突父版本应放进 .tako/conflicts：%q / %v", b, err)
	}
	if !strings.Contains(work.PromptOverlay, "冲突") || !strings.Contains(work.PromptOverlay, "shared.txt") {
		t.Fatalf("prompt 补充应含冲突说明：%q", work.PromptOverlay)
	}
	// 冲突记录持久化，RecoverTaskWork 能重建。
	rec, err := p.RecoverTaskWork(ctx, g, child, 0)
	if err != nil || !strings.Contains(rec.PromptOverlay, "shared.txt") {
		t.Fatalf("恢复应重建冲突补充：%v / %q", err, rec.PromptOverlay)
	}
}

func TestDirectoryAcceptResultManifestAndArtifacts(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "keep.txt", "不变", 0o644)
	writeProj(t, project, "mod.txt", "旧", 0o644)
	writeProj(t, project, "gone.txt", "删", 0o644)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "new.txt", "新", 0o644)
	writeProj(t, work.Root, "mod.txt", "新内容", 0o644)
	if err := os.Remove(filepath.Join(work.Root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	res := dirAccept(t, p, g, task, 1, TaskResult{
		Summary:   "改了三个文件",
		Artifacts: []TaskArtifact{{Path: "new.txt", Kind: "file"}, {Path: "mod.txt", Kind: "file"}},
	}, work)
	// 清单冻结在结果目录。
	cs, err := readResultManifest(g, ResultRef{TaskID: "at_1", ResultRevision: 1})
	if err != nil {
		t.Fatalf("读结果清单: %v", err)
	}
	if len(cs.Added) != 1 || cs.Added[0].Path != "new.txt" {
		t.Fatalf("added 不符：%+v", cs.Added)
	}
	if len(cs.Modified) != 1 || cs.Modified[0].Path != "mod.txt" {
		t.Fatalf("modified 不符：%+v", cs.Modified)
	}
	if len(cs.Deleted) != 1 || cs.Deleted[0].Path != "gone.txt" {
		t.Fatalf("deleted 不符：%+v", cs.Deleted)
	}
	// reference = 清单哈希（非空）。
	if res.Reference == "" {
		t.Fatal("reference 应非空")
	}
	// artifact 字节逐字可读。
	if b, err := p.ReadArtifact(ctx, g, ResultRef{TaskID: "at_1", ResultRevision: 1}, "new.txt"); err != nil || string(b) != "新" {
		t.Fatalf("ReadArtifact: %q / %v", b, err)
	}
	// 未变更文件不能当 artifact（程序判定）。
	task2 := AgentTask{ID: "at_2", TaskKey: "t2"}
	work2, err := p.PrepareTask(ctx, g, task2, nil)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := g.Locker.Lock(ctx, []string{g.taskLockKey("at_2")})
	_, err = p.AcceptResult(ctx, g, task2, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "keep.txt", Kind: "file"}}}, work2)
	rel()
	if err == nil {
		t.Fatal("未变更文件当 artifact 应拒绝")
	}
}

func TestDirectoryAcceptResultVerifyCommand(t *testing.T) {
	if !sandboxAvailable() {
		t.Skip("无可用 bash 沙箱，跳过验证命令测试")
	}
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	g.VerifyCommand = "test -f marker.txt"
	writeProj(t, project, "a.txt", "甲", 0o644)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 没有 marker.txt → 验证失败 → 拒绝接受。
	rel, _ := g.Locker.Lock(ctx, []string{g.taskLockKey("at_1")})
	_, err = p.AcceptResult(ctx, g, task, 1, TaskResult{Summary: "x"}, work)
	rel()
	if err == nil {
		t.Fatal("验证未通过应拒绝接受")
	}
	ev, _ := os.ReadFile(g.verificationPath("at_1"))
	if !strings.Contains(string(ev), `"failed"`) {
		t.Fatalf("证据应标 failed：%s", ev)
	}
	// 造出 marker.txt → 验证通过。
	writeProj(t, work.Root, "marker.txt", "ok", 0o644)
	res := dirAccept(t, p, g, task, 1, TaskResult{Summary: "x"}, work)
	if res.TaskID != "at_1" {
		t.Fatal("验证通过后应接受")
	}
	ev, _ = os.ReadFile(g.verificationPath("at_1"))
	if !strings.Contains(string(ev), `"passed"`) {
		t.Fatalf("证据应标 passed：%s", ev)
	}
	// 输出文件落组目录。
	if _, err := os.Stat(filepath.Join(g.Dir, "verification", "at_1.out")); err != nil {
		t.Fatalf("验证输出应落盘：%v", err)
	}
}

func TestDirectoryPublishAndChanged(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "a.txt", "原", 0o644)
	writeProj(t, project, "del.txt", "删", 0o644)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "a.txt", "改", 0o644)
	writeProj(t, work.Root, "new.txt", "新", 0o644)
	if err := os.Remove(filepath.Join(work.Root, "del.txt")); err != nil {
		t.Fatal(err)
	}
	res := dirAccept(t, p, g, task, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "a.txt", Kind: "file"}, {Path: "new.txt", Kind: "file"}}}, work)
	if _, err := p.PreparePublication(ctx, g, res); err != nil {
		t.Fatalf("PreparePublication: %v", err)
	}
	out, err := p.Publish(ctx, g, res, 1)
	if err != nil || out.Status != publishOutcomePublished {
		t.Fatalf("Publish: %+v / %v", out, err)
	}
	// 原项目被写成结果。
	if b, _ := os.ReadFile(filepath.Join(project, "a.txt")); string(b) != "改" {
		t.Fatalf("a.txt 未发布：%q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(project, "new.txt")); string(b) != "新" {
		t.Fatalf("new.txt 未发布：%q", b)
	}
	if _, err := os.Stat(filepath.Join(project, "del.txt")); !os.IsNotExist(err) {
		t.Fatal("del.txt 未删除")
	}
	// 幂等重发：已等于目标 → published。
	out, err = p.Publish(ctx, g, res, 1)
	if err != nil || out.Status != publishOutcomePublished {
		t.Fatalf("幂等 Publish: %+v / %v", out, err)
	}
	// 原项目前进（a.txt 被人改成别的）→ 新结果发布会受阻 workspace_changed。
	writeProj(t, project, "a.txt", "人改", 0o644)
	task2 := AgentTask{ID: "at_2", TaskKey: "t2"}
	work2, err := p.PrepareTask(ctx, g, task2, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work2.Root, "a.txt", "结果2", 0o644)
	res2 := dirAccept(t, p, g, task2, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "a.txt", Kind: "file"}}}, work2)
	if _, err := p.PreparePublication(ctx, g, res2); err != nil {
		t.Fatal(err)
	}
	out, err = p.Publish(ctx, g, res2, 2)
	if err != nil || out.Status != publishOutcomeBlocked || out.ErrorCode != taskErrWorkspaceChanged {
		t.Fatalf("项目前进应受阻 workspace_changed：%+v / %v", out, err)
	}
	// 受阻后原项目 a.txt 保持人改的值（不被旧结果覆盖）。
	if b, _ := os.ReadFile(filepath.Join(project, "a.txt")); string(b) != "人改" {
		t.Fatalf("受阻不应覆盖原项目：%q", b)
	}
}

func TestDirectoryReconcilePartialPublish(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "a.txt", "原A", 0o644)
	writeProj(t, project, "b.txt", "原B", 0o644)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "a.txt", "新A", 0o644)
	writeProj(t, work.Root, "b.txt", "新B", 0o644)
	res := dirAccept(t, p, g, task, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "a.txt", Kind: "file"}, {Path: "b.txt", Kind: "file"}}}, work)
	if _, err := p.PreparePublication(ctx, g, res); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：手工把 a.txt 写成目标（done 但记录还没更新），b.txt 未动。
	// 先 Publish 一次让记录绑定 epoch，再构造部分现场。
	if _, err := p.Publish(ctx, g, res, 1); err != nil {
		t.Fatal(err)
	}
	// 现在重置原项目模拟「发布中途崩溃」：a 已是新值、b 回到原值，记录里 b 标回 pending。
	writeProj(t, project, "b.txt", "原B", 0o644)
	pub, err := readPublicationRecord(g, 1)
	if err != nil || pub == nil {
		t.Fatal(err)
	}
	for i := range pub.Entries {
		if pub.Entries[i].Path == "b.txt" {
			pub.Entries[i].Status = dirPubPending
		}
	}
	if err := writePublicationRecord(g, g.publicationPath(1), pub); err != nil {
		t.Fatal(err)
	}
	// 对账：a 已相符收尾，b 未开始继续 → published。
	out, err := p.ReconcilePublication(ctx, g, 1)
	if err != nil || out.Status != publishOutcomePublished {
		t.Fatalf("部分发布对账应续成 published：%+v / %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(project, "b.txt")); string(b) != "新B" {
		t.Fatalf("b.txt 应被续写：%q", b)
	}
	// 计划外内容：a.txt 记录已 done（上一轮发布成功），但项目里的字节被改成既非基线
	// 也非目标 → 已发布路径漂移，对账无法解释 → recovery_required（doc/74 §8.3）。
	writeProj(t, project, "a.txt", "计划外", 0o644)
	out, err = p.ReconcilePublication(ctx, g, 1)
	if err != nil || out.Status != publishOutcomeBlocked || out.ErrorCode != taskErrPublicationRecovery {
		t.Fatalf("已发布路径漂移应 recovery_required：%+v / %v", out, err)
	}
}

func TestDirectoryCleanup(t *testing.T) {
	ctx := context.Background()
	p, g, project := dirTestSetup(t)
	writeProj(t, project, "a.txt", "原", 0o644)
	if err := p.PrepareGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	task := AgentTask{ID: "at_1", TaskKey: "t1"}
	work, err := p.PrepareTask(ctx, g, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, work.Root, "a.txt", "改", 0o644)
	res := dirAccept(t, p, g, task, 1, TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "a.txt", Kind: "file"}}}, work)
	// 未过窗 → 保留。
	rep, err := p.Cleanup(ctx, g, time.Now().UTC(), time.Hour)
	if err != nil || len(rep.Removed) != 0 {
		t.Fatalf("未过窗应保留：%+v / %v", rep, err)
	}
	// 过窗但发布未收口（有 pending 记录）→ 保留。
	if _, err := p.PreparePublication(ctx, g, res); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(ctx, g, res, 1); err != nil {
		t.Fatal(err)
	}
	pub, _ := readPublicationRecord(g, 1)
	pub.Entries[0].Status = dirPubPending
	_ = writePublicationRecord(g, g.publicationPath(1), pub)
	future := time.Now().UTC().Add(2 * time.Hour)
	rep, err = p.Cleanup(ctx, g, future, time.Hour)
	if err != nil || len(rep.Removed) != 0 {
		t.Fatalf("发布未收口应保留：%+v / %v", rep, err)
	}
	// 收口（全 done）+ 过窗 → 删工作目录与基线，保留结果。
	pub.Entries[0].Status = dirPubDone
	_ = writePublicationRecord(g, g.publicationPath(1), pub)
	rep, err = p.Cleanup(ctx, g, future, time.Hour)
	if err != nil || len(rep.Removed) == 0 {
		t.Fatalf("收口过窗应清理：%+v / %v", rep, err)
	}
	if _, err := os.Stat(g.workDir("at_1")); !os.IsNotExist(err) {
		t.Fatal("工作目录应被清理")
	}
	if _, err := os.Stat(g.baselineDir()); !os.IsNotExist(err) {
		t.Fatal("基线副本应被清理")
	}
	if _, err := readImmutableResult(g, ResultRef{TaskID: "at_1", ResultRevision: 1}); err != nil {
		t.Fatalf("不可变结果应保留：%v", err)
	}
}

// TestDirectoryNoProviderFieldsInTaskJSON 钉死：TaskResult / AgentTask 不含 Provider 内部字段。
func TestDirectoryNoProviderFieldsInTaskJSON(t *testing.T) {
	b, err := json.Marshal(TaskResult{Summary: "x", Artifacts: []TaskArtifact{{Path: "a", Kind: "file"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"commit", "sha256", "repo", "worktree", "branch", "reference", "provider"} {
		if strings.Contains(strings.ToLower(string(b)), forbidden) {
			t.Fatalf("TaskResult JSON 不应含 Provider 字段 %q：%s", forbidden, b)
		}
	}
}