// 本文件是持久协调通知的消费与最终交付（方案真源 = doc/70 §9；施工包 §9.2/§9.3）。
//
// 三类事件才进模型（§9.1）：task_failed / task_interrupted / orchestration_finished——普通进度
// 只进 run 账本。消费纪律：来源会话忙不认领（保持 pending，下一轮再看）；认领 + 铸协调 run +
// 写 claim_run_id 在同一笔事务里（崩溃不会留下「已认领但永远没有 run」的通知）；整组交付的
// deliveryStatus 三步（pending → delivering → delivered）各由条件更新推进，重复通知、引擎重启、
// 两个副本同时消费都造不出第二场交付 run（§9.3）。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// coordinationBox 是协调 run 的身份盒：worker.run 据此打 trace 属性（doc/70 §10.3——
// 协调／交付 run 增加组和通知 ID），交付对账据此不掺和（协调 run 不进 Task 对账）。
type coordinationBox struct {
	OrchestrationID string
	NotificationID  string
	Kind            string
	// Attempt 是这次通知的第几次处理（doc/73 §6.3）：首次认领为 1，崩溃 resume 不变，
	// 只有人工「重新处理」才 +1。trace 与日志用它区分同一通知的不同 run。
	Attempt int
}

type coordinationBoxKey struct{}

// coordinationClaim 是 resume 前反查到的协调身份（doc/73 §6.4）：只有「通知当前 claim_run_id
// 指向这场 run 且通知仍 claimed」才算；旧 attempt 的 run 不是当前 claim 时必须独立判出来，
// 以便按既有 112102 拒绝、不执行 ClaimResume。
type coordinationClaim struct {
	NotificationID  string
	OrchestrationID string
	Kind            string
	AgentID         string
	Attempt         int
}

// CoordinationClaimForRun 按 run 反查当前协调认领。没有 = 不是协调 run 或通知已不归这场 run。
// 就地认领（claimed_in_place=true）不算协调身份（doc/82 §4-12）：那条通知的 claim_run_id 指向的
// 是来源聊天 run，把它当成交付身份会让普通聊天 run 在 resume 时被裁成只读工具面。
func (s *Store) CoordinationClaimForRun(ctx context.Context, runID RunID) (*coordinationClaim, error) {
	var row struct {
		NotificationID  string `gorm:"column:notification_id"`
		OrchestrationID string `gorm:"column:orchestration_id"`
		Kind            string `gorm:"column:kind"`
		AgentID         string `gorm:"column:agent_id"`
		Attempt         int    `gorm:"column:attempt"`
	}
	res := s.db.WithContext(ctx).Raw(`
SELECT n.id AS notification_id, n.orchestration_id, n.kind,
       COALESCE(s.agent_id, '') AS agent_id,
       COALESCE((SELECT MAX(a.attempt) FROM agent_notification_attempts a WHERE a.run_id = n.claim_run_id), 0) AS attempt
  FROM agent_notifications n
  JOIN sessions s ON s.id = n.receiver_session_id
 WHERE n.claim_run_id = ? AND n.status = ? AND NOT n.claimed_in_place`, runID, notifStatusClaimed).Scan(&row)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 || row.NotificationID == "" {
		return nil, nil
	}
	return &coordinationClaim{
		NotificationID: row.NotificationID, OrchestrationID: row.OrchestrationID,
		Kind: row.Kind, AgentID: row.AgentID, Attempt: row.Attempt,
	}, nil
}

// IsStaleCoordinationAttempt 报告这场 run 是否是某条通知的历史尝试、且已不是它当前的认领者
// （doc/73 §6.4 第 2 条）：是的话不得 ClaimResume——旧交付不能借恢复复活。
func (s *Store) IsStaleCoordinationAttempt(ctx context.Context, runID RunID) (bool, error) {
	var one int
	err := s.db.WithContext(ctx).Raw(`
SELECT 1 FROM agent_notification_attempts a
  JOIN agent_notifications n ON n.id = a.notification_id
 WHERE a.run_id = ? AND (n.claim_run_id <> a.run_id OR n.status <> ?) LIMIT 1`,
		runID, notifStatusClaimed).Scan(&one).Error
	if err != nil {
		return false, err
	}
	return one == 1, nil
}

func withCoordinationBox(ctx context.Context, b *coordinationBox) context.Context {
	return context.WithValue(ctx, coordinationBoxKey{}, b)
}

func coordinationBoxFrom(ctx context.Context) *coordinationBox {
	b, _ := ctx.Value(coordinationBoxKey{}).(*coordinationBox)
	return b
}

// coordLaunch 是一次成功认领的产出：提交后用这些身份装配并启动协调 run。
type coordLaunch struct {
	NotificationID string
	Kind           string
	OrchID         string
	SessionID      SessionID
	RunID          RunID
	TenantID       string
	UserID         string
	AgentID        string
	TaskID         string
	Attempt        int
}

// coordinationRecovery 是协调输入的恢复参数（doc/73 §6.2）：首次处理时 Attempt=1、
// 其余为空；人工新尝试时由重试端点从 PG 现读上一场 run 的终态与已保存的 root assistant 正文。
type coordinationRecovery struct {
	Attempt                int
	PreviousRunID          RunID
	PreviousTerminalStatus string
	PreviousRootAssistant  string
}

// CreateCoordinationAttempt 是初次认领与人工重试共用的事务内创建函数（doc/73 §6.1）：
// 调用方已锁通知行与组行、并完成了各自的准入判定；这里只做「创建一次处理尝试」的机械动作——
// run（epoch=1、现有租约）→ mh 首条 → internal runlog user 行 → attempts 行 → 通知翻 claimed /
// 改写当前 claim → 整组交付翻 delivering / 改写 delivery_run_id → Redis runlive。
// 任何一步失败整笔回滚（含 Redis），不会留下「已认领但永远没有 run」的通知。
//
// bash_job_finished 没有组（doc/82 §5）：租户/项目从 job 读（o 是零值，不写 agent_orchestrations
// 交付列）。
func (s *Store) CreateCoordinationAttempt(ctx context.Context, tx *gorm.DB, n *AgentNotification, o *AgentOrchestration,
	rp coordinationRecovery, input string) (*coordLaunch, error) {
	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	msgID, err := newRowID("msg_")
	if err != nil {
		return nil, err
	}
	logID, err := newRowID("rl_")
	if err != nil {
		return nil, err
	}
	attemptID, err := newRowID("ana_")
	if err != nil {
		return nil, err
	}
	if rp.Attempt < 1 {
		rp.Attempt = 1
	}
	// 租户/项目：编排通知取组；bash 通知取 job（o 为空、没有组交付列可写）。
	tenantID := o.TenantID
	workspaceID := o.WorkspaceID
	if n.Kind == notifKindBashJobFinished {
		j, err := notificationJob(tx, *n)
		if err != nil {
			return nil, err
		}
		tenantID = j.TenantID
		workspaceID = j.WorkspaceID
	}
	if err := tx.Exec(`
INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
 VALUES (?, ?, 'running', 1, now() + make_interval(secs => ?), ?, now())`,
		runID, n.ReceiverSessionID, s.leaseTTL.Seconds(), workspaceID).Error; err != nil {
		return nil, err
	}
	if err := tx.Exec(`
INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
 VALUES (?, ?, ?, 1, 'user', ?, false, now())`,
		msgID, n.ReceiverSessionID, runID, input).Error; err != nil {
		return nil, err
	}
	// internal=true 的 runlog user 行（doc/70 §9.2）：程序写进来的协调输入不是用户说的话，
	// 页面不渲染；用户只看到 mAgent 的交付回答。
	if err := tx.Exec(`
INSERT INTO runlog_entries (id, session_id, seq, run_id, role, message_id, text, internal, created_at)
 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runlog_entries WHERE session_id = ?),
         ?, 'user', ?, ?, true, now())`,
		logID, n.ReceiverSessionID, n.ReceiverSessionID, runID, msgID, input).Error; err != nil {
		return nil, err
	}
	// 每次创建 run 都记一行 attempts：run_id 唯一，双击／双副本的败者在这里或前面的行锁上退出。
	if err := tx.Exec(`
INSERT INTO agent_notification_attempts (id, tenant_id, notification_id, attempt, run_id, created_at)
 VALUES (?, ?, ?, ?, ?, now())`,
		attemptID, tenantID, n.ID, rp.Attempt, runID).Error; err != nil {
		return nil, err
	}
	if err := tx.Exec(`
UPDATE agent_notifications
   SET status = ?, claim_run_id = ?, claimed_at = now(), failed_at = NULL,
       consume_error_code = '', updated_at = now()
 WHERE id = ?`, notifStatusClaimed, runID, n.ID).Error; err != nil {
		return nil, err
	}
	if n.Kind == notifKindOrchFinished {
		if err := tx.Exec(`
UPDATE agent_orchestrations
   SET delivery_status = ?, delivery_run_id = ?, delivery_error_code = '', updated_at = now()
 WHERE id = ?`, deliveryStatusDelivering, runID, o.ID).Error; err != nil {
			return nil, err
		}
	}
	var sess struct {
		UserID  string `gorm:"column:user_id"`
		AgentID string `gorm:"column:agent_id"`
	}
	if err := tx.Raw(`SELECT user_id, COALESCE(agent_id, '') AS agent_id FROM sessions WHERE id = ?`,
		n.ReceiverSessionID).Scan(&sess).Error; err != nil {
		return nil, err
	}
	if sess.UserID == "" {
		return nil, fmt.Errorf("delivery %s: 来源会话缺失", n.ID)
	}
	start, err := json.Marshal(Frame{Kind: "start"})
	if err != nil {
		return nil, err
	}
	if err := s.runliveStart(ctx, runID, 1, string(start)); err != nil {
		return nil, err
	}
	return &coordLaunch{
		NotificationID: n.ID, Kind: n.Kind, OrchID: o.ID, TaskID: n.TaskID,
		SessionID: n.ReceiverSessionID, RunID: runID,
		TenantID: tenantID, UserID: sess.UserID, AgentID: sess.AgentID,
		Attempt: rp.Attempt,
	}, nil
}

// notificationJob 取一条 bash 通知指向的命令行（doc/82 §6.5）：认领铸交付 run 与拼正文
// 都要从 job 现读事实（终态、退出码、输出路径、命令原文、租户/项目/来源会话）。
func notificationJob(tx *gorm.DB, n AgentNotification) (BackgroundJob, error) {
	if n.BackgroundJobID == nil || *n.BackgroundJobID == "" {
		return BackgroundJob{}, fmt.Errorf("bash 通知 %s 缺少命令号", n.ID)
	}
	var j BackgroundJob
	if err := tx.Where("id = ?", *n.BackgroundJobID).Take(&j).Error; err != nil {
		return BackgroundJob{}, err
	}
	return j, nil
}

// consumeAgentNotifications 每轮取至多 8 条 pending 通知逐条消费。assemble 为 nil（单测）时
// 直接返回：认领而无法启动会把通知卡在 claimed，不如整步不做——测试只验存储半。
func (ts *TaskScheduler) consumeAgentNotifications(ctx context.Context) {
	if ts.assemble == nil {
		return
	}
	for _, id := range ts.pendingNotificationIDs(ctx) {
		if v, ok := ts.retryAfter.Load(id); ok {
			if at, ok := v.(time.Time); ok && time.Now().Before(at) {
				continue // 进程内节流：装配刚失败过的通知歇一分钟再试（只管节奏，正确性在 PG）
			}
			ts.retryAfter.Delete(id)
		}
		if launch := ts.claimNotification(ctx, id); launch != nil {
			ts.launchCoordinationRun(ctx, launch)
		}
	}
}

// pendingNotificationIDs 取本轮的候选：来源会话正忙、且认领后真会铸 run 的行留在 pending（§9.2），
// 但**不占本轮名额**——否则积压的忙通知恒占前几条，别的会话的交付永远排不到。已经终止的通知
// （组停/继续过了的 plan_review、已无交付可做的 orchestration_finished）与会话忙无关，照常进来收口。
// 忙检查在 claimNotification 的事务里锁行后仍会重做一次，这里只决定「先看谁」。
//
// LEFT JOIN 组（doc/82 §5）：bash_job_finished 通知没有编排组（orchestration_id 为空串），
// 也要进候选——它靠「接收会话无活 run」这一条被选中，由 claimNotification 铸一场交付 run。
// 前两条组相关判定对 bash 行恒为假（o 列为 NULL），不影响它进候选。
func (ts *TaskScheduler) pendingNotificationIDs(ctx context.Context) []string {
	var ids []string
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT n.id FROM agent_notifications n
  LEFT JOIN agent_orchestrations o ON o.id = n.orchestration_id
 WHERE n.status = ?
   AND (
     (n.kind = ? AND (o.status <> ? OR NOT o.plan_hold))
     OR (n.kind = ? AND o.delivery_status NOT IN (?, ?))
     OR NOT EXISTS (SELECT 1 FROM runs r
                     WHERE r.session_id = n.receiver_session_id
                       AND r.status = 'running' AND r.lease_until > now())
   )
 ORDER BY n.created_at, n.id LIMIT 8`,
		notifStatusPending,
		notifKindPlanReviewRequired, orchStatusActive,
		notifKindOrchFinished, deliveryStatusPending, deliveryStatusDelivering).
		Scan(&ids).Error; err != nil {
		log.Printf("task delivery list: %v", err)
		return nil
	}
	return ids
}

// claimNotification 认领一条通知并铸它的协调 run（§9.2）。同一笔事务里依次：
// 锁通知行（两副本抢同一条只有一个进得来）→ 锁组行（交付状态翻转与认领串行化）→
// 来源会话忙检查（有活 run 不认领，保持 pending）→ 通知翻 claimed + claim_run_id →
// 铸协调 run（runs 行 + mh 首条 + runlog internal=true 的 user 行）→ 整组交付的把
// deliveryStatus 推到 delivering → Redis runlive（AcquireRun 同一条纪律：PG 写完、COMMIT 前写，
// 失败整笔回滚）。
//
// bash_job_finished 没有组（doc/82 §5）：跳过组锁与组交付判定，租户/项目/来源会话与正文事实
// 从 job 读（CreateCoordinationAttempt / buildCoordinationInput 按 kind 分支）。
func (ts *TaskScheduler) claimNotification(ctx context.Context, notifID string) *coordLaunch {
	var launch *coordLaunch
	err := ts.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n AgentNotification
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", notifID).Take(&n).Error; err != nil {
			return err
		}
		if n.Status != notifStatusPending {
			return nil // 另一个副本先到了
		}
		var o AgentOrchestration
		if n.Kind != notifKindBashJobFinished {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", n.OrchestrationID).Take(&o).Error; err != nil {
				return err
			}
			if n.Kind == notifKindOrchFinished {
				// 整组交付只认领 delivery_status=pending 的组（§9.3）：delivering = 已有交付 run 在跑，
				// 重启后由对账补状态；delivered = 已交付，通知标 consumed 清账；failed 或没有交付状态
				// （组本身 failed／cancelled）不自动重放，通知到此终止——留在 pending 会永久占住消费名额。
				switch o.DeliveryStatus {
				case deliveryStatusPending:
				case deliveryStatusDelivering:
					return nil
				default:
					return tx.Model(&AgentNotification{}).Where("id = ?", n.ID).
						Updates(map[string]any{"status": notifStatusConsumed, "consumed_at": nowUTC()}).Error
				}
			}
			if n.Kind == notifKindPlanReviewRequired {
				// 计划复核只在「组仍 active 且仍暂停」时才有意义（施工包 §6.3）：人已经点了继续、
				// 组被停止或收口后，通知没有可复核的暂停了——标 consumed 清账，不造协调 run。
				// 计划复核不是最终交付，全程不写 delivery_status。
				if o.Status != orchStatusActive || !o.PlanHold {
					return tx.Model(&AgentNotification{}).Where("id = ?", n.ID).
						Updates(map[string]any{"status": notifStatusConsumed, "consumed_at": nowUTC()}).Error
				}
			}
		}
		// 来源会话忙检查（§9.2）：用户正在聊就不打断——通知留在 pending，下一轮再看。
		// 先同 AcquireRun 清扫租约已过的死行：忙检查按「租约仍有效」判，但唯一索引只认
		// status='running'，不清会撞 one_live_run_per_session 且每轮重试都撞（永不收敛）。
		if err := expireDeadSessionRunsTx(tx, n.ReceiverSessionID); err != nil {
			return err
		}
		var live int64
		if err := tx.Raw(
			`SELECT COUNT(*) FROM runs WHERE session_id = ? AND status = 'running' AND lease_until > now()`,
			n.ReceiverSessionID).Scan(&live).Error; err != nil {
			return err
		}
		if live > 0 {
			return nil
		}
		// 协调 run 的输入在锁内现读现拼：task_failed / task_interrupted 带单任务事实，
		// orchestration_finished 带最终 Task 的结果与产物。
		// attempt 取该通知已有最大值 +1：首次认领自然是 1；装配失败退回 pending 后重新认领
		// 会铸新 run，编号必须继续（(notification_id, attempt) 唯一是并发判负的数据库事实）。
		var maxAttempt int
		if err := tx.Raw(`SELECT COALESCE(MAX(attempt), 0) FROM agent_notification_attempts
 WHERE notification_id = ?`, n.ID).Scan(&maxAttempt).Error; err != nil {
			return err
		}
		rp := coordinationRecovery{Attempt: maxAttempt + 1}
		input, err := ts.store.buildCoordinationInput(ctx, tx, n, o, rp)
		if err != nil {
			return err
		}
		launch, err = ts.store.CreateCoordinationAttempt(ctx, tx, &n, &o, rp, input)
		return err
	})
	if err != nil {
		log.Printf("task delivery claim %s: %v", notifID, err)
		return nil
	}
	return launch
}

// launchCoordinationRun 启动协调 run：来源会话的 mAgent + 原用户身份（§9.2）。装配失败把
// run 收成 error、认领退回 pending（整组交付的 deliveryStatus 同步退回），进程内节流一分钟——
// 持续失败不烧库，瞬时故障下一轮自愈。
func (ts *TaskScheduler) launchCoordinationRun(ctx context.Context, l *coordLaunch) {
	p := principal{UserID: l.UserID, TenantID: l.TenantID, Role: "member"}
	bg := withCoordinationBox(launchContext(ctx), &coordinationBox{
		OrchestrationID: l.OrchID, NotificationID: l.NotificationID, Kind: l.Kind, Attempt: l.Attempt,
	})
	bg = withPrincipal(bg, p)
	prof, aerr := ts.assemble(bg, p, l.AgentID)
	if aerr != nil {
		log.Printf("task delivery %s: 协调 run 装配失败 (%s): %v", l.RunID, aerr.Message, aerr.Err)
		finCtx := context.WithoutCancel(bg)
		if _, ok, err := ts.store.EndRunErrorOnly(finCtx, l.SessionID, l.RunID); err == nil && ok {
			ts.store.revertDeliveryClaim(finCtx, l)
		}
		ts.retryAfter.Store(l.NotificationID, time.Now().Add(time.Minute))
		return
	}
	prof.Tools = coordinationTools(bg, l.Kind, prof.Tools)
	log.Printf("task delivery: launching %s notification %s attempt %d as run %s", l.Kind, l.NotificationID, l.Attempt, l.RunID)
	go ts.worker.Run(withRunProfile(bg, prof), l.SessionID, l.RunID, 1)
}

// coordinationTools 是协调 run 的工具面按职责分派（doc/73 §6.3）：初次启动、人工新尝试与
// resume 共用这一个入口。计划复核沿用 doc/72 的白名单；其他协调／交付 run 只拿只读文件工具——
// 「读取结果并说明」与「执行新工作」分开，恢复不会顺带获得 bash／写盘／MCP／子代理。
func coordinationTools(ctx context.Context, kind string, in []tool.BaseTool) []tool.BaseTool {
	if kind == notifKindPlanReviewRequired {
		return keepPlanReviewTools(ctx, in)
	}
	return keepDeliveryReadOnlyTools(ctx, in)
}

// planReviewToolNames 是计划复核 run 的全部工具（施工包 §4.8/§6.3）：只读文件与历史检索
// + 三把决策工具（改图、放行、换继任组）。没有 bash、写工具、记忆、技能、新建编排、等人工具——
// 复核 run 必须确定性终结（作出决定或什么都不做），不能把来源会话挂在这里等人。
var planReviewToolNames = map[string]bool{
	"read_text_file": true, "read_file": true, "read_pdf_text": true, "read_media_file": true,
	"list_directory": true, "directory_tree": true, "get_file_info": true, "search_files": true,
	"search_history":       true,
	"revise_orchestration": true, "continue_orchestration": true,
	"create_successor_orchestration": true,
}

// keepPlanReviewTools 反向过滤：名单之外一律拿掉（与 dropDeliveryNoTools 的黑名单相反，
// 因为复核面的正确失败方向是「新工具默认不可用」）。
func keepPlanReviewTools(ctx context.Context, in []tool.BaseTool) []tool.BaseTool {
	kept := make([]tool.BaseTool, 0, len(in))
	for _, t := range in {
		info, err := t.Info(ctx)
		if err != nil {
			continue // 拿不到名 = 不在名单里
		}
		if planReviewToolNames[info.Name] {
			kept = append(kept, t)
		}
	}
	return kept
}

// deliveryReadOnlyToolNames 是交付／说明 run 的全部工具（doc/73 §10）：只读文件与目录。
// 白名单而不是黑名单的理由：交付面的正确失败方向是「新工具默认不可用」——未来接进来的
// bash／写盘／MCP／子代理不需要记得再来这里补一条排除。
var deliveryReadOnlyToolNames = map[string]bool{
	"read_text_file": true, "read_file": true, "read_pdf_text": true, "read_media_file": true,
	"list_directory": true, "directory_tree": true, "get_file_info": true, "search_files": true,
}

// keepDeliveryReadOnlyTools 反向过滤：名单之外一律拿掉；拿不到工具名的也拿掉（无法证明只读）。
func keepDeliveryReadOnlyTools(ctx context.Context, in []tool.BaseTool) []tool.BaseTool {
	kept := make([]tool.BaseTool, 0, len(in))
	for _, t := range in {
		info, err := t.Info(ctx)
		if err != nil {
			continue
		}
		if deliveryReadOnlyToolNames[info.Name] {
			kept = append(kept, t)
		}
	}
	return kept
}

// revertDeliveryClaim 把一次启动失败的认领退回 pending：通知回 pending、清认领坐标；
// 整组交付的 deliveryStatus 回 pending、清 delivery_run_id（条件更新：只在 delivering 时退）。
func (s *Store) revertDeliveryClaim(ctx context.Context, l *coordLaunch) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&AgentNotification{}).Where("id = ? AND status = ?", l.NotificationID, notifStatusClaimed).
			Updates(map[string]any{"status": notifStatusPending, "claim_run_id": "", "claimed_at": nil}).Error; err != nil {
			return err
		}
		if l.Kind != notifKindOrchFinished {
			return nil
		}
		return tx.Model(&AgentOrchestration{}).
			Where("id = ? AND delivery_status = ? AND delivery_run_id = ?", l.OrchID, deliveryStatusDelivering, l.RunID).
			Updates(map[string]any{"delivery_status": deliveryStatusPending, "delivery_run_id": ""}).Error
	})
	if err != nil {
		log.Printf("task delivery revert %s: %v", l.NotificationID, err)
	}
}

// ClaimNotificationInline 由来源 run 就地认领一条通知（doc/82 §4-12；供 wait_task／bash_output 调用）：
// 锁通知行 → 校验 status=pending 且接收会话 == 当前 run 会话 → 写 status=claimed、claim_run_id=当前 run、
// claimed_at、claimed_in_place=true。**不建 runs 行、不做忙检查、不写 attempt 行**：
//   - attempt 行的 run_id 是唯一索引，一场 run 会先后认领 job 与 Task 两条通知，第二条会撞索引整笔回滚；
//   - 更关键的是 attempt 行就是「这场 run 是交付 run」的身份标记（CoordinationClaimForRun／启动回填
//     都以它为据），混入就地认领会让普通聊天 run 在 resume 时被裁成只读工具面。
//
// 组状态按 kind 分：orchestration_finished 与正常认领一样写 delivery_status=delivering、
// delivery_run_id=当前 run（否则该组永远停在 pending）；bash_job_finished 没有组状态可写。
// 用 status=pending 的条件更新，同 run 重复调用是幂等空操作。返回 true = 本次真认领。
func (s *Store) ClaimNotificationInline(ctx context.Context, notifID string, runID RunID, sessionID SessionID) (bool, error) {
	claimed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n AgentNotification
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", notifID).Take(&n).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if n.Status != notifStatusPending || n.ReceiverSessionID != sessionID {
			return nil // 已被别人认领 / 已消费 / 不是本会话的通知：不认领
		}
		if n.Kind == notifKindOrchFinished && n.OrchestrationID != "" {
			// 条件守卫：组已在别的尝试名下交付中（delivering）或已收口（delivered）时不覆写
			// delivery_run_id，也不认领通知——否则旧交付 run 的归属被就地认领冲掉，对账收不了口。
			res := tx.Exec(`
UPDATE agent_orchestrations
   SET delivery_status = ?, delivery_run_id = ?, delivery_error_code = '', updated_at = now()
 WHERE id = ? AND delivery_status IN (?, ?)`, deliveryStatusDelivering, runID, n.OrchestrationID,
				deliveryStatusPending, deliveryStatusFailed)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return nil
			}
		}
		res := tx.Exec(`
UPDATE agent_notifications
   SET status = ?, claim_run_id = ?, claimed_at = now(), claimed_in_place = true, updated_at = now()
 WHERE id = ? AND status = ?`, notifStatusClaimed, runID, n.ID, notifStatusPending)
		if res.Error != nil {
			return res.Error
		}
		claimed = res.RowsAffected > 0
		return nil
	})
	return claimed, err
}

// ReconcileDeliveryRun 是交付对账（doc/73 §6.5；doc/82 §4-12 多行）：RunEnd 快路径与调度慢路径
// 都只调这一个函数。一场 run 可能同时就地认领一条 bash 通知与一条 Task／组通知（doc/82 §4-12），
// 所以按 claim_run_id 取**全部** claimed 行逐条裁决——Scan 进单结构体只处理一行会让另一条永远
// 停在 claimed，调度器补偿循环又反复撞同一行。
//
// 裁决表（每行独立；run 终态对所有行相同）：
//   - running：不动作。租约过期只产生中断读面与恢复入口，不自动 resume；
//   - complete：通知 claimed → consumed；整组交付（kind=orchestration_finished 且组仍在 delivering）
//     再把组 delivering → delivered（同一事务，条件同时匹配当前 delivery_run_id 与状态，
//     旧尝试完成不能收口新尝试）；
//   - error/stopped：通知 claimed → failed + consume_error_code（delivery_run_error／delivery_stopped）；
//     整组当前交付同步 delivering → failed；不自动重启。
//
// 组状态与组事件只在 orchestration_id 非空时写；bash 行（orchestration_id 空串）改发
// background_jobs_changed。所有状态写 updated_at，failed 写 failed_at；条件更新保证
// 重复 EndRun／多副本对账幂等。
func (s *Store) ReconcileDeliveryRun(ctx context.Context, runID RunID) error {
	var rows []struct {
		NotificationID  string `gorm:"column:notification_id"`
		OrchID          string `gorm:"column:orchestration_id"`
		BackgroundJobID string `gorm:"column:background_job_id"`
		TaskID          string `gorm:"column:task_id"`
		TenantID        string `gorm:"column:tenant_id"`
		RunStatus       string `gorm:"column:run_status"`
		Kind            string `gorm:"column:kind"`
		NotifStatus     string `gorm:"column:notif_status"`
	}
	res := s.db.WithContext(ctx).Raw(`
SELECT n.id AS notification_id, n.orchestration_id,
       COALESCE(n.background_job_id, '') AS background_job_id,
       n.task_id, n.tenant_id, r.status AS run_status, n.kind, n.status AS notif_status
  FROM agent_notifications n JOIN runs r ON r.id = n.claim_run_id
 WHERE n.claim_run_id = ? AND r.status <> 'running'`, runID).Scan(&rows)
	if res.Error != nil {
		return res.Error
	}
	for _, row := range rows {
		if row.NotifStatus != notifStatusClaimed {
			continue // 已 consumed／failed：重复对账空转（旧 attempt 的指针也早已不在这里）
		}
		changed := false
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			switch row.RunStatus {
			case "complete":
				if row.Kind == notifKindOrchFinished && row.OrchID != "" {
					r := tx.Exec(`
UPDATE agent_orchestrations
   SET delivery_status = ?, delivered_at = now(), delivery_error_code = '', updated_at = now()
 WHERE id = ? AND delivery_status = ? AND delivery_run_id = ?`,
						deliveryStatusDelivered, row.OrchID, deliveryStatusDelivering, runID)
					if r.Error != nil {
						return r.Error
					}
					if r.RowsAffected == 0 {
						return nil // 组已不归这场 run：旧尝试的 complete 不能收口当前交付
					}
				}
				r := tx.Exec(`
UPDATE agent_notifications
   SET status = ?, consumed_at = now(), updated_at = now()
 WHERE id = ? AND claim_run_id = ? AND status = ?`,
					notifStatusConsumed, row.NotificationID, runID, notifStatusClaimed)
				if r.Error != nil {
					return r.Error
				}
				changed = r.RowsAffected > 0
				return nil
			case "error", "stopped":
				code := deliveryErrRunError
				if row.RunStatus == "stopped" {
					code = deliveryErrStopped
				}
				r := tx.Exec(`
UPDATE agent_notifications
   SET status = ?, consume_error_code = ?, failed_at = now(), updated_at = now()
 WHERE id = ? AND claim_run_id = ? AND status = ?`,
					notifStatusFailed, code, row.NotificationID, runID, notifStatusClaimed)
				if r.Error != nil {
					return r.Error
				}
				if r.RowsAffected == 0 {
					return nil
				}
				changed = true
				if row.Kind != notifKindOrchFinished || row.OrchID == "" {
					return nil // 单件 Task／bash 的协调失败不动组交付状态
				}
				return tx.Exec(`
UPDATE agent_orchestrations
   SET delivery_status = ?, delivery_error_code = ?, updated_at = now()
 WHERE id = ? AND delivery_status = ? AND delivery_run_id = ?`,
					deliveryStatusFailed, code, row.OrchID, deliveryStatusDelivering, runID).Error
			default:
				return nil // 未知终态：留给人工排查，不猜
			}
		})
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		if row.TaskID != "" {
			s.PublishEvent(ctx, row.TenantID, eventAgentTasksChanged, row.TaskID)
		}
		if row.OrchID != "" {
			s.PublishEvent(ctx, row.TenantID, eventAgentOrchestrationsChanged, row.OrchID)
			s.PublishTaskWake(ctx, row.TenantID, "delivery_reconciled", row.OrchID)
		}
		if row.BackgroundJobID != "" {
			s.PublishEvent(ctx, row.TenantID, eventBackgroundJobsChanged, row.BackgroundJobID)
		}
	}
	return nil
}

// reconcileDeliveries 是调度侧的交付对账补偿（§6.5）：两条来源合并——
//   - 通知 claimed 且当前 claim run 已终态：覆盖 task_failed／task_interrupted／plan_review
//     等所有通知，不依赖组是否有 delivery_status；
//   - 组仍 delivering 且 delivery run 已终态：兜历史漂移。
//
// worker 活着时 EndRun 钩子先到，这里全部空转；崩溃后由这里收口。限批 64 条，避免一次扫全表。
func (ts *TaskScheduler) reconcileDeliveries(ctx context.Context) {
	var runIDs []string
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT r.id FROM agent_notifications n JOIN runs r ON r.id = n.claim_run_id
 WHERE n.status = ? AND r.status <> 'running'
 ORDER BY n.updated_at, n.id LIMIT 64`, notifStatusClaimed).Scan(&runIDs).Error; err != nil {
		log.Printf("task delivery reconcile list: %v", err)
		return
	}
	for _, id := range runIDs {
		if err := ts.store.ReconcileDeliveryRun(ctx, RunID(id)); err != nil {
			log.Printf("task delivery reconcile %s: %v", id, err)
		}
	}
	var deliveryRuns []string
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT o.delivery_run_id FROM agent_orchestrations o JOIN runs r ON r.id = o.delivery_run_id
 WHERE o.delivery_status = ? AND r.status <> 'running' LIMIT 64`, deliveryStatusDelivering).
		Scan(&deliveryRuns).Error; err != nil {
		log.Printf("task delivery reconcile group list: %v", err)
		return
	}
	for _, id := range deliveryRuns {
		if err := ts.store.ReconcileDeliveryRun(ctx, RunID(id)); err != nil {
			log.Printf("task delivery reconcile group %s: %v", id, err)
		}
	}
}

// groupToolNames 取一个编排组执行者用过的工具名（doc/75 §7.1 的决策输入之一）：
// runlog 的 tool_call 文本是「工具名({参数})」，只取 root 行的前缀、去重、封顶。
// 拿不到名字的行（空文本）不计；读失败只让调用方决定要不要当错误——这里是查询错误原样返回。
func groupToolNames(tx *gorm.DB, orchID string) ([]string, error) {
	var names []string
	err := tx.Raw(`
SELECT DISTINCT btrim(split_part(e.text, '(', 1)) AS name
  FROM agent_tasks t
  JOIN runs r ON r.session_id = t.execution_session_id
  JOIN runlog_entries e ON e.run_id = r.id AND e.role = 'tool_call' AND e.agent IS NULL
 WHERE t.orchestration_id = ? AND btrim(e.text) <> ''
 ORDER BY name LIMIT 40`, orchID).Scan(&names).Error
	return names, err
}

// buildCoordinationInput 在认领事务的锁内拼协调 run 的首条输入（doc/70 §9.2；恢复参数见 doc/73 §6.2；
// 决策输入见 doc/75 §7.1）：通知 kind + 原始 goal + 相关 Task／组状态、机器错误、结果摘要、
// 结果版本与产物路径、原组已用工具名、handoff 链与 replanDepth + 执行约定
// （检查这些持久事实并向用户解释／交付；不要宣称未发生的停止、恢复或外部回滚）。
// 人工新尝试（rp.Attempt>1）先声明「这是第几次处理、上次怎样结束、上次已可见的正文是什么」——
// 只作为参考写进输入，不复制成新的 assistant 行。
func (s *Store) buildCoordinationInput(ctx context.Context, tx *gorm.DB, n AgentNotification, o AgentOrchestration, rp coordinationRecovery) (string, error) {
	var t AgentTask
	if n.TaskID != "" {
		if err := tx.Where("id = ?", n.TaskID).Take(&t).Error; err != nil {
			return "", err
		}
	}
	var final AgentTask
	if n.Kind == notifKindOrchFinished && o.FinalTaskID != "" {
		if err := tx.Where("id = ?", o.FinalTaskID).Take(&final).Error; err != nil {
			return "", err
		}
	}
	head := fmt.Sprintf("【协调通知】%s\n", n.Kind)
	if n.Kind != notifKindBashJobFinished {
		head += fmt.Sprintf("编排目标：%s\n", o.Goal)
	}
	if rp.Attempt > 1 {
		head += fmt.Sprintf("这是通知 %s 的第 %d 次人工处理。上一次 run %s 以 %s 结束，机器原因 = %s。\n",
			n.ID, rp.Attempt, rp.PreviousRunID, rp.PreviousTerminalStatus, n.ConsumeErrorCode)
		if txt := strings.TrimSpace(rp.PreviousRootAssistant); txt != "" {
			head += "上一次已保存的用户可见正文（仅作参考，不要原样重发）：\n" +
				truncatePlain(txt, 2000) + "\n"
		}
		head += "请核对当前持久结果，补完尚未完成的说明；不要宣称重跑了 Task，不要重复已完成的副作用。\n"
	}
	const contract = "请检查以上持久事实并向用户解释／交付；不要宣称未发生的停止、恢复或外部回滚。"
	switch n.Kind {
	case notifKindBashJobFinished:
		// 后台命令终态（doc/82 §6.5）：正文 = job id、终态、退出码、命令原文、两个输出路径。
		// 模型不执行正文句子，只据此向用户说明这条命令的结果（它没有 bash／写盘工具面）。
		j, err := notificationJob(tx, n)
		if err != nil {
			return "", err
		}
		body := fmt.Sprintf("后台命令 %s 已到终态：%s。", j.ID, j.Status)
		if j.ExitCode != nil {
			body += fmt.Sprintf("退出码 = %d。", *j.ExitCode)
		} else {
			body += "退出码：无（被停止或进程被信号结束，没有有意义的退出码）。"
		}
		body += fmt.Sprintf("\n命令原文：%s", j.Command)
		body += fmt.Sprintf("\n完整输出落盘的逻辑相对路径：\n- stdout：%s\n- stderr：%s",
			j.StdoutRelPath, j.StderrRelPath)
		if j.Status == jobStatusStopped {
			body += "\n这条命令是收到停止请求后结束的，结果不代表成功。"
		} else if j.Status == jobStatusInterrupted {
			body += "\n这条命令的执行者失去联系（进程崩溃或机器重启，租约已过期），不会自动重跑。"
		}
		return head + body + "\n" + contract, nil
	case notifKindTaskFailed:
		body := fmt.Sprintf("任务 %s（taskId %s）执行失败：机器原因 = %s",
			t.TaskKey, t.ID, t.ErrorCode)
		if t.ErrorMessage != "" {
			body += "，说明 = " + t.ErrorMessage
		}
		if t.ResultRevision > 0 {
			body += fmt.Sprintf("\n它留有第 %d 版部分结果，可在项目文件里核对。", t.ResultRevision)
		}
		return head + body + "\n" + contract, nil
	case notifKindTaskInterrupted:
		body := fmt.Sprintf("任务 %s（taskId %s）的执行者失去联系（进程崩溃或机器重启，租约已过期）。"+
			"任务状态仍按执行中记录，恢复需要用户显式发起，不会自动重放。", t.TaskKey, t.ID)
		return head + body + "\n" + contract, nil
	case notifKindPlanReviewRequired:
		// 复核输入 = 当前图 + 版本 + 触发 Task 的结果引用 + 原组已用工具 + 重规划链 +
		// 必须做决定的约定（施工包 §6.3 / doc/75 §7.1）。不复制执行历史：要核对结果就读项目
		// 文件、查历史或直接读结果目录。
		body := fmt.Sprintf("计划暂停等待复核：组已停止领取新 Task（运行中的继续跑完），计划版本 = %d。\n"+
			"触发：任务 %s（taskId %s）第 %d 版结果已保存。", o.PlanRevision, t.TaskKey, t.ID, t.ResultRevision)
		if o.FinalTaskID != "" {
			var final AgentTask
			if err := tx.Where("id = ?", o.FinalTaskID).Take(&final).Error; err != nil {
				return "", err
			}
			body += fmt.Sprintf("\n最终交付：%s（taskId %s），尚未开始 = %v。", final.TaskKey, final.ID, final.StartedAt == nil)
		}
		var tasks []AgentTask
		if err := tx.Where("orchestration_id = ?", o.ID).Order("task_key").Find(&tasks).Error; err != nil {
			return "", err
		}
		if len(tasks) > 0 {
			body += "\n当前图："
			for _, x := range tasks {
				body += fmt.Sprintf("\n- %s [%s] %s（taskId %s）：%s", x.TaskKey, x.Status, x.AgentName,
					x.ID, truncatePlain(x.Content, 80))
				if x.ErrorCode != "" {
					body += fmt.Sprintf("；失败原因 %s：%s", x.ErrorCode, truncatePlain(x.ErrorMessage, 200))
				}
				if r := parseTaskResult(x.Result); r != nil && x.ResultRevision > 0 {
					line := fmt.Sprintf("；第 %d 版结果", x.ResultRevision)
					if sum := strings.TrimSpace(r.Summary); sum != "" {
						line += "：" + truncatePlain(sum, 300)
					}
					for _, a := range r.Artifacts {
						line += fmt.Sprintf("\n    产物 %s（%s）", a.Path, a.Kind)
					}
					body += line
				}
			}
		}
		var links []AgentTaskLink
		if err := tx.Where("orchestration_id = ?", o.ID).Find(&links).Error; err != nil {
			return "", err
		}
		if len(links) > 0 {
			byID := make(map[string]string, len(tasks))
			for _, x := range tasks {
				byID[x.ID] = x.TaskKey
			}
			body += "\n依赖边："
			for _, l := range links {
				body += fmt.Sprintf("\n- %s -> %s", byID[l.ParentTaskID], byID[l.ChildTaskID])
			}
		}
		if names, err := groupToolNames(tx, o.ID); err != nil {
			return "", err
		} else if len(names) > 0 {
			body += "\n原组执行者已用过的工具：" + strings.Join(names, "、") +
				"（判断「不得重放的动作」时参考：这些工具可能已产生外部副作用）"
		}
		// 重规划链：本组已是第几代、前代是谁。候选复用条目只能用这些组里的成功结果。
		if chain, err := s.replanChain(ctx, o); err != nil {
			return "", err
		} else if len(chain) > 1 {
			ids := make([]string, 0, len(chain)-1)
			for _, g := range chain[1:] {
				ids = append(ids, g.ID)
			}
			body += fmt.Sprintf("\n本次是第 %d 层继任组，前代依次为：%s。复用条目只能取自这条链上已成功的结果。",
				len(chain)-1, strings.Join(ids, " → "))
		}
		const reviewContract = "你必须现在做决定，先判断目标本身是否仍然可行：\n" +
			"1. 可行（feasible）：原图仍适合 → 调用 continue_orchestration 按原图继续；只是局部要调整 → 调用 revise_orchestration 改图后继续；" +
			"原计划整体走不通、需要换一条路径 → 调用 create_successor_orchestration 建继任组（目标不变、计划换一套，必须给可行性证据、复用仍有效的结果、声明不得重放的动作）。\n" +
			"2. 不可行（infeasible）：用证据向用户说明目标为什么达不成，不调用任何创建／继续工具。\n" +
			"3. 没把握（uncertain）：先读项目文件、查历史或换个办法调查；仍不确定就向用户说明现状与待定问题，不创建继任组。\n" +
			"4. 先分清执行故障与方案失效：单次模型／工具瞬断、限流、网络错误属于执行故障，先考虑恢复或重试同一计划，不要据此重规划；" +
			"只有计划本身不可行时才改图或换继任组。\n" +
			"5. 同组修订（revise_orchestration）与继任组的区别：图还能改、已开始节点仍有效 → 修订；目标没变但路径整体失效、需要全新 DAG → 继任。\n" +
			"先读项目文件核对结果再决定；不要询问用户以外的方式把本场挂起，不要宣称尚未发生的执行。"
		return head + body + "\n" + reviewContract, nil
	case notifKindOrchFinished:
		body := fmt.Sprintf("整组已收口，最终状态 = %s。", o.Status)
		if o.Status == orchStatusSucceeded && final.ID != "" {
			body += fmt.Sprintf("\n最终交付：任务 %s（taskId %s），结果版本 %d。", final.TaskKey, final.ID, final.ResultRevision)
			if r := parseTaskResult(final.Result); r != nil {
				if r.Summary != "" {
					body += "\n结果摘要：" + r.Summary
				}
				if len(r.Artifacts) > 0 {
					body += "\n产物："
					for _, a := range r.Artifacts {
						body += fmt.Sprintf("\n- %s（%s）", a.Path, a.Kind)
					}
				}
			}
		} else if final.ID != "" {
			body += fmt.Sprintf("\n最终任务 %s（taskId %s）的终态是 %s：机器原因 = %s。",
				final.TaskKey, final.ID, final.Status, final.ErrorCode)
		}
		return head + body + "\n" + contract, nil
	default:
		return "", fmt.Errorf("delivery %s: 未知通知类型 %q", n.ID, n.Kind)
	}
}

// EndRunErrorOnly 把一场 run 收成 error（协调 run 装配失败专用）：不动 mh / runlog / 账本——
// 没有产出可定稿，只翻状态、清租约。返回 ok = 这次翻赢了（false = 已被收走）。
func (s *Store) EndRunErrorOnly(ctx context.Context, sessionID SessionID, runID RunID) (int64, bool, error) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE runs SET status = 'error', finished_at = now(), lease_until = NULL
		 WHERE id = ? AND session_id = ? AND status = 'running'`, runID, sessionID)
	if res.Error != nil {
		return 0, false, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, false, nil
	}
	// terminal 帧给轮询的客户端；Redis 败不影响 PG 终态（runlive 走 TTL 收敛）。
	if tj, err := json.Marshal(Frame{Kind: "terminal", Status: "error"}); err == nil {
		if _, ferr := s.runliveFinish(ctx, runID, string(tj)); ferr != nil {
			log.Printf("delivery runlive finish %s: %v", runID, ferr)
		}
	}
	return 0, true, nil
}
