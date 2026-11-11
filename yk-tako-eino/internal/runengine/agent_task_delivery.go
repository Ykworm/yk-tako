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
	"fmt"
	"log"
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
}

type coordinationBoxKey struct{}

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
}

// consumeAgentNotifications 每轮取至多 8 条 pending 通知逐条消费。assemble 为 nil（单测）时
// 直接返回：认领而无法启动会把通知卡在 claimed，不如整步不做——测试只验存储半。
func (ts *TaskScheduler) consumeAgentNotifications(ctx context.Context) {
	if ts.assemble == nil {
		return
	}
	var ids []string
	if err := ts.store.db.WithContext(ctx).Raw(
		`SELECT id FROM agent_notifications WHERE status = ? ORDER BY created_at, id LIMIT 8`,
		notifStatusPending).Scan(&ids).Error; err != nil {
		log.Printf("task delivery list: %v", err)
		return
	}
	for _, id := range ids {
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

// claimNotification 认领一条通知并铸它的协调 run（§9.2）。同一笔事务里依次：
// 锁通知行（两副本抢同一条只有一个进得来）→ 锁组行（交付状态翻转与认领串行化）→
// 来源会话忙检查（有活 run 不认领，保持 pending）→ 通知翻 claimed + claim_run_id →
// 铸协调 run（runs 行 + mh 首条 + runlog internal=true 的 user 行）→ 整组交付的把
// deliveryStatus 推到 delivering → Redis runlive（AcquireRun 同一条纪律：PG 写完、COMMIT 前写，
// 失败整笔回滚）。
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
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", n.OrchestrationID).Take(&o).Error; err != nil {
			return err
		}
		if n.Kind == notifKindOrchFinished {
			// 整组交付只认领 delivery_status=pending 的组（§9.3）：delivering = 已有交付 run 在跑，
			// 重启后由对账补状态；delivered = 已交付，通知标 consumed 清账；failed = 不自动重放。
			switch o.DeliveryStatus {
			case deliveryStatusPending:
			case deliveryStatusDelivering:
				return nil
			case deliveryStatusDelivered:
				return tx.Model(&AgentNotification{}).Where("id = ?", n.ID).
					Updates(map[string]any{"status": notifStatusConsumed, "consumed_at": nowUTC()}).Error
			default:
				return nil
			}
		}
		// 来源会话忙检查（§9.2）：用户正在聊就不打断——通知留在 pending，下一轮再看。
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
		input, err := buildCoordinationInput(tx, n, o)
		if err != nil {
			return err
		}
		runID, err := newRunID()
		if err != nil {
			return err
		}
		msgID, err := newRowID("msg_")
		if err != nil {
			return err
		}
		logID, err := newRowID("rl_")
		if err != nil {
			return err
		}
		if err := tx.Exec(
			`UPDATE agent_notifications SET status = ?, claim_run_id = ?, claimed_at = now() WHERE id = ?`,
			notifStatusClaimed, runID, n.ID).Error; err != nil {
			return err
		}
		if n.Kind == notifKindOrchFinished {
			if err := tx.Model(&AgentOrchestration{}).
				Where("id = ? AND delivery_status = ?", o.ID, deliveryStatusPending).
				Updates(map[string]any{"delivery_status": deliveryStatusDelivering, "delivery_run_id": runID}).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec(`
INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
 VALUES (?, ?, 'running', 1, now() + make_interval(secs => ?), ?, now())`,
			runID, n.ReceiverSessionID, ts.store.leaseTTL.Seconds(), o.WorkspaceID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`
INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
 VALUES (?, ?, ?, 1, 'user', ?, false, now())`,
			msgID, n.ReceiverSessionID, runID, input).Error; err != nil {
			return err
		}
		// internal=true 的 runlog user 行（§9.2）：程序写进来的协调输入不是用户说的话，
		// 页面不渲染；用户只看到 mAgent 的交付回答。
		if err := tx.Exec(`
INSERT INTO runlog_entries (id, session_id, seq, run_id, role, message_id, text, internal, created_at)
 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runlog_entries WHERE session_id = ?),
         ?, 'user', ?, ?, true, now())`,
			logID, n.ReceiverSessionID, n.ReceiverSessionID, runID, msgID, input).Error; err != nil {
			return err
		}
		var sess struct {
			UserID  string `gorm:"column:user_id"`
			AgentID string `gorm:"column:agent_id"`
		}
		if err := tx.Raw(`SELECT user_id, COALESCE(agent_id, '') AS agent_id FROM sessions WHERE id = ?`,
			n.ReceiverSessionID).Scan(&sess).Error; err != nil {
			return err
		}
		if sess.UserID == "" {
			return fmt.Errorf("delivery %s: 来源会话缺失", n.ID)
		}
		start, err := json.Marshal(Frame{Kind: "start"})
		if err != nil {
			return err
		}
		if err := ts.store.runliveStart(ctx, runID, 1, string(start)); err != nil {
			return err
		}
		launch = &coordLaunch{
			NotificationID: n.ID, Kind: n.Kind, OrchID: o.ID,
			SessionID: n.ReceiverSessionID, RunID: runID,
			TenantID: o.TenantID, UserID: sess.UserID, AgentID: sess.AgentID,
		}
		return nil
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
		OrchestrationID: l.OrchID, NotificationID: l.NotificationID, Kind: l.Kind,
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
	prof.Tools = dropDeliveryNoTools(bg, prof.Tools)
	log.Printf("task delivery: launching %s notification %s as run %s", l.Kind, l.NotificationID, l.RunID)
	go ts.worker.Run(withRunProfile(bg, prof), l.SessionID, l.RunID, 1)
}

// dropDeliveryNoTools 把等人与再编排类工具从协调 run 的池子里拿掉（doc/70 §6.3「不获得超出
// 规格的工具」用到交付角色上）：§9.2 的交付收口条件是「协调 run complete」，所以交付必须
// 确定性终结——
//   - ask_user / enter_plan_mode / submit_plan：问出去没人自动答，run 挂起等人，
//     来源会话一直忙，后续交付（如 orchestration_finished）永远认领不了；
//   - delegate_task / create_orchestration：投递不是再编排。
//
// 2026-11-10 崩溃活体抓过：task_interrupted 交付里模型顺手 ask_user「你希望我怎么处理？」，
// 交付停在半路。拿不到名的工具原样保留（与既有 drop helper 同一取舍）。
func dropDeliveryNoTools(ctx context.Context, in []tool.BaseTool) []tool.BaseTool {
	kept := make([]tool.BaseTool, 0, len(in))
	for _, t := range in {
		info, err := t.Info(ctx)
		if err != nil {
			kept = append(kept, t)
			continue
		}
		if agentTaskNoTools[info.Name] {
			continue
		}
		kept = append(kept, t)
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

// ReconcileDeliveryRun 是交付对账（§9.3）：协调 run complete → 通知 consumed；整组完成的
// 交付再把组推 delivered + deliveredAt（同笔事务，条件更新防重）；error → 整组交付 failed
// （单任务事件的交付失败不动组——通知留在 claimed，run 走显式恢复，恢复后的 complete 再进这里）；
// stopped → 不动。幂等：重复调用读到已 consumed 就空转。
//
// 协调 run 按通知的 claim_run_id 反查，不按组的 delivery_run_id：后者只有整组完成交付才写，
// task_failed／task_interrupted 的交付 run 完成后会查无此行，通知永远停在 claimed
// （2026-11-10 崩溃活体抓过）。
func (s *Store) ReconcileDeliveryRun(ctx context.Context, runID RunID) error {
	var row struct {
		OrchID   string `gorm:"column:orchestration_id"`
		TenantID string `gorm:"column:tenant_id"`
		Status   string `gorm:"column:run_status"`
		Kind     string `gorm:"column:kind"`
	}
	res := s.db.WithContext(ctx).Raw(
		`SELECT n.orchestration_id, n.tenant_id, r.status AS run_status, n.kind
		   FROM agent_notifications n JOIN runs r ON r.id = n.claim_run_id
		  WHERE n.claim_run_id = ?`, runID).Scan(&row)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil // 不是协调 run
	}
	switch row.Status {
	case "complete":
	case "error":
		if row.Kind != notifKindOrchFinished {
			return nil // 单任务事件交付失败：通知留 claimed，run 显式恢复后由这里收口
		}
		r := s.db.WithContext(ctx).Model(&AgentOrchestration{}).
			Where("id = ? AND delivery_status = ?", row.OrchID, deliveryStatusDelivering).
			Update("delivery_status", deliveryStatusFailed)
		if r.Error == nil && r.RowsAffected > 0 {
			s.PublishEvent(ctx, row.TenantID, eventAgentOrchestrationsChanged, row.OrchID)
		}
		return r.Error
	default:
		return nil // stopped / running：留给显式恢复与下一轮
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if row.Kind == notifKindOrchFinished {
			if r := tx.Model(&AgentOrchestration{}).
				Where("id = ? AND delivery_status = ?", row.OrchID, deliveryStatusDelivering).
				Updates(map[string]any{"delivery_status": deliveryStatusDelivered, "delivered_at": nowUTC()}); r.Error != nil {
				return r.Error
			}
		}
		return tx.Model(&AgentNotification{}).
			Where("claim_run_id = ? AND status = ?", runID, notifStatusClaimed).
			Updates(map[string]any{"status": notifStatusConsumed, "consumed_at": nowUTC()}).Error
	})
	if err == nil {
		s.PublishEvent(ctx, row.TenantID, eventAgentOrchestrationsChanged, row.OrchID)
	}
	return err
}

// reconcileDeliveries 是调度侧的交付对账补偿：delivery run 已终态但组还停在 delivering 的，
// 逐场走 ReconcileDeliveryRun。worker 活着时 EndRun 钩子先到，这里全部空转；崩溃后由这里收口。
func (ts *TaskScheduler) reconcileDeliveries(ctx context.Context) {
	var rows []struct {
		RunID string `gorm:"column:delivery_run_id"`
	}
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT o.delivery_run_id FROM agent_orchestrations o JOIN runs r ON r.id = o.delivery_run_id
 WHERE o.delivery_status = ? AND r.status <> 'running'`, deliveryStatusDelivering).Scan(&rows).Error; err != nil {
		log.Printf("task delivery reconcile list: %v", err)
		return
	}
	for _, r := range rows {
		if err := ts.store.ReconcileDeliveryRun(ctx, RunID(r.RunID)); err != nil {
			log.Printf("task delivery reconcile %s: %v", r.RunID, err)
		}
	}
}

// buildCoordinationInput 在认领事务的锁内拼协调 run 的首条输入（§9.2）：
// 通知 kind + 原始 goal + 相关 Task／组状态、机器错误、结果摘要、结果版本与产物路径 +
// 执行约定（检查这些持久事实并向用户解释／交付；不要宣称未发生的停止、恢复或外部回滚）。
func buildCoordinationInput(tx *gorm.DB, n AgentNotification, o AgentOrchestration) (string, error) {
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
	head := fmt.Sprintf("【协调通知】%s\n编排目标：%s\n", n.Kind, o.Goal)
	const contract = "请检查以上持久事实并向用户解释／交付；不要宣称未发生的停止、恢复或外部回滚。"
	switch n.Kind {
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
