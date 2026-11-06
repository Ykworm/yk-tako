package runengine

import (
	"context"
	"errors"
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
	if err := s.UpdateTask(ctx, "ten_1", rid, 2, taskStatusCompleted, "已输出页脚", "", "", ""); err != nil {
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
	if err := s.UpdateTask(ctx, tenant, older, 1, taskStatusCompleted, "更早一场的产出", "", "", ""); err != nil {
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
	if err := s.UpdateTask(ctx, tenant, prev, 1, taskStatusCompleted, "第一批用例已过", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, tenant, prev, 3, taskStatusInProgress, "", "", "", ""); err != nil {
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
	// doc/64：上一场「已完成的结果」只取 completed 且 result 非空的行。
	done, err := s.ListCompletedResults(ctx, prev)
	if err != nil || len(done) != 1 || done[0].TaskNo != 1 || done[0].Result != "第一批用例已过" {
		t.Fatalf("prev 已完成结果 = %+v, %v", done, err)
	}
	if done, err := s.ListCompletedResults(ctx, older); err != nil || len(done) != 1 || done[0].Result != "更早一场的产出" {
		t.Fatalf("older 已完成结果 = %+v, %v", done, err)
	}
}

// 「上一场」段的形态：列未完成行（带说明）；全完成只给一行；doc/64 起再带已完成的结果。
func TestPreviousBoardView(t *testing.T) {
	if got := previousBoardView("", nil, nil); got != "" {
		t.Fatalf("没有上一场却渲染了 %q", got)
	}
	all := previousBoardView("r_1", nil, nil)
	if all != "上一场（r_1）已全部完成" {
		t.Fatalf("全完成 = %q", all)
	}
	rows := []RunTask{
		{TaskNo: 2, Status: taskStatusPending, Subject: "写测试", Description: "给 store 层补两条用例"},
		{TaskNo: 3, Status: taskStatusInProgress, Subject: "收口"},
	}
	got := previousBoardView("r_1", rows, nil)
	want := "上一场（r_1）还有 2 项没完成：\n#2 [pending] 写测试\n说明：给 store 层补两条用例\n#3 [in_progress] 收口"
	if got != want {
		t.Fatalf("上一场段 =\n%q\nwant\n%q", got, want)
	}
	done := []RunTask{{TaskNo: 1, Status: taskStatusCompleted, Subject: "跑基线", Result: "p95=1.8s"}}
	got = previousBoardView("r_1", nil, done)
	want = "上一场（r_1）已全部完成\n已完成的结果：\n#1 跑基线——结果：p95=1.8s"
	if got != want {
		t.Fatalf("全完成 + 结果段 =\n%q\nwant\n%q", got, want)
	}
}

// —— 标完成必须带结果（doc/64 A1）：闸只认「有字」，并给出模型能照做的拒写文本 ——
func TestTaskResultGate(t *testing.T) {
	allowed, text := taskResultGate(taskStatusCompleted, "")
	if allowed || text != taskResultRequiredText {
		t.Fatalf("completed + 空结果 = (%v, %q)，两项都该拒", allowed, text)
	}
	if allowed, _ := taskResultGate(taskStatusCompleted, " \n\t "); allowed {
		t.Fatal("只有空白的 result 也算没写")
	}
	if allowed, text := taskResultGate(taskStatusCompleted, "p95=1.8s，脚本 scripts/bench.py"); !allowed || text != "" {
		t.Fatalf("completed + 有结果 = (%v, %q)，该放行且不带文本", allowed, text)
	}
	// 其它状态与结果无关：pending / in_progress / deleted 空着也放行。
	for _, st := range []string{taskStatusPending, taskStatusInProgress, taskStatusDeleted} {
		if allowed, text := taskResultGate(st, ""); !allowed || text != "" {
			t.Fatalf("%s + 空结果 = (%v, %q)，该放行", st, allowed, text)
		}
	}
	// 拒写文本必须是「错误：」开头：toolErrorText 不重复加前缀，模型看到的就是这一句。
	if !strings.HasPrefix(taskResultRequiredText, "错误：") {
		t.Fatalf("拒写文本没以「错误：」开头: %q", taskResultRequiredText)
	}
}

// —— A2：闸在真写入路径上（拒绝不留半截；通过后两列一起写；超长截断）——
func TestUpdateTaskRequiresResultOnComplete(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDSN(t, testLease, testTTL)
	rid := RunID("r_gate")
	no, err := s.CreateTask(ctx, "ten_gate", rid, "s_gate", "跑基线", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, "ten_gate", rid, no, taskStatusInProgress, "", "", "", ""); err != nil {
		t.Fatal(err)
	}

	// 拒绝：状态与 result 都不写（整笔回滚）。
	err = s.UpdateTask(ctx, "ten_gate", rid, no, taskStatusCompleted, "", "", "", "")
	if !errors.Is(err, ErrTaskResultRequired) {
		t.Fatalf("标完成不带结果 = %v, want ErrTaskResultRequired", err)
	}
	row, err := s.GetTask(ctx, rid, no)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != taskStatusInProgress || row.Result != "" {
		t.Fatalf("拒绝后行 = %+v，状态与结果都该没动", row)
	}

	// 通过：status 与 result 一起落。
	if err := s.UpdateTask(ctx, "ten_gate", rid, no, taskStatusCompleted, "p95=1.8s", "", "", ""); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetTask(ctx, rid, no)
	if row.Status != taskStatusCompleted || row.Result != "p95=1.8s" {
		t.Fatalf("通过后行 = %+v", row)
	}

	// 已有结果的行再改别的字段（status 仍传 completed）→ 不会被二次拦。
	if err := s.UpdateTask(ctx, "ten_gate", rid, no, taskStatusCompleted, "", "跑基线（改）", "", ""); err != nil {
		t.Fatalf("已完成行改主题被拦了: %v", err)
	}
	row, _ = s.GetTask(ctx, rid, no)
	if row.Subject != "跑基线（改）" || row.Result != "p95=1.8s" {
		t.Fatalf("改主题后行 = %+v", row)
	}

	// 超长：截到 300 字并标原长，不拒写。
	long := strings.Repeat("字", 350)
	no2, err := s.CreateTask(ctx, "ten_gate", rid, "s_gate", "长结果", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, "ten_gate", rid, no2, taskStatusCompleted, long, "", "", ""); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetTask(ctx, rid, no2)
	want := strings.Repeat("字", 300) + "…〔截断：原 350 字符〕"
	if row.Result != want {
		t.Fatalf("超长结果长度 = %d，want 截断标记 %q", len([]rune(row.Result)), want[len(want)-12:])
	}
}

// —— A3：TaskList 视图带结果预览；写回执与 task 帧不带（D34 取舍）——
func TestTaskListView(t *testing.T) {
	rows := []RunTask{
		{TaskNo: 1, Status: taskStatusCompleted, Subject: "跑基线", Result: "p95=1.8s，脚本 scripts/bench.py"},
		{TaskNo: 2, Status: taskStatusCompleted, Subject: "旧行没结果"},
		{TaskNo: 3, Status: taskStatusInProgress, Subject: "收口"},
	}
	got := taskListView(rows)
	want := "看板：\n" +
		"#1 [completed] 跑基线\n  结果：p95=1.8s，脚本 scripts/bench.py\n" +
		"#2 [completed] 旧行没结果\n" +
		"#3 [in_progress] 收口\n" +
		"（还有 1 项没完成：动手前把它标 in_progress，做完立刻标 completed；卡住就建一条 blocker 记录）"
	if got != want {
		t.Fatalf("TaskList 视图 =\n%q\nwant\n%q", got, want)
	}
	// 写回执 / kind=task 帧的视图不带结果（不让每张回执重付历史结果）。
	if receipt := taskBoardView(rows); strings.Contains(receipt, "结果：") {
		t.Fatalf("taskBoardView 不该带结果: %q", receipt)
	}
	// 预览上限 100 字：超了带截断标记。
	long := strings.Repeat("字", 150)
	view := taskListView([]RunTask{{TaskNo: 1, Status: taskStatusCompleted, Subject: "长", Result: long}})
	if !strings.Contains(view, strings.Repeat("字", 100)+"…〔截断：原 150 字符〕") {
		t.Fatalf("长结果预览没按 100 字截断: %q", view)
	}
}
