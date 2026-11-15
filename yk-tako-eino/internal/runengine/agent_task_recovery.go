// 本文件是协调通知的读面与人工恢复（方案真源 = doc/73 §4.1/§4.2/§8/§9；施工包 §4.1/§4.2）。
//
//	GET  /api/notifications/:id         通知详情、结果定位与每次处理尝试
//	POST /api/notifications/:id/retry   人工创建一次新的处理尝试（只从 failed 出发）
//
// 两条端点共用同一条属主链（通知 → 编排组 → 来源会话 → 本人在租户内），非本人、跨租户与
// 已删除来源会话一律 404 117003。重试的事务本体在 Store.RetryAgentNotification：
// 与初次认领共用 CreateCoordinationAttempt，准入条件各自判定（pending 归调度器，failed 归人）。
package runengine

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errNotifNotFound        = errors.New("notification not found")
	errNotifNotRetryable    = errors.New("notification not retryable")
	errNotifArtifactInvalid = errors.New("notification result artifact invalid")
)

// notificationOwnerQuery 是通知可见性链的公共 SQL：通知 → 来源会话 → 本人。
// 与 taskOwnerQuery 同一条纪律：非本人与不存在同形（117003），不给探测通道。
// 软删的会话整条落空——「来源会话已删除」与「不存在」也不区分。
//
// 来源会话按 kind 二选一取（doc/82 §6.5）：编排通知经编排组（o.source_session_id），bash 通知
// 经 job（j.source_session_id）——bash 行的 orchestration_id 是空串，只 JOIN 组会让它整条查不到、
// 详情与重试全 404。两个 LEFT JOIN 互不冲突：编排行 background_job_id 为 NULL，bash 行
// orchestration_id 为空串，各自只命中一个；两条来源都取不到时 COALESCE 为 NULL，`s.id = NULL`
// 不成立，整条落空——这正是「来源会话已删除」要的 404。
const notificationOwnerQuery = `
SELECT n.* FROM agent_notifications n
  LEFT JOIN agent_orchestrations o ON o.id = n.orchestration_id
  LEFT JOIN background_jobs j ON j.id = n.background_job_id
  JOIN sessions s ON s.id = COALESCE(o.source_session_id, j.source_session_id)
 WHERE n.tenant_id = ? AND s.user_id = ? AND s.deleted_at IS NULL`

// notificationResultView 是详情里的结果定位（doc/73 §9）：finalTaskId + resultRevision 永远
// 指向当前持久结果，摘要与产物现读自该 Task，不用过期通知快照。
type notificationResultView struct {
	FinalTaskID    string         `json:"finalTaskId"`
	ResultRevision int            `json:"resultRevision"`
	Summary        string         `json:"summary"`
	Artifacts      []TaskArtifact `json:"artifacts"`
}

// notificationAttemptView 是一次处理尝试。status 现读自 runs（本表不复制执行状态）；
// run 行缺失时为空串（数据被动过的降级读法，不是正常路径）。
type notificationAttemptView struct {
	RunID     RunID     `json:"runId"`
	Attempt   int       `json:"attempt"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// agentNotificationView 是 GET 的完整视图。字段顺序 = 施工包 §4.1 的契约字节序：
// notificationId → orchestrationId → taskId → kind → status → runId → attempt → errorCode
// → result → createdAt → claimedAt → failedAt → consumedAt → updatedAt → attempts
// → backgroundJobId（doc/82 §6.5 追加，旧字段不重排）。
type agentNotificationView struct {
	NotificationID  string                    `json:"notificationId"`
	OrchestrationID string                    `json:"orchestrationId"`
	TaskID          string                    `json:"taskId"`
	Kind            string                    `json:"kind"`
	Status          string                    `json:"status"`
	RunID           RunID                     `json:"runId"`
	Attempt         int                       `json:"attempt"`
	ErrorCode       string                    `json:"errorCode"`
	Result          *notificationResultView   `json:"result"`
	CreatedAt       time.Time                 `json:"createdAt"`
	ClaimedAt       *time.Time                `json:"claimedAt"`
	FailedAt        *time.Time                `json:"failedAt"`
	ConsumedAt      *time.Time                `json:"consumedAt"`
	UpdatedAt       time.Time                 `json:"updatedAt"`
	Attempts        []notificationAttemptView `json:"attempts"`
	// BackgroundJobID 指向后台命令（doc/82 §6.5）：bash_job_finished 通知的 orchestration_id 为空串，
	// 前端据它打开命令详情；编排类通知该引用为 NULL，这里恒空串。空串不等于「没有这条命令」——
	// 命令是否还在由 GET /api/jobs/:id 自己判 404（被清理后按 404）。
	BackgroundJobID string `json:"backgroundJobId"`
}

// notificationRetryView 是 POST retry 的响应体（施工包 §4.2，字段序固定）。
type notificationRetryView struct {
	NotificationID string `json:"notificationId"`
	RunID          RunID  `json:"runId"`
	Status         string `json:"status"`
	Attempt        int    `json:"attempt"`
}

// GetAgentNotification 读一条通知的完整视图（doc/73 §4.1）。结果只在 orchestration_finished
// 上给出：那才是「最终交付」的对象；其他通知的 Task 结果走任务详情。
func (s *Store) GetAgentNotification(ctx context.Context, p principal, id string) (agentNotificationView, error) {
	var rows []AgentNotification
	if err := s.db.WithContext(ctx).Raw(notificationOwnerQuery+` AND n.id = ?`,
		p.TenantID, p.UserID, id).Scan(&rows).Error; err != nil {
		return agentNotificationView{}, err
	}
	if len(rows) == 0 {
		return agentNotificationView{}, errNotifNotFound
	}
	n := rows[0]
	attempts, err := s.notificationAttempts(ctx, n.ID)
	if err != nil {
		return agentNotificationView{}, err
	}
	// background_job_id 是 NULL 可空列：编排类通知为 NULL，扫成空串给前端（与 orchestrationId 同口径）。
	bgJobID := ""
	if n.BackgroundJobID != nil {
		bgJobID = *n.BackgroundJobID
	}
	view := agentNotificationView{
		NotificationID: n.ID, OrchestrationID: n.OrchestrationID, TaskID: n.TaskID,
		Kind: n.Kind, Status: n.Status, RunID: RunID(n.ClaimRunID),
		Attempt:   latestAttempt(attempts),
		ErrorCode: n.ConsumeErrorCode,
		CreatedAt: n.CreatedAt, ClaimedAt: n.ClaimedAt, FailedAt: n.FailedAt,
		ConsumedAt: n.ConsumedAt, UpdatedAt: n.UpdatedAt,
		Attempts:        attempts,
		BackgroundJobID: bgJobID,
	}
	if n.Kind == notifKindOrchFinished {
		result, err := s.finalTaskResultView(ctx, n.OrchestrationID)
		if err != nil {
			return agentNotificationView{}, err
		}
		view.Result = result
	}
	return view, nil
}

// notificationAttempts 按 attempt 升序读处理历史，status 现读自 runs。
func (s *Store) notificationAttempts(ctx context.Context, notifID string) ([]notificationAttemptView, error) {
	rows := make([]notificationAttemptView, 0)
	if err := s.db.WithContext(ctx).Raw(`
SELECT a.run_id, a.attempt, COALESCE(r.status, '') AS status, a.created_at
  FROM agent_notification_attempts a
  LEFT JOIN runs r ON r.id = a.run_id
 WHERE a.notification_id = ?
 ORDER BY a.attempt, a.created_at`, notifID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// latestAttempt 取历史里最大的 attempt；没有处理过 = 0。
func latestAttempt(attempts []notificationAttemptView) int {
	max := 0
	for _, a := range attempts {
		if a.Attempt > max {
			max = a.Attempt
		}
	}
	return max
}

// finalTaskResultView 现读组当前最终 Task 的结果（doc/73 §9）：没有结果（revision 0 / 空载荷）
// 时返回 nil，字段在 JSON 里是 null 而不是省略。
func (s *Store) finalTaskResultView(ctx context.Context, orchID string) (*notificationResultView, error) {
	var o AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", orchID).Take(&o).Error; err != nil {
		return nil, err
	}
	if o.FinalTaskID == "" {
		return nil, nil
	}
	var t AgentTask
	if err := s.db.WithContext(ctx).Where("id = ?", o.FinalTaskID).Take(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	r := parseTaskResult(t.Result)
	if r == nil || t.ResultRevision == 0 {
		return nil, nil
	}
	arts := r.Artifacts
	if arts == nil {
		arts = []TaskArtifact{}
	}
	return &notificationResultView{
		FinalTaskID: t.ID, ResultRevision: t.ResultRevision, Summary: r.Summary, Artifacts: arts,
	}, nil
}

// RetryAgentNotification 是人工重新处理（doc/73 §8.2；施工包 §4.2）：只在通知 failed 且当前
// run 已 error／stopped 时创建一场新 run。rootBase 是引擎作业根（Server 的 worker.Workspace()），
// 结果产物在事务之前按项目真实目录校验——文件系统工作不进 PG 锁。
//
// 返回 launch 非空 = 这次真的创建了新尝试（调用方提交后启动 worker）；launch 为空 = 幂等命中
// （已有正在处理的 run / 计划决策已接受而直接补 consumed），只回当前状态。
func (s *Store) RetryAgentNotification(ctx context.Context, p principal, notifID, rootBase string) (notificationRetryView, *coordLaunch, error) {
	var rows []AgentNotification
	if err := s.db.WithContext(ctx).Raw(notificationOwnerQuery+` AND n.id = ?`,
		p.TenantID, p.UserID, notifID).Scan(&rows).Error; err != nil {
		return notificationRetryView{}, nil, err
	}
	if len(rows) == 0 {
		return notificationRetryView{}, nil, errNotifNotFound
	}
	pre := rows[0]
	// 恢复前的产物校验（doc/73 §13.3）：文件型结果的每个声明产物都必须是项目根下真实可读的
	// 普通文件；路径越界、目录、软链跳根、已删除一律 117110，不把缺失产物交付成成功。
	if pre.Kind == notifKindOrchFinished {
		var o AgentOrchestration
		if err := s.db.WithContext(ctx).Where("id = ?", pre.OrchestrationID).Take(&o).Error; err != nil {
			return notificationRetryView{}, nil, err
		}
		result, err := s.finalTaskResultView(ctx, pre.OrchestrationID)
		if err != nil {
			return notificationRetryView{}, nil, err
		}
		if result != nil && len(result.Artifacts) > 0 {
			// 产物路径相对于用户内容根（doc/76 §5：content/），不是项目容器根。
			root, _, err := ensureProjectLayout(rootBase, o.TenantID, o.WorkspaceID)
			if err != nil {
				return notificationRetryView{}, nil, errNotifArtifactInvalid
			}
			for _, a := range result.Artifacts {
				if _, err := resolveArtifactPath(root, a.Path); err != nil {
					return notificationRetryView{}, nil, errNotifArtifactInvalid
				}
			}
		}
	}

	var out notificationRetryView
	var launch *coordLaunch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n AgentNotification
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", pre.ID).Take(&n).Error; err != nil {
			return err
		}
		// 按 kind 二选一取事实源（§5 表）：bash 通知没有编排组，锁组行会 `id = ''` 空手而归、
		// 整笔事务回滚；它的租户/项目/来源会话全从 job 读（`notificationJob` 在 delivery 面）。
		var o AgentOrchestration
		if n.Kind != notifKindBashJobFinished {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", n.OrchestrationID).Take(&o).Error; err != nil {
				return err
			}
		}
		// 组租户门按 kind 取（bash 行 o 是零值，用组租户会锁错租户的门）。
		tenantID := o.TenantID
		if n.Kind == notifKindBashJobFinished {
			j, jerr := notificationJob(tx, n)
			if jerr != nil {
				return jerr
			}
			tenantID = j.TenantID
		}
		var cur struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(`SELECT status FROM runs WHERE id = ?`, n.ClaimRunID).Scan(&cur).Error; err != nil {
			return err
		}
		maxAttempt := 0
		if err := tx.Raw(`SELECT COALESCE(MAX(attempt), 0) FROM agent_notification_attempts
 WHERE notification_id = ?`, n.ID).Scan(&maxAttempt).Error; err != nil {
			return err
		}
		switch n.Status {
		case notifStatusClaimed:
			// 双击／多标签／双副本：第一次已经翻成 claimed 且 run 还在跑——回现状，不再开 run。
			if cur.Status == "running" {
				out = notificationRetryView{NotificationID: n.ID, RunID: RunID(n.ClaimRunID),
					Status: notifStatusClaimed, Attempt: maxAttempt}
				return nil
			}
			return errNotifNotRetryable // claimed 但 run 已终态：等对账收口，不允许从这里跳过
		case notifStatusFailed:
			if cur.Status != "error" && cur.Status != "stopped" {
				return errNotifNotRetryable // failed 但当前 run 仍在跑 = 矛盾状态
			}
		default:
			return errNotifNotRetryable // pending / consumed / 未知
		}
		// 计划复核：决策已经被接受（组不再 active 或已放行）时不得再改图——直接把通知补成
		// consumed，返回现状；只有职责确实未消费时才创建新 run（doc/73 §4.2 第 5 条）。
		if n.Kind == notifKindPlanReviewRequired && (o.Status != orchStatusActive || !o.PlanHold) {
			if err := tx.Exec(`
UPDATE agent_notifications
   SET status = ?, consumed_at = now(), failed_at = NULL, consume_error_code = '', updated_at = now()
 WHERE id = ? AND status = ?`, notifStatusConsumed, n.ID, notifStatusFailed).Error; err != nil {
				return err
			}
			out = notificationRetryView{NotificationID: n.ID, RunID: RunID(n.ClaimRunID),
				Status: notifStatusConsumed, Attempt: maxAttempt}
			return nil
		}
		// 来源会话忙检查：用户正在聊就不打断；通知保持 failed，等用户稍后再点。
		// 先清扫租约已过的死行（AcquireRun 同一条扫尾）：忙检查按租约判，唯一索引只认
		// status='running'，不清会撞 one_live_run_per_session。
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
			return ErrSessionBusy
		}
		if err := s.lockTenantGate(tx, tenantID); err != nil {
			return err
		}
		// 上一次用户可见正文：从 PG runlog 现读 root assistant（不是易失 Redis），
		// 只进恢复输入作参考，不复制成新的 assistant 行。
		var prev string
		if n.ClaimRunID != "" {
			if err := tx.Raw(`SELECT text FROM runlog_entries
 WHERE run_id = ? AND role = 'assistant' AND agent IS NULL
 ORDER BY seq DESC LIMIT 1`, n.ClaimRunID).Scan(&prev).Error; err != nil {
				return err
			}
		}
		rp := coordinationRecovery{
			Attempt: maxAttempt + 1, PreviousRunID: RunID(n.ClaimRunID),
			PreviousTerminalStatus: cur.Status, PreviousRootAssistant: prev,
		}
		input, err := s.buildCoordinationInput(ctx, tx, n, o, rp)
		if err != nil {
			return err
		}
		launch, err = s.CreateCoordinationAttempt(ctx, tx, &n, &o, rp, input)
		if err != nil {
			return err
		}
		out = notificationRetryView{NotificationID: n.ID, RunID: launch.RunID,
			Status: notifStatusClaimed, Attempt: launch.Attempt}
		return nil
	})
	if err != nil {
		return notificationRetryView{}, nil, err
	}
	return out, launch, nil
}

// getAgentNotification = GET /api/notifications/:id（施工包 §4.1）。
func (s *Server) getAgentNotification(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	view, err := s.store.GetAgentNotification(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if notificationStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", view)
}

// retryAgentNotification = POST /api/notifications/:id/retry（施工包 §4.2）。
//
//	202 {"code":0,"message":"","data":{"notificationId":"an_…","runId":"r_…","status":"claimed","attempt":2}}
//	200 已有一场正在处理的 run（双击／多副本）或计划决策已接受（status=consumed）
//	404 117003；409 117109 现在不能重新处理；409 117110 产物不可读；409 112101 会话忙；429 113001
func (s *Server) retryAgentNotification(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	if s.tasks == nil || s.tasks.assemble == nil || s.worker == nil {
		internal(c, errors.New("task scheduler not attached"))
		return
	}
	view, launch, err := s.store.RetryAgentNotification(
		c.Request.Context(), p, c.Param("id"), s.worker.Workspace())
	if err != nil {
		if notificationStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	status := http.StatusOK
	if launch != nil {
		status = http.StatusAccepted
		s.tasks.launchCoordinationRun(c.Request.Context(), launch)
	}
	if v, gerr := s.store.GetAgentNotification(c.Request.Context(), p, view.NotificationID); gerr == nil {
		if v.TaskID != "" {
			s.store.PublishEvent(c.Request.Context(), p.TenantID, eventAgentTasksChanged, v.TaskID)
		}
		if v.OrchestrationID != "" {
			s.store.PublishEvent(c.Request.Context(), p.TenantID, eventAgentOrchestrationsChanged, v.OrchestrationID)
		}
	}
	s.store.PublishTaskWake(c.Request.Context(), p.TenantID, "notification_retry", view.NotificationID)
	writeEnvelope(c, status, codeOK, "", view)
}

// notificationStoreErr 把恢复面的业务错误翻成信封。true = 已写响应。
func notificationStoreErr(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, errNotifNotFound):
		writeEnvelope(c, http.StatusNotFound, codeNotifMissing, "找不到这条通知", nil)
	case errors.Is(err, errNotifNotRetryable):
		writeEnvelope(c, http.StatusConflict, codeNotifNotRetryable, "这条通知现在不能重新处理", nil)
	case errors.Is(err, errNotifArtifactInvalid):
		writeEnvelope(c, http.StatusConflict, codeArtifactInvalid, "结果产物已不可读取", nil)
	case errors.Is(err, ErrSessionBusy):
		writeEnvelope(c, http.StatusConflict, codeSessionRunning, "这场会话正在生成，请稍后再试", nil)
	case errors.Is(err, errTenantFull):
		writeEnvelope(c, http.StatusTooManyRequests, codeQuota, "同时在跑的任务已达上限，请稍后再试", nil)
	default:
		return false
	}
	return true
}
