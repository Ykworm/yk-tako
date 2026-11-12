// 拍2 确定性测试（施工包 §12.1 第 2–3 组）：组目录与 Provider 选择、不可变结果固化与读取。
// 纯文件系统，不打 PG/Redis/模型；锁用本机 flock（跨副本语义由 filelock 既有测试覆盖）。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wsTestGroup 造一个纯文件系统的组句柄：root 下 tenants/t1/.tako/orchestrations/or_1/workspace。
func wsTestGroup(t *testing.T) (*GroupHandle, string) {
	t.Helper()
	root := t.TempDir()
	o := AgentOrchestration{ID: "or_1", TenantID: "t1", WorkspaceID: "ws_1"}
	g, err := newGroupHandle(root, o, filepath.Join(root, "project"), newFlockFileLocker(root))
	if err != nil {
		t.Fatalf("newGroupHandle: %v", err)
	}
	return g, root
}

func TestDetectWorkspaceProvider(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{"无 .git", func(t *testing.T, root string) {}, workspaceProviderDirectory},
		{".git 目录", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, workspaceProviderGit},
		{".git 文件（worktree 形态）", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, workspaceProviderGit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			c.setup(t, root)
			if got := detectWorkspaceProvider(root); got != c.want {
				t.Fatalf("detectWorkspaceProvider = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNewGroupHandleAssembly(t *testing.T) {
	root := t.TempDir()
	locker := newFlockFileLocker(root)
	o := AgentOrchestration{ID: "or_1", TenantID: "t1"}
	if _, err := newGroupHandle("", o, "/p", locker); err == nil {
		t.Fatal("缺 root 应报错")
	}
	if _, err := newGroupHandle(root, AgentOrchestration{ID: "or_1"}, "/p", locker); err == nil {
		t.Fatal("缺 tenant 应报错")
	}
	if _, err := newGroupHandle(root, o, "", locker); err == nil {
		t.Fatal("缺 projectRoot 应报错")
	}
	if _, err := newGroupHandle(root, o, "/p", nil); err == nil {
		t.Fatal("缺 locker 应报错")
	}
	g, err := newGroupHandle(root, o, "/p", locker)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(root, "tenants", "t1", ".tako", "orchestrations", "or_1", "workspace")
	if g.Dir != wantDir {
		t.Fatalf("Dir = %q, want %q", g.Dir, wantDir)
	}
	if g.workDir("at_1") != filepath.Join(wantDir, "work", "at_1") {
		t.Fatalf("workDir 布局不符：%s", g.workDir("at_1"))
	}
	if g.resultDir("at_1", 3) != filepath.Join(wantDir, "results", "at_1", "3") {
		t.Fatalf("resultDir 布局不符：%s", g.resultDir("at_1", 3))
	}
	if g.verificationPath("at_1") != filepath.Join(wantDir, "verification", "at_1.json") {
		t.Fatalf("verificationPath 布局不符：%s", g.verificationPath("at_1"))
	}
	if g.publicationPath(7) != filepath.Join(wantDir, "publication", "7.json") {
		t.Fatalf("publicationPath 布局不符：%s", g.publicationPath(7))
	}
	if g.taskLockKey("at_1") != "orch-workspace:or_1/task:at_1" {
		t.Fatalf("taskLockKey = %q", g.taskLockKey("at_1"))
	}
}

func TestEnsureGroupState(t *testing.T) {
	ctx := context.Background()
	g, _ := wsTestGroup(t)

	// 尚未准备：读回 nil 不是错误。
	if st, err := readWorkspaceGroupState(g.Dir); err != nil || st != nil {
		t.Fatalf("未准备时应 (nil, nil)，得 %v / %v", st, err)
	}
	// 首次固定 Provider。
	if err := ensureGroupState(ctx, g, workspaceProviderGit); err != nil {
		t.Fatalf("首次 ensureGroupState: %v", err)
	}
	st, err := readWorkspaceGroupState(g.Dir)
	if err != nil || st == nil {
		t.Fatalf("读组状态: %v / %v", st, err)
	}
	if st.FormatVersion != workspaceStateFormatVersion || st.WorkspaceMode != workspaceModeIsolated || st.Provider != workspaceProviderGit {
		t.Fatalf("组状态字段不符：%+v", st)
	}
	if st.CreatedAt.IsZero() || st.UpdatedAt.IsZero() {
		t.Fatalf("时间戳未写：%+v", st)
	}
	// 幂等：同 Provider 再调不改 createdAt。
	time.Sleep(2 * time.Millisecond)
	if err := ensureGroupState(ctx, g, workspaceProviderGit); err != nil {
		t.Fatalf("幂等 ensureGroupState: %v", err)
	}
	st2, _ := readWorkspaceGroupState(g.Dir)
	if !st2.CreatedAt.Equal(st.CreatedAt) {
		t.Fatalf("幂等调用改了 createdAt：%v → %v", st.CreatedAt, st2.CreatedAt)
	}
	// 项目形态变化不允许静默换 Provider。
	if err := ensureGroupState(ctx, g, workspaceProviderDirectory); err == nil {
		t.Fatal("git → directory 应拒绝")
	}
	// 损坏的状态文件 = 错误，不是「尚未准备」。
	if err := os.WriteFile(g.statePath(), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceGroupState(g.Dir); err == nil {
		t.Fatal("损坏 state.json 应报错")
	}
	if err := ensureGroupState(ctx, g, workspaceProviderGit); err == nil {
		t.Fatal("损坏 state.json 上 ensureGroupState 应报错")
	}
}

// wsWriteSrc 在 srcRoot 造一份产物文件，返回相对路径与字节。
func wsWriteSrc(t *testing.T, srcRoot, rel, content string) TaskArtifact {
	t.Helper()
	p := filepath.Join(srcRoot, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return TaskArtifact{Path: rel, Kind: "file"}
}

func TestFinalizeImmutableResultRoundtrip(t *testing.T) {
	ctx := context.Background()
	g, root := wsTestGroup(t)
	src := filepath.Join(root, "work-src")
	a1 := wsWriteSrc(t, src, "out/report.md", "# 报告\n正文")
	a2 := wsWriteSrc(t, src, "data.bin", "\x00\x01\x02raw")

	res, err := finalizeImmutableResult(ctx, g, "at_1", 1, workspaceProviderGit, []TaskArtifact{a1, a2}, src, "commit-abc", nil, nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.TaskID != "at_1" || res.ResultRevision != 1 || res.Provider != workspaceProviderGit || res.Reference != "commit-abc" {
		t.Fatalf("结果身份不符：%+v", res)
	}
	back, err := readImmutableResult(g, ResultRef{TaskID: "at_1", ResultRevision: 1})
	if err != nil {
		t.Fatalf("readImmutableResult: %v", err)
	}
	if !back.CreatedAt.Equal(res.CreatedAt) || back.Reference != res.Reference || len(back.Artifacts) != 2 {
		t.Fatalf("读回不一致：%+v vs %+v", back, res)
	}
	for _, c := range []struct{ path, want string }{{"out/report.md", "# 报告\n正文"}, {"data.bin", "\x00\x01\x02raw"}} {
		b, err := readResultArtifact(g, ResultRef{TaskID: "at_1", ResultRevision: 1}, c.path)
		if err != nil {
			t.Fatalf("readResultArtifact(%s): %v", c.path, err)
		}
		if string(b) != c.want {
			t.Fatalf("artifact %s 字节不符：%q", c.path, b)
		}
	}
	// 结果目录里不留 staging 残留。
	entries, err := os.ReadDir(filepath.Join(g.Dir, "results", "at_1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Fatalf("staging 残留：%s", e.Name())
		}
	}
}

func TestFinalizeImmutableResultIdempotentReuse(t *testing.T) {
	ctx := context.Background()
	g, root := wsTestGroup(t)
	src := filepath.Join(root, "work-src")
	a := wsWriteSrc(t, src, "a.txt", "v1")

	first, err := finalizeImmutableResult(ctx, g, "at_1", 1, workspaceProviderDirectory, []TaskArtifact{a}, src, "hash-1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	// 同候选 revision 同身份重试：复用原结果（createdAt 不变），不制造新版本。
	again, err := finalizeImmutableResult(ctx, g, "at_1", 1, workspaceProviderDirectory, []TaskArtifact{a}, src, "hash-1", nil, nil)
	if err != nil {
		t.Fatalf("同身份重试应复用: %v", err)
	}
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("复用应返回原 createdAt：%v → %v", first.CreatedAt, again.CreatedAt)
	}
	// 同 revision 不同身份：reference 不同 / 产物清单不同 / provider 不同都拒绝。
	for name, mutate := range map[string]func(*string, *[]TaskArtifact, *string){
		"reference 不同": func(p *string, arts *[]TaskArtifact, ref *string) { *ref = "hash-2" },
		"产物清单不同":   func(p *string, arts *[]TaskArtifact, ref *string) { *arts = []TaskArtifact{{Path: "b.txt", Kind: "file"}} },
		"provider 不同": func(p *string, arts *[]TaskArtifact, ref *string) { *p = workspaceProviderGit },
	} {
		p, arts, ref := workspaceProviderDirectory, []TaskArtifact{a}, "hash-1"
		mutate(&p, &arts, &ref)
		if _, err := finalizeImmutableResult(ctx, g, "at_1", 1, p, arts, src, ref, nil, nil); err == nil {
			t.Fatalf("%s：同 revision 应拒绝覆盖", name)
		}
	}
	// 拒绝后原结果字节不变。
	back, err := readImmutableResult(g, ResultRef{TaskID: "at_1", ResultRevision: 1})
	if err != nil || back.Reference != "hash-1" {
		t.Fatalf("拒绝后原结果被改动：%+v / %v", back, err)
	}
}

func TestFinalizeImmutableResultFailureCleanup(t *testing.T) {
	ctx := context.Background()
	g, root := wsTestGroup(t)
	src := filepath.Join(root, "work-src")
	good := wsWriteSrc(t, src, "a.txt", "ok")
	// 第二个产物不存在 → 固化失败；final 不存在、staging 无残留、目标目录无垃圾。
	bad := TaskArtifact{Path: "missing.txt", Kind: "file"}
	if _, err := finalizeImmutableResult(ctx, g, "at_1", 1, workspaceProviderDirectory, []TaskArtifact{good, bad}, src, "h", nil, nil); err == nil {
		t.Fatal("缺产物文件应失败")
	}
	if _, err := readImmutableResult(g, ResultRef{TaskID: "at_1", ResultRevision: 1}); !errors.Is(err, errImmutableResultMissing) {
		t.Fatalf("失败后不应有 final：%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(g.Dir, "results", "at_1"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败后 results/at_1 应无残留，得 %d 项", len(entries))
	}
}

func TestReadImmutableResultErrorClasses(t *testing.T) {
	g, _ := wsTestGroup(t)
	ref := ResultRef{TaskID: "at_1", ResultRevision: 1}
	// 缺失 = 独立错误类别（可能尚未固化）。
	if _, err := readImmutableResult(g, ref); !errors.Is(err, errImmutableResultMissing) {
		t.Fatalf("缺失应为 errImmutableResultMissing：%v", err)
	}
	// 损坏 = 普通错误（必须人工介入，不能当缺失重试）。
	dir := g.resultDir(ref.TaskID, ref.ResultRevision)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readImmutableResult(g, ref); err == nil || errors.Is(err, errImmutableResultMissing) {
		t.Fatalf("损坏应为普通错误：%v", err)
	}
	// 身份不符（文件里写着别的 Task）= 错误。
	b, _ := json.Marshal(ImmutableResult{TaskID: "at_other", ResultRevision: 1})
	if err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readImmutableResult(g, ref); err == nil {
		t.Fatal("身份不符应报错")
	}
}

func TestReadResultArtifactAdmission(t *testing.T) {
	ctx := context.Background()
	g, root := wsTestGroup(t)
	src := filepath.Join(root, "work-src")
	a := wsWriteSrc(t, src, "a.txt", "内容")
	ref := ResultRef{TaskID: "at_1", ResultRevision: 1}
	if _, err := finalizeImmutableResult(ctx, g, "at_1", 1, workspaceProviderDirectory, []TaskArtifact{a}, src, "h", nil, nil); err != nil {
		t.Fatal(err)
	}
	// 逐字匹配：清单是 a.txt，"./a.txt" 不命中（不做 clean 后再匹配）。
	for _, p := range []string{"./a.txt", "b.txt", "../a.txt", "/etc/passwd", "a.txt "} {
		if _, err := readResultArtifact(g, ref, p); !errors.Is(err, errArtifactBad) {
			t.Fatalf("path %q 应被清单拒绝：%v", p, err)
		}
	}
	// 软链越界：把产物换成指向组外的 symlink，即使逐字命中清单也拒绝。
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(g.resultDir("at_1", 1), "artifacts", "a.txt")
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := readResultArtifact(g, ref, "a.txt"); !errors.Is(err, errArtifactBad) {
		t.Fatalf("软链越界应拒绝：%v", err)
	}
	// 产物文件缺失 = errImmutableResultMissing（与结果缺失同类）。
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := readResultArtifact(g, ref, "a.txt"); !errors.Is(err, errImmutableResultMissing) {
		t.Fatalf("产物缺失应为 errImmutableResultMissing：%v", err)
	}
}

func TestWriteVerificationEvidence(t *testing.T) {
	ctx := context.Background()
	g, _ := wsTestGroup(t)
	now := time.Now().UTC()
	fin := now.Add(time.Second)
	ev := workspaceVerificationEvidence{
		TaskID: "at_1", Status: verificationStatusPassed, ExitCode: 0,
		OutputPath: "verification/at_1.out", StartedAt: now, FinishedAt: &fin,
	}
	if err := writeVerificationEvidence(ctx, g, ev); err != nil {
		t.Fatalf("writeVerificationEvidence: %v", err)
	}
	b, err := os.ReadFile(g.verificationPath("at_1"))
	if err != nil {
		t.Fatal(err)
	}
	var back workspaceVerificationEvidence
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.TaskID != "at_1" || back.Status != verificationStatusPassed || back.ExitCode != 0 || back.FinishedAt == nil || !back.FinishedAt.Equal(fin) {
		t.Fatalf("证据读回不符：%+v", back)
	}
	// 覆盖写：running → failed，原子替换后读面只见新版。
	ev.Status = verificationStatusFailed
	ev.ExitCode = 1
	if err := writeVerificationEvidence(ctx, g, ev); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(g.verificationPath("at_1"))
	_ = json.Unmarshal(b, &back)
	if back.Status != verificationStatusFailed || back.ExitCode != 1 {
		t.Fatalf("覆盖写不符：%+v", back)
	}
}

// TestWorkspaceSummaryVerificationEvidence 钉住读面取证据的规则：无证据/running 只有
// configured+status；收尾（passed/failed）追加退出码与输出位置；shared 组没有读面。
func TestWorkspaceSummaryVerificationEvidence(t *testing.T) {
	ctx := context.Background()
	g, root := wsTestGroup(t)
	s := &Store{workspaceRoot: root}
	o := AgentOrchestration{ID: "or_1", TenantID: "t1", WorkspaceMode: workspaceModeIsolated, VerifyCommand: "exit 1"}

	shared := o
	shared.WorkspaceMode = workspaceModeShared
	if s.workspaceSummary(shared, "at_1") != nil {
		t.Fatal("shared 组不应有 workspace 读面")
	}

	v := s.workspaceSummary(o, "at_1")
	if v == nil || !v.Verification.Configured || v.Verification.Status != "" || v.Verification.ExitCode != nil || v.Verification.OutputPath != "" {
		t.Fatalf("无证据读面不符：%+v", v)
	}

	now := time.Now().UTC()
	ev := workspaceVerificationEvidence{TaskID: "at_1", Status: verificationStatusRunning, OutputPath: "verification/at_1.out", StartedAt: now}
	if err := writeVerificationEvidence(ctx, g, ev); err != nil {
		t.Fatal(err)
	}
	v = s.workspaceSummary(o, "at_1")
	if v.Verification.Status != verificationStatusRunning || v.Verification.ExitCode != nil || v.Verification.OutputPath != "" {
		t.Fatalf("running 证据不应带退出码：%+v", v.Verification)
	}

	fin := now.Add(time.Second)
	ev.Status, ev.ExitCode, ev.FinishedAt = verificationStatusFailed, 3, &fin
	if err := writeVerificationEvidence(ctx, g, ev); err != nil {
		t.Fatal(err)
	}
	v = s.workspaceSummary(o, "at_1")
	if v.Verification.Status != verificationStatusFailed || v.Verification.ExitCode == nil || *v.Verification.ExitCode != 3 || v.Verification.OutputPath != "verification/at_1.out" {
		t.Fatalf("failed 证据读面不符：%+v", v.Verification)
	}

	ev.Status, ev.ExitCode = verificationStatusPassed, 0
	if err := writeVerificationEvidence(ctx, g, ev); err != nil {
		t.Fatal(err)
	}
	v = s.workspaceSummary(o, "at_1")
	if v.Verification.Status != verificationStatusPassed || v.Verification.ExitCode == nil || *v.Verification.ExitCode != 0 {
		t.Fatalf("passed 证据应带退出码 0：%+v", v.Verification)
	}
}