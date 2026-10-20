package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func wireLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("行不是 JSON: %s", line)
		}
		lines = append(lines, row)
	}
	return lines
}

func TestSubagentWireAppendsIncrementally(t *testing.T) {
	dir := t.TempDir()
	w, err := openSubagentWire(dir, "sa_test", "researcher-fix", "deepseek/flash", "r_1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	h := newWireAppendHandler()
	ctx := withSubagentWire(context.Background(), w)

	first := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("do it")}
	if _, _, err := h.BeforeModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: first}, nil); err != nil {
		t.Fatal(err)
	}
	first = append(first, schema.AssistantMessage("calling", nil))
	if _, _, err := h.AfterModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: first}, nil); err != nil {
		t.Fatal(err)
	}
	// 工具结果在下一轮模型调用前才进 state。
	second := append(first, schema.ToolMessage("DONE", "c1"))
	if _, _, err := h.BeforeModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: second}, nil); err != nil {
		t.Fatal(err)
	}
	second = append(second, schema.AssistantMessage("finished", nil))
	if _, _, err := h.AfterModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: second}, nil); err != nil {
		t.Fatal(err)
	}
	w.finish("complete", "finished\n（本次委派实例：sa_test）")

	lines := wireLines(t, filepath.Join(dir, "sa_test.jsonl"))
	if len(lines) != 7 {
		t.Fatalf("行数 = %d，want 1 metadata + 5 message + 1 terminal", len(lines))
	}
	if lines[0]["type"] != "metadata" || lines[0]["sa_id"] != "sa_test" || lines[0]["agent_name"] != "researcher-fix" ||
		lines[0]["model_ref"] != "deepseek/flash" || lines[0]["run_id"] != "r_1" || lines[0]["resumed_from"] != "" ||
		lines[0]["protocol"] != float64(1) || lines[0]["created_at"] == "" {
		t.Fatalf("metadata = %#v", lines[0])
	}
	for i := 1; i <= 5; i++ {
		if lines[i]["type"] != "message" {
			t.Fatalf("第 %d 行 type = %v", i, lines[i]["type"])
		}
	}
	last := lines[6]
	if last["type"] != "terminal" || last["status"] != "complete" || last["finished_at"] == "" || last["result"] == "" {
		t.Fatalf("terminal = %#v", last)
	}

	items, err := listSubagentWires(dir)
	if err != nil || len(items) != 1 || items[0].Status != "complete" || items[0].SaID != "sa_test" || items[0].FinishedAt == "" {
		t.Fatalf("list = %#v err=%v", items, err)
	}
}

func TestSubagentWireShrinkKeepsPriorLines(t *testing.T) {
	dir := t.TempDir()
	w, err := openSubagentWire(dir, "sa_fold", "researcher", "m", "r", "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	h := newWireAppendHandler()
	ctx := withSubagentWire(context.Background(), w)
	full := []*schema.Message{
		schema.SystemMessage("sys"),
		schema.UserMessage("u"),
		schema.AssistantMessage("a1", nil),
		schema.ToolMessage("tool", "c1"),
	}
	if _, _, err := h.BeforeModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: full}, nil); err != nil {
		t.Fatal(err)
	}
	// 折叠把历史换短，末条是这一轮的新回复。
	short := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("summary"), schema.AssistantMessage("final", nil)}
	if _, _, err := h.AfterModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: short}, nil); err != nil {
		t.Fatal(err)
	}
	lines := wireLines(t, filepath.Join(dir, "sa_fold.jsonl"))
	if len(lines) != 6 { // metadata + 4 条折前原文 + 这一轮回复
		t.Fatalf("行数 = %d，want 6", len(lines))
	}
	msg := lines[5]["msg"].(map[string]any)
	if msg["content"] != "final" {
		t.Fatalf("折后末条 = %#v", msg)
	}
}

func TestSubagentWireInterruptHasNoTerminal(t *testing.T) {
	dir := t.TempDir()
	w, err := openSubagentWire(dir, "sa_half", "researcher", "m", "r", "")
	if err != nil {
		t.Fatal(err)
	}
	h := newWireAppendHandler()
	ctx := withSubagentWire(context.Background(), w)
	msgs := []*schema.Message{schema.UserMessage("u")}
	if _, _, err := h.BeforeModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: msgs}, nil); err != nil {
		t.Fatal(err)
	}
	w.close() // 中断：不 finish

	items, err := listSubagentWires(dir)
	if err != nil || len(items) != 1 {
		t.Fatalf("list = %#v err=%v", items, err)
	}
	if items[0].Status != "interrupted" || items[0].FinishedAt != "" || items[0].AgentName != "researcher" {
		t.Fatalf("半成品标注 = %#v", items[0])
	}
	if _, err := listSubagentWires(filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	empty, err := listSubagentWires(filepath.Join(dir, "missing"))
	if err != nil || len(empty) != 0 || empty == nil {
		t.Fatalf("缺目录应是空切片: %#v %v", empty, err)
	}
}

func TestDelegationToolWritesWireUnderSessionDir(t *testing.T) {
	root := t.TempDir()
	tenant := "ten_1"
	ref := WorkspaceRef{
		TenantID:   tenant,
		SessionID:  "s_wire",
		Root:       filepath.Join(root, "tenants", tenant, "workspaces", "w_1"),
		TenantRoot: filepath.Join(root, "tenants", tenant),
	}
	ctx := withWorkspace(context.Background(), ref)
	ctx = withJournalTarget(ctx, journalTarget{runID: "r_wire", epoch: 1})
	table := newAgentModelTable("deepseek/flash", nil)
	table.set("researcher-fix", agentModelEntry{ref: "deepseek/flash"})
	ctx = withAgentModelTable(ctx, table)

	inner := &fakeInvokableTool{info: &schema.ToolInfo{Name: "researcher-fix"}, out: "报告很长很长"}
	d := &delegationTool{inner: inner, sem: make(chan struct{}, 1), capBytes: 6, name: "researcher-fix"}
	out, err := d.InvokableRun(ctx, `{"request":"q"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "（本次委派实例：sa_") {
		t.Fatalf("回传应带实例 id: %q", out)
	}
	if !strings.Contains(out, "omitted") {
		t.Fatalf("超帽截断应仍在，id 接在后面: %q", out)
	}

	matches, err := filepath.Glob(filepath.Join(root, "tenants", tenant, "sessions", "s_wire", ".tako", "subagents", "sa_*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("会话目录档案 = %v err=%v", matches, err)
	}
	parked, _ := filepath.Glob(filepath.Join(root, "tenants", tenant, "workspaces", "w_1", ".tako", "subagents", "*.jsonl"))
	if len(parked) != 0 {
		t.Fatalf("项目目录不该有档案: %v", parked)
	}
	lines := wireLines(t, matches[0])
	if lines[0]["type"] != "metadata" || lines[0]["run_id"] != "r_wire" || lines[0]["model_ref"] != "deepseek/flash" {
		t.Fatalf("metadata = %#v", lines[0])
	}
	term := lines[len(lines)-1]
	if term["type"] != "terminal" || term["status"] != "complete" || term["result"] != out {
		t.Fatalf("terminal result 应等于回传: %#v out=%q", term, out)
	}
	if !strings.Contains(out, lines[0]["sa_id"].(string)) {
		t.Fatalf("回传 id 与档案不一致: %q vs %v", out, lines[0]["sa_id"])
	}

	// 没有作业根的调用不造档案，回传也不加尾巴（既有单测靠这个字节）。
	plain := &delegationTool{inner: inner, sem: make(chan struct{}, 1), capBytes: 0, name: "researcher-fix"}
	got, err := plain.InvokableRun(context.Background(), `{}`)
	if err != nil || got != "报告很长很长" {
		t.Fatalf("无作业根 = (%q, %v)", got, err)
	}
}

func TestDelegationToolInterruptSkipsTerminal(t *testing.T) {
	root := t.TempDir()
	tenant := "ten_1"
	ctx := withWorkspace(context.Background(), WorkspaceRef{
		TenantID: tenant, SessionID: "s_half",
		Root:       filepath.Join(root, "tenants", tenant, "sessions", "s_half"),
		TenantRoot: filepath.Join(root, "tenants", tenant),
	})
	d := &delegationTool{
		inner: &stubInvokeTool{err: context.Canceled}, sem: make(chan struct{}, 1), name: "researcher",
	}
	if _, err := d.InvokableRun(ctx, `{}`); err == nil {
		t.Fatal("取消应上抛")
	}
	dir := filepath.Join(root, "tenants", tenant, "sessions", "s_half", ".tako", "subagents")
	items, err := listSubagentWires(dir)
	if err != nil || len(items) != 1 || items[0].Status != "interrupted" {
		t.Fatalf("中断档案 = %#v err=%v", items, err)
	}
	lines := wireLines(t, filepath.Join(dir, items[0].SaID+".jsonl"))
	if len(lines) != 1 || lines[0]["type"] != "metadata" {
		t.Fatalf("中断文件应只有 metadata: %#v", lines)
	}
}

func TestDelegationToolErrorTextWritesTerminal(t *testing.T) {
	root := t.TempDir()
	tenant := "ten_1"
	ctx := withWorkspace(context.Background(), WorkspaceRef{
		TenantID: tenant, SessionID: "s_err",
		Root:       filepath.Join(root, "tenants", tenant, "sessions", "s_err"),
		TenantRoot: filepath.Join(root, "tenants", tenant),
	})
	d := &delegationTool{
		inner:    &stubInvokeTool{err: context.DeadlineExceeded},
		sem:      make(chan struct{}, 1),
		name:     "researcher",
		capBytes: 0,
	}
	// 非折叠、非溢出的错误仍上抛，不写 terminal。
	if _, err := d.InvokableRun(ctx, `{}`); err == nil {
		t.Fatal("应上抛")
	}

	d.inner = &stubInvokeTool{err: errFoldStub()}
	out, err := d.InvokableRun(ctx, `{}`)
	if err != nil || !strings.HasPrefix(out, "错误：子代理 researcher 上下文折叠失败：") || !strings.Contains(out, "（本次委派实例：sa_") {
		t.Fatalf("折叠失败应转文本并带 id: %q %v", out, err)
	}
	dir := filepath.Join(root, "tenants", tenant, "sessions", "s_err", ".tako", "subagents")
	items, err := listSubagentWires(dir)
	if err != nil {
		t.Fatal(err)
	}
	var errors, halves int
	for _, item := range items {
		lines := wireLines(t, filepath.Join(dir, item.SaID+".jsonl"))
		if item.Status == "complete" && lines[len(lines)-1]["status"] == "error" {
			errors++
		}
		if item.Status == "interrupted" {
			halves++
		}
	}
	if errors != 1 || halves != 1 {
		t.Fatalf("want 1 error terminal + 1 半成品, items=%#v", items)
	}
}

func errFoldStub() error {
	return &foldErr{}
}

type foldErr struct{}

func (e *foldErr) Error() string { return foldChainErrorMark + " 摘要失败: boom" }

var _ tool.InvokableTool = (*stubInvokeTool)(nil)
