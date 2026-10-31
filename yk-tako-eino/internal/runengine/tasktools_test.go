package runengine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// —— C3：看板活得过崩溃（存储层证明）——

// 旧线的死因是「正本在进程内存，进程退出即清空」（doc/01 §3）。v6 把正本换成 PG 表 run_tasks
// → 换进程/换副本读到的还是同一批行。下面这条测试在**同一个 PG 库**上再开一个 Store 实例
// （= 另一个进程），用它读回前一个实例写的行——证明它不依赖进程内的任何东西。
func TestTaskBoardSurvivesProcessRestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	rid := RunID("r_1")
	if _, err := s.CreateTask(ctx, "ten_1", rid, "s_1", "列出工作区文件", "", "", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, "ten_1", rid, "s_1", "创建 hello.txt", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, "ten_1", rid, 2, taskStatusCompleted, "", "", ""); err != nil {
		t.Fatal(err)
	}

	// 另一个进程（同一个库的另一个 Store 实例）
	s2, err := OpenStore(ctx, testRedisURL, dsn, testTTL, testLease)
	if err != nil {
		t.Fatalf("OpenStore（第二个实例）: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	rows, err := s2.ListTasks(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("换进程后看板行数 = %d, want 2", len(rows))
	}
	if rows[0].Subject != "列出工作区文件" || rows[0].Status != taskStatusPending {
		t.Fatalf("第 1 行 = %+v", rows[0])
	}
	// 技能归属也活在库里（D32）：第 1 行盖了 demo，第 2 行没有。
	if rows[0].SkillName != "demo" {
		t.Fatalf("第 1 行 skill_name = %q, want demo", rows[0].SkillName)
	}
	if rows[1].SkillName != "" {
		t.Fatalf("第 2 行 skill_name = %q, want 空", rows[1].SkillName)
	}
	if rows[1].Status != taskStatusCompleted {
		t.Fatalf("第 2 行状态 = %q, want completed", rows[1].Status)
	}
	// 紧凑视图也要能用（模型的眼睛 + task 帧的 text），每行带技能归属。
	view := taskBoardView(rows)
	if !strings.Contains(view, "#1 [pending] 列出工作区文件（技能：demo）") || !strings.Contains(view, "#2 [completed] 创建 hello.txt\n") {
		t.Fatalf("紧凑视图形状不对: %q", view)
	}
	// 帧附带的那个技能名 = 刚写入的行（这里是 #2，无技能 → 空）。
	if got := touchedSkill(rows); got != "" {
		t.Fatalf("touchedSkill = %q, want 空（最近更新的是 #2）", got)
	}
}

// D34/D35 的两个查询：上一场取「本会话最近一个有任务的 run」（排除当前）、只列未完成行；
// 技能名取本 run 建过任务且未删除的 distinct。
func TestPreviousBoardRunAndSkillNames(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDSN(t, testLease, testTTL)
	const tenant, session = "ten_prev", SessionID("s_prev")
	older, prev, cur := RunID("r_older"), RunID("r_prev"), RunID("r_cur")

	if _, err := s.CreateTask(ctx, tenant, older, session, "更早的一场", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, tenant, older, 1, taskStatusCompleted, "", "", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.CreateTask(ctx, tenant, prev, session, "已完成的一步", "说明一", "", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, tenant, prev, session, "没做完的一步", "跑 scripts/x.py", "", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, tenant, prev, session, "普通任务", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, tenant, prev, 1, taskStatusCompleted, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, tenant, prev, 3, taskStatusInProgress, "", "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := s.PreviousBoardRun(ctx, tenant, session, cur)
	if err != nil {
		t.Fatal(err)
	}
	if got != prev {
		t.Fatalf("上一场 = %q, want %q", got, prev)
	}
	open, err := s.ListOpenTasks(ctx, prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].TaskNo != 2 || open[1].TaskNo != 3 {
		t.Fatalf("未完成行 = %+v", open)
	}
	names, err := s.TaskSkillNames(ctx, tenant, prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "demo" {
		t.Fatalf("技能名 = %v, want [demo]", names)
	}
	// 当前 run 没有行 → 上一场对它有内容；对 prev 自己再退，上一场是 older（全完成）。
	if p2, err := s.PreviousBoardRun(ctx, tenant, session, prev); err != nil || p2 != older {
		t.Fatalf("prev 的上一场 = %q, %v", p2, err)
	}
	if open, err := s.ListOpenTasks(ctx, older); err != nil || len(open) != 0 {
		t.Fatalf("older 未完成行 = %+v, %v", open, err)
	}
}

// 「上一场」段的两种形态：列未完成行（带说明）；全完成只给一行。
func TestPreviousBoardView(t *testing.T) {
	if got := previousBoardView("", nil); got != "" {
		t.Fatalf("没有上一场却渲染了 %q", got)
	}
	all := previousBoardView("r_1", nil)
	if all != "上一场（r_1）已全部完成" {
		t.Fatalf("全完成 = %q", all)
	}
	rows := []RunTask{
		{TaskNo: 2, Status: taskStatusPending, Subject: "写测试", Description: "给 store 层补两条用例"},
		{TaskNo: 3, Status: taskStatusInProgress, Subject: "收口"},
	}
	got := previousBoardView("r_1", rows)
	want := "上一场（r_1）还有 2 项没完成：\n#2 [pending] 写测试\n说明：给 store 层补两条用例\n#3 [in_progress] 收口"
	if got != want {
		t.Fatalf("上一场段 =\n%q\nwant\n%q", got, want)
	}
}
