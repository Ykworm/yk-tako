// 本文件是持久 Task 与编排组的 HTTP 端点（方案真源 = doc/70 §4；施工包 §4）。
//
// 七个端点的共性：信封照旧 writeEnvelope；错误全部经 taskStoreErr 翻成业务码——
// 创建校验失败 117101（原因进 data.reason）、幂等冲突 117102、来源会话/项目/执行者/模型
// 复用 112001/114001/112003/111408，可见性失败一律 404（非本人与不存在同形，不给探测通道）。
// 写面挂 RequireTenantRole("member")（viewer 403 111203，既有中间件行为），读面只挂 authn，
// 再靠 store 层的可见性 JOIN 兜住。resume 端点在 agent_task_runner.go（要复用执行装配）。
package runengine

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// taskPlanEnv 组创建服务的模型校验闭包：从租户运行时配置现读 raw + secrets，
// ref 校验走 TenantModels.Resolve（带进程内缓存，领取装配时重复解析不付第二遍钱）。
// 模型面没配（worker.models 为 nil）与模型 ref 解析不了都归 111408（施工包 §4.4）。
func (s *Server) taskPlanEnv(ctx context.Context, p principal) (planEnv, error) {
	if s.worker == nil || s.worker.models == nil {
		return planEnv{}, errCreateModel
	}
	cfg, raw, err := s.store.RuntimeConfigRecord(ctx, p.TenantID)
	if err != nil {
		return planEnv{}, err
	}
	secrets, err := unsealSecretMap(cfg.Secrets)
	if err != nil {
		log.Printf("taskPlanEnv unseal tenant=%s: %v", p.TenantID, err)
		return planEnv{}, err
	}
	return planEnv{resolveModel: func(ctx context.Context, ref string) error {
		_, _, err := s.worker.models.Resolve(ctx, p.TenantID, ref, raw, secrets)
		return err
	}}, nil
}

// taskStoreErr 把 store 层业务错误翻成信封。true = 已写响应；false = 不认识的错，调用方走 internal。
func taskStoreErr(c *gin.Context, err error) bool {
	var pe *planError
	switch {
	case errors.As(err, &pe):
		writeEnvelope(c, http.StatusBadRequest, codePlanInvalid, "任务计划不合法", gin.H{"reason": pe.reason})
	case errors.Is(err, errPlanConflict):
		writeEnvelope(c, http.StatusConflict, codePlanConflict, "这个创建键已经用于另一份计划", nil)
	case errors.Is(err, errCreateSource):
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
	case errors.Is(err, errCreateWorkspace), errors.Is(err, ErrWorkspaceNotFound):
		writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
	case errors.Is(err, errCreateAgent):
		writeEnvelope(c, http.StatusNotFound, codeAgentMissing, "找不到执行者规格", nil)
	case errors.Is(err, errCreateModel):
		writeEnvelope(c, http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", nil)
	case errors.Is(err, errTaskNotFound):
		writeEnvelope(c, http.StatusNotFound, codeTaskMissing, "找不到这件任务", nil)
	case errors.Is(err, errOrchNotFound):
		writeEnvelope(c, http.StatusNotFound, codeOrchMissing, "找不到这个编排组", nil)
	case errors.Is(err, errTaskNotStoppable):
		writeEnvelope(c, http.StatusConflict, codeTaskFinished, "这件任务已经结束，不能停止", nil)
	case errors.Is(err, errOrchNotStoppable):
		writeEnvelope(c, http.StatusConflict, codeOrchFinished, "这个编排组已经结束，不能停止", nil)
	default:
		return false
	}
	return true
}

// postAgentTask 创建单件 Task（施工包 §4.1）。
//
//	202 {"code":0,"message":"","data":{"taskId":"at_…","orchestrationId":"or_…","status":"ready","currentRunId":""}}
//	400 117101 计划不合法（reason 进 data.reason）；409 117102 幂等键已用于另一份计划
//	404 112001/114001/112003 来源会话/项目/执行者缺失；409 111408 模型不可用
//
// 必填项在这里先判（111002），形状规则（长度、key 词法）归统一创建服务——只有一份校验。
func (s *Server) postAgentTask(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req struct {
		IdempotencyKey  string `json:"idempotencyKey"`
		WorkspaceID     string `json:"workspaceId"`
		SourceSessionID string `json:"sourceSessionId"`
		Content         string `json:"content"`
		Agent           string `json:"agent"`
		ModelRef        string `json:"modelRef"`
		PlanCheckpoint  bool   `json:"planCheckpoint,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if req.IdempotencyKey == "" || req.WorkspaceID == "" || req.SourceSessionID == "" ||
		req.Content == "" || req.Agent == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "idempotencyKey、workspaceId、sourceSessionId、content、agent 都是必填", nil)
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
	resp, err := s.store.CreateAgentTask(c.Request.Context(), p, CreateTaskCommand{
		IdempotencyKey:  req.IdempotencyKey,
		WorkspaceID:     req.WorkspaceID,
		SourceSessionID: SessionID(req.SourceSessionID),
		Content:         req.Content,
		Agent:           req.Agent,
		ModelRef:        req.ModelRef,
		PlanCheckpoint:  req.PlanCheckpoint,
	}, env)
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusAccepted, codeOK, "", resp)
}

// postAgentOrchestration 创建固定 DAG 编排组（施工包 §4.2）。形状校验全在创建服务的
// normalizeOrchestrationPlan（1–32 节点、key 词法、防环、恰好一个最终节点），handler 只拆请求体。
func (s *Server) postAgentOrchestration(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req struct {
		IdempotencyKey  string     `json:"idempotencyKey"`
		WorkspaceID     string     `json:"workspaceId"`
		SourceSessionID string     `json:"sourceSessionId"`
		Goal            string     `json:"goal"`
		Tasks           []planTask `json:"tasks"`
		Links           []planLink `json:"links"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if req.IdempotencyKey == "" || req.WorkspaceID == "" || req.SourceSessionID == "" || req.Goal == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "idempotencyKey、workspaceId、sourceSessionId、goal 都是必填", nil)
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
	resp, err := s.store.CreateAgentOrchestration(c.Request.Context(), p, CreateOrchestrationCommand{
		IdempotencyKey:  req.IdempotencyKey,
		WorkspaceID:     req.WorkspaceID,
		SourceSessionID: SessionID(req.SourceSessionID),
		Goal:            req.Goal,
		Tasks:           req.Tasks,
		Links:           req.Links,
	}, env)
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusAccepted, codeOK, "", resp)
}

// getAgentTask 读一件完整 Task（施工包 §4.3）。interrupted 在 store 层读侧派生。
func (s *Server) getAgentTask(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
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
	writeEnvelope(c, http.StatusOK, codeOK, "", view)
}

// listWorkspaceAgentTasks 列本人项目下的 Task（施工包 §4.3：createdAt/id 倒序，V1 无游标）。
func (s *Server) listWorkspaceAgentTasks(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	views, err := s.store.ListWorkspaceTasks(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"tasks": views})
}

// getAgentOrchestration 读组、节点、边与最终交付状态（施工包 §4.3）。
func (s *Server) getAgentOrchestration(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	view, err := s.store.GetAgentOrchestration(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", view)
}

// stopTaskRun 是 task/group stop 落库后的收尾半：复用 run 停止链路（条件 UPDATE + 进程内取消）。
// 失败只记日志——取消请求已持久化（任务行已翻 / 组在 cancelling），这半漏了由调度对账兜住
// （running 节点的 run 终态进 ReconcileRun），不影响响应。
func (s *Server) stopTaskRun(ctx context.Context, runID RunID) {
	status, found, err := s.store.StopRun(ctx, runID)
	if err != nil {
		log.Printf("POST task stop StopRun %s: %v", runID, err)
		return
	}
	if found && status == "stopped" {
		// 用户停止永远最大：不看 epoch，注册表里是谁就 cancel 谁（与 stopRun 同一条规则）。
		if cancel, hit := s.worker.runCancels.Load(runID); hit {
			cancel.(*runCancel).cancel()
		}
	}
}

// stopAgentTask 停一件 Task（施工包 §4.3/§8.4）。
//
//	200 {"code":0,"message":"","data":{"taskId":"at_…","status":"cancelled"}}
//	404 117001；409 117104 已终态；重复停止幂等返回 200
//
// 存储事务先翻任务行（submit 立即失效），提交后再 StopRun + 进程内取消；running 之外的
// 状态没有 run 可停。
func (s *Server) stopAgentTask(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	status, runID, err := s.store.StopAgentTask(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	if runID != "" {
		s.stopTaskRun(c.Request.Context(), runID)
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"taskId": c.Param("id"), "status": status})
}

// stopAgentOrchestration 停整组（施工包 §8.4）。
//
//	202 {"code":0,"message":"","data":{"orchestrationId":"or_…","status":"cancelling|cancelled"}}
//	404 117002；409 117105 已终态
//
// 组内全部终态时当场落 cancelled（响应就是 cancelled）；还有 running 节点时回 cancelling，
// running 的 run 在这里收尾（StopRun + 进程内取消），组状态由调度轮在收完后翻 cancelled。
func (s *Server) stopAgentOrchestration(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	status, runIDs, err := s.store.StopAgentOrchestration(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	for _, runID := range runIDs {
		s.stopTaskRun(c.Request.Context(), runID)
	}
	writeEnvelope(c, http.StatusAccepted, codeOK, "", gin.H{"orchestrationId": c.Param("id"), "status": status})
}
