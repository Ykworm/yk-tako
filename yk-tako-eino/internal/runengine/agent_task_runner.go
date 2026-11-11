// 本文件是持久 Task 的执行装配、启动与恢复（方案真源 = doc/70 §6.2/§6.3/§7.2/§10.1/§10.3；施工包 §6.2/§6.3/§10.1）。
//
// 与普通聊天的分工：领取（铸 session/run/首条输入）在 agent_task_scheduler.go 的领取事务里；
// 这里只做提交之后的事——装配执行者 profile、启动 Worker.Run、装配失败把 run 收成 error 并把
// Task 落 failed/assembly_failed。resume 两个入口（POST /api/tasks/:id/resume 与普通 run 的
// /api/runs/:runId/resume 撞上执行 session）都复用 ClaimResume → 账本重放 → Redis seed 的
// 现有链条，只把装配口换成执行者（旧 worker 醒来后不得换回管理员身份）。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// taskExecutorPrompt 拼在执行者规格正文之后（doc/70 §7.1）：交付契约写在 system 指令里，
// 与首条输入末尾的执行约定（buildTaskFirstInput）互为冗余，一条丢了另一条还在。
const taskExecutorPrompt = `你是一场持久任务的执行者：你的首条输入里有目标、上游结果与执行约定。
完成约定的工作、把产物写进项目根之后，调用 submit_task_result 提交交接摘要，然后结束本轮。
不要把工具回执当作已经成功交付：结果由程序对账裁决，成功与否以任务状态为准。`

// agentTaskNoTools 是执行者基座里不给的工具名（doc/70 §6.3「不获得超出规格的工具」）：
//   - delegate_task / create_orchestration：Task 执行者不得再创建异步编排（§6.3 第 2 条）；
//   - ask_user / enter_plan_mode / submit_plan：等人与计划审批需要本人在场——执行 run 挂在
//     kind=agent_task 的会话上，用户看不见它，问了也没人答。执行契约是「做完就交结果」。
var agentTaskNoTools = map[string]bool{
	"delegate_task": true, "create_orchestration": true,
	"ask_user": true, "enter_plan_mode": true, "submit_plan": true,
}

// agentTaskExecutorBase 组执行者的工具基座：冻结池减去等人与异步创建，加上执行者专属的
// submit_task_result，再并上本场连上的 MCP。记忆/检索与技能工具随后由既有 drop helper 剔除
// （执行者是子代理规格当根，子代理结构性没有这两类，doc/52 §2.8 / doc/65 §5.2）。
func (w *Worker) agentTaskExecutorBase(ctx context.Context, mcpTools []tool.BaseTool) []tool.BaseTool {
	base := make([]tool.BaseTool, 0, len(w.tools)+len(mcpTools)+1)
	for _, t := range w.tools {
		info, err := t.Info(ctx)
		if err != nil {
			base = append(base, t) // 拿不到名的原样保留：与既有 drop helper 同一取舍
			continue
		}
		if agentTaskNoTools[info.Name] {
			continue
		}
		base = append(base, t)
	}
	base = dropMemoryAndSearchTools(ctx, base)
	base = dropSkillTools(ctx, base)
	base = append(base, mcpTools...)
	if st, err := newSubmitTaskResultTool(w.store); err == nil {
		base = append(base, st)
	} else {
		log.Printf("task executor: submit_task_result 装配失败: %v", err)
	}
	return base
}

// agentTaskRow 按 id 读 Task 与它的组（内部路径：领取/恢复前刚做过可见性判定，这里不再 JOIN 权限）。
func (s *Store) agentTaskRow(ctx context.Context, taskID string) (AgentTask, AgentOrchestration, error) {
	var t AgentTask
	if err := s.db.WithContext(ctx).Where("id = ?", taskID).Take(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AgentTask{}, AgentOrchestration{}, errTaskNotFound
		}
		return AgentTask{}, AgentOrchestration{}, err
	}
	var o AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", t.OrchestrationID).Take(&o).Error; err != nil {
		return AgentTask{}, AgentOrchestration{}, err
	}
	return t, o, nil
}

// agentTaskByExecutionSession 执行 session 反查 Task（普通 run resume 撞上执行 session 时用）。
// kind=agent_task 的会话与 Task 行同事务出生，理论上恒有；查不到 = 数据被动过，按没有处理。
func (s *Store) agentTaskByExecutionSession(ctx context.Context, sessID SessionID) (AgentTask, bool, error) {
	var t AgentTask
	err := s.db.WithContext(ctx).Where("execution_session_id = ?", sessID).Take(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AgentTask{}, false, nil
	}
	if err != nil {
		return AgentTask{}, false, err
	}
	return t, true, nil
}

// assembleAgentTaskRun 装一场 Task 执行的 profile（doc/70 §6.3）：
//   - 执行者规格按 agent_name 在当前租户重解析（规格可能在创建后被删或改）；
//   - 模型：Task 行写了具体 ref 用 ref；否则继承来源会话 mAgent 的模型（继承档语义 = 父 run 的主模型，
//     对 Task 而言父 = 来源会话），再落到租户默认；
//   - 工具：agentTaskExecutorBase 过一遍规格的四态策略（resolveToolPool），MCP 按租户配置现连；
//   - Instruction = 规格正文 + 租户规范 + 笔记快照 + 交付契约（taskExecutorPrompt）。
//
// 与 assembleRun 的差别：这里不拒 subagent 规格、不附子代理委派（agentSpecHandler 在执行 run
// 里早退）、不装技能目录；规格解析失败 / 模型不可用 / MCP 全挂都算装配失败，由 launchTaskRun
// 落 run error + Task failed/assembly_failed（§6.2「不能留下 running 但永远没有 worker 的行」）。
func (s *Server) assembleAgentTaskRun(ctx context.Context, p principal, taskID string) (*RunProfile, *assembleError) {
	if s.worker == nil || s.worker.models == nil {
		return nil, failAssemble(http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", errors.New("tenant models not configured"))
	}
	t, o, err := s.store.agentTaskRow(ctx, taskID)
	if err != nil {
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	cfg, raw, err := s.store.RuntimeConfigRecord(ctx, p.TenantID)
	if err != nil {
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	secrets, err := unsealSecretMap(cfg.Secrets)
	if err != nil {
		log.Printf("task assemble unseal tenant=%s: %v", p.TenantID, err)
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	// 执行者规格重解析（§6.3 第 1 条）：找不到 = 规格在创建后被删，执行无从谈起。
	var spec AgentSpec
	found := false
	specs, err := s.store.EffectiveSubagentSpecs(ctx, p.TenantID, p.UserID)
	if err != nil {
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	for _, sp := range specs {
		if sp.Name == t.AgentName {
			spec, found = sp, true
			break
		}
	}
	if !found {
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次",
			fmt.Errorf("task %s: 执行者规格 %q 不存在", t.ID, t.AgentName))
	}
	// 模型三落：Task 的显式 ref → 来源会话 mAgent 的 ref → 租户默认。
	ref := strings.TrimSpace(t.ModelRef)
	if ref == "" || isInheritRef(ref) {
		sourceRef := ""
		var sess struct {
			AgentID string `gorm:"column:agent_id"`
		}
		if err := s.store.db.WithContext(ctx).Raw(
			`SELECT COALESCE(agent_id, '') AS agent_id FROM sessions WHERE id = ?`, o.SourceSessionID).Scan(&sess).Error; err != nil {
			return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		}
		if sess.AgentID != "" {
			if row, aerr := s.store.ResolveRunAgent(ctx, p.TenantID, p.UserID, sess.AgentID); aerr == nil {
				sourceRef = row.ModelRef
			}
		}
		ref = effectiveModelRef(sourceRef, cfg.ModelRef, s.worker.models.RootRef())
	}
	built, _, err := s.worker.models.Resolve(ctx, p.TenantID, ref, raw, secrets)
	if err != nil {
		log.Printf("task assemble model tenant=%s ref=%s: %v", p.TenantID, ref, err)
		return nil, failAssemble(http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", err)
	}
	conns, mcpTools, aerr := s.connectRunMCP(ctx, p, cfg, secrets)
	if aerr != nil {
		closeMCP(conns)
		return nil, aerr
	}
	pool, terr := resolveToolPool(ctx, spec, s.worker.agentTaskExecutorBase(ctx, mcpTools))
	if terr != nil {
		closeMCP(conns)
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次",
			fmt.Errorf("task %s: 执行者规格 %q 工具解析失败: %w", t.ID, t.AgentName, terr))
	}
	instruction := spec.Prompt
	if team := teamInstructionsSection(s.store.teamInstructionsFor(ctx)); team != "" {
		instruction = instruction + "\n\n" + team
	}
	// 笔记快照与子代理同政策（读库失败 = 记忆层不可信不静默 → 装配失败）。
	snap, err := renderRunNotes(ctx, s.store, s.worker.workspace)
	if err != nil {
		closeMCP(conns)
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次",
			fmt.Errorf("task %s: 笔记快照读库失败: %w", t.ID, err))
	}
	if snap != "" {
		instruction = instruction + "\n\n" + snap
	}
	instruction = instruction + "\n\n" + taskExecutorPrompt
	log.Printf("task executor %s: agent=%s ref=%s tools=%d", t.ID, t.AgentName, built.Ref, len(pool))
	_, toolNames := poolIndex(ctx, pool)
	log.Printf("task executor tools %s: %s", t.AgentName, strings.Join(toolNames, ", "))
	return &RunProfile{
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		ModelRef:    built.Ref,
		Model:       built.Chat,
		Translator:  TranslatorFor(built.Protocol),
		Instruction: instruction,
		Tools:       pool,
		ConfigVer:   configVersion(raw),
		KeyFP:       built.KeyFP,
		Provider:    built.Provider,
		Vision:      built.Vision,
		Resolver: tenantResolver{
			models: s.worker.models, tenantID: p.TenantID, raw: raw, secrets: secrets,
		},
		conns: conns,
	}, nil
}

// FailClaimedTaskRun 把装配失败的执行 run 收尾（§6.2）：run 翻 error（与死行清扫同一形状：
// finished_at 定格、租约清空），Task 落 failed/assembly_failed 并写 task_failed 通知。
// 全程条件更新：run 已被停止 / Task 已被对账收走时各步自然空转，不复活任何终态。
func (s *Store) FailClaimedTaskRun(ctx context.Context, runID RunID, message string) error {
	var taskID, tenantID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(
			`UPDATE runs SET status = 'error', finished_at = now(), lease_until = NULL
			 WHERE id = ? AND status = 'running'`, runID).Error; err != nil {
			return err
		}
		var row struct {
			TaskID   string `gorm:"column:task_id"`
			TenantID string `gorm:"column:tenant_id"`
			OrchID   string `gorm:"column:orchestration_id"`
			Receiver string `gorm:"column:source_session_id"`
			Epoch    int64  `gorm:"column:lease_epoch"`
		}
		if err := tx.Raw(`
SELECT t.id AS task_id, t.tenant_id, t.orchestration_id, o.source_session_id, r.lease_epoch
  FROM agent_tasks t
  JOIN agent_orchestrations o ON o.id = t.orchestration_id
  JOIN runs r ON r.id = t.current_run_id
 WHERE t.current_run_id = ?`, runID).Scan(&row).Error; err != nil {
			return err
		}
		if row.TaskID == "" {
			return nil // 不是 Task 的 run：与 ReconcileRun 同一条边界
		}
		r := tx.Model(&AgentTask{}).
			Where("id = ? AND status = ?", row.TaskID, agentTaskStatusRunning).
			Updates(map[string]any{
				"status": agentTaskStatusFailed, "error_code": taskErrAssemblyFailed,
				"error_message": message, "finished_at": nowUTC(),
			})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil // Task 已被对账 / 停止收走：幂等
		}
		taskID, tenantID = row.TaskID, row.TenantID
		return s.insertNotificationTx(tx, AgentNotification{
			TenantID: row.TenantID, OrchestrationID: row.OrchID, TaskID: row.TaskID,
			ReceiverSessionID: SessionID(row.Receiver), Kind: notifKindTaskFailed,
			DedupKey:  fmt.Sprintf("%s:%s:%s:%d", notifKindTaskFailed, row.TaskID, runID, row.Epoch),
			ErrorCode: taskErrAssemblyFailed, Status: notifStatusPending,
		})
	})
	if err != nil {
		return err
	}
	if taskID != "" {
		s.PublishEvent(ctx, tenantID, eventAgentTasksChanged, taskID)
	}
	return nil
}

// launchTaskRun 启动一场刚领取的执行（§6.2 第 5 步）：装配执行者 → 失败收尾（FailClaimedTaskRun
// + terminal 帧给轮询的客户端）→ 成功则 go Worker.Run。装配口为 nil（单测）时只记日志：
// run 行留给显式续跑或死行清扫，这是测试模式的已知边界。
func (ts *TaskScheduler) launchTaskRun(ctx context.Context, c agentTaskClaim) {
	if ts.assembleTask == nil {
		log.Printf("task runner %s: 没有装配口（测试模式），run %s 留待显式接管", c.TaskID, c.RunID)
		return
	}
	bg := withAgentTaskRunBox(launchContext(ctx),
		&agentTaskRunBox{TaskID: c.TaskID, OrchestrationID: c.OrchestrationID, SourceRunID: c.SourceRunID})
	p := principal{UserID: c.UserID, TenantID: c.TenantID, Role: "member"}
	bg = withPrincipal(bg, p)
	prof, aerr := ts.assembleTask(bg, p, c.TaskID)
	if aerr != nil {
		log.Printf("task runner %s: 装配失败 (%s): %v", c.TaskID, aerr.Message, aerr.Err)
		finCtx := context.WithoutCancel(bg)
		if err := ts.store.FailClaimedTaskRun(finCtx, c.RunID, aerr.Message); err != nil {
			log.Printf("task runner %s: 装配失败收尾也失败: %v", c.TaskID, err)
			return
		}
		if tj, jerr := json.Marshal(Frame{Kind: "terminal", Status: "error"}); jerr == nil {
			if _, ferr := ts.store.runliveFinish(finCtx, c.RunID, string(tj)); ferr != nil {
				log.Printf("task runner %s: terminal 帧写入失败（runlive 走 TTL 收敛）: %v", c.RunID, ferr)
			}
		}
		return
	}
	log.Printf("task runner: launching task %s (key %s) as run %s", c.TaskID, c.TaskKey, c.RunID)
	go ts.worker.Run(withRunProfile(bg, prof), c.SessionID, c.RunID, c.Epoch)
}

// resumeAgentTask 是 POST /api/tasks/:id/resume（doc/70 §10.1；施工包 §10.1）：
//
//	202 {"code":0,"message":"","data":{"taskId":"at_…","runId":"r_…","status":"running","resumeCount":1}}
//	404 117001；409 117103 现在不能恢复（已终态 / 有活执行者 / 次数耗尽同归此码）
//
// 复用 ClaimResume（接管 CAS：epoch+1 + 续租约）→ 账本重放 → Redis seed 的现有链条；
// 装配口换成执行者。任何 claim 之后的失败都 AbandonClaim：租约改到过期，下一发立刻可接。
func (s *Server) resumeAgentTask(c *gin.Context) {
	ctx := c.Request.Context()
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	view, err := s.store.GetAgentTask(ctx, p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	if view.CurrentRunID == "" {
		writeEnvelope(c, http.StatusConflict, codeTaskNotResumable, "这件任务还没有执行记录，不能恢复", nil)
		return
	}
	runID := RunID(view.CurrentRunID)
	claim, err := s.store.ClaimResume(ctx, runID, s.resumeMax)
	switch {
	case errors.Is(err, errRunNotFound), errors.Is(err, errNotResumable), errors.Is(err, errResumeExhausted):
		writeEnvelope(c, http.StatusConflict, codeTaskNotResumable, "这件任务现在不能恢复", nil)
		return
	case errors.Is(err, errRunAlive):
		writeEnvelope(c, http.StatusConflict, codeRunAlive, "这件任务的执行还活着，别抢", nil)
		return
	case err != nil:
		internal(c, err)
		return
	}
	// 挂起中的问/方案行接回现场（doc/47 §3.3）：读失败同样放弃认领。
	questions, err := s.store.ListQuestions(ctx, runID, "")
	if err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	replay, err := replayJournal(runID, claim.Rows, questions)
	if err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	frameJSON, err := json.Marshal(Frame{Kind: "resume", Text: replay.Seed})
	if err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	if err := s.store.seedResume(ctx, runID, claim.Epoch, string(frameJSON)); err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	prof, aerr := s.assembleAgentTaskRun(ctx, p, view.TaskID)
	if aerr != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		writeEnvelope(c, aerr.Status, aerr.Code, aerr.Message, nil)
		return
	}
	bg := withAgentTaskRunBox(launchContext(c.Request.Context()),
		&agentTaskRunBox{TaskID: view.TaskID, OrchestrationID: view.OrchestrationID})
	bg = withPrincipal(bg, p)
	go s.worker.RunResume(withRunProfile(bg, prof), claim.SessionID, runID, claim.Epoch, replay)
	writeEnvelope(c, http.StatusAccepted, codeOK, "", resumeTaskResponse{
		TaskID: view.TaskID, RunID: runID, Status: "running", ResumeCount: int(claim.Epoch - 1),
	})
}

// resumeTaskResponse 是 task resume 的响应体（doc/70 §4：taskId → runId → status → resumeCount）。
type resumeTaskResponse struct {
	TaskID      string `json:"taskId"`
	RunID       RunID  `json:"runId"`
	Status      string `json:"status"`
	ResumeCount int    `json:"resumeCount"`
}
