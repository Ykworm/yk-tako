package runengine

// 历史对话混合检索的 outbox 存储层回归（doc/84 §4.2/§4.3/§6.1；施工包 P1 第 7 条）。
//
// 断言的三个数据库不变量（与 memory_jobs 同一套纪律）：
//   - 登记唯一：同事件重复登记回读原行（UNIQUE 撞行回读），move 带归属修正；
//   - 领取互斥：两个副本（两个 Store 实例、同一份 PG）并发领同一批，FOR UPDATE
//     SKIP LOCKED 保证每行只被一个副本拿到；
//   - 写回守卫：完成/失败/续租必须匹配 id + claim_epoch，旧代次一行都改不了；
//   - 退避死信：瞬时失败 1/5/30 分钟退避，第三次 failed 持久死信；永久错误当场死信。
//
// 投递侧断言至少一次语义：relay 投递成功才写 published_at；标记丢失 → 重投 →
// 重复消息由消费侧认领条件挡住（见 dialogue_clients_test.go 的消费端配对断言）。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// newDialogueHybridStore 起一个 hybrid 已装配的隔离存储：指针已播种（version=1）、
// 登记口已知道 hybrid 与 embedding 模型、RAG 开（对话块索引循环的输入在）。
func newDialogueHybridStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t, testLease, testTTL)
	if _, err := s.EnsureDialogueRetrievalPointer(context.Background(), "tako_dialogue_test", "tako_dialogue_test"); err != nil {
		t.Fatal(err)
	}
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: "/tmp/ws-test", RAG: true})
	s.ConfigureDialogueRetrieval(true, "test-embed-model")
	return s
}

// dialogueJobRow 读一条任务行（无行 Fatal）。
func dialogueJobRow(t *testing.T, s *Store, runID, kind, target string) *DialogueIndexJob {
	t.Helper()
	var job DialogueIndexJob
	if err := s.db.Raw(
		`SELECT * FROM dialogue_index_jobs WHERE run_id = ? AND kind = ? AND target = ?`,
		runID, kind, target).Scan(&job).Error; err != nil {
		t.Fatalf("读任务 %s/%s/%s: %v", runID, kind, target, err)
	}
	if job.ID == "" {
		t.Fatalf("任务 %s/%s/%s 不存在", runID, kind, target)
	}
	return &job
}

// forceDialogueJobDue 把租约拨过期、退避拨到期（真钟下代替 FastForward）。
func forceDialogueJobDue(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.db.Exec(
		`UPDATE dialogue_index_jobs SET lease_until = now() - interval '1 hour',
		        next_attempt_at = now() - interval '1 hour' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
}

// TestDialogueOutboxRegistrationIdempotent：同一事件重复登记回读原行——
// 不产生新行、共用 event_id、纯幂等（不重置已领取的状态）。
func TestDialogueOutboxRegistrationIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	spec := DialogueIndexEventSpec{
		RunID: "r_1", SessionID: "s_1",
		TenantID: testOwner.TenantID, UserID: testOwner.UserID,
		Kind: DialogueJobKindUpsert, EmbeddingModel: "test-embed-model",
	}
	eid1, err := s.RegisterDialogueIndexJobs(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.count(t, &DialogueIndexJob{}, "run_id = ?", "r_1"); got != 2 {
		t.Fatalf("首次登记行数 = %d, want 2（es + milvus）", got)
	}
	es := dialogueJobRow(t, s, "r_1", DialogueJobKindUpsert, DialogueTargetES)
	mil := dialogueJobRow(t, s, "r_1", DialogueJobKindUpsert, DialogueTargetMilvus)
	if es.EventID != eid1 || mil.EventID != eid1 {
		t.Fatalf("event_id 不共用：es=%s mil=%s want %s", es.EventID, mil.EventID, eid1)
	}
	if es.EmbeddingModel != "test-embed-model" || es.TenantID != testOwner.TenantID {
		t.Fatalf("任务归属/模型没落行: %+v", es)
	}

	// 重复登记：回读原 event_id，行数不变。
	eid2, err := s.RegisterDialogueIndexJobs(ctx, spec)
	if err != nil || eid2 != eid1 {
		t.Fatalf("重复登记 = (%s, %v), want (%s, nil)", eid2, err, eid1)
	}
	if got := s.count(t, &DialogueIndexJob{}, "run_id = ?", "r_1"); got != 2 {
		t.Fatalf("重复登记后行数 = %d, want 2", got)
	}

	// 已领取（running）的任务重复登记不改状态（纯幂等，不是重置）。
	claimed, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领 = %d err=%v, want 1", len(claimed), err)
	}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec); err != nil {
		t.Fatal(err)
	}
	after := dialogueJobRow(t, s, "r_1", DialogueJobKindUpsert, DialogueTargetES)
	if after.Status != DialogueJobStatusRunning || after.Attempts != 1 {
		t.Fatalf("running 任务被重复登记重置: status=%s attempts=%d", after.Status, after.Attempts)
	}
}

// TestDialogueMoveCorrectionResetsTerminal：move 冲突的修正语义——归属变了更新
// workspace_id；已终态（done）的行重置回 pending、清 published_at（relay 重投），
// 处理 A→B→A 的来回移动。
func TestDialogueMoveCorrectionResetsTerminal(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	spec := DialogueIndexEventSpec{
		RunID: "r_m", SessionID: "s_m", TenantID: testOwner.TenantID,
		Kind: DialogueJobKindMove, WorkspaceID: "ws_A",
	}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec); err != nil {
		t.Fatal(err)
	}
	es := dialogueJobRow(t, s, "r_m", DialogueJobKindMove, DialogueTargetES)

	// 领取并做完（终态 done）。
	claimed, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领 = %d err=%v", len(claimed), err)
	}
	if err := s.completeDialogueJob(ctx, es.ID, claimed[0].ClaimEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.markDialogueJobPublished(ctx, es.EventID, DialogueTargetES); err != nil {
		t.Fatal(err)
	}

	// move 到 B：done 行重置回 pending，workspace 更新，published_at 清掉。
	spec.WorkspaceID = "ws_B"
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec); err != nil {
		t.Fatal(err)
	}
	moved := dialogueJobRow(t, s, "r_m", DialogueJobKindMove, DialogueTargetES)
	if moved.Status != DialogueJobStatusPending || moved.WorkspaceID != "ws_B" {
		t.Fatalf("move 后 = status=%s ws=%s, want pending/ws_B", moved.Status, moved.WorkspaceID)
	}
	if moved.PublishedAt != nil || moved.DoneAt != nil || moved.Attempts != 0 {
		t.Fatalf("move 重置不干净: published=%v done=%v attempts=%d",
			moved.PublishedAt, moved.DoneAt, moved.Attempts)
	}

	// relay 会重新投递：published_at 缺失的行重新出现在未投递扫描里。
	unpub, err := s.unpublishedDialogueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range unpub {
		if j.ID == moved.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("重置后的 move 行不在未投递扫描里（relay 不会重投）")
	}

	// move 回 A（A→B→A）：再重置一次。
	spec.WorkspaceID = "ws_A"
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec); err != nil {
		t.Fatal(err)
	}
	back := dialogueJobRow(t, s, "r_m", DialogueJobKindMove, DialogueTargetES)
	if back.Status != DialogueJobStatusPending || back.WorkspaceID != "ws_A" {
		t.Fatalf("move 回 A = status=%s ws=%s", back.Status, back.WorkspaceID)
	}
}

// TestDialogueClaimConcurrentReplicas：两个副本（两个 Store 实例、同一份 PG）并发
// 认领同一批任务——FOR UPDATE SKIP LOCKED 保证每行只被一个副本拿到，attempts 只加一次。
func TestDialogueClaimConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	s1, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	if _, err := s1.EnsureDialogueRetrievalPointer(ctx, "tako_dialogue_test", "tako_dialogue_test"); err != nil {
		t.Fatal(err)
	}
	s1.ConfigureDialogueRetrieval(true, "m")
	s2 := secondStoreOn(t, dsn)

	// 登记 6 个事件（12 行），拿全部任务 id。
	ids := make([]string, 0, 12)
	for i := 0; i < 6; i++ {
		rid := RunID("r_cc" + string(rune('a'+i)))
		if _, err := s1.RegisterDialogueIndexJobs(ctx, DialogueIndexEventSpec{
			RunID: rid, SessionID: SessionID("s_cc" + string(rune('a'+i))),
			TenantID: testOwner.TenantID, Kind: DialogueJobKindUpsert,
		}); err != nil {
			t.Fatal(err)
		}
		var rows []DialogueIndexJob
		if err := s1.db.Raw(`SELECT * FROM dialogue_index_jobs WHERE run_id = ?`, rid).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) != 12 {
		t.Fatalf("登记出 %d 行, want 12", len(ids))
	}

	// 两个副本同时认领同一批。
	start := make(chan struct{})
	var (
		mu       sync.Mutex
		gotA     = map[string]bool{}
		gotB     = map[string]bool{}
		wg       sync.WaitGroup
		firstErr error
	)
	claim := func(s *Store, into map[string]bool) {
		defer wg.Done()
		<-start
		jobs, err := s.claimDialogueJobsByIDs(ctx, ids, dialogueJobLease)
		if err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			return
		}
		mu.Lock()
		for _, j := range jobs {
			into[j.ID] = true
		}
		mu.Unlock()
	}
	wg.Add(2)
	go claim(s1, gotA)
	go claim(s2, gotB)
	close(start)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("并发认领出错: %v", firstErr)
	}

	// 每行恰好被一个副本拿到：并集 = 全集，交集 = 空。
	for _, id := range ids {
		a, b := gotA[id], gotB[id]
		if a == b {
			t.Fatalf("任务 %s 认领分布不对: A=%v B=%v（重复领取或双双漏领）", id, a, b)
		}
	}
	if len(gotA)+len(gotB) != len(ids) {
		t.Fatalf("认领总数 = %d+%d, want %d", len(gotA), len(gotB), len(ids))
	}
	// 领过的行：status=running、attempts=1（只有一次认领生效）。
	var bad int
	if err := s1.db.Raw(
		`SELECT count(*) FROM dialogue_index_jobs WHERE status != 'running' OR attempts != 1`,
	).Scan(&bad).Error; err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d 行的 status/attempts 形态不对（认领不原子）", bad)
	}
}

// TestDialogueStaleEpochWriteRejected：旧 claim_epoch 的完成/失败/续租一行都改不了
// （副本崩溃 → 租约过期 → 另一副本接管 → 旧 attempt 的迟到写回被守卫拒绝）。
func TestDialogueStaleEpochWriteRejected(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	spec := DialogueIndexEventSpec{RunID: "r_e", SessionID: "s_e", TenantID: "t", Kind: DialogueJobKindUpsert}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec); err != nil {
		t.Fatal(err)
	}
	es := dialogueJobRow(t, s, "r_e", DialogueJobKindUpsert, DialogueTargetES)

	// 第一代次领取。
	first, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease)
	if err != nil || len(first) != 1 {
		t.Fatalf("首次认领 = %d err=%v", len(first), err)
	}
	oldEpoch := first[0].ClaimEpoch

	// 模拟崩溃接管：租约拨过期，重新领取 → claim_epoch 递增。
	forceDialogueJobDue(t, s, es.ID)
	taken, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease)
	if err != nil || len(taken) != 1 {
		t.Fatalf("接管认领 = %d err=%v", len(taken), err)
	}
	newEpoch := taken[0].ClaimEpoch
	if newEpoch != oldEpoch+1 {
		t.Fatalf("claim_epoch = %d, want %d+1", newEpoch, oldEpoch)
	}

	// 旧代次的三个写全部被拒绝：完成、失败、续租。
	if err := s.completeDialogueJob(ctx, es.ID, oldEpoch); err != nil {
		t.Fatal(err)
	}
	row := dialogueJobRow(t, s, "r_e", DialogueJobKindUpsert, DialogueTargetES)
	if row.Status != DialogueJobStatusRunning {
		t.Fatalf("旧 epoch 完成写回生效了: status=%s", row.Status)
	}
	if err := s.failDialogueJob(ctx, es.ID, oldEpoch, "es_unavailable", false); err != nil {
		t.Fatal(err)
	}
	row = dialogueJobRow(t, s, "r_e", DialogueJobKindUpsert, DialogueTargetES)
	if row.Status != DialogueJobStatusRunning || row.LastError != "" {
		t.Fatalf("旧 epoch 失败写回生效了: status=%s last_error=%q", row.Status, row.LastError)
	}
	if ok, err := s.renewDialogueJobLease(ctx, es.ID, oldEpoch, dialogueJobLease); err != nil || ok {
		t.Fatalf("旧 epoch 续租 = (%v, %v), want (false, nil)", ok, err)
	}

	// 新代次写回正常生效。
	if ok, err := s.renewDialogueJobLease(ctx, es.ID, newEpoch, dialogueJobLease); err != nil || !ok {
		t.Fatalf("新 epoch 续租 = (%v, %v), want (true, nil)", ok, err)
	}
	if err := s.completeDialogueJob(ctx, es.ID, newEpoch); err != nil {
		t.Fatal(err)
	}
	row = dialogueJobRow(t, s, "r_e", DialogueJobKindUpsert, DialogueTargetES)
	if row.Status != DialogueJobStatusDone || row.LastError != "" {
		t.Fatalf("新 epoch 完成: status=%s last_error=%q", row.Status, row.LastError)
	}
	// 终态行不可再领取。
	again, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease)
	if err != nil || len(again) != 0 {
		t.Fatalf("done 行被再次认领 = %d err=%v", len(again), err)
	}
}

// TestDialogueFailBackoffAndDeadLetter：瞬时失败按 1/5/30 分钟退避（attempts 1-based
// 取下标），第三次失败进 failed 持久死信；永久错误当场死信；退避中的行不被扫描。
func TestDialogueFailBackoffAndDeadLetter(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	claimOne := func(runID, kind, target string) (*DialogueIndexJob, int64) {
		t.Helper()
		row := dialogueJobRow(t, s, runID, kind, target)
		forceDialogueJobDue(t, s, row.ID)
		jobs, err := s.claimDialogueJobsByIDs(ctx, []string{row.ID}, dialogueJobLease)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("认领 %s = %d err=%v", row.ID, len(jobs), err)
		}
		// 返回认领快照（带本次 attempts+1 与新 epoch）：登记后先读行再认领会拿到旧值。
		return jobs[0], jobs[0].ClaimEpoch
	}

	// ── 瞬时三连退避 → 死信 ──
	spec := DialogueIndexEventSpec{RunID: "r_b1", SessionID: "s_b1", TenantID: "t", Kind: DialogueJobKindUpsert}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec); err != nil {
		t.Fatal(err)
	}
	wantBackoff := dialogueRetryBackoff
	for attempt := 1; attempt <= dialogueMaxAttempts; attempt++ {
		claimed, epoch := claimOne("r_b1", DialogueJobKindUpsert, DialogueTargetES)
		if claimed.Attempts != attempt {
			t.Fatalf("第 %d 次认领 attempts = %d", attempt, claimed.Attempts)
		}
		if err := s.failDialogueJob(ctx, claimed.ID, epoch, "es_unavailable", false); err != nil {
			t.Fatal(err)
		}
		after := dialogueJobRow(t, s, "r_b1", DialogueJobKindUpsert, DialogueTargetES)
		if attempt < dialogueMaxAttempts {
			// 回 pending，next_attempt_at ≈ now + 第 attempt 档退避。
			if after.Status != DialogueJobStatusPending {
				t.Fatalf("第 %d 次失败后 status = %s, want pending", attempt, after.Status)
			}
			delay := time.Until(after.NextAttemptAt)
			want := wantBackoff[attempt-1]
			if delay < want-10*time.Second || delay > want+20*time.Second {
				t.Fatalf("第 %d 次失败退避 = %v, want ≈%v", attempt, delay, want)
			}
			if after.LastError != "es_unavailable" {
				t.Fatalf("last_error = %q, want 机器分类 es_unavailable", after.LastError)
			}
		} else {
			// 第三次：failed 持久死信。
			if after.Status != DialogueJobStatusFailed {
				t.Fatalf("第三次失败后 status = %s, want failed", after.Status)
			}
		}
	}
	dead := dialogueJobRow(t, s, "r_b1", DialogueJobKindUpsert, DialogueTargetES)
	if dead.Status != DialogueJobStatusFailed || dead.LastError != "es_unavailable" {
		t.Fatalf("死信行 = status=%s last_error=%q", dead.Status, dead.LastError)
	}
	// 死信不被对账扫描捞起来。
	due, err := s.dueDialogueJobs(ctx, DialogueTargetES, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range due {
		if j.ID == dead.ID {
			t.Fatal("failed 死信出现在对账扫描里（会被无限补投）")
		}
	}

	// ── 永久错误当场死信，不耗三档 ──
	spec2 := DialogueIndexEventSpec{RunID: "r_b2", SessionID: "s_b2", TenantID: "t", Kind: DialogueJobKindUpsert}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	row, epoch := claimOne("r_b2", DialogueJobKindUpsert, DialogueTargetES)
	if err := s.failDialogueJob(ctx, row.ID, epoch, "embedding_dimension_mismatch", true); err != nil {
		t.Fatal(err)
	}
	pdead := dialogueJobRow(t, s, "r_b2", DialogueJobKindUpsert, DialogueTargetES)
	if pdead.Status != DialogueJobStatusFailed || pdead.Attempts != 1 {
		t.Fatalf("永久死信 = status=%s attempts=%d, want failed/1（当场死信）",
			pdead.Status, pdead.Attempts)
	}

	// ── 退避中的行不被对账扫描捞起 ──
	spec3 := DialogueIndexEventSpec{RunID: "r_b3", SessionID: "s_b3", TenantID: "t", Kind: DialogueJobKindUpsert}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec3); err != nil {
		t.Fatal(err)
	}
	row3, epoch3 := claimOne("r_b3", DialogueJobKindUpsert, DialogueTargetES)
	if err := s.failDialogueJob(ctx, row3.ID, epoch3, "es_unavailable", false); err != nil {
		t.Fatal(err)
	}
	due3, err := s.dueDialogueJobs(ctx, DialogueTargetES, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range due3 {
		if j.ID == row3.ID {
			t.Fatal("退避中的行被对账扫描捞起（跳过退避窗口）")
		}
	}
}

// TestDialogueRelayAtLeastOnce：relay 投递成功才写 published_at；标记丢失 → 重投
// 出现重复消息（消费侧挡，见消费端测试）；投递失败不写标记，下轮重投。
func TestDialogueRelayAtLeastOnce(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	spec := DialogueIndexEventSpec{
		RunID: "r_rl", SessionID: "s_rl", TenantID: testOwner.TenantID,
		Kind: DialogueJobKindUpsert, EmbeddingModel: "test-embed-model",
	}
	eid, err := s.RegisterDialogueIndexJobs(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	q := newFakeDialogueQueue()
	relay := newDialogueOutboxRelay(s, q)
	n, err := relay.relayOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("首轮投递 = %d err=%v, want 2", n, err)
	}
	// 消息正文只有五个真信息字段 + 分区键 = run_id。
	pub := q.Published()
	if len(pub) != 2 {
		t.Fatalf("发出 %d 条, want 2", len(pub))
	}
	for _, m := range pub {
		if m.EventID != eid || m.RunID != "r_rl" || m.Kind != DialogueJobKindUpsert ||
			m.IndexVersion != 1 || m.Key != "r_rl" {
			t.Fatalf("消息形状不对: %+v", m)
		}
		if m.Target != DialogueTargetES && m.Target != DialogueTargetMilvus {
			t.Fatalf("target 词表外: %s", m.Target)
		}
	}
	// 投递成功 → published_at 写上。
	es := dialogueJobRow(t, s, "r_rl", DialogueJobKindUpsert, DialogueTargetES)
	if es.PublishedAt == nil {
		t.Fatal("投递成功但 published_at 没写")
	}
	// 没有未投递行 → 第二轮 0 条。
	if n, err := relay.relayOnce(ctx); err != nil || n != 0 {
		t.Fatalf("二轮投递 = %d err=%v, want 0", n, err)
	}

	// 标记丢失（写 published_at 前崩溃的等价形态）：同一条重投 → 重复消息。
	if err := s.db.Exec(
		`UPDATE dialogue_index_jobs SET published_at = NULL WHERE event_id = ?`, eid).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := relay.relayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("重投 = %d err=%v, want 2", n, err)
	}
	if len(q.Published()) != 4 {
		t.Fatalf("重复投递后队列 = %d 条, want 4（至少一次语义允许重复）", len(q.Published()))
	}

	// 投递失败（broker 不可达的等价形态）：published_at 不写，下一轮补投。
	spec2 := DialogueIndexEventSpec{RunID: "r_rl2", SessionID: "s_rl2", TenantID: "t", Kind: DialogueJobKindUpsert}
	if _, err := s.RegisterDialogueIndexJobs(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	q.SetFailNextPublish(true)
	if n, err := relay.relayOnce(ctx); err != nil {
		t.Fatalf("注入投递失败不该作为 error 上抛（relay 只记账）: %v", err)
	} else if n != 1 { // 4 条里 es/milvus 各两条，只 fail 第一条
		t.Fatalf("注入失败轮成功条数 = %d, want 1", n)
	}
	if got := s.count(t, &DialogueIndexJob{}, "run_id = ? AND published_at IS NULL", "r_rl2"); got == 0 {
		t.Fatal("投递失败的行被写了 published_at")
	}
	if n, err := relay.relayOnce(ctx); err != nil || n != 1 {
		t.Fatalf("补投 = %d err=%v, want 1", n, err)
	}
}

// TestDialogueRelayBackfillsUnfinishedSideOnly：单边完成只补另一边——ES 侧已 done 且
// published_at 已写（完成态不再投），milvus 侧还没投出去（published_at 缺失）→ relay
// 一轮只补投 milvus 一条。这是「两个目标都 done 事件才完成」的补齐机制（doc/84 §5.2）：
// 不需要额外记账，published_at 缺失本身就是「这一边还没完成」的凭据。
func TestDialogueRelayBackfillsUnfinishedSideOnly(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	spec := DialogueIndexEventSpec{
		RunID: "r_half", SessionID: "s_half", TenantID: testOwner.TenantID,
		Kind: DialogueJobKindUpsert, EmbeddingModel: "test-embed-model",
	}
	eid, err := s.RegisterDialogueIndexJobs(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	q := newFakeDialogueQueue()
	relay := newDialogueOutboxRelay(s, q)
	if n, err := relay.relayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("首轮投递 = %d err=%v, want 2", n, err)
	}

	// ES 侧消费完成（done），published_at 已在首轮写上——它不该被重投。
	es := dialogueJobRow(t, s, "r_half", DialogueJobKindUpsert, DialogueTargetES)
	forceDialogueJobDue(t, s, es.ID)
	claimed, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领 ES = %d err=%v", len(claimed), err)
	}
	if err := s.completeDialogueJob(ctx, es.ID, claimed[0].ClaimEpoch); err != nil {
		t.Fatal(err)
	}

	// milvus 侧还没被消费（等价于 relay 写 published_at 前崩溃）：清它的标记。
	if err := s.db.Exec(
		`UPDATE dialogue_index_jobs SET published_at = NULL WHERE event_id = ? AND target = ?`,
		eid, DialogueTargetMilvus).Error; err != nil {
		t.Fatal(err)
	}

	n, err := relay.relayOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("补投 = %d err=%v, want 只补 milvus 1 条", n, err)
	}
	pub := q.Published()
	last := pub[len(pub)-1]
	if last.Target != DialogueTargetMilvus || last.EventID != eid || last.Kind != DialogueJobKindUpsert {
		t.Fatalf("补投消息 = %+v, want milvus 侧的 upsert", last)
	}
}

// TestDialogueRegisterWithoutPointerFails：指针行没播种就登记 = 明确报错
// （hybrid 装配必须先 EnsureDialogueRetrievalPointer），不静默写出行。
func TestDialogueRegisterWithoutPointerFails(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL) // 没播种指针
	_, err := s.RegisterDialogueIndexJobs(ctx, DialogueIndexEventSpec{
		RunID: "r_x", SessionID: "s_x", TenantID: "t", Kind: DialogueJobKindUpsert,
	})
	if err == nil || !strings.Contains(err.Error(), "dialogue_retrieval_pointer") {
		t.Fatalf("无指针登记 err = %v, want 指针未初始化错误", err)
	}
	if got := s.count(t, &DialogueIndexJob{}, "1 = 1"); got != 0 {
		t.Fatalf("失败登记写出了 %d 行", got)
	}
}

// TestDialoguePGSearchModeRegistersNothing：pg_search（默认）模式零新增行为——
// chunk 照写、outbox 不登记、不碰任务表。
func TestDialoguePGSearchModeRegistersNothing(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t) // ConfigureMemory 无 RAG/无 hybrid 开关
	s.completeRun(t, "s_pg", "r_pg", "pg_search 模式的一场")
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 1 {
		t.Fatalf("indexDialogueBatch = %d err=%v, want 1（chunk 照写）", n, err)
	}
	if got := s.count(t, &DialogueChunk{}, "run_id = ?", "r_pg"); got != 1 {
		t.Fatalf("chunk 数 = %d, want 1", got)
	}
	if got := s.count(t, &DialogueIndexJob{}, "1 = 1"); got != 0 {
		t.Fatalf("pg_search 模式登记了 %d 条任务, want 0", got)
	}
}

// TestDialogueIndexRunRegistersJobs：hybrid 开启后，indexDialogueRun 的同一笔事务
// 写 chunk + 登记两条目标任务（es/milvus 共用 event_id），归属项目 id 来自 runs.workspace_id。
func TestDialogueIndexRunRegistersJobs(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "检索项目")
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := SessionID("s_hw"), RunID("r_hw")
	s.mustAcquireWithWorkspace(t, sid, rid, "m_hw", "混合检索的测试问题", ws.ID)
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 1 {
		t.Fatalf("indexDialogueBatch = %d err=%v, want 1", n, err)
	}

	es := dialogueJobRow(t, s, string(rid), DialogueJobKindUpsert, DialogueTargetES)
	mil := dialogueJobRow(t, s, string(rid), DialogueJobKindUpsert, DialogueTargetMilvus)
	if es.EventID != mil.EventID {
		t.Fatalf("两目标 event_id 不共用: %s vs %s", es.EventID, mil.EventID)
	}
	if es.WorkspaceID != ws.ID || mil.WorkspaceID != ws.ID {
		t.Fatalf("任务 workspace = %s/%s, want 项目 id %s", es.WorkspaceID, mil.WorkspaceID, ws.ID)
	}
	if es.Kind != DialogueJobKindUpsert || es.IndexVersion != 1 || es.EmbeddingModel != "test-embed-model" {
		t.Fatalf("任务行形状: %+v", es)
	}
	if got := s.count(t, &DialogueChunk{}, "run_id = ?", rid); got != 1 {
		t.Fatalf("chunk 数 = %d, want 1（同事务写入）", got)
	}
}

// TestDialogueDeleteCascadeRegistersDeleteJobs：项目删除的同一笔事务里——先给每个
// 有 chunk 的 run 登记 kind=delete 任务（归属保留在任务行），再删 chunk。删完后
// 任务行还在（外部副本的清理凭据），published_at 缺失（relay 会投递）。
func TestDialogueDeleteCascadeRegistersDeleteJobs(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "要删的项目")
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := SessionID("s_dc"), RunID("r_dc")
	s.mustAcquireWithWorkspace(t, sid, rid, "m_dc", "删除前的最后一场", ws.ID)
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 1 {
		t.Fatalf("indexDialogueBatch = %d err=%v, want 1", n, err)
	}

	if err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, ws.ID); err != nil {
		t.Fatal(err)
	}

	// chunk 没了；delete 任务保留（外部副本清理的凭据），归属与定稿时间都在任务行里。
	if got := s.count(t, &DialogueChunk{}, "run_id = ?", rid); got != 0 {
		t.Fatalf("级联删后 chunk 数 = %d, want 0", got)
	}
	del := dialogueJobRow(t, s, string(rid), DialogueJobKindDelete, DialogueTargetES)
	delMil := dialogueJobRow(t, s, string(rid), DialogueJobKindDelete, DialogueTargetMilvus)
	if del.EventID != delMil.EventID {
		t.Fatalf("delete 任务 event_id 不共用: %s vs %s", del.EventID, delMil.EventID)
	}
	if del.WorkspaceID != ws.ID || del.Status != DialogueJobStatusPending {
		t.Fatalf("delete 任务行 = %+v", del)
	}
	if del.PublishedAt != nil {
		t.Fatal("delete 任务已标记投递（还没 relay 过）")
	}
}

// TestDialogueMoveJobsOnSetSessionWorkspace：会话移动的同一笔事务里给每个有 chunk
// 的 run 登记 kind=move 任务（归属 = 新项目 id；解绑 = ""）。
func TestDialogueMoveJobsOnSetSessionWorkspace(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	wsA, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "项目A")
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "项目B")
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := SessionID("s_mv"), RunID("r_mv")
	s.mustAcquireWithWorkspace(t, sid, rid, "m_mv", "要搬的一场", wsA.ID)
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 1 {
		t.Fatalf("indexDialogueBatch = %d err=%v, want 1", n, err)
	}

	// A → B：move 任务登记，归属 = B。
	if err := s.SetSessionWorkspace(ctx, sid, testOwner, wsB.ID); err != nil {
		t.Fatal(err)
	}
	mv := dialogueJobRow(t, s, string(rid), DialogueJobKindMove, DialogueTargetES)
	mvMil := dialogueJobRow(t, s, string(rid), DialogueJobKindMove, DialogueTargetMilvus)
	if mv.EventID != mvMil.EventID || mv.WorkspaceID != wsB.ID {
		t.Fatalf("move 任务 = es(ws=%s evt=%s) mil(evt=%s)", mv.WorkspaceID, mv.EventID, mvMil.EventID)
	}

	// 解绑（B → 无）：move 任务归属 = ""（消费者据此清外部元数据）。
	if err := s.SetSessionWorkspace(ctx, sid, testOwner, ""); err != nil {
		t.Fatal(err)
	}
	un := dialogueJobRow(t, s, string(rid), DialogueJobKindMove, DialogueTargetES)
	if un.WorkspaceID != "" {
		t.Fatalf("解绑 move 任务 workspace = %q, want 空串", un.WorkspaceID)
	}
}

// TestDialogueConsumerVersionFilterIgnoredOldJob：index_version 与指针不符的任务
// 直接翻 done、不写外部（旧代次不得覆盖新代次）。
func TestDialogueConsumerVersionFilterIgnoredOldJob(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	spec := DialogueIndexEventSpec{RunID: "r_v1", SessionID: "s_v1", TenantID: "t", Kind: DialogueJobKindUpsert}
	eid, err := s.RegisterDialogueIndexJobs(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	es := dialogueJobRow(t, s, "r_v1", DialogueJobKindUpsert, DialogueTargetES)
	// 把这条任务拨成旧代次。
	if err := s.db.Exec(`UPDATE dialogue_index_jobs SET index_version = 99 WHERE id = ?`, es.ID).Error; err != nil {
		t.Fatal(err)
	}

	q := newFakeDialogueQueue()
	fakeES := &fakeDialogueESIndex{}
	cons := newDialogueConsumer(s, q, newDialogueESProcessor(s, fakeES))
	msgs := []dialogueMessage{{
		EventID: eid, RunID: "r_v1", Target: DialogueTargetES,
		Kind: DialogueJobKindUpsert, IndexVersion: 99, Key: "r_v1",
	}}
	if err := cons.handleBatch(ctx, msgs); err != nil {
		t.Fatalf("handleBatch: %v", err)
	}
	after := dialogueJobRow(t, s, "r_v1", DialogueJobKindUpsert, DialogueTargetES)
	if after.Status != DialogueJobStatusDone {
		t.Fatalf("旧版本任务 status = %s, want done（直接完成不重试）", after.Status)
	}
	if len(fakeES.upserts) != 0 || len(fakeES.deletes) != 0 {
		t.Fatalf("旧版本任务写了外部: upserts=%d deletes=%d", len(fakeES.upserts), len(fakeES.deletes))
	}
}
