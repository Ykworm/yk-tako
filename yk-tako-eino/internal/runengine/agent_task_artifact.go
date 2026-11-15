// 本文件是隔离结果的两个人工读面/恢复端点（方案真源 = doc/74 §4.4/§4.5/§11.1；施工包 §4.4/§4.5）：
//
//	GET  /api/tasks/:id/artifact?path=…      当前结果版本的不可变产物字节（不进信封）
//	POST /api/tasks/:id/retry-publication    发布受阻后人工创建一场新的处理 run
//
// artifact 只认不可变结果清单：path 逐字命中当前 resultRevision 声明的产物才读，
// 不用主项目同路径文件代替（项目当前文件走既有 GET /api/media）。
// retry-publication 只认 isolated 最终 Task 的发布三码 blocked：同一执行 session 铸新 run
// （旧 run 与 runlog 保留），Task 回 running、组发布状态复位，执行者按 Provider 恢复说明
// 处理后重新提交——新结果走既有提交/发布链，不另造恢复专用裁决。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// errArtifactUnreadable：结果引用缺失、产物不在清单、越界或文件已不可读（409 117110）。
	errArtifactUnreadable = errors.New("结果产物已不可读取")
	// errTaskNotRepublishable：非最终、非发布受阻、状态矛盾等（409 117112）。
	errTaskNotRepublishable = errors.New("任务现在不能重新发布")
)

// TaskArtifact 读一件 Task 当前结果版本里声明的产物字节（施工包 §4.4 的存储半）。
// 属主链与任务详情同一条（非本人与不存在同形 117001）；shared 与 isolated 都有不可变结果
// （doc/76 §4/§5），读口同一条；结果缺失/路径失效归 errArtifactUnreadable，Provider 内部
// 异常原样上抛（handler 翻 500）。
func (s *Store) TaskArtifact(ctx context.Context, p principal, taskID, path string) ([]byte, error) {
	var rows []AgentTask
	if err := s.db.WithContext(ctx).Raw(taskOwnerQuery+` AND t.id = ?`, p.TenantID, p.UserID, taskID).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errTaskNotFound
	}
	t := rows[0]
	var o AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", t.OrchestrationID).Take(&o).Error; err != nil {
		return nil, err
	}
	if t.ResultRevision == 0 || parseTaskResult(t.Result) == nil {
		return nil, errArtifactUnreadable
	}
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		return nil, err
	}
	provider, err := s.workspaceProvider(g)
	if err != nil {
		return nil, err
	}
	b, err := provider.ReadArtifact(ctx, g, ResultRef{TaskID: t.ID, ResultRevision: t.ResultRevision}, path)
	if err != nil {
		if errors.Is(err, errArtifactBad) || errors.Is(err, errImmutableResultMissing) || os.IsNotExist(err) {
			return nil, errArtifactUnreadable
		}
		return nil, err
	}
	return b, nil
}

// getTaskArtifact = GET /api/tasks/:id/artifact?path=…（施工包 §4.4）。
// 成功回原始字节 + Content-Type（已知扩展名按媒体表，其余按内容嗅探），不进 JSON 信封。
func (s *Server) getTaskArtifact(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	path := c.Query("path")
	data, err := s.store.TaskArtifact(c.Request.Context(), p, c.Param("id"), path)
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	mime := mediaMimeByExt[strings.ToLower(filepath.Ext(path))]
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	c.Data(http.StatusOK, mime, data)
}

// publicationRetryView 是 POST retry-publication 的响应体（施工包 §4.5，字段序固定）：
// taskId → runId → status → publicationEpoch。publicationEpoch 是本次处理完成后发布将使用的
// 代次（当前组代次 +1：受阻那场的代次已用过，准入只会递增）。
type publicationRetryView struct {
	TaskID           string `json:"taskId"`
	RunID            RunID  `json:"runId"`
	Status           string `json:"status"`
	PublicationEpoch int64  `json:"publicationEpoch"`
}

// RetryTaskPublication 是人工重新发布（施工包 §4.5）：只在最终 Task 存储 blocked
// 且错误码是发布三码、当前结果版本与不可变结果仍有效时，在**同一执行 session** 铸一场新 run。
// shared 与 isolated 同一判据——doc/76 §4 让 shared 也走发布链，受阻后同样需要这条恢复路径。
// 返回 claim 非空 = 真的创建了处理 run（调用方提交后启动）；claim 为空 = 幂等命中
// （已有当前处理 run 活着），只回现状。
//
// 事务内的状态翻转：Task blocked→running（清发布错误、指向新 run）；组 publication_status
// blocked→''（新 run 提交结果后 ReconcileRun 的 publishing 分支重新落 pending，准入代次 +1）。
// 文件系统校验（不可变结果存在）在事务之前做——文件工作不进 PG 锁。
func (s *Store) RetryTaskPublication(ctx context.Context, p principal, taskID string) (publicationRetryView, *agentTaskClaim, error) {
	var rows []AgentTask
	if err := s.db.WithContext(ctx).Raw(taskOwnerQuery+` AND t.id = ?`, p.TenantID, p.UserID, taskID).
		Scan(&rows).Error; err != nil {
		return publicationRetryView{}, nil, err
	}
	if len(rows) == 0 {
		return publicationRetryView{}, nil, errTaskNotFound
	}
	pre := rows[0]
	var o AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", pre.OrchestrationID).Take(&o).Error; err != nil {
		return publicationRetryView{}, nil, err
	}
	if pre.ID != o.FinalTaskID {
		return publicationRetryView{}, nil, errTaskNotRepublishable
	}
	if pre.Status != agentTaskStatusBlocked && pre.Status != agentTaskStatusRunning {
		return publicationRetryView{}, nil, errTaskNotRepublishable
	}
	if pre.Status == agentTaskStatusBlocked && !isPublicationErrorCode(pre.ErrorCode) {
		return publicationRetryView{}, nil, errTaskNotRepublishable
	}
	// 结果有效性（事务外）：当前版本必须已提交且不可变结果完整，丢失 = 117110。
	if pre.ResultRevision == 0 || parseTaskResult(pre.Result) == nil {
		return publicationRetryView{}, nil, errArtifactUnreadable
	}
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		return publicationRetryView{}, nil, err
	}
	if _, err := readImmutableResult(g, ResultRef{TaskID: pre.ID, ResultRevision: pre.ResultRevision}); err != nil {
		if errors.Is(err, errImmutableResultMissing) {
			return publicationRetryView{}, nil, errArtifactUnreadable
		}
		return publicationRetryView{}, nil, err
	}
	providerKind := ""
	if st, serr := readWorkspaceGroupState(g.Dir); serr == nil && st != nil {
		providerKind = st.Provider
	}

	ctx, span := startStep(ctx, "task_publication_retry")
	span.SetAttributes(
		attribute.String("tako.orchestration.id", o.ID),
		attribute.String("tako.task.id", pre.ID),
		attribute.String("tako.workspace.provider", providerKind),
		attribute.Int("tako.workspace.result_revision", pre.ResultRevision),
		attribute.String("tako.workspace.publication_error_code", pre.ErrorCode),
	)
	var out publicationRetryView
	var claim *agentTaskClaim
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 锁序与发布收尾同向（组行 → Task 行）：finishPublication / ReconcileRun 不会与本事务交错成环。
		var locked AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", o.ID).Take(&locked).Error; err != nil {
			return err
		}
		var t AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", pre.ID).Take(&t).Error; err != nil {
			return err
		}
		if locked.Status != orchStatusActive || t.ID != locked.FinalTaskID {
			return errTaskNotRepublishable
		}
		switch t.Status {
		case agentTaskStatusRunning:
			// 双击／多标签／双副本：已有当前处理 run 活着 → 回现状，不新建。
			if t.CurrentRunID != nil {
				var alive int64
				if err := tx.Raw(
					`SELECT COUNT(*) FROM runs WHERE id = ? AND status = 'running' AND lease_until > now()`,
					*t.CurrentRunID).Scan(&alive).Error; err != nil {
					return err
				}
				if alive > 0 {
					out = publicationRetryView{TaskID: t.ID, RunID: RunID(*t.CurrentRunID),
						Status: agentTaskStatusRunning, PublicationEpoch: locked.PublicationEpoch + 1}
					return nil
				}
			}
			return errTaskNotRepublishable // running 但 run 已死 = interrupted，走 resume 不走这里
		case agentTaskStatusBlocked:
			if !isPublicationErrorCode(t.ErrorCode) {
				return errTaskNotRepublishable // blocked 的既有依赖语义不归本端点
			}
		default:
			return errTaskNotRepublishable
		}
		if locked.PublicationStatus != publicationStatusBlocked && locked.PublicationStatus != "" {
			return errTaskNotRepublishable // publishing/pending/published 与 blocked Task 矛盾
		}
		if t.ExecutionSessionID == nil || *t.ExecutionSessionID == "" {
			return errTaskNotRepublishable // 执行 session 缺失 = 数据被动过
		}
		sessID := SessionID(*t.ExecutionSessionID)
		// 先清扫租约已过的死行（AcquireRun 同一条扫尾）：唯一索引只认 status='running'。
		if err := expireDeadSessionRunsTx(tx, sessID); err != nil {
			return err
		}
		var live int64
		if err := tx.Raw(
			`SELECT COUNT(*) FROM runs WHERE session_id = ? AND status = 'running' AND lease_until > now()`,
			sessID).Scan(&live).Error; err != nil {
			return err
		}
		if live > 0 {
			return errTaskNotRepublishable // blocked 但 session 里有活 run = 矛盾状态
		}
		if err := s.lockTenantGate(tx, locked.TenantID); err != nil {
			return err
		}
		input := buildPublicationRecoveryInput(locked, t)
		runID, err := s.insertPublicationRetryRun(tx, t, sessID, input)
		if err != nil {
			return err
		}
		res := tx.Model(&AgentTask{}).
			Where("id = ? AND status = ?", t.ID, agentTaskStatusBlocked).
			Updates(map[string]any{
				"status": agentTaskStatusRunning, "current_run_id": string(runID),
				"error_code": "", "error_message": "", "finished_at": nil, "updated_at": nowUTC(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errTaskNotRepublishable
		}
		if locked.PublicationStatus == publicationStatusBlocked {
			if err := tx.Model(&AgentOrchestration{}).
				Where("id = ? AND publication_status = ?", locked.ID, publicationStatusBlocked).
				Updates(map[string]any{
					"publication_status": "", "publication_error_code": "", "updated_at": nowUTC(),
				}).Error; err != nil {
				return err
			}
		}
		// Redis 半殿后（AcquireRun 同式）：失败整笔回滚，不留「running 但没有 runlive」的行。
		start, err := json.Marshal(Frame{Kind: "start"})
		if err != nil {
			return err
		}
		if err := s.runliveStart(ctx, runID, 1, string(start)); err != nil {
			return err
		}
		claim = &agentTaskClaim{
			TaskID: t.ID, TaskKey: t.TaskKey, SessionID: sessID, RunID: runID,
			TenantID: locked.TenantID, UserID: p.UserID, WorkspaceID: t.WorkspaceID,
			Epoch: 1, FirstInput: input,
			OrchestrationID: locked.ID, SourceRunID: locked.SourceRunID,
		}
		out = publicationRetryView{TaskID: t.ID, RunID: runID,
			Status: agentTaskStatusRunning, PublicationEpoch: locked.PublicationEpoch + 1}
		span.SetAttributes(
			attribute.String("tako.run.id", string(runID)),
			attribute.Int64("tako.workspace.publication_epoch", out.PublicationEpoch),
		)
		return nil
	})
	finishSpan(span, err)
	if err != nil {
		return publicationRetryView{}, nil, err
	}
	s.PublishEvent(ctx, p.TenantID, eventAgentTasksChanged, out.TaskID)
	s.PublishEvent(ctx, p.TenantID, eventAgentOrchestrationsChanged, o.ID)
	s.PublishTaskWake(ctx, p.TenantID, "publication_retry", out.TaskID)
	return out, claim, nil
}

// insertPublicationRetryRun 在**既有执行 session** 里铸一场新 run（施工包 §4.5）：
// run 行（epoch 1、租约走 DB 时钟）→ mh seq1 user 行 → runlog user 行（internal=false，
// 与首次领取同口径：执行指令对执行 session 的账本是正常内容）。旧 run 与旧账本行不动。
func (s *Store) insertPublicationRetryRun(tx *gorm.DB, t AgentTask, sessID SessionID, input string) (RunID, error) {
	runID, err := newRunID()
	if err != nil {
		return "", err
	}
	msgID, err := newRowID("msg_")
	if err != nil {
		return "", err
	}
	logID, err := newRowID("rl_")
	if err != nil {
		return "", err
	}
	if err := tx.Exec(`
INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, workspace_id, created_at)
 VALUES (?, ?, 'running', 1, now() + make_interval(secs => ?), ?, now())`,
		runID, sessID, s.leaseTTL.Seconds(), t.WorkspaceID).Error; err != nil {
		return "", err
	}
	if err := tx.Exec(`
INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
 VALUES (?, ?, ?, 1, 'user', ?, false, now())`,
		msgID, sessID, runID, input).Error; err != nil {
		return "", err
	}
	if err := tx.Exec(`
INSERT INTO runlog_entries (id, session_id, seq, run_id, role, message_id, text, created_at)
 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runlog_entries WHERE session_id = ?),
         ?, 'user', ?, ?, now())`,
		logID, sessID, sessID, runID, msgID, input).Error; err != nil {
		return "", err
	}
	return runID, nil
}

// buildPublicationRecoveryInput 构造重新发布处理 run 的首条输入（施工包 §4.5）：
// 原始目标 → 本件 key/content → 当前 resultRevision → 通用受阻原因 + Provider 恢复说明
// （= 受阻时 PublishOutcome.Detail，已存 error_message）→ 执行约定。
func buildPublicationRecoveryInput(o AgentOrchestration, t AgentTask) string {
	var b strings.Builder
	b.WriteString("你是一件持久 Task 的执行者。这是一场人工重新发布处理：结果已经提交并固化，但发布回原项目时受阻，需要你处理后重新提交。\n\n## 编排目标\n")
	b.WriteString(o.Goal)
	b.WriteString("\n\n## 本件任务\nkey: ")
	b.WriteString(t.TaskKey)
	b.WriteString("\n")
	b.WriteString(t.Content)
	b.WriteString("\n\n## 当前结果\nresultRevision: ")
	b.WriteString(fmt.Sprint(t.ResultRevision))
	b.WriteString("（已固化的不可变结果；重新提交会生成新版本）\n\n## 发布受阻原因\n")
	b.WriteString(publicationErrorText(t.ErrorCode))
	b.WriteString("（")
	b.WriteString(t.ErrorCode)
	b.WriteString("）\n")
	if t.ErrorMessage != "" {
		b.WriteString("Provider 恢复说明：")
		b.WriteString(t.ErrorMessage)
		b.WriteString("\n")
	}
	b.WriteString("\n## 执行约定\n")
	b.WriteString("在当前工作现场按恢复说明处理发布障碍（例如重新合并新基线、裁决冲突），完成后调用 submit_task_result 重新提交结果，然后结束本轮。")
	b.WriteString("不要把工具回执当作已经交付：只有 submit_task_result 的回执才算提交。\n")
	return b.String()
}

// publicationErrorText 把发布三码翻成通用展示文案（与 Provider 恢复说明分开：
// 前者是引擎词表，后者是 Provider 对现场的具体解释）。
func publicationErrorText(code string) string {
	switch code {
	case taskErrWorkspaceChanged:
		return "项目已前进，旧结果不能覆盖"
	case taskErrWorkspaceDirty:
		return "项目有未保存内容"
	case taskErrPublicationRecovery:
		return "现场无法解释，需人工检查"
	}
	return "发布受阻"
}

// retryTaskPublication = POST /api/tasks/:id/retry-publication（施工包 §4.5）。
//
//	202 {"code":0,"message":"","data":{"taskId":"at_…","runId":"r_…","status":"running","publicationEpoch":2}}
//	200 已有当前处理 run 活着（双击／多副本）
//	404 117001；403 111203（viewer，中间件）；409 117112 现在不能重新发布；
//	409 117110 结果产物已不可读取；429 113001 租户容量满
func (s *Server) retryTaskPublication(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	if s.tasks == nil || s.tasks.assembleTask == nil || s.worker == nil {
		internal(c, errors.New("task scheduler not attached"))
		return
	}
	view, claim, err := s.store.RetryTaskPublication(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	status := http.StatusOK
	if claim != nil {
		status = http.StatusAccepted
		s.tasks.launchTaskRun(c.Request.Context(), *claim)
	}
	writeEnvelope(c, status, codeOK, "", view)
}