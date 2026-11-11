// 本文件是计划修订的三个 HTTP 端点（方案真源 = doc/72 §4.2–§4.4；施工包同节）：
//
//	POST /api/orchestrations/:id/revisions   接受一次修订（201）
//	POST /api/orchestrations/:id/continue    沿当前图继续（200）
//	GET  /api/orchestrations/:id/revisions   修订历史（200，revision 升序）
//
// 三个 handler 只拆请求体与翻业务码；校验、CAS、预算、整图不变量全在 agent_task_revision.go
// 的统一服务里——HTTP 与模型工具走同一份判定。
package runengine

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// revisionStoreErr 把修订服务的业务错误翻成信封。true = 已写响应。
// planError 归 400/117101（原因进 data.reason）；三种状态冲突各有独立码（doc/72 §4.9）。
func revisionStoreErr(c *gin.Context, err error) bool {
	var pe *planError
	switch {
	case errors.As(err, &pe):
		writeEnvelope(c, http.StatusBadRequest, codePlanInvalid, "任务计划不合法", gin.H{"reason": pe.reason})
	case errors.Is(err, errRevisionStale):
		writeEnvelope(c, http.StatusConflict, codeRevisionStale, "计划已有新修订，请重新读取后再试", nil)
	case errors.Is(err, errOrchNotRevisable):
		writeEnvelope(c, http.StatusConflict, codeNotRevisable, "这个编排组现在不能修订", nil)
	case errors.Is(err, errRevisionConflict):
		writeEnvelope(c, http.StatusConflict, codeRevisionConflict, "这个修订键已经用于另一次修订", nil)
	default:
		return taskStoreErr(c, err)
	}
	return true
}

// revisionTaskInput 是修订请求里的新增节点（§4.2 的完整节点字段）。
type revisionTaskInput struct {
	Key            string `json:"key"`
	Content        string `json:"content"`
	Agent          string `json:"agent"`
	ModelRef       string `json:"modelRef"`
	PlanCheckpoint bool   `json:"planCheckpoint"`
}

// postOrchestrationRevision = POST /api/orchestrations/:id/revisions（§4.2）。
func (s *Server) postOrchestrationRevision(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req struct {
		IdempotencyKey string              `json:"idempotencyKey"`
		BaseRevision   int                 `json:"baseRevision"`
		Reason         string              `json:"reason"`
		AddTasks       []revisionTaskInput `json:"addTasks"`
		AddLinks       []planLink          `json:"addLinks"`
		CancelTasks    []string            `json:"cancelTasks"`
		FinalTaskKey   string              `json:"finalTaskKey"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	env, err := s.taskPlanEnv(c.Request.Context(), p)
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	cmd := RevisionCommand{
		IdempotencyKey: req.IdempotencyKey, BaseRevision: req.BaseRevision, Reason: req.Reason,
		CancelTasks: req.CancelTasks, FinalTaskKey: req.FinalTaskKey, AddLinks: req.AddLinks,
	}
	for _, t := range req.AddTasks {
		cmd.AddTasks = append(cmd.AddTasks, planTask{
			Key: t.Key, Content: t.Content, Agent: t.Agent, ModelRef: t.ModelRef,
			PlanCheckpoint: t.PlanCheckpoint,
		})
	}
	resp, err := s.store.CreatePlanRevision(c.Request.Context(), p, c.Param("id"), cmd, env)
	if err != nil {
		if revisionStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", resp)
}

// postOrchestrationContinue = POST /api/orchestrations/:id/continue（§4.3）。
func (s *Server) postOrchestrationContinue(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req struct {
		BaseRevision int `json:"baseRevision"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	resp, err := s.store.ContinueOrchestration(c.Request.Context(), p, c.Param("id"), req.BaseRevision)
	if err != nil {
		if revisionStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", resp)
}

// getOrchestrationRevisions = GET /api/orchestrations/:id/revisions（§4.4）。
func (s *Server) getOrchestrationRevisions(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	rows, err := s.store.ListPlanRevisions(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if revisionStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"revisions": rows})
}
