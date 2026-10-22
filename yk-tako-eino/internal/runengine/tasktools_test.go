package runengine

import (
	"context"
	"strings"
	"testing"
)

// —— C3：看板活得过崩溃（存储层证明）——

// 旧线的死因是「正本在进程内存，进程退出即清空」（doc/01 §3）。v6 把正本换成 PG 表 run_tasks
// → 换进程/换副本读到的还是同一批行。下面这条测试在**同一个 PG 库**上再开一个 Store 实例
// （= 另一个进程），用它读回前一个实例写的行——证明它不依赖进程内的任何东西。
func TestTaskBoardSurvivesProcessRestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	rid := RunID("r_1")
	if _, err := s.CreateTask(ctx, "ten_1", rid, "s_1", "列出工作区文件", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, "ten_1", rid, "s_1", "创建 hello.txt", "", ""); err != nil {
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
	if rows[1].Status != taskStatusCompleted {
		t.Fatalf("第 2 行状态 = %q, want completed", rows[1].Status)
	}
	// 紧凑视图也要能用（模型的眼睛 + task 帧的 text）
	view := taskBoardView(rows)
	if !strings.Contains(view, "#1 [pending] 列出工作区文件") || !strings.Contains(view, "#2 [completed]") {
		t.Fatalf("紧凑视图形状不对: %q", view)
	}
}
