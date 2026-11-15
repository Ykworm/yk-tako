// 重规划交接文件与结果复用的纯文件系统测试（施工包 §10.1）：
// frontmatter 渲染／解析往返、注入拒绝、标记与快照读写、复用结果复制与带入工作现场。
// 不碰 PG/Redis：所有输入都是本函数自己造的文件。
package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSafeFrontmatterScalar(t *testing.T) {
	for _, v := range []string{"or_01HZX", "at_x-1", "ABC_9"} {
		if got, err := safeFrontmatterScalar(v); err != nil || got != v {
			t.Fatalf("safeFrontmatterScalar(%q) = %q, %v", v, got, err)
		}
	}
	// 模型输入拼不出额外 YAML 字段：换行、冒号、空格、非 ASCII 一律拒绝。
	for _, v := range []string{"", "a b", "a:b", "a\nkind: x", "中文", "a/b"} {
		if _, err := safeFrontmatterScalar(v); err == nil {
			t.Fatalf("safeFrontmatterScalar(%q) 应报错", v)
		}
	}
}

func TestRenderParseHandoff(t *testing.T) {
	doc := handoffDoc{
		Kind: handoffKind, OrchestrationID: "or_new", ReplanOf: "or_old", ReplanDepth: 2,
		Reuse: []handoffReuseEntry{{TaskID: "at_a", ResultRevision: 1}, {TaskID: "at_b", ResultRevision: 3}},
	}
	arts := map[string][]TaskArtifact{
		"at_a": {{Path: "report.md", Kind: "file"}},
	}
	configNote := "程序记录的工作区配置变更（其余配置沿用原组）：\n- 目标项目改为 ws_2"
	raw, err := renderHandoff(doc, "原目标依赖的数据源已下线", "改用本地数据继续",
		"本地数据覆盖最近 30 天", configNote, []string{"删除线上数据"}, arts)
	if err != nil {
		t.Fatalf("renderHandoff: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"---\nkind: " + handoffKind + "\norchestrationId: or_new\nreplanOf: or_old\nreplanDepth: 2\nreuse:",
		"  - taskId: at_a\n    resultRevision: 1\n",
		"  - taskId: at_b\n    resultRevision: 3\n",
		"# 为什么重规划", "# 为什么创建继任组", "# 目标可行性证据",
		"# 哪些结果仍有效", "# 不得重放的动作",
		"本地数据覆盖最近 30 天",
		"- at_a（resultRevision 1）；产物：report.md",
		"- 删除线上数据",
		configNote,
		"以上由模型声明，程序未验证，也不会自动重放。",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("handoff 缺 %q：\n%s", want, body)
		}
	}
	// 五段的先后顺序是契约（读的人先看为什么，再看证据与复用）。
	order := []string{"# 为什么重规划", "# 为什么创建继任组", "# 目标可行性证据", "# 哪些结果仍有效", "# 不得重放的动作"}
	last := -1
	for _, h := range order {
		i := strings.Index(body, h)
		if i <= last {
			t.Fatalf("章节顺序不对：%s 出现在 %d（上一个 %d）", h, i, last)
		}
		last = i
	}

	got, gotBody, err := parseHandoff(body)
	if err != nil {
		t.Fatalf("parseHandoff: %v", err)
	}
	if got.Kind != doc.Kind || got.OrchestrationID != doc.OrchestrationID ||
		got.ReplanOf != doc.ReplanOf || got.ReplanDepth != doc.ReplanDepth {
		t.Fatalf("frontmatter 往返不一致：%+v vs %+v", got, doc)
	}
	if len(got.Reuse) != 2 || got.Reuse[0] != doc.Reuse[0] || got.Reuse[1] != doc.Reuse[1] {
		t.Fatalf("reuse 往返不一致：%+v", got.Reuse)
	}
	if !strings.Contains(gotBody, "# 为什么重规划") || !strings.Contains(gotBody, configNote) {
		t.Fatalf("正文丢失：\n%s", gotBody)
	}
}

func TestRenderHandoffEmptySections(t *testing.T) {
	raw, err := renderHandoff(handoffDoc{
		Kind: handoffKind, OrchestrationID: "or_1", ReplanOf: "or_0", ReplanDepth: 1,
	}, "原因", "理由", "证据", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := string(raw)
	if !strings.Contains(b, "reuse: []") {
		t.Fatalf("空复用的 frontmatter 应写 []：\n%s", b)
	}
	if !strings.Contains(b, "（这次继任不复用旧结果）") {
		t.Fatalf("空复用缺说明：\n%s", b)
	}
	if !strings.Contains(b, "（模型没有声明不可重放的动作）") {
		t.Fatalf("空不得重放缺说明：\n%s", b)
	}
}

func TestRenderHandoffRejectsInjection(t *testing.T) {
	base := handoffDoc{Kind: handoffKind, OrchestrationID: "or_1", ReplanOf: "or_0", ReplanDepth: 1}
	bad := base
	bad.OrchestrationID = "or_1\nkind: evil"
	if _, err := renderHandoff(bad, "r", "s", "e", "", nil, nil); err == nil {
		t.Fatal("orchestrationId 含换行应报错")
	}
	bad = base
	bad.Reuse = []handoffReuseEntry{{TaskID: "at_a\nkind: evil", ResultRevision: 1}}
	if _, err := renderHandoff(bad, "r", "s", "e", "", nil, nil); err == nil {
		t.Fatal("taskId 含换行应报错")
	}
}

func TestParseHandoffMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"缺 frontmatter", "# 只有正文\n"},
		{"未闭合", "---\nkind: x\n"},
		{"reuse 列表项出现在 reuse 头之前", "---\nkind: orchestration-replan\n  - taskId: at_a\n    resultRevision: 1\n---\n"},
		{"reuse 子字段无归属", "---\nkind: orchestration-replan\n    taskId: at_a\n---\n"},
		{"reuse 项缺 revision", "---\nkind: x\norchestrationId: or_1\nreplanOf: or_0\nreplanDepth: 1\nreuse:\n  - taskId: at_a\n---\n"},
		{"replanDepth 非整数", "---\nkind: x\nreplanDepth: 一\n---\n"},
		{"reuse 非列表", "---\nkind: x\nreuse: 3\n---\n"},
	}
	for _, c := range cases {
		if _, _, err := parseHandoff(c.raw); err == nil {
			t.Fatalf("%s：应报错", c.name)
		}
	}
}

func TestReadHandoffMissingAndIncomplete(t *testing.T) {
	dir := t.TempDir()
	doc, body, err := readHandoff(dir, "or_1")
	if err != nil || doc != nil || body != "" {
		t.Fatalf("文件不存在应 (nil, \"\", nil)，得 %v / %q / %v", doc, body, err)
	}
	// depth=0 是「不完整」：读侧不接受，避免详情把它当合法继任组。
	incomplete := "---\nkind: " + handoffKind + "\norchestrationId: or_1\nreplanOf: or_0\nreplanDepth: 0\nreuse: []\n---\n\n正文\n"
	if err := os.WriteFile(filepath.Join(dir, handoffFileName), []byte(incomplete), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readHandoff(dir, "or_1"); err == nil {
		t.Fatal("depth=0 应报不完整")
	}
}

func TestSuccessorMarkRoundtrip(t *testing.T) {
	dir := t.TempDir()
	if m, err := readSuccessorMark(dir); err != nil || m != nil {
		t.Fatalf("不存在应 (nil, nil)，得 %+v / %v", m, err)
	}
	want := successorMark{
		OrchestrationID: "or_old", SuccessorOrchestrationID: "or_new",
		CreationKey: "successor:or_old:k1", CreatedAt: nowUTC(),
	}
	if err := writeSuccessorMark(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readSuccessorMark(dir)
	if err != nil || got == nil {
		t.Fatalf("读回：%+v / %v", got, err)
	}
	if got.OrchestrationID != want.OrchestrationID || got.SuccessorOrchestrationID != want.SuccessorOrchestrationID ||
		got.CreationKey != want.CreationKey {
		t.Fatalf("标记往返不一致：%+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, successorFileName), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSuccessorMark(dir); err == nil {
		t.Fatal("损坏的标记应报错")
	}
}

func TestGroupReuseRoundtrip(t *testing.T) {
	dir := t.TempDir()
	if f, err := readGroupReuse(dir); err != nil || f != nil {
		t.Fatalf("不存在应 (nil, nil)，得 %+v / %v", f, err)
	}
	want := groupReuseFile{
		OrchestrationID: "or_new", ReplanOf: "or_old",
		Reuse: []groupReuseEntry{{
			SourceOrchestrationID: "or_old", TaskID: "at_a", ResultRevision: 1,
			Provider: workspaceProviderDirectory, Reference: "hash",
		}},
	}
	if err := writeGroupReuse(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readGroupReuse(dir)
	if err != nil || got == nil {
		t.Fatalf("读回：%+v / %v", got, err)
	}
	if got.OrchestrationID != "or_new" || got.ReplanOf != "or_old" ||
		len(got.Reuse) != 1 || got.Reuse[0].TaskID != "at_a" || got.Reuse[0].Reference != "hash" {
		t.Fatalf("快照往返不一致：%+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, groupReuseFileName), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGroupReuse(dir); err == nil {
		t.Fatal("损坏的快照应报错")
	}
}

// seedImmutableResult 在组目录造一份不可变结果（result.json + artifacts/ 下的文件）。
func seedImmutableResult(t *testing.T, g *GroupHandle, taskID string, rev int, files map[string]string) {
	t.Helper()
	dir := g.resultDir(taskID, rev)
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	arts := make([]TaskArtifact, 0, len(paths))
	for _, p := range paths {
		full := filepath.Join(dir, "artifacts", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			t.Fatal(err)
		}
		arts = append(arts, TaskArtifact{Path: p, Kind: "file"})
	}
	res := ImmutableResult{
		TaskID: taskID, ResultRevision: rev, Provider: workspaceProviderDirectory,
		Artifacts: arts, CreatedAt: nowUTC(), Reference: "hash-" + taskID,
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyGroupReuse(t *testing.T) {
	g, _ := wsTestGroup(t)
	container := filepath.Dir(g.Dir)
	seedImmutableResult(t, g, "at_a", 1, map[string]string{
		"report.md":  "报告正文\n",
		"docs/a.txt": "资料 A\n",
	})
	if err := writeGroupReuse(container, groupReuseFile{
		OrchestrationID: "or_new", ReplanOf: "or_old",
		Reuse: []groupReuseEntry{{TaskID: "at_a", ResultRevision: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	app, err := applyGroupReuse(g, work)
	if err != nil {
		t.Fatalf("applyGroupReuse: %v", err)
	}
	want := []string{"docs/a.txt", "report.md"}
	if len(app.Applied) != len(want) {
		t.Fatalf("Applied = %v, want %v", app.Applied, want)
	}
	for i, p := range want {
		if app.Applied[i] != p {
			t.Fatalf("Applied = %v, want %v", app.Applied, want)
		}
		b, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("带入的文件缺失 %s: %v", p, err)
		}
		if !strings.Contains(string(b), "报告") && !strings.Contains(string(b), "资料") {
			t.Fatalf("%s 的字节不对：%q", p, b)
		}
	}
	if !strings.Contains(app.Note, "本次工作现场已带入前代任务仍有效的结果") ||
		!strings.Contains(app.Note, "report.md") {
		t.Fatalf("提示段缺失：%q", app.Note)
	}

	// 字节已相同 = 幂等跳过，不重复记 Applied/Conflicts。
	app2, err := applyGroupReuse(g, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(app2.Applied) != 0 || len(app2.Conflicts) != 0 || app2.Note != "" {
		t.Fatalf("幂等重放不该有动作：%+v", app2)
	}

	// 现场已有不同内容：不覆盖，记冲突交模型裁决。
	if err := os.WriteFile(filepath.Join(work, "report.md"), []byte("模型自己的版本\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app3, err := applyGroupReuse(g, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(app3.Applied) != 0 || len(app3.Conflicts) != 1 || app3.Conflicts[0] != "report.md" {
		t.Fatalf("冲突应只有 report.md：%+v", app3)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "report.md")); string(b) != "模型自己的版本\n" {
		t.Fatalf("冲突时不得覆盖现场：%q", b)
	}
	if !strings.Contains(app3.Note, "需要你裁决") {
		t.Fatalf("冲突提示缺失：%q", app3.Note)
	}
}

func TestApplyGroupReuseMissingResult(t *testing.T) {
	g, _ := wsTestGroup(t)
	if err := writeGroupReuse(filepath.Dir(g.Dir), groupReuseFile{
		Reuse: []groupReuseEntry{{TaskID: "at_a", ResultRevision: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyGroupReuse(g, t.TempDir()); err == nil || !strings.Contains(err.Error(), "不可读") {
		t.Fatalf("缺结果应报不可读，得 %v", err)
	}
}

func TestCopyImmutableResult(t *testing.T) {
	oldG, _ := wsTestGroup(t)
	newG, _ := wsTestGroup(t)
	seedImmutableResult(t, oldG, "at_a", 1, map[string]string{"out.txt": "结果\n"})
	ref := ResultRef{TaskID: "at_a", ResultRevision: 1}
	if err := copyImmutableResult(oldG, newG, ref); err != nil {
		t.Fatalf("copyImmutableResult: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(newG.resultDir("at_a", 1), "artifacts", "out.txt"))
	if err != nil || string(b) != "结果\n" {
		t.Fatalf("复制后的产物：%q / %v", b, err)
	}
	if _, err := readImmutableResult(newG, ref); err != nil {
		t.Fatalf("复制后的 result.json 不可读：%v", err)
	}
	// 目标已存在：拒绝覆盖（幂等由调用方按「已存在且可读」跳过）。
	if err := copyImmutableResult(oldG, newG, ref); err == nil {
		t.Fatal("目标已存在应报错")
	}

	// 声明了产物但字节缺失：复制前就拒绝。
	oldG2, _ := wsTestGroup(t)
	seedImmutableResult(t, oldG2, "at_b", 1, map[string]string{"gone.txt": "x"})
	if err := os.Remove(filepath.Join(oldG2.resultDir("at_b", 1), "artifacts", "gone.txt")); err != nil {
		t.Fatal(err)
	}
	newG2, _ := wsTestGroup(t)
	if err := copyImmutableResult(oldG2, newG2, ResultRef{TaskID: "at_b", ResultRevision: 1}); err == nil ||
		!strings.Contains(err.Error(), "不可读") {
		t.Fatalf("缺产物应报不可读，得 %v", err)
	}
}

// reusePair 造同一个作业根下的两个组句柄（旧组与新组共用一个项目根与锁目录）。
func reusePair(t *testing.T, project string) (*GroupHandle, *GroupHandle) {
	t.Helper()
	root := filepath.Dir(project)
	locker := newFlockFileLocker(root)
	oldG, err := newGroupHandle(root, AgentOrchestration{ID: "or_old", TenantID: "t1", WorkspaceID: "ws_1"}, project, locker)
	if err != nil {
		t.Fatal(err)
	}
	newG, err := newGroupHandle(root, AgentOrchestration{ID: "or_new", TenantID: "t1", WorkspaceID: "ws_1"}, project, locker)
	if err != nil {
		t.Fatal(err)
	}
	return oldG, newG
}

// TestDirectoryPrepareTaskAppliesGroupReuse 钉住 doc/75 §7.4 的 Directory 半：
// 继任组的无父 Task 领取时把快照带进工作现场，文件成为本次交付的输入基线。
func TestDirectoryPrepareTaskAppliesGroupReuse(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	oldG, newG := reusePair(t, project)
	p := &directoryProvider{}
	if err := p.PrepareGroup(ctx, oldG); err != nil {
		t.Fatal(err)
	}
	oldTask := AgentTask{ID: "at_old", TaskKey: "a"}
	oldWork, err := p.PrepareTask(ctx, oldG, oldTask, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, oldWork.Root, "report.md", "前代报告\n", 0o644)
	oldRes := dirAccept(t, p, oldG, oldTask, 1,
		TaskResult{Summary: "旧结果", Artifacts: []TaskArtifact{{Path: "report.md", Kind: "file"}}}, oldWork)
	ref := ResultRef{TaskID: oldTask.ID, ResultRevision: oldRes.ResultRevision}
	if err := copyImmutableResult(oldG, newG, ref); err != nil {
		t.Fatal(err)
	}
	if err := writeGroupReuse(filepath.Dir(newG.Dir), groupReuseFile{
		OrchestrationID: "or_new", ReplanOf: "or_old",
		Reuse: []groupReuseEntry{{
			SourceOrchestrationID: "or_old", TaskID: ref.TaskID, ResultRevision: ref.ResultRevision,
			Provider: oldRes.Provider, Reference: oldRes.Reference,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareGroup(ctx, newG); err != nil {
		t.Fatal(err)
	}

	task := AgentTask{ID: "at_new", TaskKey: "d"}
	work, err := p.PrepareTask(ctx, newG, task, nil)
	if err != nil {
		t.Fatalf("PrepareTask: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(work.Root, "report.md")); err != nil || string(b) != "前代报告\n" {
		t.Fatalf("复用结果未带进现场：%q / %v", b, err)
	}
	if !strings.Contains(work.PromptOverlay, "带入前代任务仍有效的结果") || work.Conflicts != 0 {
		t.Fatalf("复用提示不符：%q conflicts=%d", work.PromptOverlay, work.Conflicts)
	}
	// 幂等：现场已存在时 PrepareTask 直接返回，不重放复用。
	work2, err := p.PrepareTask(ctx, newG, task, nil)
	if err != nil || work2.Root != work.Root {
		t.Fatalf("幂等 PrepareTask：%+v / %v", work2, err)
	}
	// 复用文件是本次交付的输入基线：接受结果时按新增文件固化，模型不必重写声明。
	if res := dirAccept(t, p, newG, task, 1, TaskResult{Summary: "继承基线"}, work); res.ResultRevision != 1 {
		t.Fatalf("接受结果 revision = %d", res.ResultRevision)
	}
}

// TestGitPrepareTaskCommitsGroupReuse 钉住 doc/75 §7.4 的 Git 半：复用文件必须先提交，
// 否则 AcceptResult 的干净工作树检查会因未跟踪文件拒绝本次交付。
func TestGitPrepareTaskCommitsGroupReuse(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitMust(t, project, "init", "-b", "main")
	gitMust(t, project, "config", "user.name", "test")
	gitMust(t, project, "config", "user.email", "test@localhost")
	writeProj(t, project, "README.md", "init", 0o644)
	gitMust(t, project, "add", "-A")
	gitMust(t, project, "commit", "-m", "init")
	oldG, newG := reusePair(t, project)
	p := &gitProvider{}
	if err := p.PrepareGroup(ctx, oldG); err != nil {
		t.Fatal(err)
	}
	oldTask := AgentTask{ID: "at_old", TaskKey: "a"}
	oldWork, err := p.PrepareTask(ctx, oldG, oldTask, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeProj(t, oldWork.Root, "report.md", "前代报告\n", 0o644)
	gitCommitAll(t, oldWork.Root, "old result")
	oldRes := gitAccept(t, p, oldG, oldTask, 1,
		TaskResult{Summary: "旧结果", Artifacts: []TaskArtifact{{Path: "report.md", Kind: "file"}}}, oldWork)
	ref := ResultRef{TaskID: oldTask.ID, ResultRevision: oldRes.ResultRevision}
	if err := copyImmutableResult(oldG, newG, ref); err != nil {
		t.Fatal(err)
	}
	if err := writeGroupReuse(filepath.Dir(newG.Dir), groupReuseFile{
		OrchestrationID: "or_new", ReplanOf: "or_old",
		Reuse: []groupReuseEntry{{
			SourceOrchestrationID: "or_old", TaskID: ref.TaskID, ResultRevision: ref.ResultRevision,
			Provider: oldRes.Provider, Reference: oldRes.Reference,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareGroup(ctx, newG); err != nil {
		t.Fatal(err)
	}

	task := AgentTask{ID: "at_new", TaskKey: "d"}
	work, err := p.PrepareTask(ctx, newG, task, nil)
	if err != nil {
		t.Fatalf("PrepareTask: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(work.Root, "report.md")); err != nil || string(b) != "前代报告\n" {
		t.Fatalf("复用结果未带进现场：%q / %v", b, err)
	}
	if !strings.Contains(work.PromptOverlay, "带入前代任务仍有效的结果") {
		t.Fatalf("复用提示缺失：%q", work.PromptOverlay)
	}
	if st := gitMust(t, work.Root, "status", "--porcelain"); strings.Contains(st, "report.md") {
		t.Fatalf("复用文件应已提交：%s", st)
	}
	// 关键断言：干净工作树检查通过，接受成功。
	if res := gitAccept(t, p, newG, task, 1, TaskResult{Summary: "继承基线"}, work); res.Reference == "" {
		t.Fatal("接受结果应返回引用")
	}
}
