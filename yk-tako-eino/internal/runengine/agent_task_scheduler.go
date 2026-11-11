// 本文件是持久 Task 的调度程序与其 Store 半（方案真源 = doc/70 §7.2/§8/§9；施工包 §7.2/§8/§9）。
//
// 职责切分：worker 的 EndRun 钩子在 run 定稿当场调 ReconcileRun（快路径）；本文件的调度循环
// 每秒跑一轮同一批 Store 方法作为崩溃后的补偿（慢路径，§7.2「调度循环也调用同一函数」）。
// 两条路径都收敛到同一组条件 UPDATE——幂等由 WHERE status='running' 保证，谁先到谁生效。
//
// 每轮顺序（§8.2）即依赖：只有终态已落库，blocked 传播才读它；只有 ready 已落库，领取才发生；
// 只有通知已落库，交付才可能启动。正确性全部押在 PG（行锁 + 条件更新 + 唯一索引），
// 不新增任何 Redis 键；每副本跑同一循环，靠锁与条件更新保证不超发、不重复。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// agentTaskClaim 是一次成功领取的产出：领取事务里铸好的执行 session、run、首条输入，
// 以及装配启动所需的身份。Epoch 恒为 1（出生令牌，doc/30 §7）。
type agentTaskClaim struct {
	TaskID      string
	TaskKey     string
	SessionID   SessionID
	RunID       RunID
	TenantID    string
	UserID      string
	WorkspaceID string
	Epoch       int64
	FirstInput  string
	// OrchestrationID / SourceRunID 进执行 run 的 trace 属性（doc/70 §10.3）。
	OrchestrationID string
	SourceRunID     string
}

// ReconcileRun 对账一场已终态的 run 到它的 Task（§7.2）。不是 Task 的 run（普通聊天）直接跳过；
// Task 不在 running（已对账 / 已被停止）也是幂等 no-op。
//
// 裁决表：complete + 本 run 的有效提交（result_run_id 指回这场 run）→ succeeded；
// complete + 没有有效提交 → failed/result_not_submitted；error → failed/run_error；
// stopped → cancelled。崩溃可能发生在 run complete 与 Task 翻转之间，所以条件 UPDATE
// （WHERE status='running'）是幂等的本体：下一次看到同一 run 仍能完成同一转换，不新增行。
func (s *Store) ReconcileRun(ctx context.Context, runID RunID) error {
	var row struct {
		RunStatus       string `gorm:"column:run_status"`
		Epoch           int64  `gorm:"column:lease_epoch"`
		TaskID          string `gorm:"column:task_id"`
		TenantID        string `gorm:"column:tenant_id"`
		OrchestrationID string `gorm:"column:orchestration_id"`
		Receiver        string `gorm:"column:source_session_id"`
		Status          string `gorm:"column:status"`
		ResultRunID     string `gorm:"column:result_run_id"`
		Result          string `gorm:"column:result"`
		ResultRevision  int    `gorm:"column:result_revision"`
		PlanCheckpoint  bool   `gorm:"column:plan_checkpoint"`
	}
	res := s.db.WithContext(ctx).Raw(`
SELECT r.status AS run_status, r.lease_epoch, t.id AS task_id, t.tenant_id, t.orchestration_id,
       o.source_session_id, t.status, t.result_run_id, t.result, t.result_revision, t.plan_checkpoint
  FROM runs r
  JOIN agent_tasks t ON t.current_run_id = r.id
  JOIN agent_orchestrations o ON o.id = t.orchestration_id
 WHERE r.id = ?`, runID).Scan(&row)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 || row.Status != agentTaskStatusRunning {
		return nil // 不是 Task 的 run，或 Task 已收尾：幂等 no-op
	}
	status := ""
	errCode := ""
	errMsg := ""
	switch row.RunStatus {
	case "complete":
		if row.ResultRunID == string(runID) && row.Result != "" {
			status = agentTaskStatusSucceeded
		} else {
			status = agentTaskStatusFailed
			errCode = taskErrResultNotSubmit
			errMsg = "执行正常结束但没有提交结果"
		}
	case "error":
		status = agentTaskStatusFailed
		errCode = taskErrRunError
		errMsg = "执行 run 出错"
	case "stopped":
		status = agentTaskStatusCancelled
	default:
		return nil // running：只有租约过期一种，归中断检测步（不翻状态）
	}
	updates := map[string]any{"status": status, "finished_at": nowUTC()}
	if errCode != "" {
		updates["error_code"] = errCode
		updates["error_message"] = errMsg
	}
	checkpointed := false
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r := tx.Model(&AgentTask{}).Where("id = ? AND status = ?", row.TaskID, agentTaskStatusRunning).
			Updates(updates)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil // 另一个副本 / EndRun 钩子先到了：幂等
		}
		if status == agentTaskStatusFailed {
			return s.insertNotificationTx(tx, AgentNotification{
				TenantID: row.TenantID, OrchestrationID: row.OrchestrationID, TaskID: row.TaskID,
				ReceiverSessionID: SessionID(row.Receiver), Kind: notifKindTaskFailed,
				DedupKey:  fmt.Sprintf("%s:%s:%s:%d", notifKindTaskFailed, row.TaskID, runID, row.Epoch),
				ErrorCode: errCode, Status: notifStatusPending,
			})
		}
		if status != agentTaskStatusSucceeded || !row.PlanCheckpoint {
			return nil
		}
		// 检查点（doc/72 §6.1）：Task 成功与「组暂停 + 复核通知」同事务。锁组并确认仍 active；
		// 组已被停止或收口时不再暂停（否则会留下一个没人能继续的 hold）。
		var o AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", row.OrchestrationID).Take(&o).Error; err != nil {
			return err
		}
		if o.Status != orchStatusActive {
			return nil
		}
		if err := tx.Exec(`UPDATE agent_orchestrations SET plan_hold = true, updated_at = now()
 WHERE id = ? AND status = ?`, o.ID, orchStatusActive).Error; err != nil {
			return err
		}
		checkpointed = true
		return s.insertNotificationTx(tx, AgentNotification{
			TenantID: row.TenantID, OrchestrationID: row.OrchestrationID, TaskID: row.TaskID,
			ReceiverSessionID: SessionID(row.Receiver), Kind: notifKindPlanReviewRequired,
			DedupKey: fmt.Sprintf("%s:%s:%d", notifKindPlanReviewRequired, row.TaskID, row.ResultRevision),
			Status:   notifStatusPending,
		})
	}); err != nil {
		return err
	}
	s.PublishEvent(ctx, row.TenantID, eventAgentTasksChanged, row.TaskID)
	if checkpointed {
		s.PublishEvent(ctx, row.TenantID, eventAgentOrchestrationsChanged, row.OrchestrationID)
		s.PublishTaskWake(ctx, row.TenantID, "checkpoint", row.TaskID)
	}
	return nil
}

// insertNotificationTx 落一条协调通知：dedup_key 唯一索引 + ON CONFLICT DO NOTHING，
// 同一件事（同一 run 的失败 / 同一代的中断 / 同一 revision 的整组完成）无论写多少次只留一行。
func (s *Store) insertNotificationTx(tx *gorm.DB, n AgentNotification) error {
	id, err := newRowID("an_")
	if err != nil {
		return err
	}
	n.ID = id
	return tx.Exec(`INSERT INTO agent_notifications
		(id, tenant_id, orchestration_id, task_id, receiver_session_id, kind, dedup_key, status, error_code, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, now())
		ON CONFLICT (dedup_key) DO NOTHING`,
		n.ID, n.TenantID, n.OrchestrationID, n.TaskID, n.ReceiverSessionID, n.Kind, n.DedupKey, n.Status, n.ErrorCode).Error
}

// insertInterruptedOnClaimTx 在接管事务里补写被接管代的 task_interrupted 通知（§9.1）：
// 引擎重启后用户可能在调度器的中断检测 tick 之前就 resume，这条通知不能取决于两者谁先到。
// run 的 session 不是 agent_task 执行会话（普通聊天续跑）时整条 SELECT 落空，不插；
// 调度器已经写过的同一代由 dedup_key 挡掉，双写只留一行。
func (s *Store) insertInterruptedOnClaimTx(tx *gorm.DB, runID RunID, epoch int64) error {
	var row struct {
		TaskID          string `gorm:"column:task_id"`
		TenantID        string `gorm:"column:tenant_id"`
		OrchestrationID string `gorm:"column:orchestration_id"`
		Receiver        string `gorm:"column:source_session_id"`
	}
	if err := tx.Raw(`
SELECT t.id AS task_id, t.tenant_id, t.orchestration_id, o.source_session_id
  FROM agent_tasks t
  JOIN agent_orchestrations o ON o.id = t.orchestration_id
 WHERE t.execution_session_id = (SELECT session_id FROM runs WHERE id = ?)`, runID).Scan(&row).Error; err != nil {
		return err
	}
	if row.TaskID == "" {
		return nil
	}
	return s.insertNotificationTx(tx, AgentNotification{
		TenantID: row.TenantID, OrchestrationID: row.OrchestrationID, TaskID: row.TaskID,
		ReceiverSessionID: SessionID(row.Receiver), Kind: notifKindTaskInterrupted,
		DedupKey: fmt.Sprintf("%s:%s:%s:%d", notifKindTaskInterrupted, row.TaskID, runID, epoch),
		Status:   notifStatusPending,
	})
}

// reconcileFinishedRuns 是调度侧的对账补偿：找出 Task 还在 running、但当前 run 已终态的行，
// 逐场走 ReconcileRun。worker 活着时 EndRun 钩子先到，这里全部空转；崩溃后由这里收口。
func (ts *TaskScheduler) reconcileFinishedRuns(ctx context.Context) {
	var rows []struct {
		TaskID string `gorm:"column:task_id"`
		RunID  string `gorm:"column:run_id"`
	}
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT t.id AS task_id, r.id AS run_id
  FROM agent_tasks t JOIN runs r ON r.id = t.current_run_id
 WHERE t.status = ? AND r.status <> 'running'`, agentTaskStatusRunning).Scan(&rows).Error; err != nil {
		log.Printf("task scheduler reconcile list: %v", err)
		return
	}
	for _, r := range rows {
		if err := ts.store.ReconcileRun(ctx, RunID(r.RunID)); err != nil {
			log.Printf("task scheduler reconcile %s: %v", r.RunID, err)
		}
	}
}

// detectInterruptedTasks 找「Task running 但当前 run 租约已过期」的行：状态不翻（读侧 interrupted），
// 只写去重的 task_interrupted 通知（dedup 含 run 与 epoch——恢复同一代不重复写，§9.1）。
func (ts *TaskScheduler) detectInterruptedTasks(ctx context.Context) {
	var rows []struct {
		TaskID          string `gorm:"column:task_id"`
		TenantID        string `gorm:"column:tenant_id"`
		OrchestrationID string `gorm:"column:orchestration_id"`
		Receiver        string `gorm:"column:source_session_id"`
		RunID           string `gorm:"column:run_id"`
		Epoch           int64  `gorm:"column:lease_epoch"`
	}
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT t.id AS task_id, t.tenant_id, t.orchestration_id, o.source_session_id, r.id AS run_id, r.lease_epoch
  FROM agent_tasks t
  JOIN runs r ON r.id = t.current_run_id
  JOIN agent_orchestrations o ON o.id = t.orchestration_id
 WHERE t.status = ? AND r.status = 'running' AND r.lease_until < now()`, agentTaskStatusRunning).Scan(&rows).Error; err != nil {
		log.Printf("task scheduler interrupted list: %v", err)
		return
	}
	for _, r := range rows {
		err := ts.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return ts.store.insertNotificationTx(tx, AgentNotification{
				TenantID: r.TenantID, OrchestrationID: r.OrchestrationID, TaskID: r.TaskID,
				ReceiverSessionID: SessionID(r.Receiver), Kind: notifKindTaskInterrupted,
				DedupKey: fmt.Sprintf("%s:%s:%s:%d", notifKindTaskInterrupted, r.TaskID, r.RunID, r.Epoch),
				Status:   notifStatusPending,
			})
		})
		if err != nil {
			log.Printf("task scheduler interrupted notify %s: %v", r.TaskID, err)
			continue
		}
		// 通知落库即叫醒：来源会话空闲时下一拍就能开恢复交付，不必等慢速对账。
		ts.store.PublishTaskWake(ctx, r.TenantID, "task_interrupted", r.TaskID)
	}
}

// PropagateBlockedTasks 把 failed／cancelled 节点的全部未运行后代写 blocked／dependency_failed
// （§8.4）：递归 CTE 求坏节点的后代闭包，再对其中还 pending 的行落 blocked。running 的后代
// 不受影响（独立分支照跑，结果由对账裁决）；已经 blocked 的行幂等不动。
// 每件被翻状态的 Task 发一条任务事件（§10.3：状态翻转在 PG 成功后发）——
// 否则页面上被挡住的下游要等到别的什么事件才从 pending 刷成 blocked。
func (s *Store) PropagateBlockedTasks(ctx context.Context) (int64, error) {
	var rows []struct {
		ID       string `gorm:"column:id"`
		TenantID string `gorm:"column:tenant_id"`
	}
	err := s.db.WithContext(ctx).Raw(`
WITH RECURSIVE bad AS (
  SELECT l.child_task_id AS id
    FROM agent_task_links l JOIN agent_tasks p ON p.id = l.parent_task_id
   WHERE p.status IN ('failed', 'cancelled')
  UNION
  SELECT l.child_task_id FROM agent_task_links l JOIN bad b ON b.id = l.parent_task_id
)
UPDATE agent_tasks SET status = ?, error_code = ?, error_message = ?, finished_at = now(), updated_at = now()
 WHERE status = ? AND id IN (SELECT id FROM bad)
RETURNING id, tenant_id`,
		agentTaskStatusBlocked, taskErrDependencyFailed, "父任务失败，这件任务不能执行", agentTaskStatusPending).
		Scan(&rows).Error
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		s.PublishEvent(ctx, r.TenantID, eventAgentTasksChanged, r.ID)
	}
	return int64(len(rows)), nil
}

// PromoteReadyTasks 把 parents 全部 succeeded 的 pending 提成 ready（§8.2 第 3 步）。
// 无 parents 的 pending 不该存在（创建时就落 ready），但谓词对空 parents 集合恒真，
// 万一出现也会被提走，不会卡死。被提走的每件同样发任务事件。
func (s *Store) PromoteReadyTasks(ctx context.Context) (int64, error) {
	var rows []struct {
		ID       string `gorm:"column:id"`
		TenantID string `gorm:"column:tenant_id"`
	}
	err := s.db.WithContext(ctx).Raw(`
UPDATE agent_tasks t SET status = ?, updated_at = now()
 WHERE t.status = ?
   AND t.orchestration_id IN (SELECT id FROM agent_orchestrations WHERE status = ?)
   AND NOT EXISTS (
     SELECT 1 FROM agent_task_links l JOIN agent_tasks p ON p.id = l.parent_task_id
      WHERE l.child_task_id = t.id AND p.status <> ?)
RETURNING t.id, t.tenant_id`,
		agentTaskStatusReady, agentTaskStatusPending, orchStatusActive, agentTaskStatusSucceeded).
		Scan(&rows).Error
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		s.PublishEvent(ctx, r.TenantID, eventAgentTasksChanged, r.ID)
	}
	return int64(len(rows)), nil
}

// finishCancellingRuns 给 cancelling 组里还 running 的节点补停止（§8.2 第 4 步 / §8.4）：
// 复用 StopRun（条件 UPDATE，不复活终态）+ 本进程注册表取消。API 停止端点已对它抓到的
// current_run_ids 做过一轮；这里兜的是停止之后才领取、或租约过期等漏网情形。
func (ts *TaskScheduler) finishCancellingRuns(ctx context.Context) {
	var rows []struct {
		TaskID string `gorm:"column:task_id"`
		RunID  string `gorm:"column:run_id"`
	}
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT t.id AS task_id, t.current_run_id AS run_id
  FROM agent_tasks t JOIN agent_orchestrations o ON o.id = t.orchestration_id
 WHERE o.status = ? AND t.status = ? AND t.current_run_id IS NOT NULL`,
		orchStatusCancelling, agentTaskStatusRunning).Scan(&rows).Error; err != nil {
		log.Printf("task scheduler cancelling list: %v", err)
		return
	}
	for _, r := range rows {
		status, found, err := ts.store.StopRun(ctx, RunID(r.RunID))
		if err != nil {
			log.Printf("task scheduler cancelling stop %s: %v", r.RunID, err)
			continue
		}
		if found && status == "stopped" {
			if cancel, hit := ts.worker.runCancels.Load(RunID(r.RunID)); hit {
				cancel.(*runCancel).cancel()
			}
		}
	}
}

// AdvanceOrchestrations 把「节点全部终态」的组收口（§8.4 第 2 条、§9.1、§9.3）：
// cancelling → cancelled（不写通知——取消没有东西可交付）；active 组看最终 Task：
// succeeded → 组 succeeded + delivery_status=pending + orchestration_finished 通知；
// 其余（failed / blocked / cancelled）→ 组 failed。通知与组状态同一事务（§9.3）。
func (s *Store) AdvanceOrchestrations(ctx context.Context) (int, error) {
	var ids []string
	if err := s.db.WithContext(ctx).Raw(`
SELECT o.id FROM agent_orchestrations o
 WHERE o.status IN (?, ?)
   AND (o.status = ? OR o.plan_hold = false)
   AND NOT EXISTS (
     SELECT 1 FROM agent_tasks t WHERE t.orchestration_id = o.id
       AND t.status IN (?, ?, ?, ?))`,
		orchStatusActive, orchStatusCancelling, orchStatusCancelling,
		agentTaskStatusPending, agentTaskStatusReady, agentTaskStatusRunning, agentTaskStatusBlocked).
		Scan(&ids).Error; err != nil {
		return 0, err
	}
	advanced := 0
	for _, id := range ids {
		tenantID := ""
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var o AgentOrchestration
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&o).Error; err != nil {
				return err
			}
			if o.Status != orchStatusActive && o.Status != orchStatusCancelling {
				return nil // 锁后复查：另一个副本先收口了
			}
			if o.Status == orchStatusActive && o.PlanHold {
				return nil // 计划暂停：锁后复查，等 continue／revision 放行
			}
			tenantID = o.TenantID
			var final AgentTask
			if err := tx.Where("id = ?", o.FinalTaskID).Take(&final).Error; err != nil {
				return err
			}
			group := orchStatusFailed
			if o.Status == orchStatusCancelling {
				group = orchStatusCancelled
			} else if final.Status == agentTaskStatusSucceeded {
				group = orchStatusSucceeded
			}
			updates := map[string]any{"status": group, "finished_at": nowUTC()}
			if group == orchStatusSucceeded {
				updates["delivery_status"] = deliveryStatusPending
			}
			if err := tx.Model(&AgentOrchestration{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
			if o.Status == orchStatusCancelling {
				return nil // 取消收口不写通知（§9.1：只有失败/中断/整组完成进模型）
			}
			return s.insertNotificationTx(tx, AgentNotification{
				TenantID: o.TenantID, OrchestrationID: o.ID,
				ReceiverSessionID: o.SourceSessionID, Kind: notifKindOrchFinished,
				DedupKey: fmt.Sprintf("%s:%s:%d", notifKindOrchFinished, o.ID, final.ResultRevision),
				Status:   notifStatusPending,
			})
		})
		if err != nil {
			log.Printf("task scheduler advance %s: %v", id, err)
			continue
		}
		advanced++
		// 组事件按真实租户发：PublishEvent 对空租户是空操作，早先这里发空串——
		// 组收口／交付翻转在页面上永远等不到事件，全靠任务事件捎带刷新。
		s.PublishEvent(ctx, tenantID, eventAgentOrchestrationsChanged, id)
	}
	return advanced, nil
}

// ClaimReadyInGroup 在一个组内原子领取至多 (组上限 − 活数) 件 ready Task（§8.3）：
// 锁组行 → 锁候选行 → 锁定后重读状态 → 组容量（running 且租约有效才算活，interrupted 不占）
// → 租户容量（lockTenantGate，与插 run 同事务）→ 铸执行 session（kind=agent_task）+ run +
// 首条输入（run_model_messages / runlog user 行）→ 翻 Task 行。Redis runlive 在 PG 写完之后、
// COMMIT 之前写——失败整笔回滚，「什么都没写」与 AcquireRun 同一条纪律。
// 不在这里装配模型、不启动 goroutine：启动是提交后的事（§6.2 第 5 步）。
func (s *Store) ClaimReadyInGroup(ctx context.Context, orchID string) ([]agentTaskClaim, error) {
	var claims []agentTaskClaim
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", orchID).Take(&o).Error; err != nil {
			return err
		}
		// plan_hold = 检查点已触发：本组停止新领取（在途执行不受影响）。
		if o.Status != orchStatusActive || o.PlanHold {
			return nil
		}
		var owner struct {
			UserID string `gorm:"column:user_id"`
		}
		if err := tx.Raw(`SELECT user_id FROM sessions WHERE id = ?`, o.SourceSessionID).Scan(&owner).Error; err != nil {
			return err
		}
		if owner.UserID == "" {
			return fmt.Errorf("claim %s: 来源会话缺失", orchID)
		}
		var live int64
		if err := tx.Raw(`
SELECT COUNT(*) FROM agent_tasks t JOIN runs r ON r.id = t.current_run_id
 WHERE t.orchestration_id = ? AND t.status = ? AND r.status = 'running' AND r.lease_until > now()`,
			orchID, agentTaskStatusRunning).Scan(&live).Error; err != nil {
			return err
		}
		budget := orchestrationConcurrency - int(live)
		if budget <= 0 {
			return nil
		}
		var candidates []AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("orchestration_id = ? AND status = ?", orchID, agentTaskStatusReady).
			Order("created_at, id").Limit(budget).Find(&candidates).Error; err != nil {
			return err
		}
		for _, t := range candidates {
			if t.Status != agentTaskStatusReady {
				continue // 锁定后重读；理论上恒真，防御一行
			}
			var parents []AgentTask
			if err := tx.Raw(`
SELECT t.* FROM agent_task_links l JOIN agent_tasks t ON t.id = l.parent_task_id
 WHERE l.child_task_id = ? AND t.status = ? ORDER BY t.task_key`,
				t.ID, agentTaskStatusSucceeded).Scan(&parents).Error; err != nil {
				return err
			}
			firstInput := buildTaskFirstInput(o.Goal, t, parents)
			// 租户容量：数与插同事务，每件候选单独过一遍（一次事务领多件时不能共用一次计数）。
			if err := s.lockTenantGate(tx, o.TenantID); err != nil {
				return err
			}
			sessID, runID, err := s.insertTaskExecutionRun(tx, o, t, owner.UserID, firstInput)
			if err != nil {
				return err
			}
			claims = append(claims, agentTaskClaim{
				TaskID: t.ID, TaskKey: t.TaskKey, SessionID: sessID, RunID: runID,
				TenantID: o.TenantID, UserID: owner.UserID, WorkspaceID: t.WorkspaceID,
				Epoch: 1, FirstInput: firstInput,
				OrchestrationID: o.ID, SourceRunID: o.SourceRunID,
			})
		}
		// Redis 半殿后（AcquireRun 同式）：任一失败整笔回滚。
		start, err := json.Marshal(Frame{Kind: "start"})
		if err != nil {
			return err
		}
		for _, c := range claims {
			if err := s.runliveStart(ctx, c.RunID, 1, string(start)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// insertTaskExecutionRun 铸一场 Task 执行：执行 session（kind=agent_task，不进普通聊天列表）→
// run 行（出生即 running、epoch 1、租约走 DB 时钟）→ mh seq1 user 行 → runlog user 行
// （internal=false：执行 run 的指令对执行 session 的账本是正常内容）→ 翻 Task 行
// （running + execution_session_id + current_run_id + started_at）。
func (s *Store) insertTaskExecutionRun(tx *gorm.DB, o AgentOrchestration, t AgentTask, userID, firstInput string) (SessionID, RunID, error) {
	sessID, err := newSessionID()
	if err != nil {
		return "", "", err
	}
	runID, err := newRunID()
	if err != nil {
		return "", "", err
	}
	msgID, err := newRowID("msg_")
	if err != nil {
		return "", "", err
	}
	logID, err := newRowID("rl_")
	if err != nil {
		return "", "", err
	}
	if err := tx.Exec(`SAVEPOINT task_claim`).Error; err != nil {
		return "", "", err
	}
	if err := tx.Exec(`
INSERT INTO sessions (id, title, tenant_id, user_id, visibility, workspace_id, kind, created_at)
 VALUES (?, ?, ?, ?, 'private', ?, ?, now())`,
		sessID, truncateTitle(t.Content), o.TenantID, userID, t.WorkspaceID, sessionKindAgentTask).Error; err != nil {
		_ = tx.Exec(`ROLLBACK TO SAVEPOINT task_claim`).Error
		return "", "", err
	}
	if err := tx.Exec(`
INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
 VALUES (?, ?, 'running', 1, now() + make_interval(secs => ?), ?, now())`,
		runID, sessID, s.leaseTTL.Seconds(), t.WorkspaceID).Error; err != nil {
		_ = tx.Exec(`ROLLBACK TO SAVEPOINT task_claim`).Error
		return "", "", err
	}
	if err := tx.Exec(`
INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
 VALUES (?, ?, ?, 1, 'user', ?, false, now())`,
		msgID, sessID, runID, firstInput).Error; err != nil {
		_ = tx.Exec(`ROLLBACK TO SAVEPOINT task_claim`).Error
		return "", "", err
	}
	if err := tx.Exec(`
INSERT INTO runlog_entries (id, session_id, seq, run_id, role, message_id, text, created_at)
 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runlog_entries WHERE session_id = ?),
         ?, 'user', ?, ?, now())`,
		logID, sessID, sessID, runID, msgID, firstInput).Error; err != nil {
		_ = tx.Exec(`ROLLBACK TO SAVEPOINT task_claim`).Error
		return "", "", err
	}
	res := tx.Model(&AgentTask{}).
		Where("id = ? AND status = ?", t.ID, agentTaskStatusReady).
		Updates(map[string]any{
			"status": agentTaskStatusRunning, "execution_session_id": string(sessID),
			"current_run_id": string(runID), "started_at": nowUTC(),
		})
	if res.Error != nil {
		_ = tx.Exec(`ROLLBACK TO SAVEPOINT task_claim`).Error
		return "", "", res.Error
	}
	if res.RowsAffected == 0 {
		_ = tx.Exec(`ROLLBACK TO SAVEPOINT task_claim`).Error
		return "", "", fmt.Errorf("claim %s: 领取竞争失败", t.ID)
	}
	return sessID, runID, nil
}

// TaskScheduler 是持久 Task 的调度循环。每副本一个；正确性由 PG 行锁、条件更新与
// 唯一索引承担（§8.1）。assemble 是装配口（普通聊天 profile / Task 执行 profile 各自实现），
// 由 main 装配时注入；测试可传 nil 只验存储半。
type TaskScheduler struct {
	store    *Store
	worker   *Worker
	interval time.Duration
	// assemble 用来源 session 的 mAgent 身份装配协调 run 的 profile（§9.2）；失败回 *assembleError。
	assemble func(ctx context.Context, p principal, agentID string) (*RunProfile, *assembleError)
	// assembleTask 装配 Task 执行者的 profile（agent_task_runner.go）；失败回 *assembleError。
	assembleTask func(ctx context.Context, p principal, taskID string) (*RunProfile, *assembleError)
	// inputTimeout 是 Task 提问的截止时长；装配执行者时进身份盒（openWaitRow 据此写 expires_at）。
	inputTimeout time.Duration
	// retryAfter 是装配失败后的进程内重试节流（通知 ID → 最早重试时刻）：只管节奏不管正确性，
	// 认领与否的真值永远在 PG。
	retryAfter sync.Map
}

func NewTaskScheduler(store *Store, worker *Worker, interval time.Duration) *TaskScheduler {
	return &TaskScheduler{store: store, worker: worker, interval: interval}
}

// AttachTaskScheduler 把 Server 的两条装配口交给调度器（main 装配时用）：普通聊天的
// assembleRun 给协调 run，agent_task_runner.go 的装配给 Task 执行者。
func (s *Server) AttachTaskScheduler(ts *TaskScheduler) {
	ts.assemble = s.assembleRun
	ts.assembleTask = s.assembleAgentTaskRun
	ts.inputTimeout = s.taskInputTimeout
}

// Start 显式启动调度循环（§8.1：不藏在路由注册里），返回停止函数。
func (ts *TaskScheduler) Start(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	go ts.Run(ctx)
	return cancel
}

// Run 是事件唤醒 + 慢速对账的调度循环（doc/72 §9.2）：
//   - 启动时先记下内部流位置，再完整跑一轮（启动窗口不靠事件补）；
//   - 之后 XREAD BLOCK 等 tk:agent-task-wake，最长 5 秒空醒只检查 ctx 与对账时钟；
//   - 有事件立即跑同一轮 RunOnce；距上次完整对账达到 interval 也跑一轮；
//   - 单副本不并发跑 RunOnce：一次读到的多条事件合并成一轮；Redis 错误退避 1 秒；
//   - 不用消费者组分配业务事实——各副本都可醒，执行权仍由 PG 裁决。
func (ts *TaskScheduler) Run(ctx context.Context) {
	lastID := "$"
	if rows, err := ts.store.rdb.XRevRangeN(ctx, keyTaskWake, "+", "-", 1).Result(); err == nil && len(rows) > 0 {
		lastID = rows[0].ID
	}
	ts.RunOnce(ctx)
	lastReconcile := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		wait := 5 * time.Second
		if remaining := ts.interval - time.Since(lastReconcile); remaining < wait {
			if remaining < 100*time.Millisecond {
				remaining = 100 * time.Millisecond
			}
			wait = remaining
		}
		res, err := ts.store.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{keyTaskWake, lastID},
			Count:   64,
			Block:   wait,
		}).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, redis.Nil) {
				log.Printf("task scheduler wake: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
		var triggered bool
		lastID, triggered = wakeBatch(res, lastID)
		if triggered || time.Since(lastReconcile) >= ts.interval {
			ts.RunOnce(ctx)
			lastReconcile = time.Now()
		}
	}
}

// wakeBatch 把一次 XRead 读到的多条唤醒合并成一轮（§9.2：不按事件逐条跑）：
// 事件只带诊断字段、不承载事实，所以只看最后一条推进位置。
func wakeBatch(streams []redis.XStream, lastID string) (string, bool) {
	triggered := false
	for _, s := range streams {
		if n := len(s.Messages); n > 0 {
			lastID = s.Messages[n-1].ID
			triggered = true
		}
	}
	return lastID, triggered
}

// RunOnce 是一轮调度（§8.2 的顺序）。单轮函数独立成方法：测试不依赖 ticker。
func (ts *TaskScheduler) RunOnce(ctx context.Context) {
	ts.reconcileFinishedRuns(ctx)
	ts.detectInterruptedTasks(ctx)
	ts.expireTaskInputs(ctx)
	if n, err := ts.store.PropagateBlockedTasks(ctx); err != nil {
		log.Printf("task scheduler propagate: %v", err)
	} else if n > 0 {
		log.Printf("task scheduler: blocked %d tasks", n)
	}
	if n, err := ts.store.PromoteReadyTasks(ctx); err != nil {
		log.Printf("task scheduler promote: %v", err)
	} else if n > 0 {
		log.Printf("task scheduler: promoted %d tasks", n)
	}
	ts.finishCancellingRuns(ctx)
	if _, err := ts.store.AdvanceOrchestrations(ctx); err != nil {
		log.Printf("task scheduler advance: %v", err)
	}
	ts.reconcileDeliveries(ctx)
	ts.claimReady(ctx)
	ts.consumeNotifications(ctx)
}

// claimReady 找有 ready 节点的 active 组逐组领取；领取成功后装配并启动（提交后启动，§6.2 第 5 步）。
func (ts *TaskScheduler) claimReady(ctx context.Context) {
	var orchIDs []string
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT o.id FROM agent_orchestrations o
 WHERE o.status = ? AND o.plan_hold = false AND EXISTS (
   SELECT 1 FROM agent_tasks t WHERE t.orchestration_id = o.id AND t.status = ?)
 ORDER BY o.created_at, o.id`, orchStatusActive, agentTaskStatusReady).Scan(&orchIDs).Error; err != nil {
		log.Printf("task scheduler claim list: %v", err)
		return
	}
	for _, id := range orchIDs {
		claims, err := ts.store.ClaimReadyInGroup(ctx, id)
		if err != nil {
			log.Printf("task scheduler claim %s: %v", id, err)
			continue
		}
		for _, c := range claims {
			log.Printf("task scheduler: claimed task %s (key %s) as run %s", c.TaskID, c.TaskKey, c.RunID)
			ts.launchTaskRun(ctx, c)
		}
	}
}

// expiredTaskInput 是一行「Task 当前 run 上已到期的待答问题」候选。
type expiredTaskInput struct {
	QuestionID      string `gorm:"column:question_id"`
	TaskID          string `gorm:"column:task_id"`
	TenantID        string `gorm:"column:tenant_id"`
	OrchestrationID string `gorm:"column:orchestration_id"`
	RunID           string `gorm:"column:run_id"`
	Epoch           int64  `gorm:"column:lease_epoch"`
}

// listExpiredTaskInputs 走 (status, expires_at) 索引取候选：普通主会话问题 expires_at 恒 NULL，
// 永不进候选；taskID 非空时只查这一件（resume 前置检查用）；限批防积压。
func (s *Store) listExpiredTaskInputs(ctx context.Context, taskID string) ([]expiredTaskInput, error) {
	q := `
SELECT q.id AS question_id, t.id AS task_id, t.tenant_id, t.orchestration_id,
       r.id AS run_id, r.lease_epoch
  FROM run_questions q
  JOIN runs r ON r.id = q.run_id
  JOIN agent_tasks t ON t.current_run_id = r.id
 WHERE q.status = ? AND q.expires_at IS NOT NULL AND q.expires_at <= now()
   AND t.status = ? AND r.status = 'running'`
	args := []any{questionStatusPending, agentTaskStatusRunning}
	if taskID != "" {
		q += ` AND t.id = ?`
		args = append(args, taskID)
	}
	q += ` ORDER BY q.expires_at, q.id LIMIT 64`
	var rows []expiredTaskInput
	err := s.db.WithContext(ctx).Raw(q, args...).Scan(&rows).Error
	return rows, err
}

// expireTaskInputs 收口 Task 提问超时（doc/72 §7.3）：找当前执行 run 上「pending 且已到期」的问题，
// 按统一锁顺序（组 → Task → 问题）逐件落失败。回答与超时由行锁/CAS 决定唯一赢家：
// answer 先赢，这里查不到 pending；这里先赢，迟到的 answer 回 115102。
//
// 单件失败只记日志：下一轮同一行仍在（状态没翻就还是 pending），幂等重试；不中断整轮。
func (ts *TaskScheduler) expireTaskInputs(ctx context.Context) {
	rows, err := ts.store.listExpiredTaskInputs(ctx, "")
	if err != nil {
		log.Printf("task scheduler timeout list: %v", err)
		return
	}
	for _, r := range rows {
		runID := RunID(r.RunID)
		failed, err := ts.store.expireTaskInput(ctx, r.TaskID, r.OrchestrationID, runID, r.QuestionID, r.Epoch)
		if err != nil {
			log.Printf("task scheduler timeout %s: %v", r.TaskID, err)
			continue
		}
		if !failed {
			continue
		}
		// Task 已 failed；停 run 走既有条件更新（已是终态则原样读回），再取消本进程内的执行。
		if status, found, err := ts.store.StopRun(ctx, runID); err != nil {
			log.Printf("task scheduler timeout stop %s: %v", runID, err)
		} else if found && status == "stopped" {
			if cancel, hit := ts.worker.runCancels.Load(runID); hit {
				cancel.(*runCancel).cancel()
			}
		}
		ts.store.PublishEvent(ctx, r.TenantID, eventAgentTasksChanged, r.TaskID)
		ts.store.PublishTaskWake(ctx, r.TenantID, "task_timeout", r.TaskID)
	}
}

// expireTaskInputsForTask 是 resume 前置检查：这件 Task 当前 run 若已有到期问题，先按同一服务
// 收口（与调度器同一事务路径），收口后 resume 只能看到 failed 并回 117103。
func (s *Store) expireTaskInputsForTask(ctx context.Context, taskID string) (bool, error) {
	rows, err := s.listExpiredTaskInputs(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		failed, err := s.expireTaskInput(ctx, r.TaskID, r.OrchestrationID, RunID(r.RunID), r.QuestionID, r.Epoch)
		if err != nil {
			return false, err
		}
		if failed {
			s.PublishEvent(ctx, r.TenantID, eventAgentTasksChanged, r.TaskID)
			s.PublishTaskWake(ctx, r.TenantID, "task_timeout", r.TaskID)
			return true, nil
		}
	}
	return false, nil
}

// expireTaskInput 是超时收口的单件事务。锁顺序固定「组 → Task → 问题」：修订、领取、组收口
// 都按这一顺序拿锁，这里不能反向。锁后复查三件事：Task 仍 running、问题仍 pending 且仍到期
// （到期判定用数据库时钟，不用进程时钟）。返回 true = 这次真的把 Task 翻成了失败
// （没赢的并发方返回 false，不发事件）。
func (s *Store) expireTaskInput(ctx context.Context, taskID, orchID string, runID RunID, questionID string, epoch int64) (bool, error) {
	failed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", orchID).Take(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var t AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", taskID).Take(&t).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if t.Status != agentTaskStatusRunning || t.CurrentRunID == nil || *t.CurrentRunID != string(runID) {
			return nil // 已被对账 / 被停止收走 / 换了执行代：幂等空转
		}
		var q struct {
			Status  string `gorm:"column:status"`
			Expired bool   `gorm:"column:expired"`
		}
		if err := tx.Raw(`SELECT status, (expires_at IS NOT NULL AND expires_at <= now()) AS expired
  FROM run_questions WHERE id = ? FOR UPDATE`, questionID).Scan(&q).Error; err != nil {
			return err
		}
		if q.Status != questionStatusPending || !q.Expired {
			return nil // 回答先赢，或锁等待期间这一行已经变了
		}
		r := tx.Model(&AgentTask{}).Where("id = ? AND status = ?", taskID, agentTaskStatusRunning).
			Updates(map[string]any{
				"status": agentTaskStatusFailed, "error_code": taskErrInputTimeout,
				"error_message": "等待回答已超时", "finished_at": nowUTC(),
			})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil
		}
		failed = true
		// 这场 run 其余 pending 问题一并 stale：Task 已经失败，再答也注不回任何执行者。
		if err := tx.Exec(`UPDATE run_questions SET status = ? WHERE run_id = ? AND status = ?`,
			questionStatusStale, runID, questionStatusPending).Error; err != nil {
			return err
		}
		return s.insertNotificationTx(tx, AgentNotification{
			TenantID: t.TenantID, OrchestrationID: t.OrchestrationID, TaskID: t.ID,
			ReceiverSessionID: o.SourceSessionID, Kind: notifKindTaskFailed,
			DedupKey:  fmt.Sprintf("%s:%s:%s:%d", notifKindTaskFailed, t.ID, runID, epoch),
			ErrorCode: taskErrInputTimeout, Status: notifStatusPending,
		})
	})
	return failed, err
}

// consumeNotifications 消费持久协调通知（§9.2）——实现在 agent_task_delivery.go。
func (ts *TaskScheduler) consumeNotifications(ctx context.Context) {
	ts.consumeAgentNotifications(ctx)
}
