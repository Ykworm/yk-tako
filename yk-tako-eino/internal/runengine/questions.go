// questions.go 是「等人机器」的持久态（doc/47 §3.1 / §5.1）：一次「run 在等这个人这一行」
// 落成 run_questions 的一行。kind 列区分 ask_user 的追问（question）与 submit_plan 的方案审批（plan）。
//
// 状态机只有 pending → answered：answer 端点的 CAS（UPDATE ... WHERE status='pending'）保证
// 一答终答；影响 0 行 = 已答过/已失效（115101）。挂起期间 run 仍是 running（租约照续），
// 行是唯一的「在等什么」真源——多副本下人在哪个入口答、worker 在哪个进程等，都靠它对齐。
package runengine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 行的 kind 与 status 词表（doc/47 §5.1）。
const (
	questionKindQuestion = "question"
	questionKindPlan     = "plan"

	questionStatusPending  = "pending"
	questionStatusAnswered = "answered"
	questionStatusStale    = "stale"

	// 方案行的审批结论（doc/47 §5.3，2026-10-23 交互修订补）：
	//   approve = 批准（可带改写后的方案全文）；
	//   revise  = 驳回并要求修改（plan_mode 不解除，模型改完重新 submit_plan）。
	// 问题行的 decision 恒为空串。
	questionDecisionApprove = "approve"
	questionDecisionRevise  = "revise"
)

// RunQuestion = `run_questions` 表。P/Q 共用：kind=plan 时 question 列存方案全文，
// answer 列存人批准时编辑后的方案全文（未编辑 = 空串）或驳回意见；decision 列存
// approve/revise（问题行恒空）。ToolCallID 对齐 journal 的 intent（resume 重放按它把调用与行对上）；
// Args 是工具原始参数 JSON（question 帧的 args 同源，前端从里面解析 options）。
type RunQuestion struct {
	ID         string    `gorm:"primaryKey"`
	TenantID   string    `gorm:"not null;index:run_questions_run,priority:1"`
	Workspace  string    `gorm:"not null;default:''"`
	RunID      RunID     `gorm:"not null;index:run_questions_run,priority:2"`
	SessionID  SessionID `gorm:"not null;index:run_questions_session"`
	ToolCallID string    `gorm:"not null;default:''"`
	Kind       string    `gorm:"not null"`
	Question   string    `gorm:"not null"`
	Args       string    `gorm:"not null;default:''"`
	Status     string    `gorm:"not null;index:run_questions_run,priority:3"`
	Answer     string    `gorm:"not null;default:''"`
	Decision   string    `gorm:"not null;default:''"`
	CreatedAt  time.Time `gorm:"not null"`
	AnsweredAt *time.Time
}

func (RunQuestion) TableName() string { return "run_questions" }

var (
	ErrQuestionNotFound = errors.New("question not found")
	// ErrQuestionAnswered = CAS 没改到行，但行确实存在：已答过，或已被清扫器收成 stale。
	ErrQuestionAnswered = errors.New("question already answered")
)

// InsertQuestion 落一行（id 空则铸 qs_<ULID>）。状态缺省 pending。
func (s *Store) InsertQuestion(ctx context.Context, q RunQuestion) (RunQuestion, error) {
	if q.ID == "" {
		id, err := newRowID("qs_")
		if err != nil {
			return RunQuestion{}, err
		}
		q.ID = id
	}
	if q.Status == "" {
		q.Status = questionStatusPending
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = time.Now()
	}
	if err := s.db.WithContext(ctx).Create(&q).Error; err != nil {
		return RunQuestion{}, err
	}
	return q, nil
}

// Question 按 id 取一行。不存在 → ErrQuestionNotFound（HTTP 层回 115002）。
func (s *Store) Question(ctx context.Context, id string) (RunQuestion, error) {
	var row RunQuestion
	err := s.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RunQuestion{}, ErrQuestionNotFound
	}
	return row, err
}

// ListQuestions 列一场 run 的行（按 created_at, id）。status 空 = 不筛。
func (s *Store) ListQuestions(ctx context.Context, runID RunID, status string) ([]RunQuestion, error) {
	q := s.db.WithContext(ctx).Where("run_id = ?", runID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var rows []RunQuestion
	err := q.Order("created_at, id").Find(&rows).Error
	return rows, err
}

// AnswerQuestion 是「一答终答」的 CAS：只有 pending 行能被改。
// 影响 1 行 = 这次回答赢；0 行时再判存在性——不存在回 ErrQuestionNotFound，
// 存在（answered/stale）回 ErrQuestionAnswered。decision 只有 plan 行用得上，
// 问题行传空串；方案行的空 decision 在 API 层已归一成 approve，这里不再猜。
func (s *Store) AnswerQuestion(ctx context.Context, id, answer, decision string) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE run_questions SET status = ?, answer = ?, decision = ?, answered_at = now() WHERE id = ? AND status = ?`,
		questionStatusAnswered, answer, decision, id, questionStatusPending)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 1 {
		return nil
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&RunQuestion{}).Where("id = ?", id).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return ErrQuestionNotFound
	}
	return ErrQuestionAnswered
}

// RunWorkspace 读这场 run 挂的项目（行不存在返回空串）。行落库时的 workspace 列取它。
func (s *Store) RunWorkspace(ctx context.Context, runID RunID) (string, error) {
	var rows []struct {
		WorkspaceID *string `gorm:"column:workspace_id"`
	}
	err := s.db.WithContext(ctx).Raw(`SELECT workspace_id FROM runs WHERE id = ?`, runID).Scan(&rows).Error
	if err != nil || len(rows) == 0 || rows[0].WorkspaceID == nil {
		return "", err
	}
	return *rows[0].WorkspaceID, nil
}

// questionView 是 list 端点的响应行（doc/47 §5.3）。args 一并给出：刷新后前端靠
// questions?status=pending 重建问题卡，options 只能从 args 解析（帧可能已过 TTL）。
type questionView struct {
	QuestionID string    `json:"questionId"`
	RunID      RunID     `json:"runId"`
	Kind       string    `json:"kind"`
	Question   string    `json:"question"`
	Args       string    `json:"args,omitempty"`
	Status     string    `json:"status"`
	Decision   string    `json:"decision,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// listQuestions（doc/47 §5.3）= GET /api/runs/:runId/questions?status=pending。
// 可见性跟 run 走：看不见这场 run 就是 404 112002。
func (s *Server) listQuestions(c *gin.Context) {
	runID := RunID(c.Param("runId"))
	if denyRunRead(c, s.viewRun(c, runID)) {
		return
	}
	status := c.Query("status")
	switch status {
	case "", questionStatusPending, questionStatusAnswered, questionStatusStale:
	default:
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "status 只能是 pending / answered / stale", nil)
		return
	}
	rows, err := s.store.ListQuestions(c.Request.Context(), runID, status)
	if err != nil {
		internal(c, err)
		return
	}
	views := make([]questionView, 0, len(rows))
	for _, r := range rows {
		views = append(views, questionView{
			QuestionID: r.ID, RunID: r.RunID, Kind: r.Kind, Question: r.Question,
			Args: r.Args, Status: r.Status, Decision: r.Decision, CreatedAt: r.CreatedAt,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", views)
}

// answerQuestion（doc/47 §5.3）= POST /api/questions/:id/answer，
// body {"answer":"…","decision":"approve|revise"}。
// 空串是合法答案：plan 行「未编辑直接批准」就是 answer=""。decision 缺省 = approve
// （老客户端与原契约不变）；问题行不接受 decision。权限跟 run 走：只有 run 的会话主人能答。
func (s *Server) answerQuestion(c *gin.Context) {
	var req struct {
		Answer   string `json:"answer"`
		Decision string `json:"decision"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	switch req.Decision {
	case "", questionDecisionApprove, questionDecisionRevise:
	default:
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "decision 只能是 approve / revise", nil)
		return
	}
	row, err := s.store.Question(c.Request.Context(), c.Param("id"))
	if errors.Is(err, ErrQuestionNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeQuestionMissing, "找不到这个提问", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	if denyRunWrite(c, s.viewRun(c, row.RunID)) {
		return
	}
	decision := req.Decision
	if row.Kind == questionKindPlan {
		if decision == "" {
			decision = questionDecisionApprove
		}
	} else if decision != "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "只有方案审批能带 decision", nil)
		return
	}
	err = s.store.AnswerQuestion(c.Request.Context(), row.ID, req.Answer, decision)
	switch {
	case errors.Is(err, ErrQuestionNotFound):
		writeEnvelope(c, http.StatusNotFound, codeQuestionMissing, "找不到这个提问", nil)
	case errors.Is(err, ErrQuestionAnswered):
		writeEnvelope(c, http.StatusConflict, codeQuestionAnswered, "这个问题已经答过了", nil)
	case err != nil:
		internal(c, err)
	default:
		writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"questionId": row.ID, "status": questionStatusAnswered})
	}
}

// devQuestionCheck（-dev only，只读诊断口）= Q1 的 seed-question 脚手架转正后的形态
// （doc/47 §6 转正点 = Q4 验收后）。只读：按 run 列问/方案行的 id/kind/status/toolCallId，
// 供无模型时核对「挂起行在不在、答没答、与 journal intent 对没对上」。
//
//	POST /api/dev/question-check {"runId":"r_…","status":"pending"}
//	200 {"code":0,"message":"","data":{"runId":"r_…","total":1,"rows":[{"questionId":"qs_…","kind":"question","status":"pending","toolCallId":"call_…"}]}}
func (s *Server) devQuestionCheck(c *gin.Context) {
	var req struct {
		RunID  string `json:"runId"`
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.RunID) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	switch req.Status {
	case "", questionStatusPending, questionStatusAnswered, questionStatusStale:
	default:
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "status 只能是 pending / answered / stale", nil)
		return
	}
	if denyRunRead(c, s.viewRun(c, RunID(req.RunID))) {
		return
	}
	rows, err := s.store.ListQuestions(c.Request.Context(), RunID(req.RunID), req.Status)
	if err != nil {
		internal(c, err)
		return
	}
	type rowView struct {
		QuestionID string `json:"questionId"`
		Kind       string `json:"kind"`
		Status     string `json:"status"`
		Decision   string `json:"decision,omitempty"`
		ToolCallID string `json:"toolCallId"`
	}
	views := make([]rowView, 0, len(rows))
	for _, r := range rows {
		views = append(views, rowView{
			QuestionID: r.ID, Kind: r.Kind, Status: r.Status,
			Decision: r.Decision, ToolCallID: r.ToolCallID,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"runId": req.RunID, "total": len(views), "rows": views})
}
