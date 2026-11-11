// 本文件是持久 Task 的存储半回归测试（施工包 §12.2）：真 PG（5433 隔离临时库）+ 真 Redis。
//
// 覆盖面：创建原子性与幂等（同键同计划回原组、同键不同计划 117102）、
// 提升与阻断传播（parents 全 succeeded 才 ready；failed/cancelled 的多层后代全 blocked、
// 独立分支不受影响）、两个 Store 实例并发领取只产出一场 run、组容量 2、
// 结果提交的校验与幂等、对账裁决表、EndRun 漏调后的补偿对账、
// 来源会话忙时通知保持 pending、交付对账的幂等收口、
// 执行 session 不进普通聊天列表、来源会话／项目删除保护。
//
// 这些测试不装配模型、不启动 worker goroutine：装配 nil（TaskScheduler.store 半直接验），
// 领取／对账／通知全靠 store 方法与原始 UPDATE 摆状态。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// secondStore 在同一个 PG 库上再开一个 Store 实例（= 另一个进程读同一份持久化数据）。
func secondStore(t *testing.T, dsn string) *Store {
	t.Helper()
	st, err := OpenStore(context.Background(), testRedisURL, dsn, testTTL, testLease)
	if err != nil {
		t.Fatalf("OpenStore(2nd): %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedTaskEnv 建一套 Task 测试的最小环境：出厂 researcher 规格（validatePlanAgents 读全局
// agent_defaults）+ 一个项目 + 一场挂在该项目下的普通聊天会话。返回项目 ID 与来源会话 ID。
func seedTaskEnv(t *testing.T, s *Store, name string) (string, SessionID) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Create(&AgentDefault{ID: "ad_test_" + name, Name: name, Kind: "subagent"}).Error; err != nil {
		t.Fatalf("seed agent default: %v", err)
	}
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "项目-" + name)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	sid, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	sess := Session{
		ID: sid, Title: "来源会话", TenantID: testOwner.TenantID, UserID: testOwner.UserID,
		Visibility: "private", WorkspaceID: &ws.ID, Kind: sessionKindChat,
	}
	if err := s.db.Create(&sess).Error; err != nil {
		t.Fatalf("seed source session: %v", err)
	}
	return ws.ID, sid
}

// mkTwoLayerCmd 造「A、B 并行 → C 汇总」的标准两层图命令。
func mkTwoLayerCmd(wsID string, sessID SessionID, key string) CreateOrchestrationCommand {
	return CreateOrchestrationCommand{
		IdempotencyKey:  key,
		WorkspaceID:     wsID,
		SourceSessionID: sessID,
		Goal:            "调研 A、B 后汇总",
		Tasks: []planTask{
			{Key: "A", Content: "调研 A", Agent: "researcher"},
			{Key: "B", Content: "调研 B", Agent: "researcher"},
			{Key: "C", Content: "汇总成报告", Agent: "researcher"},
		},
		Links: []planLink{{Parent: "A", Child: "C"}, {Parent: "B", Child: "C"}},
	}
}

// claimOne 领取组里恰一件 ready Task（断言恰好一件）。
func claimOne(t *testing.T, s *Store, orchID string) agentTaskClaim {
	t.Helper()
	claims, err := s.ClaimReadyInGroup(context.Background(), orchID)
	if err != nil {
		t.Fatalf("ClaimReadyInGroup: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	return claims[0]
}

// claimAll 领取组里所有能领的 ready Task（组容量内）。
func claimAll(t *testing.T, s *Store, orchID string) []agentTaskClaim {
	t.Helper()
	claims, err := s.ClaimReadyInGroup(context.Background(), orchID)
	if err != nil {
		t.Fatalf("ClaimReadyInGroup: %v", err)
	}
	return claims
}

// pickClaim 从一批领取里按组内 key 取一件。
func pickClaim(t *testing.T, claims []agentTaskClaim, key string) agentTaskClaim {
	t.Helper()
	for _, c := range claims {
		if c.TaskKey == key {
			return c
		}
	}
	t.Fatalf("领取结果里没有 key=%s：%v", key, claims)
	return agentTaskClaim{}
}

// taskStatus 读一件 Task 的当前存储状态与错误码。
func taskStatus(t *testing.T, s *Store, taskID string) (string, string) {
	t.Helper()
	var row struct {
		Status    string
		ErrorCode string
	}
	if err := s.db.Raw(`SELECT status, error_code FROM agent_tasks WHERE id = ?`, taskID).
		Scan(&row).Error; err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return row.Status, row.ErrorCode
}

// setRunStatus 把一场 run 摆到指定终态（模拟 worker 定稿 / 崩溃后的库上事实）。
func setRunStatus(t *testing.T, s *Store, runID RunID, status string) {
	t.Helper()
	if err := s.db.Exec(`UPDATE runs SET status = ?, finished_at = now() WHERE id = ?`, status, runID).
		Error; err != nil {
		t.Fatalf("set run %s = %s: %v", runID, status, err)
	}
}

// submitOK 提交一份合法结果（wsRoot 由调用方给；无产物）。
func submitOK(t *testing.T, s *Store, taskID string, runID RunID, epoch int64) int {
	t.Helper()
	rev, err := s.SubmitTaskResult(context.Background(), taskID, runID, epoch,
		TaskResult{Summary: "完成"}, t.TempDir())
	if err != nil {
		t.Fatalf("SubmitTaskResult: %v", err)
	}
	return rev
}

// TestAgentTaskCreateIdempotency 钉住创建幂等（施工包 §12.2）：
// 同键同计划 → 原组原 Task（不建第二行）；同键不同计划 → errPlanConflict；
// 单节点组落 ready、currentRunId 为空。
func TestAgentTaskCreateIdempotency(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()

	cmd := CreateTaskCommand{IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "写一份摘要", Agent: "researcher"}
	r1, err := s.CreateAgentTask(ctx, testOwner, cmd, planEnv{})
	if err != nil {
		t.Fatalf("CreateAgentTask: %v", err)
	}
	if r1.Status != agentTaskStatusReady || r1.CurrentRunID != "" {
		t.Fatalf("初始状态 = %+v，要 ready + 空 run", r1)
	}
	r2, err := s.CreateAgentTask(ctx, testOwner, cmd, planEnv{})
	if err != nil {
		t.Fatalf("同键同计划重试失败: %v", err)
	}
	if r2.TaskID != r1.TaskID || r2.OrchestrationID != r1.OrchestrationID {
		t.Fatalf("幂等重试建了新行：%+v vs %+v", r1, r2)
	}
	if got := s.count(t, &AgentOrchestration{}, "source_session_id = ?", sessID); got != 1 {
		t.Fatalf("编排组行数 = %d, want 1", got)
	}
	if got := s.count(t, &AgentTask{}, "orchestration_id = ?", r1.OrchestrationID); got != 1 {
		t.Fatalf("Task 行数 = %d, want 1", got)
	}

	other := cmd
	other.Content = "改主意了，写另一份"
	if _, err := s.CreateAgentTask(ctx, testOwner, other, planEnv{}); !errors.Is(err, errPlanConflict) {
		t.Fatalf("同键不同计划 err = %v, want errPlanConflict", err)
	}
}

// TestAgentTaskCreatePublishesEvents 钉住施工包 §10.3 的「创建在 PG 成功后发」：
// 创建后租户事件流里要有每个新 Task 的 agent_tasks_changed——缺了它，任务页的列表
// 要等调度轮第一次状态翻转才出现新行，blocked 的尾巴要等到上游跑完才可见。
func TestAgentTaskCreatePublishesEvents(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()

	orch, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "k1"), planEnv{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var rows []AgentTask
	if err := s.db.Where("orchestration_id = ?", orch.OrchestrationID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	wantIDs := map[string]bool{}
	for _, r := range rows {
		wantIDs[r.ID] = false
	}

	msgs, err := s.rdb.XRange(ctx, keyEvents(testOwner.TenantID), "-", "+").Result()
	if err != nil {
		t.Fatalf("读事件流: %v", err)
	}
	orchEvents := 0
	taskEvents := map[string]bool{}
	for _, m := range msgs {
		typ, _ := m.Values["type"].(string)
		id, _ := m.Values["id"].(string)
		if typ == eventAgentOrchestrationsChanged && id == orch.OrchestrationID {
			orchEvents++
		}
		if typ == eventAgentTasksChanged {
			if _, ok := wantIDs[id]; ok {
				taskEvents[id] = true
			}
		}
	}
	if orchEvents != 1 {
		t.Fatalf("组事件条数 = %d, want 1", orchEvents)
	}
	for id := range wantIDs {
		if !taskEvents[id] {
			t.Fatalf("创建后事件流缺 Task %s 的 agent_tasks_changed", id)
		}
	}
}

// TestAgentTaskCreateValidation 钉住写库前的校验面（施工包 §6.1）：
// 执行者不存在、项目不挂在来源会话上、来源会话被删，各自的业务错误。
func TestAgentTaskCreateValidation(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()

	badAgent := CreateTaskCommand{IdempotencyKey: "k", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "x", Agent: "ghost"}
	if _, err := s.CreateAgentTask(ctx, testOwner, badAgent, planEnv{}); !errors.Is(err, errCreateAgent) {
		t.Fatalf("未知执行者 err = %v, want errCreateAgent", err)
	}

	badWS := CreateTaskCommand{IdempotencyKey: "k", WorkspaceID: "ws_别的", SourceSessionID: sessID,
		Content: "x", Agent: "researcher"}
	if _, err := s.CreateAgentTask(ctx, testOwner, badWS, planEnv{}); !errors.Is(err, errCreateWorkspace) {
		t.Fatalf("项目不符 err = %v, want errCreateWorkspace", err)
	}

	// 删除来源会话后创建要被拒（errCreateSource），证明 FOR SHARE 与删除事务互斥这条链。
	other, otherSess := seedTaskEnv(t, s, "researcher2")
	if err := s.db.Exec(`UPDATE sessions SET deleted_at = now() WHERE id = ?`, otherSess).Error; err != nil {
		t.Fatal(err)
	}
	deleted := CreateTaskCommand{IdempotencyKey: "k", WorkspaceID: other, SourceSessionID: otherSess,
		Content: "x", Agent: "researcher"}
	if _, err := s.CreateAgentTask(ctx, testOwner, deleted, planEnv{}); !errors.Is(err, errCreateSource) {
		t.Fatalf("已删会话 err = %v, want errCreateSource", err)
	}
}

// TestAgentTaskPromoteBlockAdvance 钉住提升与传播（施工包 §12.2）：
// A 成功而 B 未成功时 C 保持 pending；A、B 都 succeeded 后 C 才 ready；
// failed 的多层后代全部 blocked（independent 分支不动），组不收口。
func TestAgentTaskPromoteBlockAdvance(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "k1"), planEnv{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var rows []AgentTask
	if err := s.db.Where("orchestration_id = ?", orch.OrchestrationID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	byKey := map[string]AgentTask{}
	for _, r := range rows {
		byKey[r.TaskKey] = r
	}

	// 初始：A、B ready，C pending。
	if st, _ := taskStatus(t, s, byKey["A"].ID); st != agentTaskStatusReady {
		t.Fatalf("A 初始 = %s, want ready", st)
	}
	if st, _ := taskStatus(t, s, byKey["C"].ID); st != agentTaskStatusPending {
		t.Fatalf("C 初始 = %s, want pending", st)
	}

	// A 成功（提交 → run complete → 对账）。A、B 同时 ready，一批领两件。
	claims := claimAll(t, s, orch.OrchestrationID)
	if len(claims) != 2 {
		t.Fatalf("首批领 %d 件, want 2（A、B）", len(claims))
	}
	claimA := pickClaim(t, claims, "A")
	if claimA.TaskID != byKey["A"].ID {
		t.Fatalf("领到 %s, want A", claimA.TaskKey)
	}
	submitOK(t, s, claimA.TaskID, claimA.RunID, claimA.Epoch)
	setRunStatus(t, s, claimA.RunID, "complete")
	if err := s.ReconcileRun(ctx, claimA.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := taskStatus(t, s, byKey["A"].ID); st != agentTaskStatusSucceeded {
		t.Fatalf("A 对账后 = %s, want succeeded", st)
	}
	// B 未成功：C 不能被提走。
	if st, _ := taskStatus(t, s, byKey["C"].ID); st != agentTaskStatusPending {
		t.Fatalf("A 成功但 B 未成功时 C = %s, want pending", st)
	}

	// B 失败（run error → 对账 → failed + task_failed 通知）。
	claimB := pickClaim(t, claims, "B")
	setRunStatus(t, s, claimB.RunID, "error")
	if err := s.ReconcileRun(ctx, claimB.RunID); err != nil {
		t.Fatal(err)
	}
	stB, codeB := taskStatus(t, s, byKey["B"].ID)
	if stB != agentTaskStatusFailed || codeB != taskErrRunError {
		t.Fatalf("B = %s/%s, want failed/run_error", stB, codeB)
	}
	// blocked 传播：C 是 B 的直接后代 → blocked/dependency_failed。
	n, err := s.PropagateBlockedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("传播了 %d 件, want 1（只有 C）", n)
	}
	if st, code := taskStatus(t, s, byKey["C"].ID); st != agentTaskStatusBlocked || code != taskErrDependencyFailed {
		t.Fatalf("C = %s/%s, want blocked/dependency_failed", st, code)
	}
	// 组不收口没有通知：只有 B 的 task_failed 一条。
	if got := s.count(t, &AgentNotification{}, "orchestration_id = ?", orch.OrchestrationID); got != 1 {
		t.Fatalf("通知数 = %d, want 1（task_failed）", got)
	}
	// 状态翻转也发事件（§10.3）：B 失败、C 被挡，事件流里要有这两条任务事件。
	msgs, err := s.rdb.XRange(ctx, keyEvents(testOwner.TenantID), "-", "+").Result()
	if err != nil {
		t.Fatalf("读事件流: %v", err)
	}
	gotTaskEvents := map[string]int{}
	for _, m := range msgs {
		typ, _ := m.Values["type"].(string)
		id, _ := m.Values["id"].(string)
		if typ == eventAgentTasksChanged {
			gotTaskEvents[id]++
		}
	}
	for _, key := range []string{"B", "C"} {
		if gotTaskEvents[byKey[key].ID] == 0 {
			t.Fatalf("事件流缺 %s 的状态翻转事件", key)
		}
	}
}

// TestAgentTaskPromoteAfterAllParentsSucceed 钉住「两者都成功后 C ready」的正路径。
func TestAgentTaskPromoteAfterAllParentsSucceed(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "k1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []AgentTask
	s.db.Where("orchestration_id = ?", orch.OrchestrationID).Find(&rows)
	byKey := map[string]AgentTask{}
	for _, r := range rows {
		byKey[r.TaskKey] = r
	}
	claims := claimAll(t, s, orch.OrchestrationID)
	if len(claims) != 2 {
		t.Fatalf("首批领 %d 件, want 2（A、B）", len(claims))
	}
	for _, key := range []string{"A", "B"} {
		c := pickClaim(t, claims, key)
		submitOK(t, s, c.TaskID, c.RunID, c.Epoch)
		setRunStatus(t, s, c.RunID, "complete")
		if err := s.ReconcileRun(ctx, c.RunID); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PromoteReadyTasks(ctx); err != nil || n != 1 {
		t.Fatalf("promote n=%d err=%v, want 1", n, err)
	}
	if st, _ := taskStatus(t, s, byKey["C"].ID); st != agentTaskStatusReady {
		t.Fatalf("C = %s, want ready（A、B 都 succeeded）", st)
	}
	// 领取 C：首条输入要带两个上游的 taskId 与 revision（施工包 §7.3 的引用面）。
	c := claimOne(t, s, orch.OrchestrationID)
	if c.TaskID != byKey["C"].ID {
		t.Fatalf("领到 %s, want C", c.TaskKey)
	}
	for _, want := range []string{byKey["A"].ID, "resultRevision=1", byKey["B"].ID, "## 执行约定"} {
		if !strings.Contains(c.FirstInput, want) {
			t.Fatalf("C 首条输入缺 %q\n---\n%s", want, c.FirstInput)
		}
	}
}

// TestAgentTaskGroupCap 钉住组容量（施工包 §2 第 11 条）：一组 4 件 ready，
// 第一轮最多领 2 件；1 件收尾后再领 1 件（活数下降才放新位）。
func TestAgentTaskGroupCap(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	cmd := CreateOrchestrationCommand{IdempotencyKey: "k", WorkspaceID: wsID, SourceSessionID: sessID, Goal: "四件并行"}
	for i := 0; i < 4; i++ {
		cmd.Tasks = append(cmd.Tasks, planTask{Key: fmt.Sprintf("t%d", i), Content: "并行件", Agent: "researcher"})
		cmd.Links = append(cmd.Links, planLink{Parent: fmt.Sprintf("t%d", i), Child: "final"})
	}
	cmd.Tasks = append(cmd.Tasks, planTask{Key: "final", Content: "汇总", Agent: "researcher"})
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, cmd, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimReadyInGroup(ctx, orch.OrchestrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != orchestrationConcurrency {
		t.Fatalf("第一轮领 %d 件, want %d", len(first), orchestrationConcurrency)
	}
	// 活数未降：再领为空。
	more, err := s.ClaimReadyInGroup(ctx, orch.OrchestrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 0 {
		t.Fatalf("容量未释放还能领 %d 件", len(more))
	}
	// 一件成功收尾：活数 1 → budget 1。
	submitOK(t, s, first[0].TaskID, first[0].RunID, first[0].Epoch)
	setRunStatus(t, s, first[0].RunID, "complete")
	if err := s.ReconcileRun(ctx, first[0].RunID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimReadyInGroup(ctx, orch.OrchestrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("收尾一件后领 %d 件, want 1", len(second))
	}
}

// TestAgentTaskClaimRaceTwoStores 钉住多副本领取（施工包 §12.2）：两个 Store 实例
// （= 两个进程读同一份 PG）同时领同一组，3 件 ready 只能产出 2 场 run（组容量），
// 每件 Task 恰有一条 current_run_id。
func TestAgentTaskClaimRaceTwoStores(t *testing.T) {
	s1, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s1, "researcher")
	ctx := context.Background()
	cmd := CreateOrchestrationCommand{IdempotencyKey: "k", WorkspaceID: wsID, SourceSessionID: sessID, Goal: "三件并行"}
	for i := 0; i < 3; i++ {
		cmd.Tasks = append(cmd.Tasks, planTask{Key: fmt.Sprintf("t%d", i), Content: "并行件", Agent: "researcher"})
		cmd.Links = append(cmd.Links, planLink{Parent: fmt.Sprintf("t%d", i), Child: "final"})
	}
	cmd.Tasks = append(cmd.Tasks, planTask{Key: "final", Content: "汇总", Agent: "researcher"})
	orch, err := s1.CreateAgentOrchestration(ctx, testOwner, cmd, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	s2 := secondStore(t, dsn)
	var mu sync.Mutex
	var total int
	var wg sync.WaitGroup
	for _, st := range []*Store{s1, s2} {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			claims, err := st.ClaimReadyInGroup(ctx, orch.OrchestrationID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			total += len(claims)
			mu.Unlock()
		}(st)
	}
	wg.Wait()
	if total != orchestrationConcurrency {
		t.Fatalf("两副本合计领 %d 件, want %d（组容量）", total, orchestrationConcurrency)
	}
	// 每件被领的 Task 恰一条 run；没被领的保持 ready。
	var withRun int64
	if err := s1.db.Raw(`SELECT COUNT(*) FROM agent_tasks WHERE orchestration_id = ? AND current_run_id IS NOT NULL`,
		orch.OrchestrationID).Scan(&withRun).Error; err != nil {
		t.Fatal(err)
	}
	if withRun != int64(total) {
		t.Fatalf("有 run 的 Task = %d, claims = %d", withRun, total)
	}
}

// TestAgentTaskSubmitValidation 钉住结果提交的裁决面（施工包 §12.2）：
// summary 必填、artifact 必须是项目根下真实普通文件（绝对路径 / .. / 不存在都拒）、
// 同代同字节幂等回原 revision、改字节递增、旧 epoch 与已定稿 run 拒绝（stale）、
// 非 running 的 Task 拒绝（not_running）。
func TestAgentTaskSubmitValidation(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "out", "report.md"), []byte("# 报告"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "写报告", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)

	if _, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch, TaskResult{Summary: "  "}, root); !errors.Is(err, errSubmitBad) {
		t.Fatalf("空 summary err = %v, want errSubmitBad", err)
	}
	bad := []TaskArtifact{
		{Path: "/etc/passwd", Kind: "x"},
		{Path: "../escape.md", Kind: "x"},
		{Path: "out/missing.md", Kind: "x"},
		{Path: "out", Kind: "x"}, // 目录不行
	}
	for _, a := range bad {
		_, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch, TaskResult{Summary: "s", Artifacts: []TaskArtifact{a}}, root)
		if !errors.Is(err, errArtifactBad) {
			t.Fatalf("artifact %q err = %v, want errArtifactBad", a.Path, err)
		}
	}
	rev1, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch,
		TaskResult{Summary: "好了", Artifacts: []TaskArtifact{{Path: "out/report.md", Kind: "report"}}}, root)
	if err != nil {
		t.Fatalf("合法提交失败: %v", err)
	}
	if rev1 != 1 {
		t.Fatalf("首次 revision = %d, want 1", rev1)
	}
	// 同代同字节重提：幂等回原 revision。
	rev1b, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch,
		TaskResult{Summary: "好了", Artifacts: []TaskArtifact{{Path: "out/report.md", Kind: "report"}}}, root)
	if err != nil || rev1b != rev1 {
		t.Fatalf("幂等重提 = %d, %v; want %d, nil", rev1b, err, rev1)
	}
	// 改字节 → revision 2。
	rev2, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch, TaskResult{Summary: "改好了"}, root)
	if err != nil || rev2 != 2 {
		t.Fatalf("改字节重提 = %d, %v; want 2", rev2, err)
	}
	// 旧 epoch（接管令牌落后）拒绝。
	if _, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch+1, TaskResult{Summary: "未来代"}, root); !errors.Is(err, errSubmitStale) {
		t.Fatalf("未来 epoch err = %v, want errSubmitStale", err)
	}
	// run 已定稿（complete）后再提交 → stale。
	setRunStatus(t, s, c.RunID, "complete")
	if _, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch, TaskResult{Summary: "迟到的提交"}, root); !errors.Is(err, errSubmitStale) {
		t.Fatalf("run 定稿后提交 err = %v, want errSubmitStale", err)
	}
	// Task 已 cancelled 后提交 → not_running。
	if _, _, err := s.StopAgentTask(ctx, testOwner, c.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch, TaskResult{Summary: "x"}, root); !errors.Is(err, errSubmitNotRunning) {
		t.Fatalf("取消后提交 err = %v, want errSubmitNotRunning", err)
	}
}

// TestAgentTaskReconcileVerdicts 钉住对账裁决表（施工包 §12.2）：
// complete 无提交 → failed/result_not_submitted；error → failed/run_error；
// stopped → cancelled；running 不动；重复对账不重复写通知。
func TestAgentTaskReconcileVerdicts(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()

	mkTask := func(key string) (string, RunID) {
		resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
			IdempotencyKey: key, WorkspaceID: wsID, SourceSessionID: sessID, Content: "件 " + key, Agent: "researcher",
		}, planEnv{})
		if err != nil {
			t.Fatal(err)
		}
		c := claimOne(t, s, resp.OrchestrationID)
		return c.TaskID, c.RunID
	}

	// complete + 无提交。
	taskID, runID := mkTask("k1")
	setRunStatus(t, s, runID, "complete")
	if err := s.ReconcileRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if st, code := taskStatus(t, s, taskID); st != agentTaskStatusFailed || code != taskErrResultNotSubmit {
		t.Fatalf("无提交收尾 = %s/%s, want failed/result_not_submitted", st, code)
	}
	if got := s.count(t, &AgentNotification{}, "task_id = ?", taskID); got != 1 {
		t.Fatalf("task_failed 通知 = %d, want 1", got)
	}
	// 重复对账同一 run：幂等，通知不重复。
	if err := s.ReconcileRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if got := s.count(t, &AgentNotification{}, "task_id = ?", taskID); got != 1 {
		t.Fatalf("重复对账后通知 = %d, want 1", got)
	}

	// stopped → cancelled（取消不写通知）。
	taskID2, runID2 := mkTask("k2")
	setRunStatus(t, s, runID2, "stopped")
	if err := s.ReconcileRun(ctx, runID2); err != nil {
		t.Fatal(err)
	}
	if st, _ := taskStatus(t, s, taskID2); st != agentTaskStatusCancelled {
		t.Fatalf("stopped 收尾 = %s, want cancelled", st)
	}
	if got := s.count(t, &AgentNotification{}, "task_id = ?", taskID2); got != 0 {
		t.Fatalf("取消不该写通知，实际 %d", got)
	}

	// running（租约未过）不动。
	_, runID3 := mkTask("k3")
	if err := s.ReconcileRun(ctx, runID3); err != nil {
		t.Fatal(err)
	}

	// 普通聊天 run（不是 Task）对账是 no-op。
	if err := s.ReconcileRun(ctx, "r_不存在的run"); err != nil {
		t.Fatalf("非 Task run 对账报错: %v", err)
	}
}

// TestAgentTaskMissedEndRunReconcile 钉住崩溃补偿（施工包 §12.2）：
// 进程 A 领取并提交、run 落了 complete 但没来得及对账就崩了；
// 进程 B（另一个 Store 实例）的调度对账能把它收成 succeeded，且失败路径只写一条通知。
func TestAgentTaskMissedEndRunReconcile(t *testing.T) {
	s1, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s1, "researcher")
	ctx := context.Background()
	resp, err := s1.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "跨进程收尾", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s1, resp.OrchestrationID)
	submitOK(t, s1, c.TaskID, c.RunID, c.Epoch)
	setRunStatus(t, s1, c.RunID, "complete")
	// s1 没有 ReconcileRun——崩溃点。

	s2 := secondStore(t, dsn)
	if err := s2.ReconcileRun(ctx, c.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := taskStatus(t, s2, c.TaskID); st != agentTaskStatusSucceeded {
		t.Fatalf("B 进程对账后 = %s, want succeeded", st)
	}
	// 失败路径的补偿：另一组，run error 后由 B 对账，通知恰好一条（重复对账也不加）。
	resp2, err := s1.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k2", WorkspaceID: wsID, SourceSessionID: sessID, Content: "跨进程失败", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c2 := claimOne(t, s1, resp2.OrchestrationID)
	setRunStatus(t, s1, c2.RunID, "error")
	if err := s2.ReconcileRun(ctx, c2.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s2.ReconcileRun(ctx, c2.RunID); err != nil {
		t.Fatal(err)
	}
	if got := s2.count(t, &AgentNotification{}, "task_id = ?", c2.TaskID); got != 1 {
		t.Fatalf("跨进程失败通知 = %d, want 1", got)
	}
}

// TestAgentTaskFailClaimedRun 钉住装配失败收尾（施工包 §6.2）：领取后装配失败时
// FailClaimedTaskRun 把 run 收成 error（租约清空）、Task 落 failed/assembly_failed 并写
// 恰好一条 task_failed 通知，重复调用幂等不加行。它同时钉住那条 SQL 必须连出 runs 表
// 才拿得到 lease_epoch——缺 JOIN 的写法在这条测试当场红（2026-11-10 活体验收抓过一次）。
func TestAgentTaskFailClaimedRun(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "装配会失败的一件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	if err := s.FailClaimedTaskRun(ctx, c.RunID, "执行者规格不存在"); err != nil {
		t.Fatal(err)
	}
	if st, code := taskStatus(t, s, c.TaskID); st != agentTaskStatusFailed || code != taskErrAssemblyFailed {
		t.Fatalf("task = %s/%s, want failed/assembly_failed", st, code)
	}
	var run struct {
		Status  string
		NoLease bool
	}
	if err := s.db.Raw(`SELECT status, (lease_until IS NULL) AS no_lease FROM runs WHERE id = ?`, c.RunID).
		Scan(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != "error" || !run.NoLease {
		t.Fatalf("run = %s（租约已清=%v）, want error + 租约清空", run.Status, run.NoLease)
	}
	if got := s.count(t, &AgentNotification{}, "task_id = ?", c.TaskID); got != 1 {
		t.Fatalf("task_failed 通知 = %d, want 1", got)
	}
	if err := s.FailClaimedTaskRun(ctx, c.RunID, "重复收尾"); err != nil {
		t.Fatal(err)
	}
	if got := s.count(t, &AgentNotification{}, "task_id = ?", c.TaskID); got != 1 {
		t.Fatalf("重复收尾后通知 = %d, want 1", got)
	}
}

// TestAgentTaskNotificationBusyReceiver 钉住来源会话忙不认领（施工包 §12.2）：
// 来源会话有活 run 时通知保持 pending；释放后恰好认领一次（铸一场协调 run，
// 通知翻 claimed、组交付翻 delivering），重复消费不再产生第二场。
func TestAgentTaskNotificationBusyReceiver(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "交付一件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	submitOK(t, s, c.TaskID, c.RunID, c.Epoch)
	setRunStatus(t, s, c.RunID, "complete")
	if err := s.ReconcileRun(ctx, c.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceOrchestrations(ctx); err != nil {
		t.Fatal(err)
	}
	var notif AgentNotification
	if err := s.db.Where("orchestration_id = ? AND kind = ?", resp.OrchestrationID, notifKindOrchFinished).
		Take(&notif).Error; err != nil {
		t.Fatalf("整组完成通知缺失: %v", err)
	}

	// 来源会话忙：挂一场活 run（租约有效）→ 不认领。
	busyRun := "r_busy_" + notif.ID[:8]
	if err := s.db.Exec(`INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, created_at)
		VALUES (?, ?, 'running', 1, now() + make_interval(secs => 120), now())`, busyRun, sessID).Error; err != nil {
		t.Fatal(err)
	}
	ts := &TaskScheduler{store: s}
	if launch := ts.claimNotification(ctx, notif.ID); launch != nil {
		t.Fatalf("来源会话忙还被认领了")
	}
	if st, _ := notifStatus(t, s, notif.ID); st != notifStatusPending {
		t.Fatalf("忙时通知状态 = %s, want pending", st)
	}

	// 释放后再认领：恰好一场协调 run，通知 claimed，组 delivering。
	setRunStatus(t, s, RunID(busyRun), "complete")
	launch := ts.claimNotification(ctx, notif.ID)
	if launch == nil {
		t.Fatalf("释放后没有认领")
	}
	if launch.SessionID != sessID || launch.Kind != notifKindOrchFinished {
		t.Fatalf("认领产出 = %+v", launch)
	}
	if st, _ := notifStatus(t, s, notif.ID); st != notifStatusClaimed {
		t.Fatalf("认领后状态 = %s, want claimed", st)
	}
	var delivery string
	if err := s.db.Raw(`SELECT delivery_status FROM agent_orchestrations WHERE id = ?`, resp.OrchestrationID).
		Scan(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if delivery != deliveryStatusDelivering {
		t.Fatalf("组交付状态 = %s, want delivering", delivery)
	}
	// 重复消费：通知已 claimed，不再产生第二场协调 run。
	if again := ts.claimNotification(ctx, notif.ID); again != nil {
		t.Fatalf("重复认领成功")
	}
	// 协调 run 确实挂在来源会话上（receiver），busy run 已 complete。
	if got := s.count(t, &Run{}, "session_id = ? AND status = 'running'", sessID); got != 1 {
		t.Fatalf("来源会话上的活 run = %d, want 1（协调 run）", got)
	}
	_ = launch.RunID
}

func notifStatus(t *testing.T, s *Store, id string) (string, string) {
	t.Helper()
	var row struct {
		Status     string
		ClaimRunID string
	}
	if err := s.db.Raw(`SELECT status, claim_run_id FROM agent_notifications WHERE id = ?`, id).
		Scan(&row).Error; err != nil {
		t.Fatalf("read notif: %v", err)
	}
	return row.Status, row.ClaimRunID
}

// TestAgentTaskDeliveryReconcile 钉住交付对账（施工包 §12.2）：
// 协调 run complete → delivered + 通知 consumed，重复对账幂等；
// 协调 run error → delivery failed；非协调 run 的 run 不进对账。
func TestAgentTaskDeliveryReconcile(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()

	getToDelivering := func(key string) (orchID, deliveryRunID string) {
		resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
			IdempotencyKey: key, WorkspaceID: wsID, SourceSessionID: sessID, Content: "交付 " + key, Agent: "researcher",
		}, planEnv{})
		if err != nil {
			t.Fatal(err)
		}
		c := claimOne(t, s, resp.OrchestrationID)
		submitOK(t, s, c.TaskID, c.RunID, c.Epoch)
		setRunStatus(t, s, c.RunID, "complete")
		if err := s.ReconcileRun(ctx, c.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdvanceOrchestrations(ctx); err != nil {
			t.Fatal(err)
		}
		var notif AgentNotification
		if err := s.db.Where("orchestration_id = ? AND kind = ?", resp.OrchestrationID, notifKindOrchFinished).
			Take(&notif).Error; err != nil {
			t.Fatal(err)
		}
		ts := &TaskScheduler{store: s}
		launch := ts.claimNotification(ctx, notif.ID)
		if launch == nil {
			t.Fatalf("%s: 认领失败", key)
		}
		return resp.OrchestrationID, string(launch.RunID)
	}

	// complete → delivered。
	orchID, deliveryRun := getToDelivering("k1")
	setRunStatus(t, s, RunID(deliveryRun), "complete")
	if err := s.ReconcileDeliveryRun(ctx, RunID(deliveryRun)); err != nil {
		t.Fatal(err)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if o.DeliveryStatus != deliveryStatusDelivered || o.DeliveredAt == nil {
		t.Fatalf("交付状态 = %s/%v, want delivered/非空", o.DeliveryStatus, o.DeliveredAt)
	}
	if got := s.count(t, &AgentNotification{}, "orchestration_id = ? AND status = ?", orchID, notifStatusConsumed); got != 1 {
		t.Fatalf("consumed 通知 = %d, want 1", got)
	}
	// 幂等：再对账一次不报错、不变化。
	if err := s.ReconcileDeliveryRun(ctx, RunID(deliveryRun)); err != nil {
		t.Fatal(err)
	}
	if got := s.count(t, &AgentNotification{}, "orchestration_id = ?", orchID); got != 1 {
		t.Fatalf("重复对账后通知 = %d, want 1", got)
	}

	// error → failed。
	orchID2, deliveryRun2 := getToDelivering("k2")
	setRunStatus(t, s, RunID(deliveryRun2), "error")
	if err := s.ReconcileDeliveryRun(ctx, RunID(deliveryRun2)); err != nil {
		t.Fatal(err)
	}
	var o2 AgentOrchestration
	s.db.Where("id = ?", orchID2).Take(&o2)
	if o2.DeliveryStatus != deliveryStatusFailed {
		t.Fatalf("error 交付 = %s, want failed", o2.DeliveryStatus)
	}

	// 非协调 run：no-op。
	setRunStatus(t, s, RunID(deliveryRun), "complete")
	if err := s.ReconcileDeliveryRun(ctx, "r_无关run"); err != nil {
		t.Fatalf("非协调 run 对账报错: %v", err)
	}
}

// TestAgentTaskHiddenSessionAndRunlog 钉住账本归属（施工包 §12.2）：
// 执行 session kind=agent_task，不进普通聊天列表（ListSessions 只回 kind='chat'）；
// 执行 run 的首条 user 行 internal=false（执行账本的正常内容），
// 协调 run 的 user 行 internal=true（程序写进来的协调输入，页面不渲染）。
func TestAgentTaskHiddenSessionAndRunlog(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "看得见看不见", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)

	sessions, err := s.ListSessions(ctx, testOwner, "", wsID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range sessions {
		if sess.ID == c.SessionID {
			t.Fatalf("执行 session %s 出现在普通聊天列表", c.SessionID)
		}
	}
	if got := s.count(t, &Session{}, "id = ? AND kind = ?", c.SessionID, sessionKindAgentTask); got != 1 {
		t.Fatalf("执行 session kind 校验失败（count=%d）", got)
	}
	// 执行 run 的首条输入：runlog internal=false；mh 有对应 seq1 user 行。
	var internal bool
	if err := s.db.Raw(`SELECT internal FROM runlog_entries WHERE session_id = ? AND run_id = ?`,
		c.SessionID, c.RunID).Scan(&internal).Error; err != nil {
		t.Fatal(err)
	}
	if internal {
		t.Fatalf("执行 run 的 user 行不该是 internal")
	}
	if got := s.count(t, &RunModelMessage{}, "run_id = ? AND seq = 1 AND role = 'user'", c.RunID); got != 1 {
		t.Fatalf("mh seq1 = %d, want 1", got)
	}
}

// TestAgentTaskDeleteProtection 钉住删除保护（施工包 §12.2）：
// 来源会话／项目上还有活着的编排组（active / 待交付）时整笔拒绝；
// 整组取消收口后删除放行。
func TestAgentTaskDeleteProtection(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	orch, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "k1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, sessID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("有活跃组时删会话 err = %v, want ErrSessionBusy", err)
	}
	if _, err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, wsID); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("有活跃组时删项目 err = %v, want ErrWorkspaceBusy", err)
	}
	// 取消整组 → 组 cancelled → 删除放行。
	status, _, err := s.StopAgentOrchestration(ctx, testOwner, orch.OrchestrationID)
	if err != nil {
		t.Fatal(err)
	}
	if status != orchStatusCancelled {
		t.Fatalf("停止返回 = %s, want cancelled（无 running 节点当场收口）", status)
	}
	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, sessID); err != nil {
		t.Fatalf("取消后删会话: %v", err)
	}
	// 已删的会话再创建 → errCreateSource（可见性链把已删会话当不存在）。
	if _, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "k2"), planEnv{}); !errors.Is(err, errCreateSource) {
		t.Fatalf("已删会话再建组 err = %v, want errCreateSource", err)
	}
}

// TestAgentTaskStopRunningTask 钉住停 running 的 Task（施工包 §4.3）：
// Task 行立刻翻 cancelled（submit 失效），返回当前 run ID 交给调用方收尾；
// 对账 stopped run 幂等（Task 已不是 running，no-op）。
func TestAgentTaskStopRunningTask(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "停我", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	status, runID, err := s.StopAgentTask(ctx, testOwner, c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if status != agentTaskStatusCancelled || runID != c.RunID {
		t.Fatalf("stop 返回 = %s/%s, want cancelled/%s", status, runID, c.RunID)
	}
	if st, _ := taskStatus(t, s, c.TaskID); st != agentTaskStatusCancelled {
		t.Fatalf("stop 后 Task = %s, want cancelled", st)
	}
	setRunStatus(t, s, c.RunID, "stopped")
	if err := s.ReconcileRun(ctx, c.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := taskStatus(t, s, c.TaskID); st != agentTaskStatusCancelled {
		t.Fatalf("对账后 = %s, want cancelled（不被 stopped 覆盖）", st)
	}
	// 重复停止幂等。
	if status2, _, err := s.StopAgentTask(ctx, testOwner, c.TaskID); err != nil || status2 != agentTaskStatusCancelled {
		t.Fatalf("重复停止 = %s, %v", status2, err)
	}
}

// TestAgentTaskVisibilityIsolation 钉住可见性（施工包 §4.3）：别人的 Task 查不到，
// 与不存在同形（不给探测通道）。
func TestAgentTaskVisibilityIsolation(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "我的任务", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	stranger := principal{UserID: "u_other", TenantID: testOwner.TenantID, Role: "member"}
	if _, err := s.GetAgentTask(ctx, stranger, resp.TaskID); !errors.Is(err, errTaskNotFound) {
		t.Fatalf("外人读 Task err = %v, want errTaskNotFound", err)
	}
	if _, err := s.GetAgentOrchestration(ctx, stranger, resp.OrchestrationID); !errors.Is(err, errOrchNotFound) {
		t.Fatalf("外人读组 err = %v, want errOrchNotFound", err)
	}
}

// TestAgentTaskResultArtifactsNormalized 钉住读侧归一（施工包 §4.3 契约形状）：
// 没带 artifacts 的结果落库是 null（保留提交原样），但 GET 视图必须回 []——
// 前端按 result.artifacts.length 渲染，null 让详情抽屉整个渲染崩掉。
func TestAgentTaskResultArtifactsNormalized(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "只交摘要", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	submitOK(t, s, c.TaskID, c.RunID, c.Epoch)

	// 落库字节保留原样：没带 artifacts 就是 null。
	var stored string
	if err := s.db.Raw(`SELECT result FROM agent_tasks WHERE id = ?`, c.TaskID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `"artifacts":null`) {
		t.Fatalf("落库字节 = %s, want 原样保留 null", stored)
	}

	view, err := s.GetAgentTask(ctx, testOwner, c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Result == nil || view.Result.Artifacts == nil || len(view.Result.Artifacts) != 0 {
		t.Fatalf("视图 result.artifacts = %+v, want 非nil 空数组", view.Result)
	}
}

// TestAgentTaskDetectInterrupted 钉住中断检测（施工包 §12.2 / §9.1）：
// 租约过期只写去重的 task_interrupted 通知（状态不翻——interrupted 是读侧派生），
// 同一代重复检测不重复写。
func TestAgentTaskDetectInterrupted(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "失联件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	// 租约过期：runs 行仍 running，但 lease_until 过去。
	if err := s.db.Exec(`UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = ?`, c.RunID).
		Error; err != nil {
		t.Fatal(err)
	}
	ts := &TaskScheduler{store: s}
	ts.detectInterruptedTasks(ctx)
	if got := s.count(t, &AgentNotification{}, "task_id = ? AND kind = ?", c.TaskID, notifKindTaskInterrupted); got != 1 {
		t.Fatalf("中断通知 = %d, want 1", got)
	}
	ts.detectInterruptedTasks(ctx)
	if got := s.count(t, &AgentNotification{}, "task_id = ? AND kind = ?", c.TaskID, notifKindTaskInterrupted); got != 1 {
		t.Fatalf("重复检测后通知 = %d, want 1（dedup）", got)
	}
	// 状态仍是 running：interrupted 不入库。
	if st, _ := taskStatus(t, s, c.TaskID); st != agentTaskStatusRunning {
		t.Fatalf("中断后存储状态 = %s, want running（读侧派生）", st)
	}
	// 读侧派生：视图翻 interrupted。
	view, err := s.GetAgentTask(ctx, testOwner, c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != agentTaskStatusInterrupted {
		t.Fatalf("视图状态 = %s, want interrupted", view.Status)
	}
}

// TestAgentTaskResumeNotifiesInterrupted 钉住「接管即告知」（施工包 §9.1 的另一半，
// 2026-11-10 崩溃活体抓过：resume 在调度器 tick 之前完成时通知一条都没写）：
// 调度器检测与用户 resume 谁先到，被接管代的 task_interrupted 都恰好一条——
// resume 先到由接管事务补写，调度器先到则接管里的写入被 dedup 挡掉。
func TestAgentTaskResumeNotifiesInterrupted(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	expireLease := func(runID RunID) {
		t.Helper()
		if err := s.db.Exec(`UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = ?`, runID).
			Error; err != nil {
			t.Fatal(err)
		}
	}
	countNotif := func(taskID string) int64 {
		t.Helper()
		return s.count(t, &AgentNotification{}, "task_id = ? AND kind = ?", taskID, notifKindTaskInterrupted)
	}

	// 调度器先到：先写通知，resume 接管同一代——dedup 保证不重。
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "调度器先到的件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	expireLease(c.RunID)
	(&TaskScheduler{store: s}).detectInterruptedTasks(ctx)
	if got := countNotif(c.TaskID); got != 1 {
		t.Fatalf("调度器先写通知 = %d, want 1", got)
	}
	if _, err := s.ClaimResume(ctx, c.RunID, 3); err != nil {
		t.Fatal(err)
	}
	if got := countNotif(c.TaskID); got != 1 {
		t.Fatalf("resume 接管后通知 = %d, want 1（dedup）", got)
	}

	// resume 先到：调度器还没 tick，接管事务补写通知。
	resp2, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k2", WorkspaceID: wsID, SourceSessionID: sessID, Content: "resume 先到的件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c2 := claimOne(t, s, resp2.OrchestrationID)
	expireLease(c2.RunID)
	if _, err := s.ClaimResume(ctx, c2.RunID, 3); err != nil {
		t.Fatal(err)
	}
	if got := countNotif(c2.TaskID); got != 1 {
		t.Fatalf("resume 补写通知 = %d, want 1", got)
	}
	// 普通聊天 run 的续跑不产通知：走同一 ClaimResume，session 不是 agent_task。
	chatRun := "r_chat_" + c2.TaskID[4:12]
	if err := s.db.Exec(`INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, created_at)
		VALUES (?, ?, 'running', 1, now() - interval '1 second', now())`, chatRun, sessID).Error; err != nil {
		t.Fatal(err)
	}
	before := s.count(t, &AgentNotification{}, "kind = ?", notifKindTaskInterrupted)
	if _, err := s.ClaimResume(ctx, RunID(chatRun), 3); err != nil {
		t.Fatal(err)
	}
	if after := s.count(t, &AgentNotification{}, "kind = ?", notifKindTaskInterrupted); after != before {
		t.Fatalf("普通聊天续跑多了通知：%d → %d", before, after)
	}
}

// TestDropDeliveryNoTools 钉住协调 run 的工具面（doc/70 §6.3 用到交付角色）：
// 2026-11-10 崩溃活体抓过——task_interrupted 交付里模型顺手 ask_user「你希望我怎么处理？」，
// run 挂起等人、来源会话一直忙，orchestration_finished 永远认领不了。交付必须确定性终结：
// 等人与再编排五件不进交付池，普通工具原样保留。
func TestDropDeliveryNoTools(t *testing.T) {
	in := []tool.BaseTool{
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "ask_user"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "enter_plan_mode"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "submit_plan"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "delegate_task"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "create_orchestration"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "read_file"}},
	}
	out := dropDeliveryNoTools(context.Background(), in)
	if len(out) != 1 {
		t.Fatalf("交付池剩 %d 件, want 1（只留 read_file）", len(out))
	}
	info, err := out[0].Info(context.Background())
	if err != nil || info.Name != "read_file" {
		t.Fatalf("留下的工具 = %v/%v, want read_file", info, err)
	}
}

// TestAgentTaskInterruptedDeliveryConsumed 钉住单任务事件交付的收口（2026-11-10 活体抓过：
// ReconcileDeliveryRun 只按组的 delivery_run_id 反查，task_interrupted 的交付没写它，
// 协调 run complete 后通知永远停在 claimed）：complete 即 consumed 且幂等；
// error 的通知留 claimed——run 走显式恢复，组交付状态不动。
func TestAgentTaskInterruptedDeliveryConsumed(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k1", WorkspaceID: wsID, SourceSessionID: sessID, Content: "中断交付的件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, resp.OrchestrationID)
	mkNotif := func(key string) string {
		t.Helper()
		if err := s.insertNotificationTx(s.db, AgentNotification{
			TenantID: testOwner.TenantID, OrchestrationID: resp.OrchestrationID, TaskID: c.TaskID,
			ReceiverSessionID: sessID,
			Kind:              notifKindTaskInterrupted, DedupKey: "task_interrupted:t:" + key,
			Status: notifStatusPending,
		}); err != nil {
			t.Fatal(err)
		}
		var n AgentNotification
		if err := s.db.Where("dedup_key = ?", "task_interrupted:t:"+key).Take(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n.ID
	}

	// complete 的交付 run：认领 → 翻 run 终态 → 对账收 consumed，重复对账幂等。
	n1 := mkNotif("c1")
	ts := &TaskScheduler{store: s}
	l1 := ts.claimNotification(ctx, n1)
	if l1 == nil {
		t.Fatalf("认领失败（来源会话应空闲）")
	}
	setRunStatus(t, s, l1.RunID, "complete")
	if err := s.ReconcileDeliveryRun(ctx, l1.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := notifStatus(t, s, n1); st != notifStatusConsumed {
		t.Fatalf("complete 后通知 = %s, want consumed", st)
	}
	if err := s.ReconcileDeliveryRun(ctx, l1.RunID); err != nil {
		t.Fatal(err)
	}
	if got := s.count(t, &AgentNotification{}, "id = ?", n1); got != 1 {
		t.Fatalf("重复对账多出了行 = %d, want 1", got)
	}

	// error 的交付 run：通知留 claimed（run 显式恢复后收口），组交付状态不动。
	n2 := mkNotif("e1")
	l2 := ts.claimNotification(ctx, n2)
	if l2 == nil {
		t.Fatalf("第二条认领失败")
	}
	setRunStatus(t, s, l2.RunID, "error")
	if err := s.ReconcileDeliveryRun(ctx, l2.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := notifStatus(t, s, n2); st != notifStatusClaimed {
		t.Fatalf("error 后通知 = %s, want claimed（留给显式恢复）", st)
	}
}
