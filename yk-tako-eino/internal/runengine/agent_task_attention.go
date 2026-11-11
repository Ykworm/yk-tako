// 本文件是两个读面（方案真源 = doc/72 §4.5/§4.6/§8.1；施工包同节）：
//
//	GET /api/tasks/:id/questions        Task 当前执行上的问题行（复用 questionView）
//	GET /api/workspaces/:id/task-attention  项目作用域的「需要我处理」聚合
//
// 两类都不建未读表、不存计数副本：全部从 PG 现算，删掉对应事实（回答、继续、恢复）
// 下一次查询就消失。交付失败是例外——它持续可见，直到服务端状态真的改变。
package runengine

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// listTaskQuestions = GET /api/tasks/:id/questions?status=pending|answered|stale（§4.5）。
// 鉴权走 GetAgentTask 的同一条 Task → 编排组 → 来源会话属主链；未领取的 Task 回空数组。
func (s *Server) listTaskQuestions(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	status := c.Query("status")
	switch status {
	case "", questionStatusPending, questionStatusAnswered, questionStatusStale:
	default:
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "status 只能是 pending / answered / stale", nil)
		return
	}
	view, err := s.store.GetAgentTask(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	views := make([]questionView, 0)
	if view.CurrentRunID != "" {
		rows, err := s.store.ListQuestions(c.Request.Context(), RunID(view.CurrentRunID), status)
		if err != nil {
			internal(c, err)
			return
		}
		for _, r := range rows {
			views = append(views, questionView{
				QuestionID: r.ID, RunID: r.RunID, ToolCallID: r.ToolCallID, Kind: r.Kind, Question: r.Question,
				Args: r.Args, Status: r.Status, Decision: r.Decision, Answer: r.Answer, CreatedAt: r.CreatedAt,
				ExpiresAt: r.ExpiresAt,
			})
		}
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", views)
}

// attentionItem 是「需要我处理」的一行（§4.6：字段恒出现，不适用时用 "" / 0 / 实际状态）。
type attentionItem struct {
	Kind            string `json:"kind"`
	TaskID          string `json:"taskId"`
	OrchestrationID string `json:"orchestrationId"`
	Title           string `json:"title"`
	Count           int    `json:"count"`
	QuestionID      string `json:"questionId"`
	RunID           RunID  `json:"runId"`
	// NotificationID 跟随 runId（doc/73 §4.4）：交付失败行直接带着可恢复的通知，
	// 前端据此调用 POST /api/notifications/:id/retry；没有对应通知时为空串。
	NotificationID string    `json:"notificationId"`
	Status         string    `json:"status"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// TaskAttention 现算一个项目下的四类处理项（§8.1；doc/73 §4.4 扩了通知归属）：
//   - question：Task 与当前 run 都 running、租约有效、问题 pending 且未过期（每件 Task 一行，
//     questionId 取最早待答、count 为活跃待答数）；
//   - plan_review：组 active + plan_hold（notificationId 指向待决策的 plan_review_required）；
//   - interrupted：Task running 但当前 run 已不是有效存活；或通知 claimed 但它的消费 run
//     已不是有效存活（读侧派生同一规则）；
//   - delivery_failed：组的 delivery_status=failed，以及非整组通知自身 status=failed
//     （taskId／标题按通知内容给出，不因组没有 delivery_status 而隐身）。
//
// 排序 updatedAt ASC, kind, taskId, orchestrationId, notificationId——刷新不随机跳动。
func (s *Store) TaskAttention(ctx context.Context, p principal, workspaceID string) ([]attentionItem, error) {
	var one int
	if err := s.db.WithContext(ctx).Raw(
		`SELECT 1 FROM workspaces WHERE id = ? AND tenant_id = ? AND user_id = ?`,
		workspaceID, p.TenantID, p.UserID).Scan(&one).Error; err != nil {
		return nil, err
	}
	if one == 0 {
		return nil, ErrWorkspaceNotFound
	}
	var questions []struct {
		Kind            string    `gorm:"column:kind"`
		TaskID          string    `gorm:"column:task_id"`
		OrchestrationID string    `gorm:"column:orchestration_id"`
		Title           string    `gorm:"column:title"`
		Count           int       `gorm:"column:count"`
		QuestionID      string    `gorm:"column:question_id"`
		RunID           string    `gorm:"column:run_id"`
		Status          string    `gorm:"column:status"`
		UpdatedAt       time.Time `gorm:"column:updated_at"`
	}
	if err := s.db.WithContext(ctx).Raw(`
SELECT 'question' AS kind, t.id AS task_id, t.orchestration_id, t.task_key AS title,
       COUNT(q.id) AS count,
       (array_agg(q.id ORDER BY q.created_at, q.id))[1] AS question_id,
       r.id AS run_id, t.status, MAX(q.created_at) AS updated_at
  FROM agent_tasks t
  JOIN runs r ON r.id = t.current_run_id
  JOIN run_questions q ON q.run_id = r.id
 WHERE t.tenant_id = ? AND t.workspace_id = ?
   AND t.status = ? AND r.status = 'running' AND r.lease_until > now()
   AND q.status = ? AND (q.expires_at IS NULL OR q.expires_at > now())
 GROUP BY t.id, t.orchestration_id, t.task_key, r.id, t.status`,
		p.TenantID, workspaceID, agentTaskStatusRunning, questionStatusPending).Scan(&questions).Error; err != nil {
		return nil, err
	}
	items := make([]attentionItem, 0, len(questions))
	for _, q := range questions {
		items = append(items, attentionItem{
			Kind: q.Kind, TaskID: q.TaskID, OrchestrationID: q.OrchestrationID, Title: q.Title,
			Count: q.Count, QuestionID: q.QuestionID, RunID: RunID(q.RunID), Status: q.Status,
			UpdatedAt: q.UpdatedAt,
		})
	}
	var groups []struct {
		Kind            string    `gorm:"column:kind"`
		OrchestrationID string    `gorm:"column:orchestration_id"`
		Title           string    `gorm:"column:title"`
		RunID           string    `gorm:"column:run_id"`
		NotificationID  string    `gorm:"column:notification_id"`
		Status          string    `gorm:"column:status"`
		UpdatedAt       time.Time `gorm:"column:updated_at"`
	}
	if err := s.db.WithContext(ctx).Raw(`
SELECT kind, orchestration_id, title, run_id, notification_id, status, updated_at FROM (
  SELECT 'plan_review' AS kind, o.id AS orchestration_id, o.goal AS title, '' AS run_id,
         COALESCE((SELECT n.id FROM agent_notifications n
                    WHERE n.orchestration_id = o.id AND n.kind = ? AND n.status = ?
                    ORDER BY n.created_at DESC, n.id DESC LIMIT 1), '') AS notification_id,
         o.status, o.updated_at
    FROM agent_orchestrations o
   WHERE o.tenant_id = ? AND o.workspace_id = ? AND o.status = ? AND o.plan_hold = true
  UNION ALL
  SELECT 'delivery_failed', o.id, o.goal, o.delivery_run_id,
         COALESCE((SELECT n.id FROM agent_notifications n
                    WHERE n.orchestration_id = o.id AND n.kind = ? AND n.status = ?
                    ORDER BY n.created_at DESC, n.id DESC LIMIT 1), ''),
         o.status, o.updated_at
    FROM agent_orchestrations o
   WHERE o.tenant_id = ? AND o.workspace_id = ? AND o.delivery_status = ?
) x`,
		notifKindPlanReviewRequired, notifStatusPending,
		p.TenantID, workspaceID, orchStatusActive,
		notifKindOrchFinished, notifStatusFailed,
		p.TenantID, workspaceID, deliveryStatusFailed).Scan(&groups).Error; err != nil {
		return nil, err
	}
	for _, g := range groups {
		items = append(items, attentionItem{
			Kind: g.Kind, OrchestrationID: g.OrchestrationID, Title: g.Title, Count: 1,
			RunID: RunID(g.RunID), NotificationID: g.NotificationID, Status: g.Status, UpdatedAt: g.UpdatedAt,
		})
	}
	var failedNotifs []struct {
		TaskID          string    `gorm:"column:task_id"`
		OrchestrationID string    `gorm:"column:orchestration_id"`
		Title           string    `gorm:"column:title"`
		RunID           string    `gorm:"column:run_id"`
		NotificationID  string    `gorm:"column:notification_id"`
		Status          string    `gorm:"column:status"`
		UpdatedAt       time.Time `gorm:"column:updated_at"`
	}
	if err := s.db.WithContext(ctx).Raw(`
SELECT n.task_id, n.orchestration_id,
       COALESCE(NULLIF(t.task_key, ''), o.goal) AS title,
       n.claim_run_id AS run_id, n.id AS notification_id, n.status,
       COALESCE(n.failed_at, n.updated_at) AS updated_at
  FROM agent_notifications n
  JOIN agent_orchestrations o ON o.id = n.orchestration_id
  LEFT JOIN agent_tasks t ON t.id = n.task_id
 WHERE n.tenant_id = ? AND o.workspace_id = ?
   AND n.kind <> ? AND n.status = ?`,
		p.TenantID, workspaceID, notifKindOrchFinished, notifStatusFailed).Scan(&failedNotifs).Error; err != nil {
		return nil, err
	}
	for _, n := range failedNotifs {
		items = append(items, attentionItem{
			Kind: "delivery_failed", TaskID: n.TaskID, OrchestrationID: n.OrchestrationID, Title: n.Title,
			Count: 1, RunID: RunID(n.RunID), NotificationID: n.NotificationID, Status: n.Status,
			UpdatedAt: n.UpdatedAt,
		})
	}
	var interrupted []struct {
		TaskID          string    `gorm:"column:task_id"`
		OrchestrationID string    `gorm:"column:orchestration_id"`
		Title           string    `gorm:"column:title"`
		RunID           string    `gorm:"column:run_id"`
		UpdatedAt       time.Time `gorm:"column:updated_at"`
	}
	if err := s.db.WithContext(ctx).Raw(`
SELECT t.id AS task_id, t.orchestration_id, t.task_key AS title,
       COALESCE(t.current_run_id, '') AS run_id, t.updated_at
  FROM agent_tasks t
  LEFT JOIN runs r ON r.id = t.current_run_id
 WHERE t.tenant_id = ? AND t.workspace_id = ? AND t.status = ?
   AND t.current_run_id IS NOT NULL
   AND (r.id IS NULL OR r.status <> 'running' OR r.lease_until <= now())`,
		p.TenantID, workspaceID, agentTaskStatusRunning).Scan(&interrupted).Error; err != nil {
		return nil, err
	}
	for _, t := range interrupted {
		items = append(items, attentionItem{
			Kind: "interrupted", TaskID: t.TaskID, OrchestrationID: t.OrchestrationID, Title: t.Title,
			Count: 1, RunID: RunID(t.RunID), Status: agentTaskStatusInterrupted, UpdatedAt: t.UpdatedAt,
		})
	}
	// 协调 run 租约过期：通知仍 claimed 但它的消费 run 已不是有效存活（读侧派生，
	// 与 Task 中断同一条规则）。点击后进入来源会话的恢复入口（GET 该通知拿 runId）。
	var coordInterrupted []struct {
		TaskID          string    `gorm:"column:task_id"`
		OrchestrationID string    `gorm:"column:orchestration_id"`
		Title           string    `gorm:"column:title"`
		RunID           string    `gorm:"column:run_id"`
		NotificationID  string    `gorm:"column:notification_id"`
		UpdatedAt       time.Time `gorm:"column:updated_at"`
	}
	if err := s.db.WithContext(ctx).Raw(`
SELECT n.task_id, n.orchestration_id,
       COALESCE(NULLIF(t.task_key, ''), o.goal) AS title,
       n.claim_run_id AS run_id, n.id AS notification_id, n.updated_at
  FROM agent_notifications n
  JOIN agent_orchestrations o ON o.id = n.orchestration_id
  LEFT JOIN agent_tasks t ON t.id = n.task_id
  LEFT JOIN runs r ON r.id = n.claim_run_id
 WHERE n.tenant_id = ? AND o.workspace_id = ?
   AND n.status = ? AND n.claim_run_id <> ''
   AND (r.id IS NULL OR r.status <> 'running' OR r.lease_until <= now())`,
		p.TenantID, workspaceID, notifStatusClaimed).Scan(&coordInterrupted).Error; err != nil {
		return nil, err
	}
	for _, n := range coordInterrupted {
		items = append(items, attentionItem{
			Kind: "interrupted", TaskID: n.TaskID, OrchestrationID: n.OrchestrationID, Title: n.Title,
			Count: 1, RunID: RunID(n.RunID), NotificationID: n.NotificationID,
			Status: agentTaskStatusInterrupted, UpdatedAt: n.UpdatedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.Before(b.UpdatedAt)
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.TaskID != b.TaskID {
			return a.TaskID < b.TaskID
		}
		if a.OrchestrationID != b.OrchestrationID {
			return a.OrchestrationID < b.OrchestrationID
		}
		return a.NotificationID < b.NotificationID
	})
	return items, nil
}

// getTaskAttention = GET /api/workspaces/:id/task-attention（§4.6）。
func (s *Server) getTaskAttention(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	items, err := s.store.TaskAttention(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"items": items})
}
