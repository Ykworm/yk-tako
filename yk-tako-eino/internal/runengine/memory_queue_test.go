package runengine

// 消费者与对账器的确定性测试（doc/65 §4.6）：队列用进程内 fake，模型用脚本化假模型，
// DB 走真 PG。钉的是「消息只是叫醒，DB 才是状态真源」的四个故障语义：
// 重复消息不重复执行、消费者崩了可接管、处理失败 DB 驱动重试（MQ 不双头重试）、
// 消息丢了（或没发过）对账器补投。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fastConsumer 造一个全快参数的消费者（租约/等待/对账都调小，测试不用等真 30 秒）。
func fastConsumer(t *testing.T, s *Store, responses ...string) (*memoryConsumer, *fakeMemoryQueue, *memoryScriptModel) {
	t.Helper()
	q := newFakeMemoryQueue()
	e, m := newScriptExtractor(t, s, responses...)
	c := newMemoryConsumer(s, q, e)
	c.lease = 10 * time.Second
	c.deferDelay = time.Minute
	c.reconcileEvery = 30 * time.Second
	return c, q, m
}

// registerJob 登记一条任务并返回 id（默认 inrun；字段按需覆盖）。
func registerJob(t *testing.T, s *Store, spec MemoryJobSpec) string {
	t.Helper()
	if spec.TenantID == "" {
		spec.TenantID = "t1"
	}
	if spec.UserID == "" {
		spec.UserID = "u1"
	}
	if spec.Workspace == "" {
		spec.Workspace = "p1"
	}
	if spec.Kind == "" {
		spec.Kind = MemoryJobKindInrun
	}
	if spec.TargetLayer == "" && spec.Kind == MemoryJobKindInrun {
		spec.TargetLayer = MemoryLayerRecall
	}
	id, err := s.RegisterMemoryJob(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestMemoryClaimByIDConditions：认领条件裁决一切——
// 未到期不认领；到期 pending 认领（epoch/attempts/租约三样都动）；活租约不认领；
// 过期租约可接管；done/failed 永不认领。
func TestMemoryClaimByIDConditions(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	id := registerJob(t, s, MemoryJobSpec{RunID: "r_c1", SessionID: "s_c1", Quote: "记", SourceSeq: 1})
	// 未到期（next_attempt_at 在未来）不认领。
	if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() + interval '1 hour' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimMemoryJobByID(ctx, id, time.Minute); err != nil || job != nil {
		t.Fatalf("未到期应不可认领: job=%+v err=%v", job, err)
	}
	// 到点 → 认领成功，三样都动。
	if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() - interval '1 second' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	job, err := s.claimMemoryJobByID(ctx, id, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("到期 pending 应可认领: %+v %v", job, err)
	}
	if job.Status != MemoryJobStatusRunning || job.Attempts != 1 || job.ClaimEpoch != 1 || job.LeaseUntil == nil {
		t.Fatalf("认领后 = %+v, want running/attempts=1/epoch=1/有租约", job)
	}
	// 活租约 → 别的副本抢不走。
	if again, err := s.claimMemoryJobByID(ctx, id, time.Minute); err != nil || again != nil {
		t.Fatalf("活租约应不可认领: %+v %v", again, err)
	}
	// 租约拨过期 → 可接管，epoch 与 attempts 递增。
	if err := s.db.Exec(`UPDATE memory_jobs SET lease_until = now() - interval '1 second' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	taken, err := s.claimMemoryJobByID(ctx, id, time.Minute)
	if err != nil || taken == nil {
		t.Fatalf("过期租约应可接管: %+v %v", taken, err)
	}
	if taken.ClaimEpoch != 2 || taken.Attempts != 2 {
		t.Fatalf("接管后 = epoch %d attempts %d, want 2/2", taken.ClaimEpoch, taken.Attempts)
	}
	// done → 永不认领。
	if err := s.CompleteMemoryJob(ctx, id, taken.ClaimEpoch); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimMemoryJobByID(ctx, id, time.Minute); err != nil || job != nil {
		t.Fatalf("done 不应被认领: %+v %v", job, err)
	}
}

// TestMemoryWakeAckPaths：五类「直接 ack、不动状态、不调模型」的消息。
func TestMemoryWakeAckPaths(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	c, _, m := fastConsumer(t, s) // 模型零脚本：被调用即炸，恰好当探针

	// 不存在的任务（项目删除后旧消息）：ack，不重建。
	if err := c.handleWake(ctx, memoryMessage{JobID: "mj_nope"}); err != nil {
		t.Fatalf("缺失任务应 ack: %v", err)
	}
	// done 与 failed：直接 ack。
	for _, kind := range []struct{ status string }{{MemoryJobStatusDone}, {MemoryJobStatusFailed}} {
		id := registerJob(t, s, MemoryJobSpec{RunID: RunID("r_ack" + kind.status), SessionID: "s_ack", Quote: "记"})
		if err := s.db.Exec(`UPDATE memory_jobs SET status = ? WHERE id = ?`, kind.status, id).Error; err != nil {
			t.Fatal(err)
		}
		if err := c.handleWake(ctx, memoryMessage{JobID: id}); err != nil {
			t.Fatalf("%s 任务应 ack: %v", kind.status, err)
		}
	}
	// 活租约的 running（重复消息）：ack，不重复执行。
	id := registerJob(t, s, MemoryJobSpec{RunID: "r_live", SessionID: "s_ack", Quote: "记"})
	claimJobFor(t, s, id)
	if err := c.handleWake(ctx, memoryMessage{JobID: id}); err != nil {
		t.Fatalf("活租约重复消息应 ack: %v", err)
	}
	// 未到点的 pending（退避中）：ack，对账器到点再叫。
	id2 := registerJob(t, s, MemoryJobSpec{RunID: "r_wait", SessionID: "s_ack", Quote: "记"})
	if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() + interval '1 hour' WHERE id = ?`, id2).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.handleWake(ctx, memoryMessage{JobID: id2}); err != nil {
		t.Fatalf("未到点应 ack: %v", err)
	}
	if m.callCount() != 0 {
		t.Fatalf("五类 ack 路径都不该打模型，实际 %d 次", m.callCount())
	}
}

// TestMemoryConsumerEndToEnd：登记 → 叫醒 → 消费 → 写库 → done；重复消息不重复写。
func TestMemoryConsumerEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	c, q, _ := fastConsumer(t, s,
		`{"entries":[{"scope":"user","content":"部署走 30080","quote":"记住部署端口"}]}`,
		// 场上没有已有记忆 → 筛选步零批、不打模型；脚本直接接最终裁决。
		`{"actions":[{"op":"insert","scope":"user","content":"部署走 30080","quote":"记住部署端口"}]}`,
	)
	id := registerJob(t, s, MemoryJobSpec{RunID: "r_e2e", SessionID: "s_e2e", Quote: "记住部署端口", SourceSeq: 1})
	if err := q.Publish(ctx, memoryMessage{JobID: id, Key: "s_e2e"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx); err != nil {
		t.Fatalf("消费: %v", err)
	}
	row := s.jobOf(t, "r_e2e")
	if row.Status != MemoryJobStatusDone {
		t.Fatalf("job = %+v, want done", row)
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "部署走 30080"); n != 1 {
		t.Fatalf("记忆行 = %d, want 1", n)
	}
	// 同一任务的消息再来一次（重复投递）：done → ack，不再写。
	q2 := newFakeMemoryQueue()
	if err := q2.Publish(ctx, memoryMessage{JobID: id, Key: "s_e2e"}); err != nil {
		t.Fatal(err)
	}
	c2, _, _ := fastConsumer(t, s) // 零脚本：被调用即炸
	c2.queue = q2
	if err := c2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "部署走 30080"); n != 1 {
		t.Fatalf("重复消费后记忆行 = %d, want 1（不重复执行）", n)
	}
}

// TestMemoryConsumerFailureBacksOff：处理失败 → 回 pending + last_error + 退避
// （第 1 次 1 分钟、第 2 次 5 分钟）+ **消息仍被 ack**（fake 队列不重投——
// 重试由对账器到点补投，禁止 MQ 与 DB 双头重试）。
func TestMemoryConsumerFailureBacksOff(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id := registerJob(t, s, MemoryJobSpec{RunID: "r_fail", SessionID: "s_fail", Quote: "记", SourceSeq: 1})

	// 第 1 次失败：模型输出不可解析。
	c, q, _ := fastConsumer(t, s, `{这不是 JSON`)
	if err := c.handleWake(ctx, memoryMessage{JobID: id}); err != nil {
		t.Fatalf("处理失败也该 ack: %v", err)
	}
	row := s.jobOf(t, "r_fail")
	if row.Status != MemoryJobStatusPending || row.LastError == "" {
		t.Fatalf("第 1 次失败后 = %+v, want pending + last_error", row)
	}
	if d := time.Until(row.NextAttemptAt); d < 55*time.Second || d > 65*time.Second {
		t.Fatalf("第 1 次失败退避 = %v, want ≈1 分钟", d)
	}
	if len(q.Published()) != 0 {
		t.Fatalf("失败后队列又投了 %d 条, want 0（MQ 不双头重试）", len(q.Published()))
	}
	// 拨到点 → 第 2 次失败 → ≈5 分钟。
	if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() - interval '1 second' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	c2, _, _ := fastConsumer(t, s, `还是坏输出`)
	if err := c2.handleWake(ctx, memoryMessage{JobID: id}); err != nil {
		t.Fatal(err)
	}
	row = s.jobOf(t, "r_fail")
	if row.Status != MemoryJobStatusPending || row.Attempts != 2 {
		t.Fatalf("第 2 次失败后 = %+v, want pending/attempts=2", row)
	}
	if d := time.Until(row.NextAttemptAt); d < 4*time.Minute-5*time.Second || d > 5*time.Minute+5*time.Second {
		t.Fatalf("第 2 次失败退避 = %v, want ≈5 分钟", d)
	}
}

// TestMemoryPostrunWaitsForUnfinishedInrun：同 session 有未完成 inrun → postrun
// 等待：保持 pending、**不消耗 attempts**、短 next_attempt_at、不打模型；
// inrun **failed 不算未完成** → postrun 继续，但记 inrun_failed 警告（job 行 + 日志）。
func TestMemoryPostrunWaitsForUnfinishedInrun(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	c, _, m := fastConsumer(t, s,
		// r_p2（无候选 → 筛选零批）：抽条目 + 最终裁决。
		`{"entries":[{"scope":"user","content":"整场抽出的条目","quote":"原话"}]}`,
		`{"actions":[{"op":"insert","scope":"user","content":"整场抽出的条目","quote":"原话"}]}`,
		// r_p1（此时「整场抽出的条目」已是候选 → 筛选有一批）：抽条目 + 筛相关 + 最终裁决。
		`{"entries":[{"scope":"user","content":"整场抽出的条目","quote":"原话"}]}`,
		`{"relevant":[]}`,
		`{"actions":[{"op":"insert","scope":"user","content":"整场抽出的条目","quote":"原话"}]}`,
	)

	inrun := registerJob(t, s, MemoryJobSpec{RunID: "r_i1", SessionID: "s_p1", Kind: MemoryJobKindInrun, Quote: "记", SourceSeq: 1})
	postrun := registerJob(t, s, MemoryJobSpec{RunID: "r_p1", SessionID: "s_p1", Kind: MemoryJobKindPostrun, Quote: "整场"})

	// inrun pending（未完成）→ postrun 等待。
	if err := c.handleWake(ctx, memoryMessage{JobID: postrun}); err != nil {
		t.Fatal(err)
	}
	row := s.jobOf(t, "r_p1")
	if row.Status != MemoryJobStatusPending || row.Attempts != 0 {
		t.Fatalf("等待的 postrun = %+v, want pending 且不消耗 attempts", row)
	}
	if d := time.Until(row.NextAttemptAt); d <= 0 || d > c.deferDelay+5*time.Second {
		t.Fatalf("等待的 postrun next_attempt_at = %v, want ≈%v（短延迟，由对账器再叫）", d, c.deferDelay)
	}
	if m.callCount() != 0 {
		t.Fatalf("等待路径不该打模型，实际 %d 次", m.callCount())
	}

	// 不同 session 的 postrun 不被别家的 inrun 卡住。
	other := registerJob(t, s, MemoryJobSpec{RunID: "r_p2", SessionID: "s_p2", Kind: MemoryJobKindPostrun})
	if err := c.handleWake(ctx, memoryMessage{JobID: other}); err != nil {
		t.Fatal(err)
	}
	if got := s.jobOf(t, "r_p2"); got.Status != MemoryJobStatusDone {
		t.Fatalf("别家 session 的 postrun 被卡住了: %+v", got)
	}

	// inrun 走到 failed：不再是「未完成」→ postrun 继续，但 job 行记 inrun_failed 警告。
	for i := 0; i < memoryMaxAttempts; i++ {
		job, err := s.claimMemoryJobByID(ctx, inrun, time.Minute)
		if err != nil || job == nil {
			t.Fatalf("第 %d 次认领 inrun: %+v %v", i+1, job, err)
		}
		if err := s.FailMemoryJob(ctx, inrun, job.ClaimEpoch, errors.New("模型炸了")); err != nil {
			t.Fatal(err)
		}
		// 退避拨回过去，让下一次认领立刻能拿到（最后一次不需要）。
		if i < memoryMaxAttempts-1 {
			if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() - interval '1 second' WHERE id = ?`, inrun).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() - interval '1 second' WHERE id = ?`, postrun).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.handleWake(ctx, memoryMessage{JobID: postrun}); err != nil {
		t.Fatal(err)
	}
	row = s.jobOf(t, "r_p1")
	if row.Status != MemoryJobStatusDone {
		t.Fatalf("failed inrun 不该卡 postrun: %+v", row)
	}
	if !strings.Contains(row.LastError, "inrun_failed") {
		t.Fatalf("postrun 的 job 行没记 inrun_failed 警告: last_error=%q", row.LastError)
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "整场抽出的条目"); n != 2 {
		t.Fatalf("两个 postrun 各写一行（设计上不做正文去重）, 记忆行 = %d", n)
	}
}

// TestMemoryReconcilerRepublishes：对账器补投四类——超龄 pending、到点重试、
// 租约过期 running、postrun 等待到期；done/failed/未到期/活租约不投；只发 job_id。
func TestMemoryReconcilerRepublishes(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	c, q, _ := fastConsumer(t, s)

	overdue := registerJob(t, s, MemoryJobSpec{RunID: "r_rc1", SessionID: "s_rc", Quote: "记"}) // 登记即到期
	future := registerJob(t, s, MemoryJobSpec{RunID: "r_rc2", SessionID: "s_rc", Quote: "记"})
	if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() + interval '1 hour' WHERE id = ?`, future).Error; err != nil {
		t.Fatal(err)
	}
	expiredRunning := registerJob(t, s, MemoryJobSpec{RunID: "r_rc3", SessionID: "s_rc", Quote: "记"})
	claimJobFor(t, s, expiredRunning)
	if err := s.db.Exec(`UPDATE memory_jobs SET lease_until = now() - interval '1 second' WHERE id = ?`, expiredRunning).Error; err != nil {
		t.Fatal(err)
	}
	liveRunning := registerJob(t, s, MemoryJobSpec{RunID: "r_rc4", SessionID: "s_rc", Quote: "记"})
	claimJobFor(t, s, liveRunning) // 租约是活的
	doneJob := registerJob(t, s, MemoryJobSpec{RunID: "r_rc5", SessionID: "s_rc", Kind: MemoryJobKindPostrun})
	doneClaim := claimJobFor(t, s, doneJob) // done 翻转也要走 epoch 守卫
	if err := s.CompleteMemoryJob(ctx, doneJob, doneClaim.ClaimEpoch); err != nil {
		t.Fatal(err)
	}

	n, err := c.reconcileOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("补投 %d 条, want 2（超龄 pending + 租约过期 running）", n)
	}
	got := map[string]bool{}
	for _, msg := range q.Published() {
		got[msg.JobID] = true
		if msg.Key == "" {
			t.Fatalf("补投消息缺分区 key: %+v", msg)
		}
	}
	if !got[overdue] || !got[expiredRunning] {
		t.Fatalf("补投了 %v, want 含 %s 与 %s", got, overdue, expiredRunning)
	}
	for _, absent := range []string{future, liveRunning, doneJob} {
		if got[absent] {
			t.Fatalf("不该补投 %s", absent)
		}
	}
}

// TestMemoryRenewLease：续约顺延租约；旧 epoch / 非 running 续不上（返回 false）。
func TestMemoryRenewLease(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id := registerJob(t, s, MemoryJobSpec{RunID: "r_rn", SessionID: "s_rn", Quote: "记", SourceSeq: 1})
	job := claimJobFor(t, s, id)
	before := *job.LeaseUntil

	time.Sleep(10 * time.Millisecond)
	ok, err := s.renewMemoryJobLease(ctx, id, job.ClaimEpoch, time.Minute)
	if err != nil || !ok {
		t.Fatalf("续约: ok=%v err=%v", ok, err)
	}
	cur := s.jobOf(t, "r_rn")
	if !cur.LeaseUntil.After(before) {
		t.Fatalf("续约后租约 %v 没有顺延（之前 %v）", cur.LeaseUntil, before)
	}
	// 旧 epoch 续不上。
	if ok, err := s.renewMemoryJobLease(ctx, id, job.ClaimEpoch-1, time.Minute); err != nil || ok {
		t.Fatalf("旧 epoch 续约 = %v %v, want false", ok, err)
	}
	// done 之后续不上。
	if err := s.CompleteMemoryJob(ctx, id, job.ClaimEpoch); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.renewMemoryJobLease(ctx, id, job.ClaimEpoch, time.Minute); err != nil || ok {
		t.Fatalf("done 后续约 = %v %v, want false", ok, err)
	}
}

// TestMemoryRenewLoopCancelsOnLeaseLoss：租约被接管 → 续约循环取消本次处理
// （处理中的抽取器随 ctx 取消，写回被 epoch 守卫拒绝）。
func TestMemoryRenewLoopCancelsOnLeaseLoss(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id := registerJob(t, s, MemoryJobSpec{RunID: "r_rl", SessionID: "s_rl", Quote: "记", SourceSeq: 1})
	job := claimJobFor(t, s, id)

	// 模拟接管：租约拨过期后由「另一个副本」认领（epoch+1）。
	if err := s.db.Exec(`UPDATE memory_jobs SET lease_until = now() - interval '1 second' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.claimMemoryJobByID(ctx, id, time.Minute); err != nil {
		t.Fatal(err)
	}

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	c := &memoryConsumer{store: s, lease: 300 * time.Millisecond} // tick = 100ms：测试不用等秒级
	c.renewLoop(ctx, pctx, cancel, job.ID, job.ClaimEpoch, done)
	<-done
	if pctx.Err() == nil {
		t.Fatal("租约丢失后应取消处理 ctx")
	}
}

// TestMemoryWakeFuncPublishesJobID：登记后的叫醒只发 job_id + 分区 key（消息不是任务正文）。
func TestMemoryWakeFuncPublishesJobID(t *testing.T) {
	q := newFakeMemoryQueue()
	wake := newMemoryWakeFunc(q)
	wake(context.Background(), &MemoryJob{ID: "mj_w", SessionID: "s_w", TenantID: "t1", Workspace: "p1", Kind: MemoryJobKindInrun})
	pub := q.Published()
	if len(pub) != 1 || pub[0].JobID != "mj_w" || pub[0].Key != "s_w" {
		t.Fatalf("published = %+v, want 单条 {job_id, key=s_w}", pub)
	}
	// tidy 没有来源 session：key 用 tidy:<tenant>:<workspace>，不伪造用户会话。
	wake(context.Background(), &MemoryJob{ID: "mj_w2", TenantID: "t1", Workspace: "p1", Kind: MemoryJobKindTidy})
	pub = q.Published()
	if len(pub) != 2 || pub[1].Key != "tidy:t1:p1" {
		t.Fatalf("published = %+v, want tidy 键 tidy:t1:p1", pub)
	}
	// 空任务不发。
	wake(context.Background(), nil)
	if len(q.Published()) != 2 {
		t.Fatalf("空任务不该发消息")
	}
}

// TestMemoryQueueFakeRequeue：handler 失败 → 消息放回队首（不丢）；下一轮重投。
func TestMemoryQueueFakeRequeue(t *testing.T) {
	ctx := context.Background()
	q := newFakeMemoryQueue()
	if err := q.Publish(ctx, memoryMessage{JobID: "mj_q", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := q.Consume(ctx, func(context.Context, memoryMessage) error {
		calls++
		return fmt.Errorf("第一次失败")
	}); err == nil {
		t.Fatal("handler 失败应上抛")
	}
	if calls != 1 {
		t.Fatalf("handler 调了 %d 次, want 1", calls)
	}
	// 消息还在队里（没丢）：
	seen := 0
	if err := q.Consume(ctx, func(_ context.Context, msg memoryMessage) error {
		seen++
		if msg.JobID != "mj_q" {
			t.Fatalf("重投的消息不对: %+v", msg)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("重投看到 %d 条, want 1（消息不丢）", seen)
	}
}

// TestRedpandaQueueFailsFastOnUnreachableBroker：kgo.NewClient 是懒连接，
// 启动探针必须在构造期就把「broker 不可达」报出来（C3 §1.3），且错误文案不含密码。
// 127.0.0.1:1（tcpmux）正常机器不会有监听者；真 broker 的 topic 缺失路径归活体脚本。
func TestRedpandaQueueFailsFastOnUnreachableBroker(t *testing.T) {
	const pass = "probe-pass-must-not-leak"
	_, err := newRedpandaQueue(memoryQueueConfig{
		Brokers:  []string{"127.0.0.1:1"},
		Topic:    "tako.memory.extract",
		Group:    "tako-memory-extractor-v1",
		User:     "probe",
		Password: pass,
		Mech:     "scram-sha-256",
	})
	if !errors.Is(err, ErrMemoryQueueUnavailable) {
		t.Fatalf("err = %v, want ErrMemoryQueueUnavailable", err)
	}
	if strings.Contains(err.Error(), pass) {
		t.Fatal("错误文案泄露密码")
	}
}
