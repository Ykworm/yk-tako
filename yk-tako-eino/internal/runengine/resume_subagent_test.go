package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func metaLine(id, agent, created string) string {
	b, err := json.Marshal(wireMetadata{
		Type: "metadata", SaID: id, AgentName: agent, ModelRef: "m", RunID: "r",
		Protocol: subagentWireProtocol, CreatedAt: created,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func msgLine(text string) string {
	b, err := json.Marshal(wireMessageLine{Type: "message", Msg: schema.UserMessage(text)})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func termLine(at string) string {
	b, err := json.Marshal(wireTerminal{Type: "terminal", Status: "complete", Result: "done", FinishedAt: at})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeWireFile(t *testing.T, dir, id string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocateSubagentArchiveAddressingAndRejects(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour).Format(time.RFC3339Nano)
	createdNew := now.Add(-time.Minute).Format(time.RFC3339Nano)
	createdOld := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)

	idNew := "sa_01M5D7NEW000000000000000AA"
	idOld := "sa_01M5D7AGED0000000000000000AA"
	idHalf := "sa_01M5D7PART00000000000000AA"
	idPast := "sa_01M5D7PAST000000000000000AA"
	idBad := "sa_01M5D7BADMSG0000000000000AA"

	writeWireFile(t, dir, idOld, metaLine(idOld, "researcher-fix", createdOld), msgLine("旧"), termLine(fresh))
	writeWireFile(t, dir, idNew, metaLine(idNew, "researcher-fix", createdNew), msgLine("新"), termLine(fresh))
	writeWireFile(t, dir, idHalf, metaLine(idHalf, "researcher-fix", createdNew), msgLine("半截"))
	pastAt := now.Add(-subagentArchiveTTL - time.Second).Format(time.RFC3339Nano)
	writeWireFile(t, dir, idPast, metaLine(idPast, "researcher-fix", createdOld), msgLine("过期"), termLine(pastAt))
	writeWireFile(t, dir, idBad,
		metaLine(idBad, "researcher-fix", createdOld),
		"{broken",
		msgLine("后"),
		termLine(fresh),
	)

	// 两个都给时以 id 为准，不取更新的那份。
	arch, reject := locateSubagentArchive(dir, idOld, "researcher-fix", now)
	if reject != "" || arch.meta.SaID != idOld || len(arch.messages) != 1 {
		t.Fatalf("按 id 应取旧档: reject=%q arch=%#v", reject, arch)
	}
	// 只给规格名：跳过没终态的，取 created_at 最新的完整档。
	arch, reject = locateSubagentArchive(dir, "", "researcher-fix", now)
	if reject != "" || arch.meta.SaID != idNew {
		t.Fatalf("按 agent 应取最新完整档 %s: reject=%q got=%v", idNew, reject, arch)
	}
	// 只有半成品时，按规格名是找不到，不是未完成。
	only := t.TempDir()
	writeWireFile(t, only, idHalf, metaLine(idHalf, "researcher-fix", createdNew), msgLine("半截"))
	if _, reject = locateSubagentArchive(only, "", "researcher-fix", now); reject != msgArchiveMissing {
		t.Fatalf("按 agent 忽略半成品应是找不到: %q", reject)
	}
	if _, reject = locateSubagentArchive(dir, idHalf, "", now); reject != msgArchiveIncomplete {
		t.Fatalf("按 id 半成品: %q", reject)
	}
	if _, reject = locateSubagentArchive(dir, idPast, "", now); reject != msgArchiveExpired {
		t.Fatalf("过期: %q", reject)
	}
	if _, reject = locateSubagentArchive(dir, "sa_01M5D7ABSENT0000000000000AA", "", now); reject != msgArchiveMissing {
		t.Fatalf("缺失: %q", reject)
	}
	if _, reject = locateSubagentArchive(dir, "sa_../secret", "", now); reject != msgArchiveMissing {
		t.Fatalf("非法 id: %q", reject)
	}
	if _, reject = locateSubagentArchive(dir, idBad, "", now); reject != msgArchiveCorrupt {
		t.Fatalf("中间坏行: %q", reject)
	}
	// 刚好卡在窗口边上：finished_at + ttl == now，还没过期。
	edgeID := "sa_01M5D7EDGE000000000000000AA"
	edgeAt := now.Add(-subagentArchiveTTL).Format(time.RFC3339Nano)
	writeWireFile(t, dir, edgeID, metaLine(edgeID, "researcher-fix", createdOld), msgLine("边"), termLine(edgeAt))
	if _, reject = locateSubagentArchive(dir, edgeID, "", now); reject != "" {
		t.Fatalf("窗口边界应仍可 resume: %q", reject)
	}

	// -archive-ttl 12h 时，13 小时前的终态按配置过期；默认 168h 的入口仍认它。
	shortID := "sa_01M5D7SHR000000000000000AA"
	shortAt := now.Add(-13 * time.Hour).Format(time.RFC3339Nano)
	writeWireFile(t, dir, shortID, metaLine(shortID, "researcher-fix", shortAt), msgLine("短"), termLine(shortAt))
	if _, reject = locateSubagentArchiveWindow(dir, shortID, "", now, 12*time.Hour); reject != msgArchiveExpired {
		t.Fatalf("12h 窗口应过期: %q", reject)
	}
	if _, reject = locateSubagentArchive(dir, shortID, "", now); reject != "" {
		t.Fatalf("默认 168h 仍应可 resume: %q", reject)
	}
}

func TestLocateDropsPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339Nano)
	id := "sa_01M5D7END0000000000000000AA"
	writeWireFile(t, dir, id,
		metaLine(id, "researcher-fix", at),
		msgLine("保住"),
		termLine(at),
		`{"type":"message","msg":`,
	)
	arch, reject := locateSubagentArchive(dir, id, "", now)
	if reject != "" || arch == nil || len(arch.messages) != 1 {
		n := 0
		if arch != nil {
			n = len(arch.messages)
		}
		t.Fatalf("末行半截应丢掉，其余仍可 resume: reject=%q n=%d", reject, n)
	}
	if arch.messages[0].Content != "保住" {
		t.Fatalf("留下的消息 = %#v", arch.messages[0])
	}
}

func TestResumeModelInputKeepsHistoryAndAppendsUser(t *testing.T) {
	hist := []*schema.Message{schema.SystemMessage("人设"), schema.UserMessage("上次")}
	out, err := resumeModelInput(hist, "再核对")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Content != "人设" || out[1].Content != "上次" || out[2].Role != schema.User || out[2].Content != "再核对" {
		t.Fatalf("输入 = %#v", out)
	}
	if len(hist) != 2 {
		t.Fatal("原切片被改了")
	}
}

func TestResumeRejectsDoNotOpenWireOrTakeGate(t *testing.T) {
	root := t.TempDir()
	tenant := "ten_r"
	ref := WorkspaceRef{
		TenantID:   tenant,
		SessionID:  "s_resume",
		Root:       filepath.Join(root, "tenants", tenant, "workspaces", "w_1"),
		TenantRoot: filepath.Join(root, "tenants", tenant),
	}
	dir, err := subagentWireDir(ref)
	if err != nil {
		t.Fatal(err)
	}
	id := "sa_01M5D7PART00000000000000AA"
	path := writeWireFile(t, dir, id, metaLine(id, "ghost", time.Now().UTC().Format(time.RFC3339Nano)), msgLine("半"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	sem := make(chan struct{}, 1)
	sem <- struct{}{} // 满闸。拒绝路径若去占闸，下面的超时会先到。
	h := &agentSpecHandler{sem: sem}
	tool := newResumeSubagentTool(h)
	ctx, cancel := context.WithTimeout(withWorkspace(context.Background(), ref), time.Second)
	defer cancel()
	ctx = withSubagentAssembly(ctx, &subagentAssembly{specs: []AgentSpec{{Name: "researcher-fix", Description: "d", Prompt: "p"}}})

	out, err := tool.InvokableRun(ctx, `{"id":"`+id+`","instruction":"继续"}`)
	if err != nil || out != msgArchiveIncomplete {
		t.Fatalf("半成品 = (%q, %v)", out, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("原档案被改了")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("拒绝不应开新档案: %v", matches)
	}

	out, err = tool.InvokableRun(ctx, `{"id":"`+id+`","instruction":"  "}`)
	if err != nil || !strings.Contains(out, "instruction 不能为空") {
		t.Fatalf("空指令 = (%q, %v)", out, err)
	}
	out, err = tool.InvokableRun(ctx, `{"id":"sa_01M5D7ABSENT0000000000000AA","instruction":"继续"}`)
	if err != nil || out != msgArchiveMissing {
		t.Fatalf("找不到 = (%q, %v)", out, err)
	}

	ghost := "sa_01M5D7AWAY000000000000000AA"
	done := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	writeWireFile(t, dir, ghost, metaLine(ghost, "gone", done), msgLine("有终态"), termLine(done))
	out, err = tool.InvokableRun(ctx, `{"id":"`+ghost+`","instruction":"继续"}`)
	if err != nil || out != msgSpecMissing {
		t.Fatalf("规格没了 = (%q, %v)", out, err)
	}
	matches, _ = filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(matches) != 2 {
		t.Fatalf("规格缺失不应开新档案: %v", matches)
	}
}

func TestResumeChainWritesResumedFrom(t *testing.T) {
	root := t.TempDir()
	tenant := "ten_c"
	ref := WorkspaceRef{
		TenantID:   tenant,
		SessionID:  "s_chain",
		Root:       filepath.Join(root, "tenants", tenant, "workspaces", "w_1"),
		TenantRoot: filepath.Join(root, "tenants", tenant),
	}
	ctx := withWorkspace(context.Background(), ref)
	ctx = withJournalTarget(ctx, journalTarget{runID: "r_chain", epoch: 1})
	table := newAgentModelTable("deepseek/flash", nil)
	table.set("researcher-fix", agentModelEntry{ref: "deepseek/flash"})
	ctx = withAgentModelTable(ctx, table)

	source := "sa_01M5D7PART00000000000000AA"
	sem := make(chan struct{}, 1)
	out, err := runSubagentGuarded(ctx, nil, sem, 0, "researcher-fix", source, func(ctx context.Context) (string, error) {
		w := subagentWireFrom(ctx)
		if w == nil || w.id == "" || w.id == source {
			t.Fatalf("新实例 id = %#v", w)
		}
		return "核对完了", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "核对完了") || !strings.Contains(out, "（本次委派实例：sa_") {
		t.Fatalf("回传 = %q", out)
	}
	dir, err := subagentWireDir(ref)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("新档案 = %v %v", matches, err)
	}
	lines := wireLines(t, matches[0])
	if lines[0]["agent_name"] != "researcher-fix" || lines[0]["resumed_from"] != source || lines[0]["run_id"] != "r_chain" {
		t.Fatalf("metadata = %#v", lines[0])
	}
	last := lines[len(lines)-1]
	if last["type"] != "terminal" || last["status"] != "complete" || !strings.Contains(last["result"].(string), "本次委派实例") {
		t.Fatalf("terminal = %#v", last)
	}
	if len(sem) != 0 {
		t.Fatal("闸没还")
	}
}
