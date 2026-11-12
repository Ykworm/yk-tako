// 拍6 确定性测试（施工包 §12.1 第 8 组）：artifact 与 retry-publication 的权限、状态与存储语义。
// 真 PG/Redis + 临时作业根（directoryProvider 全链），不打模型；blocked 状态走真实发布链造出
// （项目前进 → ReconcileRun 快路径发布受阻），不手摆发布状态冒充。
package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedIsolatedSubmitted 造一个 isolated 单件组并推进到「结果已提交 rev1、run 仍 running」：
// 项目基线 report.md=原始 → 创建 → 领取 → 准备工作现场 → 改成 结果v1 → 提交。
// 调用方随后决定 run 终态与项目是否前进（决定发布成功还是受阻）。
func seedIsolatedSubmitted(t *testing.T, s *Store, agentName, key string) (projDir string, c agentTaskClaim, work TaskWork) {
	t.Helper()
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, agentName)
	root := t.TempDir()
	s.SetWorkspaceRoot(root)
	projDir, err := workspaceDir(root, testOwner.TenantID, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "report.md"), []byte("原始"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: key, WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "改报告", Agent: agentName, WorkspaceMode: "isolated",
	}, planEnv{})
	if err != nil {
		t.Fatalf("创建 isolated 单件: %v", err)
	}
	c = claimOne(t, s, resp.OrchestrationID)
	work, _, err = s.prepareTaskWork(ctx, c.TaskID)
	if err != nil {
		t.Fatalf("prepareTaskWork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work.Root, "report.md"), []byte("结果v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev, err := s.SubmitTaskResult(ctx, c.TaskID, c.RunID, c.Epoch,
		TaskResult{Summary: "完成", Artifacts: []TaskArtifact{{Path: "report.md", Kind: "report"}}}, work.Root)
	if err != nil || rev != 1 {
		t.Fatalf("提交 rev = %d, %v; want 1, nil", rev, err)
	}
	return projDir, c, work
}

// blockPublication 把已提交的 isolated 单件推进到真实 blocked：项目 report.md 被人前进 →
// run complete → ReconcileRun 快路径发布 → workspace_changed。返回组 ID。
func blockPublication(t *testing.T, s *Store, projDir string, c agentTaskClaim) string {
	t.Helper()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(projDir, "report.md"), []byte("人改"), 0o644); err != nil {
		t.Fatal(err)
	}
	setRunStatus(t, s, c.RunID, "complete")
	if err := s.ReconcileRun(ctx, c.RunID); err != nil {
		t.Fatalf("ReconcileRun: %v", err)
	}
	var task AgentTask
	if err := s.db.Where("id = ?", c.TaskID).Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != agentTaskStatusBlocked || task.ErrorCode != taskErrWorkspaceChanged {
		t.Fatalf("发布受阻后 task = %s/%s, want blocked/workspace_changed", task.Status, task.ErrorCode)
	}
	if task.ErrorMessage == "" {
		t.Fatal("blocked 应带 Provider 恢复说明（error_message）")
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", c.OrchestrationID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if o.PublicationStatus != publicationStatusBlocked || o.PublicationEpoch != 1 {
		t.Fatalf("组发布状态 = %s epoch %d, want blocked/1", o.PublicationStatus, o.PublicationEpoch)
	}
	// 受阻收尾必须清掉项目发布标记（否则同项目新 run 永久 409）。
	var mark string
	if err := s.db.Raw(`SELECT publication_orchestration_id FROM workspaces WHERE id = ?`, o.WorkspaceID).
		Scan(&mark).Error; err != nil {
		t.Fatal(err)
	}
	if mark != "" {
		t.Fatalf("受阻后发布标记未清：%q", mark)
	}
	return o.ID
}

func TestTaskArtifactRead(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	projDir, c, _ := seedIsolatedSubmitted(t, s, "researcher", "k1")

	// 成功：逐字命中清单，读回不可变结果字节。
	b, err := s.TaskArtifact(ctx, testOwner, c.TaskID, "report.md")
	if err != nil || string(b) != "结果v1" {
		t.Fatalf("TaskArtifact = %q, %v; want 结果v1", b, err)
	}
	// 逐字匹配：不做 clean 后再匹配，清单外路径一律拒绝。
	for _, p := range []string{"./report.md", "report.md ", "../report.md", "/etc/passwd", "missing.md", ""} {
		if _, err := s.TaskArtifact(ctx, testOwner, c.TaskID, p); !errors.Is(err, errArtifactUnreadable) {
			t.Fatalf("path %q err = %v, want errArtifactUnreadable", p, err)
		}
	}
	// 属主链：非本人、跨租户、未知 id 同形 errTaskNotFound。
	for _, p := range []principal{
		{UserID: "u_other", TenantID: testOwner.TenantID, Role: "owner"},
		{UserID: testOwner.UserID, TenantID: "t_other", Role: "owner"},
	} {
		if _, err := s.TaskArtifact(ctx, p, c.TaskID, "report.md"); !errors.Is(err, errTaskNotFound) {
			t.Fatalf("principal %+v err = %v, want errTaskNotFound", p, err)
		}
	}
	if _, err := s.TaskArtifact(ctx, testOwner, "at_missing", "report.md"); !errors.Is(err, errTaskNotFound) {
		t.Fatalf("未知 id err = %v, want errTaskNotFound", err)
	}

	// 发布成功后读面仍是不可变结果，不用主项目同路径文件代替。
	setRunStatus(t, s, c.RunID, "complete")
	if err := s.ReconcileRun(ctx, c.RunID); err != nil {
		t.Fatal(err)
	}
	if st, _ := taskStatus(t, s, c.TaskID); st != agentTaskStatusSucceeded {
		t.Fatalf("未前进项目应发布成功，task = %s", st)
	}
	if err := os.WriteFile(filepath.Join(projDir, "report.md"), []byte("项目当前版"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err = s.TaskArtifact(ctx, testOwner, c.TaskID, "report.md")
	if err != nil || string(b) != "结果v1" {
		t.Fatalf("发布后读面被项目文件顶替：%q, %v", b, err)
	}
}

func TestTaskArtifactSharedAndLost(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()

	// shared 组：即使已提交结果也没有隔离产物。
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k-shared", WorkspaceID: wsID, SourceSessionID: sessID,
		Content: "shared 件", Agent: "researcher",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	cs := claimOne(t, s, resp.OrchestrationID)
	submitOK(t, s, cs.TaskID, cs.RunID, cs.Epoch)
	if _, err := s.TaskArtifact(ctx, testOwner, cs.TaskID, "a.txt"); !errors.Is(err, errArtifactShared) {
		t.Fatalf("shared err = %v, want errArtifactShared", err)
	}

	// isolated 尚未提交：结果引用缺失（同项目另造一件只领取不提交的）。
	_, ci, _ := seedIsolatedSubmitted(t, s, "researcher-iso", "k-iso")
	var fresh AgentTask
	if err := s.db.Where("id = ?", ci.TaskID).Take(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	resp2, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k-noresult", WorkspaceID: fresh.WorkspaceID, SourceSessionID: sessIDOf(t, s, fresh),
		Content: "未提交件", Agent: "researcher-iso", WorkspaceMode: "isolated",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	cn := claimOne(t, s, resp2.OrchestrationID)
	if _, err := s.TaskArtifact(ctx, testOwner, cn.TaskID, "report.md"); !errors.Is(err, errArtifactUnreadable) {
		t.Fatalf("未提交 err = %v, want errArtifactUnreadable", err)
	}

	// 不可变结果目录丢失（盘上事实没了）：PG 还指着 revision 也读不出。
	var o AgentOrchestration
	if err := s.db.Where("id = ?", ci.OrchestrationID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(g.resultDir(ci.TaskID, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TaskArtifact(ctx, testOwner, ci.TaskID, "report.md"); !errors.Is(err, errArtifactUnreadable) {
		t.Fatalf("结果丢失 err = %v, want errArtifactUnreadable", err)
	}
}

// sessIDOf 读一件 Task 的来源会话（创建第二件同项目任务时用）。
func sessIDOf(t *testing.T, s *Store, task AgentTask) SessionID {
	t.Helper()
	var o AgentOrchestration
	if err := s.db.Where("id = ?", task.OrchestrationID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	return o.SourceSessionID
}

func TestRetryTaskPublicationFullLoop(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	projDir, c, work := seedIsolatedSubmitted(t, s, "researcher", "k1")
	orchID := blockPublication(t, s, projDir, c)

	var before AgentTask
	if err := s.db.Where("id = ?", c.TaskID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	sessID := SessionID(*before.ExecutionSessionID)
	recoveryNote := before.ErrorMessage

	// 202 半：新建处理 run，Task 回 running，组发布状态复位。
	view, claim, err := s.RetryTaskPublication(ctx, testOwner, c.TaskID)
	if err != nil {
		t.Fatalf("RetryTaskPublication: %v", err)
	}
	if claim == nil {
		t.Fatal("首次 retry 应创建处理 run（claim 非空）")
	}
	if view.TaskID != c.TaskID || view.RunID != claim.RunID || view.Status != agentTaskStatusRunning || view.PublicationEpoch != 2 {
		t.Fatalf("view = %+v, want taskId/runId 一致、running、epoch 2", view)
	}
	var after AgentTask
	if err := s.db.Where("id = ?", c.TaskID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != agentTaskStatusRunning || after.ErrorCode != "" || after.ErrorMessage != "" || after.FinishedAt != nil {
		t.Fatalf("retry 后 task = %+v, want running 且清发布错误", after)
	}
	if after.CurrentRunID == nil || *after.CurrentRunID != string(claim.RunID) {
		t.Fatalf("currentRunId 未指向新 run：%v", after.CurrentRunID)
	}
	if after.ExecutionSessionID == nil || *after.ExecutionSessionID != string(sessID) {
		t.Fatal("执行 session 被换了")
	}
	if after.ResultRevision != 1 {
		t.Fatalf("历史结果版本被改：%d", after.ResultRevision)
	}
	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if o.Status != orchStatusActive || o.PublicationStatus != "" || o.PublicationErrorCode != "" {
		t.Fatalf("组状态 = %s/%s/%s, want active/''/''", o.Status, o.PublicationStatus, o.PublicationErrorCode)
	}
	if o.PublicationEpoch != 1 {
		t.Fatalf("准入前组代次不应递增：%d", o.PublicationEpoch)
	}

	// 新 run：同一执行 session、lease_epoch 从 1 起、旧 run 保留。
	var run struct {
		SessionID string
		Status    string
		Epoch     int64 `gorm:"column:lease_epoch"`
	}
	if err := s.db.Raw(`SELECT session_id, status, lease_epoch FROM runs WHERE id = ?`, claim.RunID).
		Scan(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.SessionID != string(sessID) || run.Status != "running" || run.Epoch != 1 {
		t.Fatalf("新 run = %+v, want 同 session/running/epoch 1", run)
	}
	var oldStatus string
	if err := s.db.Raw(`SELECT status FROM runs WHERE id = ?`, c.RunID).Scan(&oldStatus).Error; err != nil {
		t.Fatal(err)
	}
	if oldStatus != "complete" {
		t.Fatalf("旧 run 被改动：%s", oldStatus)
	}
	// 首条输入：目标 + 当前版本 + 通用原因 + Provider 恢复说明 + 重新提交约定。
	var input string
	if err := s.db.Raw(`SELECT text FROM run_model_messages WHERE run_id = ? AND seq = 1`, claim.RunID).
		Scan(&input).Error; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"重新发布", "resultRevision: 1", taskErrWorkspaceChanged, recoveryNote, "submit_task_result"} {
		if !strings.Contains(input, want) {
			t.Fatalf("处理 run 输入缺 %q：%s", want, input)
		}
	}
	// runlog：user 行、internal=false（执行指令是正常内容），旧账本行不动。
	var logRow struct {
		Role     string
		Internal bool
		Text     string
	}
	if err := s.db.Raw(`SELECT role, internal, text FROM runlog_entries WHERE run_id = ?`, claim.RunID).
		Scan(&logRow).Error; err != nil {
		t.Fatal(err)
	}
	if logRow.Role != "user" || logRow.Internal || logRow.Text != input {
		t.Fatalf("runlog 行 = %+v, want user/false/同输入", logRow)
	}
	var oldLogs int64
	if err := s.db.Raw(`SELECT COUNT(*) FROM runlog_entries WHERE run_id = ?`, c.RunID).Scan(&oldLogs).Error; err != nil {
		t.Fatal(err)
	}
	if oldLogs == 0 {
		t.Fatal("旧 run 的 runlog 被清了")
	}

	// 双击：已有当前处理 run 活着 → 回现状，不新建。
	view2, claim2, err := s.RetryTaskPublication(ctx, testOwner, c.TaskID)
	if err != nil {
		t.Fatalf("幂等 retry: %v", err)
	}
	if claim2 != nil || view2.RunID != view.RunID || view2.PublicationEpoch != 2 {
		t.Fatalf("双击 = %+v claim=%v, want 原 run/epoch 2/不新建", view2, claim2)
	}
	var runCount int64
	if err := s.db.Raw(`SELECT COUNT(*) FROM runs WHERE session_id = ?`, sessID).Scan(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 2 {
		t.Fatalf("session 里 run 数 = %d, want 2（旧 + 处理）", runCount)
	}

	// 执行者处理障碍（把项目恢复到可发布现场）后重新提交 → 新结果走既有发布链成功。
	if err := os.WriteFile(filepath.Join(projDir, "report.md"), []byte("原始"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev, err := s.SubmitTaskResult(ctx, c.TaskID, claim.RunID, claim.Epoch,
		TaskResult{Summary: "冲突已裁决", Artifacts: []TaskArtifact{{Path: "report.md", Kind: "report"}}}, work.Root)
	if err != nil || rev != 2 {
		t.Fatalf("处理 run 重提 rev = %d, %v; want 2", rev, err)
	}
	setRunStatus(t, s, claim.RunID, "complete")
	if err := s.ReconcileRun(ctx, claim.RunID); err != nil {
		t.Fatal(err)
	}
	if st, code := taskStatus(t, s, c.TaskID); st != agentTaskStatusSucceeded || code != "" {
		t.Fatalf("重新发布后 task = %s/%s, want succeeded", st, code)
	}
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	if o.PublicationStatus != publicationStatusPublished || o.PublicationEpoch != 2 {
		t.Fatalf("重新发布后组 = %s epoch %d, want published/2", o.PublicationStatus, o.PublicationEpoch)
	}
	if b, _ := os.ReadFile(filepath.Join(projDir, "report.md")); string(b) != "结果v1" {
		t.Fatalf("项目未写成结果：%q", b)
	}
	// 发布成功后再点 retry：状态已收口，拒绝。
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, c.TaskID); !errors.Is(err, errTaskNotRepublishable) {
		t.Fatalf("published 后 retry err = %v, want errTaskNotRepublishable", err)
	}
}

func TestRetryTaskPublicationRejections(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()

	// 非最终 Task：两层图里领取 A（B 才是最终）。
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	root := t.TempDir()
	s.SetWorkspaceRoot(root)
	cmd := mkTwoLayerCmd(wsID, sessID, "k-2layer")
	cmd.WorkspaceMode = "isolated"
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, cmd, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	claims := claimAll(t, s, resp.OrchestrationID)
	ca := pickClaim(t, claims, "A")
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, ca.TaskID); !errors.Is(err, errTaskNotRepublishable) {
		t.Fatalf("非最终 err = %v, want errTaskNotRepublishable", err)
	}

	// blocked 但错误码是依赖语义（不是发布三码）。
	_, cd, _ := seedIsolatedSubmitted(t, s, "researcher-dep", "k-dep")
	if err := s.db.Exec(`UPDATE agent_tasks SET status = ?, error_code = ? WHERE id = ?`,
		agentTaskStatusBlocked, taskErrDependencyFailed, cd.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, cd.TaskID); !errors.Is(err, errTaskNotRepublishable) {
		t.Fatalf("依赖 blocked err = %v, want errTaskNotRepublishable", err)
	}

	// running 但当前 run 已死（interrupted 语义）：走 resume 不走 retry。
	_, cr, _ := seedIsolatedSubmitted(t, s, "researcher-dead", "k-dead")
	setRunStatus(t, s, cr.RunID, "complete")
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, cr.TaskID); !errors.Is(err, errTaskNotRepublishable) {
		t.Fatalf("死 run err = %v, want errTaskNotRepublishable", err)
	}

	// shared 组。
	wsID2, sessID2 := seedTaskEnv(t, s, "researcher-shared")
	resp2, err := s.CreateAgentTask(ctx, testOwner, CreateTaskCommand{
		IdempotencyKey: "k-shared", WorkspaceID: wsID2, SourceSessionID: sessID2,
		Content: "shared 件", Agent: "researcher-shared",
	}, planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	csh := claimOne(t, s, resp2.OrchestrationID)
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, csh.TaskID); !errors.Is(err, errTaskNotRepublishable) {
		t.Fatalf("shared err = %v, want errTaskNotRepublishable", err)
	}

	// 属主链：非本人与未知 id 同形。
	for _, p := range []principal{
		{UserID: "u_other", TenantID: testOwner.TenantID, Role: "owner"},
		{UserID: testOwner.UserID, TenantID: "t_other", Role: "owner"},
	} {
		if _, _, err := s.RetryTaskPublication(ctx, p, cd.TaskID); !errors.Is(err, errTaskNotFound) {
			t.Fatalf("principal %+v err = %v, want errTaskNotFound", p, err)
		}
	}
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, "at_missing"); !errors.Is(err, errTaskNotFound) {
		t.Fatalf("未知 id err = %v, want errTaskNotFound", err)
	}
}

func TestRetryTaskPublicationResultLost(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	projDir, c, _ := seedIsolatedSubmitted(t, s, "researcher", "k1")
	blockPublication(t, s, projDir, c)

	var o AgentOrchestration
	if err := s.db.Where("id = ?", c.OrchestrationID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(g.resultDir(c.TaskID, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RetryTaskPublication(ctx, testOwner, c.TaskID); !errors.Is(err, errArtifactUnreadable) {
		t.Fatalf("结果丢失 err = %v, want errArtifactUnreadable", err)
	}
	// 拒绝后状态不动：仍是 blocked，没有铸出处理 run。
	if st, code := taskStatus(t, s, c.TaskID); st != agentTaskStatusBlocked || code != taskErrWorkspaceChanged {
		t.Fatalf("拒绝后 task = %s/%s, want blocked/workspace_changed", st, code)
	}
}