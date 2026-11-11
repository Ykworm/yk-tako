// 本文件是计划修订、Task 提问超时、处理项聚合与计划复核认领的回归测试（施工包 §12.2/§12.3）。
//
// 纯函数部分钉住规范化与哈希的顺序不变性；存储部分用真 PG（5433 隔离临时库）+ 真 Redis，
// 不装配模型、不启动 worker（与 agent_task_store_test.go 同一套夹具）。
package runengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestRevisionCommandNormalize 钉住修订命令的纯校验与哈希：空命令拒绝、顺序不影响哈希、
// 取消与新增互斥、key 词法。
func TestRevisionCommandNormalize(t *testing.T) {
	base := RevisionCommand{
		IdempotencyKey: "rev-1", BaseRevision: 0, Reason: "补充 D",
		AddTasks: []planTask{
			{Key: "D", Content: "补充调研 D", Agent: "researcher"},
			{Key: "E", Content: "补充调研 E", Agent: "researcher"},
		},
		AddLinks: []planLink{{Parent: "D", Child: "C"}, {Parent: "E", Child: "C"}},
	}
	n1, err := normalizeRevisionCommand(base)
	if err != nil {
		t.Fatalf("合法修订被拒：%v", err)
	}
	h1, err := n1.hash()
	if err != nil {
		t.Fatal(err)
	}
	reversed := RevisionCommand{
		IdempotencyKey: base.IdempotencyKey, BaseRevision: base.BaseRevision, Reason: base.Reason,
		AddTasks: []planTask{base.AddTasks[1], base.AddTasks[0]},
		AddLinks: []planLink{base.AddLinks[1], base.AddLinks[0]},
	}
	n2, err := normalizeRevisionCommand(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := n2.hash(); h2 != h1 {
		t.Fatalf("输入顺序改变后哈希变了")
	}

	cases := []struct {
		name string
		mut  func(*RevisionCommand)
		want string
	}{
		{"空修订", func(c *RevisionCommand) { c.AddTasks, c.AddLinks = nil, nil }, "修订命令为空"},
		{"缺原因", func(c *RevisionCommand) { c.Reason = " " }, "修订原因必填"},
		{"新增与取消同 key", func(c *RevisionCommand) { c.CancelTasks = []string{"D"} }, "既新增又取消"},
		{"key 词法", func(c *RevisionCommand) { c.AddTasks[0].Key = "1 D" }, "key 只允许"},
		{"重复 key", func(c *RevisionCommand) { c.AddTasks[1].Key = "D" }, "新增 Task key 重复"},
		{"自依赖", func(c *RevisionCommand) { c.AddLinks = []planLink{{Parent: "D", Child: "D"}} }, "自依赖"},
		{"空执行者", func(c *RevisionCommand) { c.AddTasks[0].Agent = "" }, "执行者必填"},
	}
	for _, tc := range cases {
		c := base
		c.AddTasks = append([]planTask(nil), base.AddTasks...)
		c.AddLinks = append([]planLink(nil), base.AddLinks...)
		tc.mut(&c)
		if _, err := normalizeRevisionCommand(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want 含 %q", tc.name, err, tc.want)
		}
	}
}

// TestCreatePlanRevision 钉住修订事务（施工包 §6.2）：接受后图与版本、暂停解除、审计行、
// 幂等重打、同键不同命令、过期 baseRevision。
func TestCreatePlanRevision(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "rev-core"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	orchID := resp.OrchestrationID
	// 模拟检查点暂停：修订成功必须解除。
	if err := s.db.Exec(`UPDATE agent_orchestrations SET plan_hold = true WHERE id = ?`, orchID).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RevisionCommand{
		IdempotencyKey: "rev-a", BaseRevision: 0, Reason: "A 之后需要 D",
		AddTasks: []planTask{{Key: "D", Content: "补充调研 D", Agent: "researcher"}},
		AddLinks: []planLink{{Parent: "D", Child: "C"}},
	}
	rev, err := s.CreatePlanRevision(ctx, testOwner, orchID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("接受修订：%v", err)
	}
	if rev.Revision != 1 || rev.PlanHold || len(rev.AddedTaskIDs) != 1 {
		t.Fatalf("修订响应 = %+v", rev)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if o.PlanRevision != 1 || o.PlanHold || o.FinalTaskID != rev.FinalTaskID {
		t.Fatalf("组 = revision %d hold %v final %s", o.PlanRevision, o.PlanHold, o.FinalTaskID)
	}
	var d AgentTask
	if err := s.db.Where("id = ?", rev.AddedTaskIDs[0]).Take(&d).Error; err != nil {
		t.Fatal(err)
	}
	// D 有新父边（D→C 是 D 的下游），D 自己没有父 → ready；C 仍是最终节点。
	if d.TaskKey != "D" || d.Status != agentTaskStatusReady || d.CreatedRevision != 1 {
		t.Fatalf("新增节点 = %+v", d)
	}

	// 幂等重打：同键同命令返回原修订；同键不同命令 117108。
	again, err := s.CreatePlanRevision(ctx, testOwner, orchID, cmd, planEnv{})
	if err != nil || again.RevisionID != rev.RevisionID || again.Revision != 1 {
		t.Fatalf("幂等重打 = %+v err=%v", again, err)
	}
	changed := cmd
	changed.Reason = "另一份命令"
	if _, err := s.CreatePlanRevision(ctx, testOwner, orchID, changed, planEnv{}); !errors.Is(err, errRevisionConflict) {
		t.Fatalf("同键不同命令 err = %v, want conflict", err)
	}
	// 过期 baseRevision。
	stale := RevisionCommand{
		IdempotencyKey: "rev-stale", BaseRevision: 0, Reason: "基于旧版本",
		AddTasks: []planTask{{Key: "E", Content: "补充 E", Agent: "researcher"}},
	}
	if _, err := s.CreatePlanRevision(ctx, testOwner, orchID, stale, planEnv{}); !errors.Is(err, errRevisionStale) {
		t.Fatalf("过期 baseRevision err = %v, want stale", err)
	}
}

// TestCreatePlanRevisionCancelAndFinal 钉住取消与换最终（施工包 §6.2 第 9/10 条）：
// 取消 ready 节点；取消当前最终必须给新最终；换最终后活跃节点仍须到达它。
func TestCreatePlanRevisionCancelAndFinal(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "rev-cancel"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	orchID := resp.OrchestrationID

	// 取消最终 C 却不给新最终 → 400 形状错。
	if _, err := s.CreatePlanRevision(ctx, testOwner, orchID, RevisionCommand{
		IdempotencyKey: "rev-c1", Reason: "取消 C", CancelTasks: []string{"C"},
	}, planEnv{}); err == nil || !strings.Contains(err.Error(), "必须指定新的最终节点") {
		t.Fatalf("取消最终不给新最终 err = %v", err)
	}

	// 取消 B，最终仍是 C：A→C 仍在，A 可达 C，合法。
	rev, err := s.CreatePlanRevision(ctx, testOwner, orchID, RevisionCommand{
		IdempotencyKey: "rev-c2", BaseRevision: 0, Reason: "不需要 B", CancelTasks: []string{"B"},
	}, planEnv{})
	if err != nil {
		t.Fatalf("取消 B：%v", err)
	}
	var b AgentTask
	if err := s.db.Where("orchestration_id = ? AND task_key = ?", orchID, "B").Take(&b).Error; err != nil {
		t.Fatal(err)
	}
	if b.Status != agentTaskStatusCancelled {
		t.Fatalf("B 状态 = %s, want cancelled", b.Status)
	}
	if rev.FinalTaskID == "" {
		t.Fatalf("修订响应缺 finalTaskId")
	}

	// 已有 Task 不能作为「新最终」除非 pending/ready——C 已不存在（它是最终，仍 pending），
	// 这里换最终到 A 并取消 C：A 没有下游、C 被取消，A 可达自己，合法。
	if _, err := s.CreatePlanRevision(ctx, testOwner, orchID, RevisionCommand{
		IdempotencyKey: "rev-c3", BaseRevision: 1, Reason: "A 直接交付",
		CancelTasks: []string{"C"}, FinalTaskKey: "A",
	}, planEnv{}); err != nil {
		t.Fatalf("换最终到 A：%v", err)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	var a AgentTask
	if err := s.db.Where("orchestration_id = ? AND task_key = ?", orchID, "A").Take(&a).Error; err != nil {
		t.Fatal(err)
	}
	if o.FinalTaskID != a.ID {
		t.Fatalf("最终节点 = %s, want A %s", o.FinalTaskID, a.ID)
	}
}

// TestContinueOrchestration 钉住继续：版本不匹配拒绝，成功清暂停且不递增版本，
// 重复继续是空转成功。
func TestContinueOrchestration(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "cont"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	orchID := resp.OrchestrationID
	if err := s.db.Exec(`UPDATE agent_orchestrations SET plan_hold = true WHERE id = ?`, orchID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContinueOrchestration(ctx, testOwner, orchID, 1); !errors.Is(err, errRevisionStale) {
		t.Fatalf("错版本 continue err = %v, want stale", err)
	}
	got, err := s.ContinueOrchestration(ctx, testOwner, orchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanHold || got.Revision != 0 {
		t.Fatalf("continue 响应 = %+v", got)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if o.PlanHold {
		t.Fatalf("continue 后仍暂停")
	}
	if _, err := s.ContinueOrchestration(ctx, testOwner, orchID, 0); err != nil {
		t.Fatalf("重复 continue 应成功空转：%v", err)
	}
	// 检查点可能就是最终 Task 本身：最终已经开始时，继续仍须能解除暂停（该边界只属于修订）。
	if err := s.db.Exec(`UPDATE agent_orchestrations SET plan_hold = true WHERE id = ?`, orchID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec(`UPDATE agent_tasks SET started_at = now()
 WHERE orchestration_id = ? AND id = (SELECT final_task_id FROM agent_orchestrations WHERE id = ?)`,
		orchID, orchID).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := s.ContinueOrchestration(ctx, testOwner, orchID, 0); err != nil || got.PlanHold {
		t.Fatalf("最终 Task 已开始时的 continue = %+v err=%v, want 成功解除", got, err)
	}
}

// TestExpireTaskInputs 钉住 Task 提问超时（施工包 §7.3）：过期的 pending 问题把 Task 收成
// failed/input_timeout、同 run 其余 pending 转 stale、写去重 task_failed 通知；回答 CAS 拒绝过期行。
func TestExpireTaskInputs(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "hitl", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "问一句再答", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	past := time.Now().Add(-time.Minute)
	q1, err := s.InsertQuestion(ctx, RunQuestion{
		TenantID: testOwner.TenantID, Workspace: wsID, RunID: c.RunID, SessionID: sessID,
		Kind: "ask", Question: "要红还是蓝？", ExpiresAt: &past,
	})
	if err != nil {
		t.Fatal(err)
	}
	q2, err := s.InsertQuestion(ctx, RunQuestion{
		TenantID: testOwner.TenantID, Workspace: wsID, RunID: c.RunID, SessionID: sessID,
		Kind: "ask", Question: "还要别的吗？", ExpiresAt: &past,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 回答过期行：CAS 拒绝，且不改状态。
	if err := s.AnswerQuestion(ctx, q1.ID, "红", ""); !errors.Is(err, ErrQuestionExpired) {
		t.Fatalf("回答过期问题 err = %v, want ErrQuestionExpired", err)
	}
	expired, err := s.expireTaskInputsForTask(ctx, c.TaskID)
	if err != nil || !expired {
		t.Fatalf("expire = %v err=%v", expired, err)
	}
	st, code := taskStatus(t, s, c.TaskID)
	if st != agentTaskStatusFailed || code != taskErrInputTimeout {
		t.Fatalf("Task = %s/%s, want failed/input_timeout", st, code)
	}
	for _, q := range []RunQuestion{q1, q2} {
		var got RunQuestion
		if err := s.db.Where("id = ?", q.ID).Take(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != questionStatusStale {
			t.Fatalf("问题 %s 状态 = %s, want stale", q.ID, got.Status)
		}
	}
	if got := s.count(t, &AgentNotification{}, "task_id = ? AND kind = ?", c.TaskID, notifKindTaskFailed); got != 1 {
		t.Fatalf("task_failed 通知 = %d, want 1", got)
	}
	// 收口后再答：行已是 stale 且过了截止，要回「过期」而不是「答过了」。
	if err := s.AnswerQuestion(ctx, q1.ID, "蓝", ""); !errors.Is(err, ErrQuestionExpired) {
		t.Fatalf("超时收口后回答 err = %v, want ErrQuestionExpired", err)
	}
	// 第二次：Task 已终态，幂等空转。
	if again, err := s.expireTaskInputsForTask(ctx, c.TaskID); err != nil || again {
		t.Fatalf("重复 expire = %v err=%v", again, err)
	}
}

// TestTaskAttention 钉住四类处理项（施工包 §4.6）：question / plan_review / interrupted /
// delivery_failed 都从 PG 现算，排序按 updatedAt ASC。
func TestTaskAttention(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()

	// question：领取一件 Task，落两条 pending 问题（不带截止时间）。
	qTask, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "att-q", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "等待回答", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	qc := claimOne(t, s, qTask.OrchestrationID)
	for _, text := range []string{"第一问", "第二问"} {
		if _, err := s.InsertQuestion(ctx, RunQuestion{
			TenantID: testOwner.TenantID, Workspace: wsID, RunID: qc.RunID, SessionID: sessID,
			Kind: "ask", Question: text,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// plan_review：另一个组暂停。
	pr, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "att-plan"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec(`UPDATE agent_orchestrations SET plan_hold = true WHERE id = ?`, pr.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}

	// delivery_failed：第三个组交付失败。
	df, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "att-df", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "交付失败", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec(`UPDATE agent_orchestrations SET delivery_status = ? WHERE id = ?`,
		deliveryStatusFailed, df.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}

	// interrupted：第四件 Task 的 run 租约过期。
	in, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "att-in", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "失联", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	ic := claimOne(t, s, in.OrchestrationID)
	if err := s.db.Exec(`UPDATE runs SET lease_until = now() - interval '1 minute' WHERE id = ?`, ic.RunID).Error; err != nil {
		t.Fatal(err)
	}

	items, err := s.TaskAttention(ctx, testOwner, wsID)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string][]attentionItem{}
	for _, it := range items {
		byKind[it.Kind] = append(byKind[it.Kind], it)
	}
	if len(byKind["question"]) != 1 || byKind["question"][0].Count != 2 ||
		byKind["question"][0].QuestionID == "" || byKind["question"][0].TaskID != qc.TaskID {
		t.Fatalf("question 项 = %+v", byKind["question"])
	}
	if len(byKind["plan_review"]) != 1 || byKind["plan_review"][0].OrchestrationID != pr.OrchestrationID {
		t.Fatalf("plan_review 项 = %+v", byKind["plan_review"])
	}
	if len(byKind["delivery_failed"]) != 1 || byKind["delivery_failed"][0].OrchestrationID != df.OrchestrationID {
		t.Fatalf("delivery_failed 项 = %+v", byKind["delivery_failed"])
	}
	if len(byKind["interrupted"]) != 1 || byKind["interrupted"][0].TaskID != ic.TaskID {
		t.Fatalf("interrupted 项 = %+v", byKind["interrupted"])
	}
	for i := 1; i < len(items); i++ {
		if items[i].UpdatedAt.Before(items[i-1].UpdatedAt) {
			t.Fatalf("处理项未按 updatedAt 升序：%v", items)
		}
	}

	// 回答第一问后数量下降；答完两问后 question 项消失（不依赖未读表）。
	pending, err := s.ListQuestions(ctx, qc.RunID, questionStatusPending)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending 问题 = %d err=%v", len(pending), err)
	}
	for _, q := range pending {
		if err := s.AnswerQuestion(ctx, q.ID, "答", ""); err != nil {
			t.Fatal(err)
		}
	}
	items, err = s.TaskAttention(ctx, testOwner, wsID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == "question" {
			t.Fatalf("答完后仍有 question 项：%+v", it)
		}
	}
}

// TestPlanReviewClaim 钉住计划复核的认领边界（施工包 §6.3）：组 active+plan_hold 才造协调 run；
// 暂停已解除/组不 active 时通知直接 consumed，不写 delivery_status。
func TestPlanReviewClaim(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "pr-claim"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	orchID := resp.OrchestrationID
	var taskID string
	if err := s.db.Raw(`SELECT id FROM agent_tasks WHERE orchestration_id = ? AND task_key = ?`,
		orchID, "A").Scan(&taskID).Error; err != nil {
		t.Fatal(err)
	}
	insert := func(dedup string) AgentNotification {
		t.Helper()
		n := AgentNotification{
			TenantID: testOwner.TenantID, OrchestrationID: orchID, TaskID: taskID,
			ReceiverSessionID: sessID, Kind: notifKindPlanReviewRequired,
			DedupKey: dedup, Status: notifStatusPending,
		}
		var id string
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := s.insertNotificationTx(tx, n); err != nil {
				return err
			}
			return tx.Raw(`SELECT id FROM agent_notifications WHERE dedup_key = ?`, dedup).Scan(&id).Error
		}); err != nil {
			t.Fatal(err)
		}
		n.ID = id
		return n
	}

	// 未暂停 / 已解除：认领直接 consumed，不造 run。
	n0 := insert("pr:off")
	ts := &TaskScheduler{store: s}
	if launch := ts.claimNotification(ctx, n0.ID); launch != nil {
		t.Fatalf("未暂停也造了协调 run")
	}
	if st, _ := notifStatus(t, s, n0.ID); st != notifStatusConsumed {
		t.Fatalf("未暂停通知状态 = %s, want consumed", st)
	}

	// active + plan_hold：认领成功，组 delivery_status 不变。
	if err := s.db.Exec(`UPDATE agent_orchestrations SET plan_hold = true WHERE id = ?`, orchID).Error; err != nil {
		t.Fatal(err)
	}
	n1 := insert("pr:on")
	launch := ts.claimNotification(ctx, n1.ID)
	if launch == nil || launch.Kind != notifKindPlanReviewRequired {
		t.Fatalf("暂停中认领 = %+v", launch)
	}
	var delivery string
	if err := s.db.Raw(`SELECT delivery_status FROM agent_orchestrations WHERE id = ?`, orchID).
		Scan(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if delivery != "" {
		t.Fatalf("计划复核改了 delivery_status = %q", delivery)
	}
	// 输入带图与决策约定。
	var input string
	if err := s.db.Raw(`SELECT text FROM run_model_messages WHERE run_id = ? AND seq = 1`, launch.RunID).
		Scan(&input).Error; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"plan_review_required", "continue_orchestration", "revise_orchestration", "当前图"} {
		if !strings.Contains(input, want) {
			t.Fatalf("协调输入缺 %q：\n%s", want, input)
		}
	}
	// complete → 只消费通知，不动组交付状态；plan_hold 仍由 continue/revision 清。
	setRunStatus(t, s, launch.RunID, "complete")
	if err := s.ReconcileDeliveryRun(ctx, launch.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := notifStatus(t, s, n1.ID); st != notifStatusConsumed {
		t.Fatalf("复核 run 完成后通知状态 = %s, want consumed", st)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if !o.PlanHold || o.DeliveryStatus != "" {
		t.Fatalf("复核后组 = hold %v delivery %q（未作决定应保持暂停）", o.PlanHold, o.DeliveryStatus)
	}
}
