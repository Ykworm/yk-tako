// 继任编排组的存储半测试（施工包 §10.1）：真 PG（5433 隔离临时库）+ 真 Redis + 真文件系统。
//
// 覆盖面：参数与目标可行性校验、原组终态才激活／未终态先等待、同原组只有一个继任组、
// 幂等重打与不同方案冲突、重规划链深度上限、复用条目的链内校验与快照复制、
// 跨项目复用、等待中的继任组可被停止、对账补齐标记与快照并推进状态、未迁移项目拒绝。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setOrchStatus(t *testing.T, s *Store, id, status string) {
	t.Helper()
	if err := s.db.Exec(`UPDATE agent_orchestrations SET status = ? WHERE id = ?`, status, id).Error; err != nil {
		t.Fatalf("set orchestration %s = %s: %v", id, status, err)
	}
}

func setTaskStatus(t *testing.T, s *Store, id, status string) {
	t.Helper()
	if err := s.db.Exec(`UPDATE agent_tasks SET status = ? WHERE id = ?`, status, id).Error; err != nil {
		t.Fatalf("set task %s = %s: %v", id, status, err)
	}
}

func orchestrationRow(t *testing.T, s *Store, id string) AgentOrchestration {
	t.Helper()
	var rows []AgentOrchestration
	if err := s.db.Where("id = ?", id).Find(&rows).Error; err != nil {
		t.Fatalf("read orchestration %s: %v", id, err)
	}
	if len(rows) != 1 {
		t.Fatalf("orchestration %s 行数 = %d", id, len(rows))
	}
	return rows[0]
}

// successorBaseCmd 是一份合法的最小继任方案：一个单节点新目标。
func successorBaseCmd(key string) SuccessorCommand {
	return SuccessorCommand{
		IdempotencyKey:  key,
		GoalAssessment:  GoalAssessment{Outcome: successorOutcomeFeasible, Evidence: "本地数据覆盖最近 30 天"},
		Reason:          "原目标依赖的数据源已下线",
		SuccessorReason: "改用本地数据继续",
		Goal:            "用本地数据重做汇总",
		Tasks:           []planTask{{Key: "D", Content: "整理本地数据", Agent: "researcher"}},
	}
}

// seedTerminalOriginal 建一个已成功的原组（两层图），返回项目、来源会话与原组 ID。
func seedTerminalOriginal(t *testing.T, s *Store, key string) (string, SessionID, string) {
	t.Helper()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp, err := s.CreateAgentOrchestration(context.Background(), testOwner, mkTwoLayerCmd(wsID, sessID, key), planEnv{})
	if err != nil {
		t.Fatalf("create original: %v", err)
	}
	if err := s.db.Exec(`UPDATE agent_tasks SET status = ? WHERE orchestration_id = ?`,
		agentTaskStatusSucceeded, resp.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	setOrchStatus(t, s, resp.OrchestrationID, orchStatusSucceeded)
	return wsID, sessID, resp.OrchestrationID
}

func TestNormalizeSuccessorCommand(t *testing.T) {
	base := successorBaseCmd("k")

	for name, mutate := range map[string]func(*SuccessorCommand){
		"目标不可行":          func(c *SuccessorCommand) { c.GoalAssessment.Outcome = "infeasible" },
		"可行性判定留空":        func(c *SuccessorCommand) { c.GoalAssessment.Outcome = "" },
		"缺可行性证据":         func(c *SuccessorCommand) { c.GoalAssessment.Evidence = "  " },
		"缺重规划原因":         func(c *SuccessorCommand) { c.Reason = "" },
		"缺创建继任组原因":       func(c *SuccessorCommand) { c.SuccessorReason = "" },
		"不得重放动作超过上限":     func(c *SuccessorCommand) { c.NoReplay = make([]string, successorNoReplayMax+1) },
		"不得重放动作有空行":      func(c *SuccessorCommand) { c.NoReplay = []string{"a", " "} },
		"复用条目缺 revision": func(c *SuccessorCommand) { c.Reuse = []SuccessorReuseRef{{TaskID: "at_a"}} },
		"复用条目缺 taskId":   func(c *SuccessorCommand) { c.Reuse = []SuccessorReuseRef{{ResultRevision: 1}} },
		"复用超过上限": func(c *SuccessorCommand) {
			for i := 0; i <= successorReuseMax; i++ {
				c.Reuse = append(c.Reuse, SuccessorReuseRef{TaskID: fmt.Sprintf("at_%d", i), ResultRevision: 1})
			}
		},
	} {
		cmd := base
		mutate(&cmd)
		if _, _, _, _, _, err := normalizeSuccessorCommand(cmd); err == nil {
			t.Fatalf("%s：应报错", name)
		} else {
			var se *successorError
			if !errors.As(err, &se) {
				t.Fatalf("%s：错误类型 = %T，want *successorError", name, err)
			}
		}
	}

	// 去重 + 稳定排序：同一命令重打必须得到同一 plan hash。
	cmd := base
	cmd.NoReplay = []string{"b", "a", "a"}
	cmd.Reuse = []SuccessorReuseRef{{TaskID: "at_b", ResultRevision: 2}, {TaskID: "at_a", ResultRevision: 1}, {TaskID: "at_a", ResultRevision: 1}}
	_, reason, succReason, noReplay, reuse, err := normalizeSuccessorCommand(cmd)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if reason != cmd.Reason || succReason != cmd.SuccessorReason {
		t.Fatalf("原因被改写：%q / %q", reason, succReason)
	}
	if len(noReplay) != 2 || noReplay[0] != "a" || noReplay[1] != "b" {
		t.Fatalf("noReplay = %v", noReplay)
	}
	if len(reuse) != 2 || reuse[0].TaskID != "at_a" || reuse[1].TaskID != "at_b" {
		t.Fatalf("reuse = %+v", reuse)
	}
}

func TestSuccessorVisibilityAndSourceStatus(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	_, _, origID := seedTerminalOriginal(t, s, "o1")

	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, "or_ghost", successorBaseCmd("k"), planEnv{}); !errors.Is(err, errOrchNotFound) {
		t.Fatalf("未知原组 err = %v, want errOrchNotFound", err)
	}
	other := principal{UserID: "u_other", TenantID: testOwner.TenantID, Role: "owner"}
	if _, err := s.CreateSuccessorOrchestration(ctx, other, origID, successorBaseCmd("k"), planEnv{}); !errors.Is(err, errOrchNotFound) {
		t.Fatalf("非本人原组 err = %v, want errOrchNotFound", err)
	}
	// 原组自身处于 preparing／waiting_previous 时拒绝再重规划。
	setOrchStatus(t, s, origID, orchStatusPreparing)
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, successorBaseCmd("k"), planEnv{}); !errors.Is(err, errSuccessorRefused) {
		t.Fatalf("preparing 原组 err = %v, want errSuccessorRefused", err)
	}
	setOrchStatus(t, s, origID, orchStatusWaitingPrevious)
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, successorBaseCmd("k"), planEnv{}); !errors.Is(err, errSuccessorRefused) {
		t.Fatalf("waiting_previous 原组 err = %v, want errSuccessorRefused", err)
	}

	// 目标项目与工作区配置的错误在写文件之前拦下。
	setOrchStatus(t, s, origID, orchStatusSucceeded)
	badWS := successorBaseCmd("k")
	badWS.WorkspaceID = "ws_ghost"
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, badWS, planEnv{}); !errors.Is(err, errCreateWorkspace) {
		t.Fatalf("未知目标项目 err = %v, want errCreateWorkspace", err)
	}
	badMode := successorBaseCmd("k")
	badMode.WorkspaceMode = "tmux"
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, badMode, planEnv{}); err == nil {
		t.Fatal("非法 workspaceMode 应报错")
	} else if _, ok := err.(*successorError); !ok {
		t.Fatalf("非法 workspaceMode 应翻成 successorError，得 %T", err)
	}
}

func TestSuccessorActiveWhenOriginalTerminal(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")

	// 原组用 isolated + verifyCommand，验证继任组默认继承。
	origCmd := mkTwoLayerCmd(wsID, sessID, "o1")
	origCmd.WorkspaceMode = workspaceModeIsolated
	origCmd.VerifyCommand = "make test"
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, origCmd, planEnv{})
	if err != nil {
		t.Fatalf("create original: %v", err)
	}
	if err := s.db.Exec(`UPDATE agent_tasks SET status = ? WHERE orchestration_id = ?`,
		agentTaskStatusSucceeded, resp0.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	setOrchStatus(t, s, resp0.OrchestrationID, orchStatusSucceeded)

	cmd := successorBaseCmd("k1")
	cmd.Tasks = []planTask{
		{Key: "D", Content: "整理本地数据", Agent: "researcher"},
		{Key: "E", Content: "汇总成报告", Agent: "researcher"},
	}
	cmd.Links = []planLink{{Parent: "D", Child: "E"}}
	out, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("CreateSuccessorOrchestration: %v", err)
	}
	if out.Status != orchStatusActive || out.ReplanOf != resp0.OrchestrationID || out.ReplanDepth != 1 {
		t.Fatalf("响应 = %+v", out)
	}

	succ := orchestrationRow(t, s, out.OrchestrationID)
	if succ.Status != orchStatusActive || succ.WorkspaceID != wsID ||
		succ.WorkspaceMode != workspaceModeIsolated || succ.VerifyCommand != "make test" {
		t.Fatalf("继任组行 = %+v", succ)
	}
	if succ.IdempotencyKey != successorKeyPrefix+resp0.OrchestrationID+":k1" {
		t.Fatalf("规范化幂等键 = %q", succ.IdempotencyKey)
	}

	// 出生时全部 pending，激活那一刻才把无父节点提成 ready。
	d, err := s.taskByKey(ctx, testOwner, succ.ID, "D")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.taskByKey(ctx, testOwner, succ.ID, "E")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != agentTaskStatusReady || e.Status != agentTaskStatusPending {
		t.Fatalf("节点状态 D=%s E=%s, want ready/pending", d.Status, e.Status)
	}

	// handoff 与标记落在两侧容器里；详情与 handoff 读面都能取到重规划关系。
	succContainer, err := s.groupContainerDir(succ)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := readHandoff(succContainer, succ.ID)
	if err != nil || doc == nil {
		t.Fatalf("读继任组 handoff: %v / %v", doc, err)
	}
	if doc.ReplanOf != resp0.OrchestrationID || doc.ReplanDepth != 1 || len(doc.Reuse) != 0 {
		t.Fatalf("handoff frontmatter = %+v", doc)
	}
	origContainer, err := s.groupContainerDir(orchestrationRow(t, s, resp0.OrchestrationID))
	if err != nil {
		t.Fatal(err)
	}
	mark, err := readSuccessorMark(origContainer)
	if err != nil || mark == nil {
		t.Fatalf("读 successor.json: %+v / %v", mark, err)
	}
	if mark.SuccessorOrchestrationID != succ.ID || mark.CreationKey != succ.IdempotencyKey {
		t.Fatalf("标记 = %+v", mark)
	}

	view, err := s.GetOrchestrationHandoff(ctx, testOwner, succ.ID)
	if err != nil {
		t.Fatalf("GetOrchestrationHandoff: %v", err)
	}
	if view.ReplanOf != resp0.OrchestrationID || view.ReplanDepth != 1 || view.Reuse == nil || len(view.Reuse) != 0 {
		t.Fatalf("handoff view = %+v", view)
	}
	for _, h := range []string{"# 为什么重规划", "# 为什么创建继任组", "# 目标可行性证据", "# 哪些结果仍有效", "# 不得重放的动作"} {
		if !strings.Contains(view.Body, h) {
			t.Fatalf("handoff 正文缺 %q", h)
		}
	}
	detail, err := s.GetAgentOrchestration(ctx, testOwner, succ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Orchestration.ReplanOf != resp0.OrchestrationID || detail.Orchestration.ReplanDepth != 1 {
		t.Fatalf("详情重规划字段 = %+v", detail.Orchestration)
	}
	if got := s.count(t, &AgentOrchestration{}, "idempotency_key LIKE ?",
		successorKeyPrefix+resp0.OrchestrationID+":%"); got != 1 {
		t.Fatalf("继任组行数 = %d, want 1", got)
	}
}

func TestSuccessorWaitingPreviousThenActivate(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	claims := claimAll(t, s, resp0.OrchestrationID)
	if len(claims) == 0 {
		t.Fatal("原组应有可领取的 ready Task")
	}

	cmd := successorBaseCmd("k1")
	out, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("CreateSuccessorOrchestration: %v", err)
	}
	if out.Status != orchStatusWaitingPrevious {
		t.Fatalf("原组还在跑时继任组应 waiting_previous，得 %s", out.Status)
	}
	if got := orchestrationRow(t, s, resp0.OrchestrationID).Status; got != orchStatusCancelling {
		t.Fatalf("原组状态 = %s, want cancelling（在途 run 未收尾）", got)
	}
	// 原组的未开始节点已被取消；running 节点等 run 收尾。
	for _, c := range claims {
		if st, _ := taskStatus(t, s, c.TaskID); st != agentTaskStatusRunning {
			t.Fatalf("在途 Task %s = %s, want running", c.TaskID, st)
		}
	}
	d, err := s.taskByKey(ctx, testOwner, out.OrchestrationID, "D")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != agentTaskStatusPending {
		t.Fatalf("原组未终态时继任节点应 pending，得 %s", d.Status)
	}

	// 原组收尾后再打同一个命令：幂等回读，推进到 active，不建第二组。
	if err := s.db.Exec(`UPDATE agent_tasks SET status = ? WHERE orchestration_id = ?`,
		agentTaskStatusCancelled, resp0.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	setOrchStatus(t, s, resp0.OrchestrationID, orchStatusCancelled)
	out2, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("幂等重打: %v", err)
	}
	if out2.OrchestrationID != out.OrchestrationID || out2.Status != orchStatusActive {
		t.Fatalf("重打结果 = %+v，want 同一组 active", out2)
	}
	d, err = s.taskByKey(ctx, testOwner, out.OrchestrationID, "D")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != agentTaskStatusReady {
		t.Fatalf("激活后无父节点应 ready，得 %s", d.Status)
	}
	if got := s.count(t, &AgentOrchestration{}, "idempotency_key LIKE ?",
		successorKeyPrefix+resp0.OrchestrationID+":%"); got != 1 {
		t.Fatalf("继任组行数 = %d, want 1", got)
	}
}

func TestSuccessorIdempotentAndUnique(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	_, _, origID := seedTerminalOriginal(t, s, "o1")
	cmd := successorBaseCmd("k1")

	first, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("首次创建: %v", err)
	}
	same, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("同键同方案重试: %v", err)
	}
	if same.OrchestrationID != first.OrchestrationID {
		t.Fatalf("同键重试建了新组：%s vs %s", first.OrchestrationID, same.OrchestrationID)
	}
	if got := s.count(t, &AgentOrchestration{}, "idempotency_key LIKE ?",
		successorKeyPrefix+origID+":%"); got != 1 {
		t.Fatalf("继任组行数 = %d, want 1", got)
	}

	changed := cmd
	changed.Goal = "换一个目标"
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, changed, planEnv{}); !errors.Is(err, errPlanConflict) {
		t.Fatalf("同键不同方案 err = %v, want errPlanConflict", err)
	}

	otherKey := successorBaseCmd("k2")
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, origID, otherKey, planEnv{}); !errors.Is(err, errSuccessorRefused) {
		t.Fatalf("第二个继任组 err = %v, want errSuccessorRefused", err)
	}
}

func TestSuccessorDepthLimit(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	_, _, origID := seedTerminalOriginal(t, s, "o1")
	cur := origID
	for i := 1; i <= successorMaxDepth; i++ {
		setOrchStatus(t, s, cur, orchStatusSucceeded)
		out, err := s.CreateSuccessorOrchestration(ctx, testOwner, cur, successorBaseCmd(fmt.Sprintf("k%d", i)), planEnv{})
		if err != nil {
			t.Fatalf("第 %d 层创建: %v", i, err)
		}
		if out.ReplanDepth != i {
			t.Fatalf("第 %d 层 replanDepth = %d", i, out.ReplanDepth)
		}
		cur = out.OrchestrationID
	}
	setOrchStatus(t, s, cur, orchStatusSucceeded)
	_, err := s.CreateSuccessorOrchestration(ctx, testOwner, cur, successorBaseCmd("k9"), planEnv{})
	var se *successorError
	if !errors.As(err, &se) || !strings.Contains(se.reason, "超过上限") {
		t.Fatalf("第 %d 层之后 err = %v, want 超过上限的 successorError", successorMaxDepth+1, err)
	}
}

// seedSucceededTask 领取并提交一件 Task 的结果，然后把存储状态置成 succeeded。
func seedSucceededTask(t *testing.T, s *Store, orchID, key string) AgentTask {
	t.Helper()
	claims := claimAll(t, s, orchID)
	for _, c := range claims {
		if c.TaskKey != key {
			continue
		}
		submitOK(t, s, c.TaskID, c.RunID, c.Epoch)
		setTaskStatus(t, s, c.TaskID, agentTaskStatusSucceeded)
		return taskByKeyT(t, s, orchID, key)
	}
	t.Fatalf("原组没有可领取的 Task %s（领取到 %v）", key, claims)
	return AgentTask{}
}

func taskByKeyT(t *testing.T, s *Store, orchID, key string) AgentTask {
	t.Helper()
	task, err := s.taskByKey(context.Background(), testOwner, orchID, key)
	if err != nil {
		t.Fatalf("taskByKey %s/%s: %v", orchID, key, err)
	}
	return task
}

func TestSuccessorReuseSnapshotAndValidation(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	taskA := seedSucceededTask(t, s, resp0.OrchestrationID, "A")
	taskC := taskByKeyT(t, s, resp0.OrchestrationID, "C")

	// 校验面：链内、已成功、版本精确——任何一条不满足都不写库。
	for name, refs := range map[string][]SuccessorReuseRef{
		"未成功的 Task":    {{TaskID: taskC.ID, ResultRevision: 1}},
		"版本不是当前版本":     {{TaskID: taskA.ID, ResultRevision: taskA.ResultRevision + 1}},
		"不属于这条链的 Task": {{TaskID: "at_ghost", ResultRevision: 1}},
	} {
		cmd := successorBaseCmd("bad-" + name)
		cmd.Reuse = refs
		if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, cmd, planEnv{}); err == nil {
			t.Fatalf("%s：应报错", name)
		} else if _, ok := err.(*successorError); !ok {
			t.Fatalf("%s：错误类型 = %T，want *successorError", name, err)
		}
	}
	if got := s.count(t, &AgentOrchestration{}, "idempotency_key LIKE ?",
		successorKeyPrefix+resp0.OrchestrationID+":%"); got != 0 {
		t.Fatalf("校验失败不该建行，现有 %d 行", got)
	}

	setOrchStatus(t, s, resp0.OrchestrationID, orchStatusSucceeded)
	cmd := successorBaseCmd("k1")
	cmd.Reuse = []SuccessorReuseRef{{TaskID: taskA.ID, ResultRevision: taskA.ResultRevision}}
	out, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("带复用创建: %v", err)
	}
	succ := orchestrationRow(t, s, out.OrchestrationID)
	succContainer, err := s.groupContainerDir(succ)
	if err != nil {
		t.Fatal(err)
	}
	// 快照复制进继任组自己的结果区（旧项目此后删除不影响它）。
	if _, err := readImmutableResult(&GroupHandle{Dir: filepath.Join(succContainer, "workspace")},
		ResultRef{TaskID: taskA.ID, ResultRevision: taskA.ResultRevision}); err != nil {
		t.Fatalf("继任组结果区缺复用副本：%v", err)
	}
	rf, err := readGroupReuse(succContainer)
	if err != nil || rf == nil || len(rf.Reuse) != 1 {
		t.Fatalf("reuse.json = %+v / %v", rf, err)
	}
	if rf.Reuse[0].TaskID != taskA.ID || rf.Reuse[0].ResultRevision != taskA.ResultRevision ||
		rf.Reuse[0].Provider == "" || rf.Reuse[0].SourceOrchestrationID != resp0.OrchestrationID {
		t.Fatalf("reuse 条目 = %+v", rf.Reuse[0])
	}
	doc, _, err := readHandoff(succContainer, succ.ID)
	if err != nil || doc == nil || len(doc.Reuse) != 1 || doc.Reuse[0].TaskID != taskA.ID {
		t.Fatalf("handoff reuse = %+v / %v", doc, err)
	}
	view, err := s.GetOrchestrationHandoff(ctx, testOwner, succ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Reuse) != 1 || view.Reuse[0].ResultRevision != taskA.ResultRevision {
		t.Fatalf("handoff view reuse = %+v", view.Reuse)
	}

	// 链外校验：另一个原组的 Task 不能出现在这条链的继任方案里。
	resp2, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o2"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	setOrchStatus(t, s, resp2.OrchestrationID, orchStatusSucceeded)
	cross := successorBaseCmd("k1")
	cross.Reuse = []SuccessorReuseRef{{TaskID: taskA.ID, ResultRevision: taskA.ResultRevision}}
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp2.OrchestrationID, cross, planEnv{}); err == nil ||
		!strings.Contains(err.Error(), "不属于原组或它的重规划链") {
		t.Fatalf("链外复用 err = %v", err)
	}
}

func TestSuccessorCrossProjectReuse(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	taskA := seedSucceededTask(t, s, resp0.OrchestrationID, "A")
	setOrchStatus(t, s, resp0.OrchestrationID, orchStatusSucceeded)

	ws2, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "继任项目")
	if err != nil {
		t.Fatal(err)
	}
	cmd := successorBaseCmd("k1")
	cmd.WorkspaceID = ws2.ID
	cmd.Reuse = []SuccessorReuseRef{{TaskID: taskA.ID, ResultRevision: taskA.ResultRevision}}
	out, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, cmd, planEnv{})
	if err != nil {
		t.Fatalf("跨项目创建: %v", err)
	}
	succ := orchestrationRow(t, s, out.OrchestrationID)
	if succ.WorkspaceID != ws2.ID {
		t.Fatalf("继任组项目 = %s, want %s", succ.WorkspaceID, ws2.ID)
	}
	succContainer, err := s.groupContainerDir(succ)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(succContainer, "workspace", "results", taskA.ID,
		fmt.Sprint(taskA.ResultRevision), "result.json")); err != nil {
		t.Fatalf("跨项目快照缺失：%v", err)
	}
	view, err := s.GetOrchestrationHandoff(ctx, testOwner, succ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Body, "目标项目改为 "+ws2.ID) {
		t.Fatalf("handoff 应记录目标项目变更：\n%s", view.Body)
	}
}

func TestStopWaitingPreviousSuccessor(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	_ = claimAll(t, s, resp0.OrchestrationID)
	out, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, successorBaseCmd("k1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != orchStatusWaitingPrevious {
		t.Fatalf("前置状态 = %s, want waiting_previous", out.Status)
	}
	status, _, err := s.StopAgentOrchestration(ctx, testOwner, out.OrchestrationID)
	if err != nil {
		t.Fatalf("停止等待中的继任组: %v", err)
	}
	if status != orchStatusCancelled {
		t.Fatalf("停止后状态 = %s, want cancelled", status)
	}
	d := taskByKeyT(t, s, out.OrchestrationID, "D")
	if d.Status != agentTaskStatusCancelled {
		t.Fatalf("继任组节点未取消：%s", d.Status)
	}
}

func TestReconcileSuccessorRepairsFiles(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	_ = claimAll(t, s, resp0.OrchestrationID)
	out, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, successorBaseCmd("k1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	succ := orchestrationRow(t, s, out.OrchestrationID)
	succContainer, err := s.groupContainerDir(succ)
	if err != nil {
		t.Fatal(err)
	}
	origContainer, err := s.groupContainerDir(orchestrationRow(t, s, resp0.OrchestrationID))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟「PG 已插入、后续文件步骤失败」：两份文件都缺。
	if err := os.Remove(filepath.Join(origContainer, successorFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(succContainer, groupReuseFileName)); err != nil {
		t.Fatal(err)
	}

	// 原组随后收尾；对账应补齐文件并把继任组推进到 active。
	if err := s.db.Exec(`UPDATE agent_tasks SET status = ? WHERE orchestration_id = ?`,
		agentTaskStatusCancelled, resp0.OrchestrationID).Error; err != nil {
		t.Fatal(err)
	}
	setOrchStatus(t, s, resp0.OrchestrationID, orchStatusCancelled)
	if err := s.reconcileSuccessor(ctx, succ); err != nil {
		t.Fatalf("reconcileSuccessor: %v", err)
	}
	mark, err := readSuccessorMark(origContainer)
	if err != nil || mark == nil || mark.SuccessorOrchestrationID != succ.ID ||
		mark.CreationKey != succ.IdempotencyKey {
		t.Fatalf("对账未补标记：%+v / %v", mark, err)
	}
	if rf, err := readGroupReuse(succContainer); err != nil || rf == nil {
		t.Fatalf("对账未补快照：%+v / %v", rf, err)
	}
	if got := orchestrationRow(t, s, succ.ID).Status; got != orchStatusActive {
		t.Fatalf("对账后状态 = %s, want active", got)
	}
	d := taskByKeyT(t, s, succ.ID, "D")
	if d.Status != agentTaskStatusReady {
		t.Fatalf("对账激活后无父节点应 ready，得 %s", d.Status)
	}

	// handoff 缺失是「需要人工处理」，不能凭空补出正文。
	if err := os.Remove(filepath.Join(succContainer, handoffFileName)); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileSuccessor(ctx, succ); err == nil || !strings.Contains(err.Error(), "缺少完整的 handoff") {
		t.Fatalf("缺 handoff 应报人工处理，得 %v", err)
	}
}

func TestSuccessorRejectsUnmigratedProject(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	resp0, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "o1"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	setOrchStatus(t, s, resp0.OrchestrationID, orchStatusSucceeded)
	// 存量项目容器：没有 layout.json，但已有旧字节。
	container, err := projectContainerDir(s.workspaceRoot, testOwner.TenantID, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "旧文件.txt"), []byte("存量"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSuccessorOrchestration(ctx, testOwner, resp0.OrchestrationID, successorBaseCmd("k1"), planEnv{}); !errors.Is(err, errProjectLayoutUnmigrated) {
		t.Fatalf("未迁移项目 err = %v, want errProjectLayoutUnmigrated", err)
	}
}
