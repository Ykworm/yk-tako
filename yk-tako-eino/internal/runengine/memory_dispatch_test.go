package runengine

// 本文件：pipeline 模式 master 的**派工工具**的端到端断言（memory_dispatch.go）。
//
// 这条链路是「记住 → 后台抽取 → 写进 memory_items」的第一段，断言点有三个：
//  1. 工具**只登记**，不直接写记忆（登记后 memory_items 一行不多）；
//  2. 回执说「已受理」而不是「已写成功」（master 不许对用户说已经记住了）；
//  3. 幂等：同一场 run 的同一句原话重复登记 → 复用同一行，不堆重复任务。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func TestMemoryDispatchRegistersJobOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	s.ConfigureMemory(MemoryConfig{TenantID: testOwner.TenantID, Extract: true, Recall: true})

	tl, err := newMemoryDispatchTool(s)
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := tl.(tool.InvokableTool)
	if !ok {
		t.Fatalf("工具 %T 不是可调用的 InvokableTool", tl)
	}

	// run 上下文：noteBox 提供 run/session/seq。
	runCtx := withNoteBox(withPrincipal(ctx, principal{
		TenantID: testOwner.TenantID, UserID: testOwner.UserID, Role: "owner"}),
		newNoteBox("s_disp", "r_disp", 42))

	out, err := inv.InvokableRun(runCtx, `{"targetLayer":"resident","quote":"以后 deck 放 output/v2.html"}`)
	if err != nil {
		t.Fatalf("调用派工工具: %v", err)
	}
	if !strings.Contains(out, "已受理") {
		t.Errorf("回执 = %q, want 含「已受理」（不许说已经写成功）", out)
	}
	// 回执不许**宣称**写成功。注意不能简单判「含『已经记住了』」——回执里那句
	// 「不要对用户说『已经记住了』」本身含这个短语；判的是「开头/主体的宣称」。
	if strings.HasPrefix(out, "已写入") || strings.HasPrefix(out, "已记下") || strings.HasPrefix(out, "已保存") {
		t.Errorf("回执宣称已写成功 = %q，写库在后台链路上，不能这么说", out)
	}
	if !strings.Contains(out, "不会立刻生效") {
		t.Errorf("回执没说明「本轮不会立刻生效」= %q", out)
	}
	// 只登记：记忆表一行不多。
	if n := s.count(t, &MemoryItem{}, "1 = 1"); n != 0 {
		t.Errorf("派工后 memory_items 有 %d 行，want 0（工具只登记、不直接写）", n)
	}
	// 登记了一条 inrun 任务，目标是 resident。
	var job MemoryJob
	if err := s.db.Raw(`SELECT * FROM memory_jobs WHERE kind = 'inrun'`).Scan(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Fatal("没有登记 inrun 任务")
	}
	if job.TargetLayer == nil || *job.TargetLayer != MemoryLayerResident {
		t.Errorf("任务 target_layer = %v, want resident", job.TargetLayer)
	}
	if job.Quote != "以后 deck 放 output/v2.html" {
		t.Errorf("任务 quote = %q, want 用户原话逐字", job.Quote)
	}
	if job.SourceSeq != 42 {
		t.Errorf("任务 source_seq = %d, want 42（noteBox 里的本 run user 行 seq）", job.SourceSeq)
	}

	// 幂等：同一句再登记一次 → 还是同一行任务。
	out2, err := inv.InvokableRun(runCtx, `{"targetLayer":"resident","quote":"以后 deck 放 output/v2.html"}`)
	if err != nil {
		t.Fatalf("重复派工: %v", err)
	}
	if n := s.count(t, &MemoryJob{}, "kind = 'inrun'"); n != 1 {
		t.Errorf("重复派工后 inrun 任务数 = %d, want 1（不能堆重复任务）", n)
	}
	if !strings.Contains(out2, "已受理") {
		t.Errorf("重复派工回执 = %q, want 含「已受理」", out2)
	}
}

func TestMemoryDispatchRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	tl, err := newMemoryDispatchTool(s)
	if err != nil {
		t.Fatal(err)
	}
	inv := tl.(tool.InvokableTool)
	runCtx := withNoteBox(withPrincipal(ctx, principal{
		TenantID: testOwner.TenantID, UserID: testOwner.UserID, Role: "owner"}),
		newNoteBox("s_disp2", "r_disp2", 7))

	cases := []struct {
		name string
		args string
		want string
	}{
		{"目标层非法", `{"targetLayer":"everything","quote":"x"}`, "resident"},
		{"缺目标层", `{"quote":"x"}`, "targetLayer"},
		{"quote 为空", `{"targetLayer":"resident","quote":"  "}`, "quote"},
		{"含密钥", `{"targetLayer":"resident","quote":"我的 API Key 是 sk-abcdefghijklmnopqrstuvwxyz012345"}`, "API Key"},
	}
	for _, c := range cases {
		out, err := inv.InvokableRun(runCtx, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: 回执 = %q, want 含 %q", c.name, out, c.want)
		}
	}
	if n := s.count(t, &MemoryJob{}, "1 = 1"); n != 0 {
		t.Errorf("被拒的调用仍登记了 %d 条任务，want 0", n)
	}
}

// TestMemoryDispatchPublishesWakeAfterCommit：登记**提交后**发叫醒（doc/65 §4.5）；
// 消息只带 job_id；发送失败不影响「已受理」回执（对账器兜底）。
func TestMemoryDispatchPublishesWakeAfterCommit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	s.ConfigureMemory(MemoryConfig{TenantID: testOwner.TenantID, Extract: true, Recall: true})

	q := newFakeMemoryQueue()
	var wakes int
	wake := MemoryWakeFunc(func(_ context.Context, job *MemoryJob) {
		wakes++
		if err := q.Publish(ctx, memoryMessage{JobID: job.ID, Key: memoryQueueKeyFor(job)}); err != nil {
			t.Errorf("发叫醒: %v", err)
		}
	})
	tl, err := newMemoryDispatchTool(s, wake)
	if err != nil {
		t.Fatal(err)
	}
	inv := tl.(tool.InvokableTool)
	runCtx := withNoteBox(withPrincipal(ctx, principal{
		TenantID: testOwner.TenantID, UserID: testOwner.UserID, Role: "owner"}),
		newNoteBox("s_wake", "r_wake", 9))

	out, err := inv.InvokableRun(runCtx, `{"targetLayer":"recall","quote":"以后部署走 30080"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已受理") {
		t.Fatalf("回执 = %q, want 已受理", out)
	}
	if wakes != 1 {
		t.Fatalf("登记后叫醒 %d 次, want 1", wakes)
	}
	pub := q.Published()
	if len(pub) != 1 || pub[0].JobID == "" || pub[0].Key != "s_wake" {
		t.Fatalf("叫醒消息 = %+v, want 单条 {job_id, key=s_wake}", pub)
	}
	// 幂等重放：同一句再登记（dedup 复用同一行）→ 仍发叫醒（重复消息由认领条件挡）。
	if _, err := inv.InvokableRun(runCtx, `{"targetLayer":"recall","quote":"以后部署走 30080"}`); err != nil {
		t.Fatal(err)
	}
	if wakes != 2 || len(q.Published()) != 2 {
		t.Fatalf("重放登记后叫醒 %d 次 / 队列 %d 条, want 2/2（重复无害）", wakes, len(q.Published()))
	}
}
