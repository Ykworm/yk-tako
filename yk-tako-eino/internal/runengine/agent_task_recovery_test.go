// 本文件是协调通知生命周期的存储半回归测试（施工包 §8.1）：真 PG（5433 隔离临时库）+ 真 Redis。
//
// 覆盖面：attempt 编号（首次 1、同 run 崩溃续跑不变、装配失败退回后重新认领 +1）、
// 人工重试准入矩阵（pending／claimed 在跑／claimed 终态／failed+error／忙／配额／产物缺失／
// 未知／跨租户／viewer／来源会话已删）、双击与双副本只产出一场尝试、旧尝试收口不了新认领、
// 通知详情视图的空值形态、升级回填可重复执行。
//
// 这些测试不装配模型、不启动 worker goroutine：认领、重试、对账全是 store 方法。
package runengine

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// deliverySeed 是一套「组已收口 + 通知 failed + 一条历史尝试」的造态。
type deliverySeed struct {
	wsID, orchID, taskID, notifID, claimRun string
	sessID                                  SessionID
}

// mkChatSession 在既有项目下再开一场普通聊天会话（每条通知要有自己的收件会话，
// 否则上一场认领 run 还活着，下一条会被「来源会话忙」挡住）。
func mkChatSession(t *testing.T, s *Store, wsID, title string) SessionID {
	t.Helper()
	sid, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&Session{
		ID: sid, Title: title, TenantID: testOwner.TenantID, UserID: testOwner.UserID,
		Visibility: "private", WorkspaceID: &wsID, Kind: sessionKindChat,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return sid
}

// seedFailedDelivery 造一条可人工重试的整组完成通知（doc/73 §4.2）：
// Task completed 带摘要结果 → 组 succeeded + delivery_status failed + delivery_run_error
// → 一条 error 的 claim run → 通知 failed（consume_error_code=delivery_run_error）→ attempt=1。
func seedFailedDelivery(t *testing.T, s *Store, wsID string, sessID SessionID, key string) deliverySeed {
	t.Helper()
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "delivery-" + key, WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "交付一件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatalf("CreateAgentTask: %v", err)
	}
	mustExec(t, s, `UPDATE agent_tasks
	SET status = 'completed', result = ?, result_revision = 1, finished_at = now(), updated_at = now()
	WHERE id = ?`,
		`{"summary":"交付摘要正文","artifacts":[],"metadata":{}}`, resp.TaskID)
	mustExec(t, s, `UPDATE agent_orchestrations
	SET status = ?, final_task_id = ?, final_result_revision = 1,
	    delivery_status = ?, delivery_error_code = ?, finished_at = now(), updated_at = now()
	WHERE id = ?`,
		orchStatusSucceeded, resp.TaskID, deliveryStatusFailed, deliveryErrRunError, resp.OrchestrationID)
	claimRun := "r_fail_" + key
	mustExec(t, s, `INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
	VALUES (?, ?, 'error', 1, now(), ?, now())`, claimRun, sessID, wsID)
	notifID := "an_" + key
	mustExec(t, s, `INSERT INTO agent_notifications
	(id, tenant_id, orchestration_id, task_id, receiver_session_id, kind, dedup_key, status,
	 claim_run_id, error_code, consume_error_code, created_at, claimed_at, failed_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, 'failed', ?, '', ?, now() - interval '10 minutes',
	        now() - interval '9 minutes', now() - interval '1 minute', now() - interval '1 minute')`,
		notifID, testOwner.TenantID, resp.OrchestrationID, resp.TaskID, sessID,
		notifKindOrchFinished, "orchestration_finished:"+key, claimRun, deliveryErrRunError)
	mustExec(t, s, `INSERT INTO agent_notification_attempts
	(id, tenant_id, notification_id, attempt, run_id, created_at)
	VALUES (?, ?, ?, 1, ?, now() - interval '9 minutes')`,
		"ana_"+key, testOwner.TenantID, notifID, claimRun)
	return deliverySeed{wsID: wsID, orchID: resp.OrchestrationID, taskID: resp.TaskID,
		notifID: notifID, claimRun: claimRun, sessID: sessID}
}

func mustExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if err := s.db.Exec(sql, args...).Error; err != nil {
		t.Fatalf("SQL 失败: %v\n%s", err, sql)
	}
}

func attemptCount(t *testing.T, s *Store, notifID string) int64 {
	t.Helper()
	return s.count(t, &AgentNotificationAttempt{}, "notification_id = ?", notifID)
}

func notifClaimRun(t *testing.T, s *Store, id string) string {
	t.Helper()
	_, run := notifStatus(t, s, id)
	return run
}

func deliveryState(t *testing.T, s *Store, orchID string) (string, string) {
	t.Helper()
	var row AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&row).Error; err != nil {
		t.Fatalf("read orchestration: %v", err)
	}
	return row.DeliveryStatus, row.DeliveryRunID
}

// TestNotificationAttemptNumbering 钉住 attempt 语义（doc/73 §5.2/§6.3）：
// 首次认领 = 1；同一场 run 的崩溃 resume 只增 lease_epoch，不增 attempt；
// 装配失败把认领退回 pending 后重新认领，编号接着走（MAX+1），不重置成 1。
func TestNotificationAttemptNumbering(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "attempt-num", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "失败一件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.insertNotificationTx(s.db, AgentNotification{
		TenantID: testOwner.TenantID, OrchestrationID: resp.OrchestrationID, TaskID: resp.TaskID,
		ReceiverSessionID: sessID, Kind: notifKindTaskFailed, DedupKey: "task_failed:attempt-num",
		Status: notifStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	var notif AgentNotification
	if err := s.db.Where("dedup_key = ?", "task_failed:attempt-num").Take(&notif).Error; err != nil {
		t.Fatal(err)
	}

	ts := &TaskScheduler{store: s}
	l1 := ts.claimNotification(ctx, notif.ID)
	if l1 == nil {
		t.Fatalf("首次认领失败")
	}
	if l1.Attempt != 1 || attemptCount(t, s, notif.ID) != 1 {
		t.Fatalf("首次尝试 = %d / %d 行, want 1 / 1", l1.Attempt, attemptCount(t, s, notif.ID))
	}

	// 同一场 run 的崩溃恢复：epoch+1，attempt 与认领坐标都不动。
	mustExec(t, s, `UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = ?`, l1.RunID)
	claim, err := s.ClaimResume(ctx, l1.RunID, 3)
	if err != nil {
		t.Fatalf("ClaimResume: %v", err)
	}
	if claim.Epoch != 2 {
		t.Fatalf("resume epoch = %d, want 2", claim.Epoch)
	}
	if attemptCount(t, s, notif.ID) != 1 {
		t.Fatalf("resume 新增了尝试行：%d", attemptCount(t, s, notif.ID))
	}
	if got := notifClaimRun(t, s, notif.ID); got != string(l1.RunID) {
		t.Fatalf("resume 换了认领 run：%s, want %s", got, l1.RunID)
	}
	view, err := s.GetAgentNotification(ctx, testOwner, notif.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Attempt != 1 {
		t.Fatalf("resume 后详情 attempt = %d, want 1", view.Attempt)
	}

	// 装配失败：run 收 error、认领退回 pending；重新认领编号 +1。
	if _, ok, err := s.EndRunErrorOnly(ctx, l1.SessionID, l1.RunID); err != nil || !ok {
		t.Fatalf("EndRunErrorOnly = %v/%v", ok, err)
	}
	s.revertDeliveryClaim(ctx, l1)
	if st, run := notifStatus(t, s, notif.ID); st != notifStatusPending || run != "" {
		t.Fatalf("退回后 = %s/%q, want pending/空", st, run)
	}
	l2 := ts.claimNotification(ctx, notif.ID)
	if l2 == nil {
		t.Fatalf("二次认领失败")
	}
	if l2.Attempt != 2 || attemptCount(t, s, notif.ID) != 2 {
		t.Fatalf("二次尝试 = %d / %d 行, want 2 / 2", l2.Attempt, attemptCount(t, s, notif.ID))
	}
}

// TestNotificationRetryAdmission 钉住人工重试的准入矩阵（doc/73 §4.2）：
// 失败矩阵按文档顺序逐条验，成功路径只造一次（attempt=2，组转 delivering）。
func TestNotificationRetryAdmission(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	root := t.TempDir()
	wsID, baseSess := seedTaskEnv(t, s, "researcher")
	seed := seedFailedDelivery(t, s, wsID, baseSess, "admit")

	if _, _, err := s.RetryAgentNotification(ctx, testOwner, "an_nope", root); !errors.Is(err, errNotifNotFound) {
		t.Fatalf("未知通知 err = %v, want errNotifNotFound", err)
	}
	other := principal{UserID: "u_other", TenantID: "t_other", Role: "owner"}
	if _, _, err := s.RetryAgentNotification(ctx, other, seed.notifID, root); !errors.Is(err, errNotifNotFound) {
		t.Fatalf("跨租户 err = %v, want errNotifNotFound", err)
	}
	viewer := principal{UserID: "u_viewer", TenantID: testOwner.TenantID, Role: "viewer"}
	if _, _, err := s.RetryAgentNotification(ctx, viewer, seed.notifID, root); !errors.Is(err, errNotifNotFound) {
		t.Fatalf("同租户非本人 err = %v, want errNotifNotFound", err)
	}

	// pending：调度器还没认领，人工不抢先。
	if err := s.insertNotificationTx(s.db, AgentNotification{
		TenantID: testOwner.TenantID, OrchestrationID: seed.orchID, TaskID: seed.taskID,
		ReceiverSessionID: seed.sessID, Kind: notifKindTaskFailed,
		DedupKey: "task_failed:admit-pending", Status: notifStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	var pendingNotif AgentNotification
	if err := s.db.Where("dedup_key = ?", "task_failed:admit-pending").Take(&pendingNotif).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RetryAgentNotification(ctx, testOwner, pendingNotif.ID, root); !errors.Is(err, errNotifNotRetryable) {
		t.Fatalf("pending err = %v, want errNotifNotRetryable", err)
	}

	// claimed 且 run 在跑：双击／双副本的第二次回现状，不开新 run。
	ts := &TaskScheduler{store: s}
	claim := ts.claimNotification(ctx, pendingNotif.ID)
	if claim == nil {
		t.Fatalf("pending 认领失败")
	}
	view, launch, err := s.RetryAgentNotification(ctx, testOwner, pendingNotif.ID, root)
	if err != nil || launch != nil {
		t.Fatalf("claimed+在跑 = %v/%v, want nil launch, nil err", launch, err)
	}
	if view.RunID != claim.RunID || view.Status != notifStatusClaimed || view.Attempt != 1 {
		t.Fatalf("claimed+在跑回现状 = %+v", view)
	}
	if attemptCount(t, s, pendingNotif.ID) != 1 {
		t.Fatalf("claimed+在跑多了尝试行：%d", attemptCount(t, s, pendingNotif.ID))
	}

	// claimed 但 run 已终态：矛盾态，等对账收口，不允许人工从这里跳过。
	setRunStatus(t, s, claim.RunID, "error")
	if _, _, err := s.RetryAgentNotification(ctx, testOwner, pendingNotif.ID, root); !errors.Is(err, errNotifNotRetryable) {
		t.Fatalf("claimed+终态 err = %v, want errNotifNotRetryable", err)
	}

	// failed 但来源会话有活 run（用户正在聊）：不打断。
	busySess := mkChatSession(t, s, wsID, "忙会话")
	mustExec(t, s, `INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
	VALUES ('r_admit_busy', ?, 'running', 1, now() + interval '1 hour', ?, now())`, busySess, seed.wsID)
	mustExec(t, s, `INSERT INTO agent_notifications
	(id, tenant_id, orchestration_id, task_id, receiver_session_id, kind, dedup_key, status,
	 claim_run_id, error_code, consume_error_code, created_at, claimed_at, failed_at, updated_at)
	VALUES ('an_admit_busy', ?, ?, ?, ?, ?, 'task_failed:admit-busy', 'failed',
	        ?, '', ?, now() - interval '5 minutes', now() - interval '4 minutes',
	        now() - interval '1 minute', now() - interval '1 minute')`,
		testOwner.TenantID, seed.orchID, seed.taskID, busySess, notifKindTaskFailed,
		seed.claimRun, deliveryErrRunError)
	if _, _, err := s.RetryAgentNotification(ctx, testOwner, "an_admit_busy", root); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("忙会话 err = %v, want ErrSessionBusy", err)
	}

	// 产物缺失：组结果声明了文件但盘上没有 → 拒绝，不把缺失产物交付成成功。
	missing := seedFailedDelivery(t, s, wsID, mkChatSession(t, s, wsID, "产物会话"), "admit-artifact")
	mustExec(t, s, `UPDATE agent_tasks
	SET result = '{"summary":"有产物","artifacts":[{"path":"nope/missing.txt","kind":"file"}],"metadata":{}}',
	    result_revision = 1
	WHERE id = ?`, missing.taskID)
	if _, _, err := s.RetryAgentNotification(ctx, testOwner, missing.notifID, root); !errors.Is(err, errNotifArtifactInvalid) {
		t.Fatalf("产物缺失 err = %v, want errNotifArtifactInvalid", err)
	}

	// 来源会话已删：整条归零，与不存在同形。
	mustExec(t, s, `UPDATE sessions SET deleted_at = now() WHERE id = ?`, missing.sessID)
	if _, _, err := s.RetryAgentNotification(ctx, testOwner, missing.notifID, root); !errors.Is(err, errNotifNotFound) {
		t.Fatalf("来源会话已删 err = %v, want errNotifNotFound", err)
	}
	if _, err := s.GetAgentNotification(ctx, testOwner, missing.notifID); !errors.Is(err, errNotifNotFound) {
		t.Fatalf("来源会话已删详情 err = %v, want errNotifNotFound", err)
	}

	// 成功：failed + 当前 run error → 新 run，attempt=2，组转 delivering。
	view, launch, err = s.RetryAgentNotification(ctx, testOwner, seed.notifID, root)
	if err != nil || launch == nil {
		t.Fatalf("成功重试 = %v/%v", launch, err)
	}
	if launch.Attempt != 2 || view.Attempt != 2 || view.Status != notifStatusClaimed {
		t.Fatalf("成功重试视图 = %+v / launch %+v", view, launch)
	}
	if attemptCount(t, s, seed.notifID) != 2 {
		t.Fatalf("成功重试后尝试行 = %d, want 2", attemptCount(t, s, seed.notifID))
	}
	if status, run := deliveryState(t, s, seed.orchID); status != deliveryStatusDelivering || run != string(launch.RunID) {
		t.Fatalf("组交付 = %s/%s, want delivering/%s", status, run, launch.RunID)
	}

	// 租户配额满：默认 8 场活 run，再凑满就拒绝（不在事务里铸 run）。
	mustExec(t, s, `UPDATE runs SET status = 'complete' WHERE id = 'r_admit_busy'`)
	for i := 0; i < 8; i++ {
		sid := mkChatSession(t, s, wsID, "配额会话")
		mustExec(t, s, `INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
		VALUES (?, ?, 'running', 1, now() + interval '1 hour', ?, now())`, "r_quota_"+sid[2:], sid, wsID)
	}
	quota := seedFailedDelivery(t, s, wsID, mkChatSession(t, s, wsID, "配额通知"), "admit-quota")
	if _, _, err := s.RetryAgentNotification(ctx, testOwner, quota.notifID, root); !errors.Is(err, errTenantFull) {
		t.Fatalf("配额满 err = %v, want errTenantFull", err)
	}
}

// TestNotificationRetryTwoStoresSingleLaunch 钉住双击与双副本（doc/73 §4.2/§6.1）：
// 两个 Store 实例（= 两个进程）同时重试同一条 failed 通知，事务行锁只放一个进来铸 run，
// 另一个读回现状；尝试表恰好两行（原 1 + 新 1），来源会话上恰好两场 run（原 error + 新）。
func TestNotificationRetryTwoStoresSingleLaunch(t *testing.T) {
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	seed := seedFailedDelivery(t, s, wsID, sessID, "race")
	s2 := secondStore(t, dsn)
	ctx := context.Background()
	root := t.TempDir()

	var wg sync.WaitGroup
	var mu sync.Mutex
	launches := make([]*coordLaunch, 0, 2)
	errs := make([]error, 0, 2)
	for _, st := range []*Store{s, s2} {
		st := st
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, launch, err := st.RetryAgentNotification(ctx, testOwner, seed.notifID, root)
			mu.Lock()
			defer mu.Unlock()
			launches = append(launches, launch)
			errs = append(errs, err)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("并发重试报错: %v", err)
		}
	}
	created := 0
	for _, l := range launches {
		if l != nil {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("并发重试铸了 %d 场新 run, want 1", created)
	}
	if got := attemptCount(t, s, seed.notifID); got != 2 {
		t.Fatalf("并发后尝试行 = %d, want 2", got)
	}
	if got := s.count(t, &Run{}, "session_id = ?", seed.sessID); got != 2 {
		t.Fatalf("来源会话 run = %d, want 2（原 error + 新）", got)
	}
}

// TestDeliveryReconcileOnlyCurrentClaim 钉住旧尝试不能收口新认领（doc/73 §6.4/§6.5）：
// 人工重试换 claim_run_id 之后，旧 run 的 complete／error 对账一律空转；
// 协调身份反查只认当前 claim，旧 attempt 被判 stale（resume 入口据此拒绝）。
func TestDeliveryReconcileOnlyCurrentClaim(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	seed := seedFailedDelivery(t, s, wsID, sessID, "stale")
	root := t.TempDir()
	_, launch, err := s.RetryAgentNotification(ctx, testOwner, seed.notifID, root)
	if err != nil || launch == nil {
		t.Fatalf("重试失败: %v", err)
	}

	// 旧 run 补成 complete：不消费、不改组交付。
	setRunStatus(t, s, RunID(seed.claimRun), "complete")
	if err := s.ReconcileDeliveryRun(ctx, RunID(seed.claimRun)); err != nil {
		t.Fatal(err)
	}
	if st, run := notifStatus(t, s, seed.notifID); st != notifStatusClaimed || run != string(launch.RunID) {
		t.Fatalf("旧 run 收口改了通知：%s/%s", st, run)
	}
	if status, run := deliveryState(t, s, seed.orchID); status != deliveryStatusDelivering || run != string(launch.RunID) {
		t.Fatalf("旧 run 收口改了组交付：%s/%s", status, run)
	}

	// 协调身份：旧 run 查不到 claim 且判 stale；新 run 是当前 claim、不 stale。
	if cc, err := s.CoordinationClaimForRun(ctx, RunID(seed.claimRun)); err != nil {
		t.Fatal(err)
	} else if cc != nil {
		t.Fatalf("旧 run 仍被当成协调认领：%+v", cc)
	}
	if stale, err := s.IsStaleCoordinationAttempt(ctx, RunID(seed.claimRun)); err != nil {
		t.Fatal(err)
	} else if !stale {
		t.Fatalf("旧 attempt 没被判 stale")
	}
	cc, err := s.CoordinationClaimForRun(ctx, launch.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if cc == nil || cc.Attempt != 2 || cc.NotificationID != seed.notifID {
		t.Fatalf("当前 claim 反查 = %+v", cc)
	}
	if stale, err := s.IsStaleCoordinationAttempt(ctx, launch.RunID); err != nil {
		t.Fatal(err)
	} else if stale {
		t.Fatalf("当前 claim 被判 stale")
	}

	// 当前 run complete → consumed + delivered；此后旧 run 的 error 仍不能改写。
	setRunStatus(t, s, launch.RunID, "complete")
	if err := s.ReconcileDeliveryRun(ctx, launch.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := notifStatus(t, s, seed.notifID); st != notifStatusConsumed {
		t.Fatalf("当前 run complete 后通知 = %s, want consumed", st)
	}
	if status, _ := deliveryState(t, s, seed.orchID); status != deliveryStatusDelivered {
		t.Fatalf("当前 run complete 后组交付 = %s, want delivered", status)
	}
	setRunStatus(t, s, RunID(seed.claimRun), "error")
	if err := s.ReconcileDeliveryRun(ctx, RunID(seed.claimRun)); err != nil {
		t.Fatal(err)
	}
	if st, _ := notifStatus(t, s, seed.notifID); st != notifStatusConsumed {
		t.Fatalf("旧 run error 改写了通知：%s", st)
	}
	if status, _ := deliveryState(t, s, seed.orchID); status != deliveryStatusDelivered {
		t.Fatalf("旧 run error 改写了组交付：%s", status)
	}
}

// TestNotificationDetailShape 钉住详情视图的两个形态（doc/73 §4.1）：
// 没认领过 → runId 空串、attempt 0、result null、三个时间 null、attempts []；
// 非整组通知即便 Task 有结果也不给 result（结果定位只属于最终交付）。
func TestNotificationDetailShape(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	seed := seedFailedDelivery(t, s, wsID, sessID, "detail")

	// 未认领的 task_failed。
	taskFailedID := "an_detail_pending"
	mustExec(t, s, `INSERT INTO agent_notifications
	(id, tenant_id, orchestration_id, task_id, receiver_session_id, kind, dedup_key, status,
	 error_code, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, 'task_failed:detail-pending', 'pending', 'task_failed', now(), now())`,
		taskFailedID, testOwner.TenantID, seed.orchID, seed.taskID, seed.sessID, notifKindTaskFailed)
	view, err := s.GetAgentNotification(ctx, testOwner, taskFailedID)
	if err != nil {
		t.Fatal(err)
	}
	if view.RunID != "" || view.Attempt != 0 || view.Result != nil ||
		view.ClaimedAt != nil || view.FailedAt != nil || view.ConsumedAt != nil {
		t.Fatalf("未认领视图 = %+v", view)
	}
	if view.Attempts == nil || len(view.Attempts) != 0 {
		t.Fatalf("未认领 attempts = %#v, want 非 nil 空切片", view.Attempts)
	}

	// 非整组通知：Task 有结果但视图 result 仍 nil，attempt 历史现读自 runs。
	var task AgentTask
	if err := s.db.Where("id = ?", seed.taskID).Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	// 尝试表的 run_id 唯一：这条通知要有自己的 claim run。
	claimedRun := "r_detail_claimed"
	mustExec(t, s, `INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
	VALUES (?, ?, 'running', 1, now() + interval '1 hour', ?, now())`, claimedRun, seed.sessID, seed.wsID)
	taskFailedClaimedID := "an_detail_claimed"
	mustExec(t, s, `INSERT INTO agent_notifications
	(id, tenant_id, orchestration_id, task_id, receiver_session_id, kind, dedup_key, status,
	 claim_run_id, error_code, created_at, claimed_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, 'task_failed:detail-claimed', 'claimed', ?, 'task_failed',
	        now() - interval '2 minutes', now() - interval '2 minutes', now() - interval '2 minutes')`,
		taskFailedClaimedID, testOwner.TenantID, seed.orchID, seed.taskID, seed.sessID,
		notifKindTaskFailed, claimedRun)
	mustExec(t, s, `INSERT INTO agent_notification_attempts
	(id, tenant_id, notification_id, attempt, run_id, created_at)
	VALUES ('ana_detail_claimed', ?, ?, 1, ?, now() - interval '2 minutes')`,
		testOwner.TenantID, taskFailedClaimedID, claimedRun)
	view, err = s.GetAgentNotification(ctx, testOwner, taskFailedClaimedID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Result != nil {
		t.Fatalf("非整组通知给了 result：%+v", view.Result)
	}
	if view.Attempt != 1 || len(view.Attempts) != 1 || view.Attempts[0].Status != "running" {
		t.Fatalf("任务通知 attempts = %+v, want 1 条且 status 现读自 runs=running", view.Attempts)
	}
}

// TestNotificationMigrationBackfillIdempotent 钉住升级回填可重复执行（doc/73 §5）：
// 已 failed 但没写原因的组补 delivery_run_error；已认领过的通知补一行 attempt=1；
// 同库再开 Store（第二次启动）不重复插行、不改已填的码。
func TestNotificationMigrationBackfillIdempotent(t *testing.T) {
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "backfill", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "存量件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE agent_orchestrations
	SET status = ?, delivery_status = ?, delivery_error_code = ''
	WHERE id = ?`, orchStatusSucceeded, deliveryStatusFailed, resp.OrchestrationID)
	mustExec(t, s, `INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
	VALUES ('r_backfill', ?, 'error', 1, now(), ?, now())`, sessID, wsID)
	mustExec(t, s, `INSERT INTO agent_notifications
	(id, tenant_id, orchestration_id, task_id, receiver_session_id, kind, dedup_key, status,
	 claim_run_id, error_code, created_at, claimed_at, updated_at)
	VALUES ('an_backfill', ?, ?, ?, ?, ?, 'orchestration_finished:backfill', 'claimed',
	        'r_backfill', '', now() - interval '1 hour', now() - interval '59 minutes', now())`,
		testOwner.TenantID, resp.OrchestrationID, resp.TaskID, sessID, notifKindOrchFinished)

	// 第一次重开 = 第二次启动：回填生效。
	if st := secondStore(t, dsn); st != nil {
		if got := st.count(t, &AgentNotificationAttempt{}, "notification_id = 'an_backfill'"); got != 1 {
			t.Fatalf("回填 attempt = %d, want 1", got)
		}
	}
	// 第二次重开：不重复插、不改已填的码。
	st := secondStore(t, dsn)
	var code string
	if err := st.db.Raw(`SELECT delivery_error_code FROM agent_orchestrations WHERE id = ?`,
		resp.OrchestrationID).Scan(&code).Error; err != nil {
		t.Fatal(err)
	}
	if code != deliveryErrRunError {
		t.Fatalf("组 delivery_error_code = %q, want %q", code, deliveryErrRunError)
	}
	if got := st.count(t, &AgentNotificationAttempt{}, "notification_id = 'an_backfill'"); got != 1 {
		t.Fatalf("二次回填 attempt = %d, want 1（重复启动不重复插）", got)
	}
}
