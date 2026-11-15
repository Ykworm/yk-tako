// 本文件是持久 Task 的统一创建服务、读取、停止与结果提交（方案真源 = doc/70 §4/§6/§7/§10；施工包 §6/§7）。
//
// HTTP handler 与模型工具都走这里的 CreateAgentTask / CreateAgentOrchestration，校验只有一份
// （施工包 §6.1）。纯形状校验在 agent_task_graph.go；这里的校验要读库：来源会话归属、项目绑定、
// 执行者规格、模型 ref、幂等键比对。读取统一带可见性：Task／编排组的「本人」由
// orchestration → source_session → sessions.user_id 这条链判定，非本人一律当成不存在。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// nowUTC 是终态时间戳的统一取法（UTC，与 PG now() 同轴）。
func nowUTC() time.Time { return time.Now().UTC() }

// planEnv 是创建服务的环境注入：模型 ref 校验需要租户模型注册表，而 Store 不持有它——
// HTTP 侧从 RuntimeConfigRecord 现读组装，工具侧从 RunProfile.Resolver 走，各自封成闭包传入。
// nil = 跳过模型校验（单测没有注册表时；两条生产路径都会给）。
type planEnv struct {
	resolveModel func(ctx context.Context, ref string) error
}

var (
	errTaskNotFound     = errors.New("agent task not found")
	errOrchNotFound     = errors.New("agent orchestration not found")
	errCreateSource     = errors.New("creation: source session invalid")
	errCreateWorkspace  = errors.New("creation: workspace invalid")
	errCreateAgent      = errors.New("creation: agent invalid")
	errCreateModel      = errors.New("creation: model ref invalid")
	errPlanConflict     = errors.New("creation: idempotency key reused for another plan")
	errTaskNotStoppable = errors.New("agent task already finished")
	errOrchNotStoppable = errors.New("agent orchestration already finished")
	errTaskNotResumable = errors.New("agent task not resumable")
	errSubmitBad        = errors.New("submit: result invalid")
	errArtifactBad      = errors.New("submit: artifact invalid")
	errSubmitStale      = errors.New("submit: stale execution")
	errSubmitNotRunning = errors.New("submit: task not running")
)

// createTaskResp 是 POST /api/tasks 的 data（施工包 §4.1）：未领取时 currentRunId 必须是空串。
type createTaskResp struct {
	TaskID          string `json:"taskId"`
	OrchestrationID string `json:"orchestrationId"`
	Status          string `json:"status"`
	CurrentRunID    string `json:"currentRunId"`
}

// createOrchestrationResp 是 POST /api/orchestrations 的 data（施工包 §4.2）。
type createOrchestrationResp struct {
	OrchestrationID string `json:"orchestrationId"`
	Status          string `json:"status"`
}

// CreateAgentTask 创建单件 Task：内部转成单节点编排命令，与整组共享同一条创建与幂等路径。
func (s *Store) CreateAgentTask(ctx context.Context, p principal, cmd CreateTaskCommand, env planEnv) (createTaskResp, error) {
	orch, err := s.CreateAgentOrchestration(ctx, p, singleTaskCommand(cmd), env)
	if err != nil {
		return createTaskResp{}, err
	}
	t, err := s.taskByKey(ctx, p, orch.OrchestrationID, singleTaskKey)
	if err != nil {
		return createTaskResp{}, err
	}
	return createTaskResp{
		TaskID:          t.ID,
		OrchestrationID: orch.OrchestrationID,
		Status:          t.Status,
		CurrentRunID:    t.currentRunID(),
	}, nil
}

// CreateAgentOrchestration 创建固定 DAG 编排组（施工包 §4.2）。校验顺序即依赖：
// 纯形状（graph）→ 执行者与模型（写库前）→ 事务内来源会话／项目／幂等 → 建行。
// 幂等的本体是 (tenant_id, source_session_id, idempotency_key) 唯一索引：同键同 plan_hash
// 返回原组，同键不同 hash 回 errPlanConflict；并发撞索引时败者回读胜者行比对。
func (s *Store) CreateAgentOrchestration(ctx context.Context, p principal, cmd CreateOrchestrationCommand, env planEnv) (createOrchestrationResp, error) {
	plan, err := normalizeOrchestrationPlan(cmd)
	if err != nil {
		return createOrchestrationResp{}, err // *planError → 117101
	}
	hash, err := plan.hash()
	if err != nil {
		return createOrchestrationResp{}, err
	}
	// 执行者与模型在写库前校验（施工包 §4.2）。规格视图读侧无锁——校验到领取之间被删
	// 也不拖创建：领取装配时重新确认，失败走 assembly_failed（施工包 §6.2 的既有出口）。
	if err := s.validatePlanAgents(ctx, p, plan); err != nil {
		return createOrchestrationResp{}, err
	}
	for _, t := range plan.Tasks {
		ref := strings.TrimSpace(t.ModelRef)
		if ref == "" || isInheritRef(ref) || env.resolveModel == nil {
			continue
		}
		if err := env.resolveModel(ctx, ref); err != nil {
			return createOrchestrationResp{}, fmt.Errorf("%w: %s: %v", errCreateModel, ref, err)
		}
	}

	var resp createOrchestrationResp
	var createdTaskIDs []string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 来源会话：本人所有、未删除、普通聊天。FOR SHARE 与删除事务里的 FOR UPDATE 互斥：
		// 删除查「还有没有未交付完的编排组」时，并发的创建要么已提交（删除看得见、被拒），
		// 要么等删除提交后自己看见 deleted_at 非空——两条路都不会留下指向已删会话的组。
		var sess struct {
			TenantID    string
			UserID      string
			Kind        string
			Deleted     bool
			WorkspaceID *string
		}
		res := tx.Raw(`SELECT tenant_id, user_id, kind, deleted_at IS NOT NULL AS deleted, workspace_id
			FROM sessions WHERE id = ? FOR SHARE`, cmd.SourceSessionID).Scan(&sess)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 || sess.Deleted || sess.TenantID != p.TenantID ||
			sess.UserID != p.UserID || sess.Kind != sessionKindChat {
			return errCreateSource
		}
		// 项目：必须是来源会话挂着的项目，且本人可见（2026-10-22 起项目只认属主）。
		if sess.WorkspaceID == nil || *sess.WorkspaceID != cmd.WorkspaceID {
			return errCreateWorkspace
		}
		var one int
		if err := tx.Raw(`SELECT 1 FROM workspaces WHERE id = ? AND tenant_id = ? AND user_id = ?`,
			cmd.WorkspaceID, p.TenantID, p.UserID).Scan(&one).Error; err != nil {
			return err
		}
		if one == 0 {
			return errCreateWorkspace
		}
		// 幂等：先查同键。命中即不建新行——创建幂等是「重试不产生第二组」，不是「覆盖」。
		var existing AgentOrchestration
		qerr := tx.Where("tenant_id = ? AND source_session_id = ? AND idempotency_key = ?",
			p.TenantID, cmd.SourceSessionID, cmd.IdempotencyKey).Take(&existing).Error
		if qerr == nil {
			if existing.PlanHash != hash {
				return errPlanConflict
			}
			resp = createOrchestrationResp{OrchestrationID: existing.ID, Status: existing.Status}
			return nil
		} else if !errors.Is(qerr, gorm.ErrRecordNotFound) {
			return qerr
		}

		orchID, err := newRowID("or_")
		if err != nil {
			return err
		}
		taskIDs := make(map[string]string, len(plan.Tasks))
		rows := make([]AgentTask, 0, len(plan.Tasks))
		for _, t := range plan.Tasks {
			id, err := newRowID("at_")
			if err != nil {
				return err
			}
			taskIDs[t.Key] = id
			status := agentTaskStatusPending
			if !plan.hasParents(t.Key) {
				status = agentTaskStatusReady
			}
			rows = append(rows, AgentTask{
				ID: id, OrchestrationID: orchID, TaskKey: t.Key,
				TenantID: p.TenantID, WorkspaceID: cmd.WorkspaceID,
				Content: t.Content, AgentName: t.Agent, ModelRef: t.ModelRef,
				Status: status, PlanCheckpoint: t.PlanCheckpoint,
			})
		}
		orch := AgentOrchestration{
			ID: orchID, TenantID: p.TenantID, WorkspaceID: cmd.WorkspaceID,
			SourceSessionID: cmd.SourceSessionID, SourceRunID: cmd.SourceRunID,
			Goal: plan.Goal, Status: orchStatusActive,
			FinalTaskID:    taskIDs[plan.finalKey()],
			IdempotencyKey: cmd.IdempotencyKey, PlanHash: hash,
			WorkspaceMode: plan.storageMode(), VerifyCommand: plan.VerifyCommand,
		}
		if err := tx.Create(&orch).Error; err != nil {
			if isUniqueViolation(err) {
				// 并发同键：唯一索引只留一个胜者。败者回读胜者行比对计划——同计划 = 幂等成功。
				var winner AgentOrchestration
				if rerr := tx.Where("tenant_id = ? AND source_session_id = ? AND idempotency_key = ?",
					p.TenantID, cmd.SourceSessionID, cmd.IdempotencyKey).Take(&winner).Error; rerr != nil {
					return rerr
				}
				if winner.PlanHash != hash {
					return errPlanConflict
				}
				resp = createOrchestrationResp{OrchestrationID: winner.ID, Status: winner.Status}
				return nil
			}
			return err
		}
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		if len(plan.Links) > 0 {
			links := make([]AgentTaskLink, 0, len(plan.Links))
			for _, l := range plan.Links {
				links = append(links, AgentTaskLink{
					ParentTaskID: taskIDs[l.Parent], ChildTaskID: taskIDs[l.Child],
					TenantID: p.TenantID, OrchestrationID: orchID,
				})
			}
			if err := tx.Create(&links).Error; err != nil {
				return err
			}
		}
		resp = createOrchestrationResp{OrchestrationID: orchID, Status: orchStatusActive}
		createdTaskIDs = make([]string, 0, len(rows))
		for _, r := range rows {
			createdTaskIDs = append(createdTaskIDs, r.ID)
		}
		return nil
	})
	if err != nil {
		return createOrchestrationResp{}, err
	}
	s.PublishEvent(ctx, p.TenantID, eventAgentOrchestrationsChanged, resp.OrchestrationID)
	// 创建也是任务事件（施工包 §10.3：创建在 PG 成功后发）——没有它，任务页的列表要等
	// 调度轮第一次状态翻转才出现新行；blocked 的尾巴最长要等到上游跑完才可见。
	for _, id := range createdTaskIDs {
		s.PublishEvent(ctx, p.TenantID, eventAgentTasksChanged, id)
	}
	// 内部唤醒（doc/72 §9.1）：新组可能已有 ready 节点，调度器不必等 30 秒对账。
	// PG 已提交才发；失败只丢一次叫醒，后台对账兜住。
	s.PublishTaskWake(ctx, p.TenantID, "created", resp.OrchestrationID)
	return resp, nil
}

// validatePlanAgents 校验计划里每个执行者都是当前租户、该用户可见的 subagent 规格。
func (s *Store) validatePlanAgents(ctx context.Context, p principal, plan *orchPlan) error {
	rows, err := s.ListEffectiveAgents(ctx, p.TenantID, p.UserID, "subagent")
	if err != nil {
		return err
	}
	byName := make(map[string]bool, len(rows))
	for _, r := range rows {
		byName[r.Name] = true
	}
	for _, t := range plan.Tasks {
		if !byName[strings.TrimSpace(t.Agent)] {
			return fmt.Errorf("%w: %s", errCreateAgent, t.Agent)
		}
	}
	return nil
}

// currentRunID 把可空列读成契约里的字符串（未领取 = 空串）。
func (t *AgentTask) currentRunID() string {
	if t.CurrentRunID == nil {
		return ""
	}
	return *t.CurrentRunID
}

// taskViewFrom 把一行 Task 翻成 API 视图。interrupted 在这里派生（存储仍是 running）：
// 当前 run 不在 alive 集合（租约过期或 run 行已不在）→ interrupted。
// ws 是隔离组的工作区摘要（shared 组为 nil，响应不出现该键）。
func taskViewFrom(t AgentTask, alive map[string]bool, ws *taskWorkspaceView) agentTaskView {
	v := agentTaskView{
		TaskID: t.ID, OrchestrationID: t.OrchestrationID, Key: t.TaskKey,
		Content: t.Content, Agent: t.AgentName, ModelRef: t.ModelRef,
		PlanCheckpoint: t.PlanCheckpoint, CreatedRevision: t.CreatedRevision,
		WorkspaceID: t.WorkspaceID, Status: t.Status,
		Result:         parseTaskResult(t.Result),
		ResultRevision: t.ResultRevision,
		CreatedAt:      t.CreatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt, UpdatedAt: t.UpdatedAt,
		Workspace: ws,
	}
	v.CurrentRunID = t.currentRunID()
	if t.ErrorCode != "" || t.ErrorMessage != "" {
		v.Error = &taskError{Code: t.ErrorCode, Message: t.ErrorMessage}
	}
	if t.Status == agentTaskStatusRunning && t.CurrentRunID != nil && !alive[*t.CurrentRunID] {
		v.Status = agentTaskStatusInterrupted
	}
	return v
}

// aliveRuns 一笔查出租约仍有效的 run 集合（interrupted 派生的输入）。判活用 PG now()，
// 多副本共用一个时钟（与 runs 读侧同一条谓词）。
func (s *Store) aliveRuns(ctx context.Context, runIDs []string) (map[string]bool, error) {
	alive := make(map[string]bool, len(runIDs))
	if len(runIDs) == 0 {
		return alive, nil
	}
	var ids []string
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id FROM runs WHERE id IN ? AND status = 'running' AND lease_until > now()`, runIDs).
		Scan(&ids).Error; err != nil {
		return nil, err
	}
	for _, id := range ids {
		alive[id] = true
	}
	return alive, nil
}

// taskOwnerQuery 是 Task 可见性链的公共 SQL：orchestration → source_session → 本人。
// 非本人与不存在同形（117001），不给「这场任务在但不是你的」通道。
const taskOwnerQuery = `
SELECT t.* FROM agent_tasks t
  JOIN agent_orchestrations o ON o.id = t.orchestration_id
  JOIN sessions s ON s.id = o.source_session_id
 WHERE t.tenant_id = ? AND s.user_id = ?`

// GetAgentTask 按 id 读一件 Task（施工包 §4.3）。
func (s *Store) GetAgentTask(ctx context.Context, p principal, id string) (agentTaskView, error) {
	var rows []AgentTask
	if err := s.db.WithContext(ctx).Raw(taskOwnerQuery+` AND t.id = ?`, p.TenantID, p.UserID, id).
		Scan(&rows).Error; err != nil {
		return agentTaskView{}, err
	}
	if len(rows) == 0 {
		return agentTaskView{}, errTaskNotFound
	}
	alive, err := s.aliveRuns(ctx, []string{rows[0].currentRunID()})
	if err != nil {
		return agentTaskView{}, err
	}
	orchs, err := s.orchRowsByID(ctx, []string{rows[0].OrchestrationID})
	if err != nil {
		return agentTaskView{}, err
	}
	return taskViewFrom(rows[0], alive, s.workspaceSummary(orchs[rows[0].OrchestrationID], rows[0].ID)), nil
}

// taskByKey 按 (orchestration, key) 读单节点组的唯一 Task（CreateAgentTask 的回读用）。
func (s *Store) taskByKey(ctx context.Context, p principal, orchID, key string) (AgentTask, error) {
	var rows []AgentTask
	if err := s.db.WithContext(ctx).Raw(taskOwnerQuery+` AND t.orchestration_id = ? AND t.task_key = ?`,
		p.TenantID, p.UserID, orchID, key).Scan(&rows).Error; err != nil {
		return AgentTask{}, err
	}
	if len(rows) == 0 {
		return AgentTask{}, errTaskNotFound
	}
	return rows[0], nil
}

// ListWorkspaceTasks 列本人项目下的 Task（施工包 §4.3：只列本人项目，createdAt/id 倒序）。
// sessionID 非空时按「所属编排组的 source_session_id」缩小范围（doc/80）；会话不存在 / 不是本人
// 的返回空列表（读面的缩小范围参数，不校验存在性）。
func (s *Store) ListWorkspaceTasks(ctx context.Context, p principal, workspaceID, sessionID string) ([]agentTaskView, error) {
	var one int
	if err := s.db.WithContext(ctx).Raw(
		`SELECT 1 FROM workspaces WHERE id = ? AND tenant_id = ? AND user_id = ?`,
		workspaceID, p.TenantID, p.UserID).Scan(&one).Error; err != nil {
		return nil, err
	}
	if one == 0 {
		return nil, ErrWorkspaceNotFound
	}
	q := s.db.WithContext(ctx).Where("tenant_id = ? AND workspace_id = ?", p.TenantID, workspaceID)
	if sessionID != "" {
		q = q.Where("orchestration_id IN (SELECT id FROM agent_orchestrations WHERE tenant_id = ? AND source_session_id = ?)",
			p.TenantID, sessionID)
	}
	var rows []AgentTask
	if err := q.Order("created_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	runIDs := make([]string, 0, len(rows))
	for _, t := range rows {
		if id := t.currentRunID(); id != "" {
			runIDs = append(runIDs, id)
		}
	}
	alive, err := s.aliveRuns(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	orchIDs := make([]string, 0, len(rows))
	seenOrch := make(map[string]bool, len(rows))
	for _, t := range rows {
		if !seenOrch[t.OrchestrationID] {
			seenOrch[t.OrchestrationID] = true
			orchIDs = append(orchIDs, t.OrchestrationID)
		}
	}
	orchs, err := s.orchRowsByID(ctx, orchIDs)
	if err != nil {
		return nil, err
	}
	views := make([]agentTaskView, 0, len(rows))
	for _, t := range rows {
		views = append(views, taskViewFrom(t, alive, s.workspaceSummary(orchs[t.OrchestrationID], t.ID)))
	}
	return views, nil
}

// GetAgentOrchestration 返回组、节点、边（施工包 §4.3）。可见性与 Task 同链。
func (s *Store) GetAgentOrchestration(ctx context.Context, p principal, id string) (orchestrationView, error) {
	var orows []AgentOrchestration
	if err := s.db.WithContext(ctx).Raw(`
SELECT o.* FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?`, id, p.TenantID, p.UserID).
		Scan(&orows).Error; err != nil {
		return orchestrationView{}, err
	}
	if len(orows) == 0 {
		return orchestrationView{}, errOrchNotFound
	}
	var trows []AgentTask
	if err := s.db.WithContext(ctx).
		Where("orchestration_id = ?", id).Order("created_at, task_key").Find(&trows).Error; err != nil {
		return orchestrationView{}, err
	}
	var lrows []AgentTaskLink
	if err := s.db.WithContext(ctx).
		Where("orchestration_id = ?", id).Order("parent_task_id, child_task_id").Find(&lrows).Error; err != nil {
		return orchestrationView{}, err
	}
	runIDs := make([]string, 0, len(trows))
	for _, t := range trows {
		if rid := t.currentRunID(); rid != "" {
			runIDs = append(runIDs, rid)
		}
	}
	alive, err := s.aliveRuns(ctx, runIDs)
	if err != nil {
		return orchestrationView{}, err
	}
	o := orows[0]
	views := make([]agentTaskView, 0, len(trows))
	for _, t := range trows {
		views = append(views, taskViewFrom(t, alive, s.workspaceSummary(o, t.ID)))
	}
	links := make([]taskLinkView, 0, len(lrows))
	for _, l := range lrows {
		links = append(links, taskLinkView{ParentTaskID: l.ParentTaskID, ChildTaskID: l.ChildTaskID})
	}
	// 重规划关系从 handoff 现读（PG 不存这两列）：非继任组读空，响应字节不变；
	// 文件损坏只记日志、不挡详情（详情不是交接真源，GET handoff 会如实报错）。
	replanOf, replanDepth, err := s.handoffReplanOf(o)
	if err != nil {
		log.Printf("orchestration detail %s: 读 handoff 失败: %v", o.ID, err)
	}
	return orchestrationView{
		Orchestration: orchestrationBody{
			OrchestrationID: o.ID, Goal: o.Goal, WorkspaceID: o.WorkspaceID,
			SourceSessionID: string(o.SourceSessionID), Status: o.Status,
			FinalTaskID: o.FinalTaskID, PlanRevision: o.PlanRevision, PlanHold: o.PlanHold,
			DeliveryStatus: o.DeliveryStatus, DeliveryRunID: o.DeliveryRunID,
			DeliveryErrorCode: o.DeliveryErrorCode,
			CreatedAt:         o.CreatedAt, FinishedAt: o.FinishedAt, DeliveredAt: o.DeliveredAt, UpdatedAt: o.UpdatedAt,
			Workspace:   s.workspaceSummary(o, o.FinalTaskID),
			ReplanOf:    replanOf,
			ReplanDepth: replanDepth,
		},
		Tasks: views,
		Links: links,
	}, nil
}

// StopAgentTask 停一件 Task（施工包 §4.3/§8.4）。终态 succeeded/failed 拒绝（117104）；
// 重复停止幂等。running 行先翻 cancelled——submit_task_result 立刻失效——再把当前 run 交给
// 调用方在提交后补 StopRun 与进程内取消（取消链路不属于存储事务）。
// publishing/blocked 没有活 run，直接 cancelled（doc/74 §4.6）；残留的发布状态与项目标记
// 由发布对账的放弃分支收口，已发布内容不撤销。
func (s *Store) StopAgentTask(ctx context.Context, p principal, id string) (string, RunID, error) {
	if _, err := s.GetAgentTask(ctx, p, id); err != nil {
		return "", "", err
	}
	status := ""
	var runID RunID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var t AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).Take(&t).Error; err != nil {
			return err
		}
		switch t.Status {
		case agentTaskStatusSucceeded, agentTaskStatusFailed:
			return errTaskNotStoppable
		case agentTaskStatusCancelled:
			status = agentTaskStatusCancelled
			return nil
		case agentTaskStatusPending, agentTaskStatusReady, agentTaskStatusBlocked, agentTaskStatusPublishing:
			if err := tx.Model(&AgentTask{}).Where("id = ? AND status = ?", id, t.Status).
				Updates(map[string]any{"status": agentTaskStatusCancelled, "finished_at": nowUTC()}).Error; err != nil {
				return err
			}
			status = agentTaskStatusCancelled
			return nil
		default: // running
			if t.CurrentRunID != nil {
				runID = RunID(*t.CurrentRunID)
			}
			if err := tx.Model(&AgentTask{}).Where("id = ? AND status = ?", id, agentTaskStatusRunning).
				Updates(map[string]any{"status": agentTaskStatusCancelled, "finished_at": nowUTC()}).Error; err != nil {
				return err
			}
			status = agentTaskStatusCancelled
			return nil
		}
	})
	if err != nil {
		return "", "", err
	}
	if id != "" {
		s.PublishEvent(ctx, p.TenantID, eventAgentTasksChanged, id)
		s.PublishTaskWake(ctx, p.TenantID, "stop_task", id)
	}
	return status, runID, nil
}

// StopAgentOrchestration 停整组（施工包 §8.4）：先持久化取消请求。pending/ready/blocked/publishing
// 直接 cancelled；running 节点留给调用方（调度轮）复用 StopRun 收尾。组内已全部终态时
// 当场落 cancelled（响应就回 cancelled，不用等下一轮调度），否则回 cancelling。
// 已 succeeded/failed/cancelled 的组拒绝（117105）。
// 发布状态不在这里复位：pending/publishing/blocked 留给发布对账的放弃分支清项目标记并复位
// （直接复位会让标记失去主人，项目永远拒新 run）；published 是既成事实，停止不撤销。
func (s *Store) StopAgentOrchestration(ctx context.Context, p principal, id string) (string, []RunID, error) {
	if _, err := s.GetAgentOrchestration(ctx, p, id); err != nil {
		return "", nil, err
	}
	status, runIDs, _, err := s.stopOrchestrationCore(ctx, id)
	return status, runIDs, err
}

// stopOrchestrationCore 是停止的存储半，不校验调用方可见性（继任创建与对账用）。
// preparing/waiting_previous 同样只取消自己的 pending/ready/blocked，不激活继任组。
func (s *Store) stopOrchestrationCore(ctx context.Context, id string) (string, []RunID, string, error) {
	status := ""
	tenantID := ""
	var runIDs []RunID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).Take(&o).Error; err != nil {
			return err
		}
		tenantID = o.TenantID
		switch o.Status {
		case orchStatusSucceeded, orchStatusFailed, orchStatusCancelled:
			return errOrchNotStoppable
		}
		if o.Status == orchStatusActive || o.Status == orchStatusPreparing || o.Status == orchStatusWaitingPrevious {
			if err := tx.Model(&AgentTask{}).
				Where("orchestration_id = ? AND status IN ?", id,
					[]string{agentTaskStatusPending, agentTaskStatusReady, agentTaskStatusBlocked, agentTaskStatusPublishing}).
				Updates(map[string]any{"status": agentTaskStatusCancelled, "finished_at": nowUTC()}).Error; err != nil {
				return err
			}
		}
		// running 节点的当前 run 交给调用方收尾（StopRun + 进程内取消）。
		var current []string
		if err := tx.Raw(`SELECT current_run_id FROM agent_tasks
			WHERE orchestration_id = ? AND status = ? AND current_run_id IS NOT NULL`,
			id, agentTaskStatusRunning).Scan(&current).Error; err != nil {
			return err
		}
		for _, rid := range current {
			runIDs = append(runIDs, RunID(rid))
		}
		var remaining int64
		if err := tx.Model(&AgentTask{}).Where("orchestration_id = ? AND status IN ?", id,
			[]string{agentTaskStatusPending, agentTaskStatusReady, agentTaskStatusRunning, agentTaskStatusBlocked, agentTaskStatusPublishing}).
			Count(&remaining).Error; err != nil {
			return err
		}
		next := orchStatusCancelled
		if remaining > 0 {
			next = orchStatusCancelling
		}
		if err := tx.Model(&AgentOrchestration{}).Where("id = ?", id).
			Updates(map[string]any{"status": next}).Error; err != nil {
			return err
		}
		status = next
		return nil
	})
	if err != nil {
		return "", nil, "", err
	}
	s.PublishEvent(ctx, tenantID, eventAgentOrchestrationsChanged, id)
	s.PublishTaskWake(ctx, tenantID, "stop_orchestration", id)
	return status, runIDs, tenantID, nil
}

// SubmitTaskResult 是 submit_task_result 工具的落库半（施工包 §7.1）。裁决都在这里：
// Task 必须 running；run 必须还是当前代（runs 行 running 且 lease_epoch 相等——旧 worker
// 被接管后提交在这里被拒）；内容限额；artifact 真实路径；同代同字节幂等返回原 revision。
// 提交只写结果列，不翻 Task 状态（succeeded 归对账）。
// 两种工作区模式都走 submitWorkspaceTaskResult（先固化不可变结果再存 PG，doc/74 §9.2；
// doc/76 §4 把 shared 也纳入同一条结果与发布链）。
func (s *Store) SubmitTaskResult(ctx context.Context, taskID string, runID RunID, epoch int64, result TaskResult, wsRoot string) (int, error) {
	if strings.TrimSpace(result.Summary) == "" {
		return 0, fmt.Errorf("%w: summary 必填", errSubmitBad)
	}
	if utf8Len(result.Summary) > taskSummaryMaxRunes {
		return 0, fmt.Errorf("%w: summary 超过 %d 字", errSubmitBad, taskSummaryMaxRunes)
	}
	if len(result.Artifacts) > taskArtifactsMax {
		return 0, fmt.Errorf("%w: artifacts 超过 %d 个", errSubmitBad, taskArtifactsMax)
	}
	for i := range result.Artifacts {
		clean, err := resolveArtifactPath(wsRoot, result.Artifacts[i].Path)
		if err != nil {
			return 0, err
		}
		result.Artifacts[i].Path = clean
	}
	if result.Metadata != nil {
		mb, err := json.Marshal(result.Metadata)
		if err != nil {
			return 0, fmt.Errorf("%w: metadata 序列化失败", errSubmitBad)
		}
		if len(mb) > taskMetadataMaxBytes {
			return 0, fmt.Errorf("%w: metadata 超过 %d 字节", errSubmitBad, taskMetadataMaxBytes)
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errSubmitBad, err)
	}

	t, o, err := s.agentTaskRow(ctx, taskID)
	if err != nil {
		if errors.Is(err, errTaskNotFound) {
			return 0, fmt.Errorf("%w: task 不存在", errSubmitNotRunning)
		}
		return 0, err
	}
	return s.submitWorkspaceTaskResult(ctx, t, o, runID, epoch, result, string(raw))
}

// submitWorkspaceTaskResult 是 shared／isolated 共用的提交链（doc/74 §9.2；doc/76 §4）：
// 整段在同一把工作区共享锁内——计算候选 revision → Provider 验证并固化不可变结果
// （staging → final）→ PG 再校验 run/epoch 并保存通用结果。Provider 操作不进 PG 事务。
// shared 组共用 work/shared，锁取组锁；isolated 只挡同 Task，取 Task 锁。
//
// 候选 revision：同 run/epoch/同字节重提 → 原 revision（固化时核验身份后复用）；否则 +1。
// 固化报身份冲突（该 revision 已有不同内容的 final——上次崩溃留下的孤儿，或重提时工作现场
// 变了）→ 候选 +1 重试，绝不覆盖旧版本的字节。
// Provider final 已存在但 PG 未保存时，这里保存成功才算接受；ReconcileRun 另行核验 final 完整性。
func (s *Store) submitWorkspaceTaskResult(ctx context.Context, t AgentTask, o AgentOrchestration, runID RunID, epoch int64, result TaskResult, raw string) (int, error) {
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		return 0, err
	}
	provider, err := s.workspaceProvider(g)
	if err != nil {
		return 0, err
	}
	lockKey := g.taskLockKey(t.ID)
	if g.sharedMode() {
		lockKey = g.groupLockKey()
	}
	rel, err := g.Locker.Lock(ctx, []string{lockKey})
	if err != nil {
		return 0, err
	}
	defer rel()

	// 锁内重读并预检：锁只挡同 Task 的提交，run 接管发生在 PG 侧，必须看最新行。
	var cur AgentTask
	if err := s.db.WithContext(ctx).Where("id = ?", t.ID).Take(&cur).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("%w: task 不存在", errSubmitNotRunning)
		}
		return 0, err
	}
	if cur.Status != agentTaskStatusRunning {
		return 0, errSubmitNotRunning
	}
	var runRow struct {
		Status string
		Epoch  int64
	}
	if err := s.db.WithContext(ctx).Raw(`SELECT status, lease_epoch AS epoch FROM runs WHERE id = ?`, runID).
		Scan(&runRow).Error; err != nil {
		return 0, err
	}
	if runRow.Status == "" {
		return 0, fmt.Errorf("%w: run 不存在", errSubmitStale)
	}
	if runRow.Status != "running" || runRow.Epoch != epoch {
		return 0, errSubmitStale
	}

	candidate := cur.ResultRevision + 1
	if cur.ResultRunID == string(runID) && cur.ResultEpoch == epoch && cur.Result == raw && cur.ResultRevision > 0 {
		candidate = cur.ResultRevision
	}
	work := TaskWork{Root: g.workDir(t.ID)}
	for attempt := 0; ; attempt++ {
		_, err = provider.AcceptResult(ctx, g, cur, candidate, result, work)
		if err == nil {
			break
		}
		if errors.Is(err, errImmutableIdentityConflict) && attempt < 8 {
			candidate++
			continue
		}
		return 0, err
	}

	// PG 保存半：FOR UPDATE 再校验一次（固化期间 run 可能被接管），同代同字节已保存则幂等返回。
	tenantID := cur.TenantID
	revision := 0
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", t.ID).Take(&locked).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: task 不存在", errSubmitNotRunning)
			}
			return err
		}
		if locked.Status != agentTaskStatusRunning {
			return errSubmitNotRunning
		}
		var rr struct {
			Status string
			Epoch  int64
		}
		if err := tx.Raw(`SELECT status, lease_epoch AS epoch FROM runs WHERE id = ?`, runID).
			Scan(&rr).Error; err != nil {
			return err
		}
		if rr.Status == "" {
			return fmt.Errorf("%w: run 不存在", errSubmitStale)
		}
		if rr.Status != "running" || rr.Epoch != epoch {
			return errSubmitStale
		}
		if locked.ResultRunID == string(runID) && locked.ResultEpoch == epoch && locked.Result == raw {
			revision = locked.ResultRevision
			return nil
		}
		revision = candidate
		return tx.Model(&AgentTask{}).Where("id = ?", t.ID).
			Updates(map[string]any{
				"result": raw, "result_revision": revision,
				"result_run_id": string(runID), "result_epoch": epoch,
			}).Error
	})
	if err != nil {
		return 0, err
	}
	s.PublishEvent(ctx, tenantID, eventAgentTasksChanged, t.ID)
	return revision, nil
}

// resolveArtifactPath 把调用方给的 artifact 路径校成项目根下的真实文件（施工包 §7.1）：
// 拒绝绝对路径、..、目录、不存在与软链越界；通过后返回 filepath.Clean 的相对路径。
// 真实路径包含检查用 EvalSymlinks 后的前缀比对——符号链接把路径偷换出项目根挡在这一步。
func resolveArtifactPath(root, raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("%w: artifact path 为空", errArtifactBad)
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: artifact 必须是项目根的相对路径：%s", errArtifactBad, p)
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: artifact 越出项目根：%s", errArtifactBad, p)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%w: 项目根不可用", errArtifactBad)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(realRoot, clean))
	if err != nil {
		return "", fmt.Errorf("%w: artifact 不存在：%s", errArtifactBad, clean)
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: artifact 越出项目根：%s", errArtifactBad, clean)
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: artifact 必须是项目根下的普通文件：%s", errArtifactBad, clean)
	}
	return clean, nil
}
