package runengine

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestWakeBatch 钉住唤醒合并（施工包 §11.1 第 12 条）：一次 XRead 读到的多条事件
// 只算一次唤醒，位置推进到最后一条；空批次既不唤醒也不推进。
func TestWakeBatch(t *testing.T) {
	if lastID, triggered := wakeBatch(nil, "0-0"); triggered || lastID != "0-0" {
		t.Fatalf("空批次 = %q/%v, want 0-0/false", lastID, triggered)
	}
	streams := []redis.XStream{
		{Stream: keyTaskWake, Messages: []redis.XMessage{{ID: "1-0"}, {ID: "1-1"}}},
		{Stream: keyTaskWake, Messages: []redis.XMessage{{ID: "1-2"}}},
	}
	lastID, triggered := wakeBatch(streams, "0-0")
	if !triggered || lastID != "1-2" {
		t.Fatalf("三条事件 = %q/%v, want 1-2/true（合并成一轮，位置取最后）", lastID, triggered)
	}
	if lastID, triggered = wakeBatch([]redis.XStream{{Stream: keyTaskWake}}, lastID); triggered || lastID != "1-2" {
		t.Fatalf("空流 = %q/%v, want 1-2/false", lastID, triggered)
	}
}

// TestTaskSchedulerStartupReconcileAndWake 钉住启动对账与事件唤醒（施工包 §11.1 第 12 条）：
//   - interval 设成 1h：循环自己不会走到下一轮，启动那一轮必须领走已有 ready；
//   - 后续靠内部流唤醒，间隔仍是 1h；
//   - 多条事件合并成一轮，领取仍由 PG CAS 裁决，只产生一场执行。
func TestTaskSchedulerStartupReconcileAndWake(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	create := func(key string) string {
		t.Helper()
		resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
			IdempotencyKey: key, WorkspaceID: wsID, SourceSessionID: sessID,
			Content: "唤醒测试 " + key, Agent: "researcher",
		}, planEnv{})
		if err != nil {
			t.Fatalf("CreateAgentTask(%s): %v", key, err)
		}
		return resp.TaskID
	}
	tid1 := create("wake-startup")
	// 清掉创建时发出的唤醒：第一段只验「没有事件也能靠启动对账领走」。
	if err := s.rdb.Del(ctx, keyTaskWake).Err(); err != nil {
		t.Fatal(err)
	}

	ts := &TaskScheduler{store: s, interval: time.Hour}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.Run(ctx)
	}()
	waitCond(t, "启动对账领取启动前已就绪的 Task", 5*time.Second, func() bool {
		return taskClaimed(t, s, tid1)
	})

	// 事件唤醒：创建第二件本身就会发一条唤醒；再补两条，验证合并后仍只领一次。
	tid2 := create("wake-event")
	for i := 0; i < 2; i++ {
		s.PublishTaskWake(ctx, testOwner.TenantID, "test-extra", tid2)
	}
	waitCond(t, "内部流事件唤醒领取新 ready", 5*time.Second, func() bool {
		return taskClaimed(t, s, tid2)
	})
	for _, tid := range []string{tid1, tid2} {
		if got := s.count(t, &Run{}, "id = (SELECT current_run_id FROM agent_tasks WHERE id = ?)", tid); got != 1 {
			t.Fatalf("Task %s 的执行 run = %d, want 1（重影执行或合并后重复领取）", tid, got)
		}
	}

	cancel()
	// XREAD BLOCK 最长 5 秒：取消后要等这条阻塞命令返回，给足一个窗口。
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("取消后调度循环没有退出")
	}
}

// TestTaskSchedulerRedisErrorBackoff 钉住 Redis 故障不退进程（施工包 §11.1 第 12 条）：
// 唤醒流不可达时 Run 只记日志并退避，循环继续活着，取消能立刻回。
func TestTaskSchedulerRedisErrorBackoff(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	bad := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1,
	})
	s.rdb = bad
	t.Cleanup(func() { _ = bad.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	ts := &TaskScheduler{store: s, interval: time.Hour}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.Run(ctx)
	}()
	// 跨过一轮「错误 → 退避 1 秒」；紧循环会在同窗口里打爆 CPU，这里至少证明循环没退出。
	select {
	case <-done:
		t.Fatal("Redis 不可达时调度循环退出了")
	case <-time.After(1200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后调度循环没有退出")
	}
}

// TestTaskSchedulerRunOnceOrder 钉住 RunOnce 的固定顺序（施工包 §11.1 第 12 条）：
// 上游 run 已定稿的那一轮里，收口父 Task → 晋升子 Task → 领取子 Task 必须同轮完成。
func TestTaskSchedulerRunOnceOrder(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "order"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	claims := claimAll(t, s, resp.OrchestrationID)
	if len(claims) != 2 {
		t.Fatalf("首轮领取 = %d, want 2（A、B 并行）", len(claims))
	}
	for _, c := range claims {
		submitOK(t, s, c.TaskID, c.RunID, c.Epoch)
		setRunStatus(t, s, c.RunID, "complete")
	}

	(&TaskScheduler{store: s, interval: time.Hour}).RunOnce(ctx)

	for _, c := range claims {
		if st, code := taskStatus(t, s, c.TaskID); st != agentTaskStatusSucceeded {
			t.Fatalf("父 Task %s = %s/%s, want succeeded", c.TaskKey, st, code)
		}
	}
	var child AgentTask
	if err := s.db.Where("orchestration_id = ? AND task_key = ?", resp.OrchestrationID, "C").
		Take(&child).Error; err != nil {
		t.Fatal(err)
	}
	if child.Status != agentTaskStatusRunning || child.CurrentRunID == nil || *child.CurrentRunID == "" {
		t.Fatalf("子 Task = %s（run=%v）, want 同一轮 running", child.Status, child.CurrentRunID)
	}
}

// taskClaimed 读存储事实：Task 已有执行会话与当前 run（领取的唯一证据）。
func taskClaimed(t *testing.T, s *Store, taskID string) bool {
	t.Helper()
	var n int64
	if err := s.db.Raw(
		`SELECT count(*) FROM agent_tasks WHERE id = ? AND execution_session_id IS NOT NULL AND current_run_id IS NOT NULL`,
		taskID).Scan(&n).Error; err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return n == 1
}
