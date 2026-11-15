// 本文件是继任编排组的创建、激活与崩溃恢复（方案真源 = doc/75 §4.2/§5/§7；施工包 §7）。
//
// 一个原组最多一个继任组：唯一性由两处共同表达——原组容器里的 successor.json，与 PG 里
// 规范化的创建幂等键 successor:<原组ID>:<调用方键>（施工包 §4.2/§7.2）。后者是权威：
// 标记只在 PG 插入之后写，写丢了由对账按幂等键前缀补。
//
// 状态机（doc/75 §4.2）：
//
//	preparing       记录已存在，正在写 handoff／快照／标记或停止原组；不可调度、不占模型容量
//	waiting_previous handoff 完整，原组尚未终态
//	active          原组终态后才允许；激活那一刻把无父的 pending 提成 ready
//
// 文件顺序（施工包 §7.2）：handoff 与复用快照先落盘（只有不可执行文件），再插 PG 行——
// handoff 的正文来自用户／模型输入，不落 PG，先写文件才能在崩溃后由对账补齐。
// PG 已插入但后继步骤失败 = preparing 组原地等对账，不回滚已提交的创建。
package runengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// successorOutcomeFeasible 是唯一允许创建继任组的目标可行性判定（doc/75 §4.1）。
const successorOutcomeFeasible = "feasible"

// errSuccessorRefused：原组已有继任者、或状态不允许再重规划。HTTP 409 117114。
var errSuccessorRefused = errors.New("successor creation not allowed")

// successorError 是继任方案校验失败：API 层 400/117115，原因进 data.reason。
type successorError struct{ reason string }

func (e *successorError) Error() string { return e.reason }

// GoalAssessment 是目标可行性判定（doc/75 §4.1）：只有 feasible 能创建继任组。
type GoalAssessment struct {
	Outcome  string `json:"outcome"`
	Evidence string `json:"evidence"`
}

// SuccessorReuseRef 是一条复用请求：原组（或它的重规划链）里已成功 Task 的不可变结果版本。
type SuccessorReuseRef struct {
	TaskID         string
	ResultRevision int
}

// SuccessorCommand 是创建继任组的入参（HTTP 与模型工具共用）。
type SuccessorCommand struct {
	IdempotencyKey  string
	GoalAssessment  GoalAssessment
	Reason          string
	SuccessorReason string
	NoReplay        []string
	Reuse           []SuccessorReuseRef
	// WorkspaceID 省略 = 沿用原组项目；换项目时按本人所有权校验。
	WorkspaceID string
	Goal        string
	// WorkspaceMode / VerifyCommand 同时省略 = 继承原组；否则按既有规则规范化后作为本次变更
	// 写入 handoff（施工包 §4.1）。
	WorkspaceMode string
	VerifyCommand string
	SourceRunID   string
	Tasks         []planTask
	Links         []planLink
}

// successorPlan 是规范化的继任请求，也是 plan_hash 的哈希输入：数组已排序，
// 继承后的工作区配置与目标项目都进哈希（幂等重试必须字节稳定）。
type successorPlan struct {
	ReplanOf        string              `json:"replanOf"`
	GoalAssessment  GoalAssessment      `json:"goalAssessment"`
	Reason          string              `json:"reason"`
	SuccessorReason string              `json:"successorReason"`
	NoReplay        []string            `json:"noReplay"`
	Reuse           []handoffReuseEntry `json:"reuse"`
	WorkspaceID     string              `json:"workspaceId"`
	Goal            string              `json:"goal"`
	Tasks           []planTask          `json:"tasks"`
	Links           []planLink          `json:"links"`
	WorkspaceMode   string              `json:"workspaceMode,omitempty"`
	VerifyCommand   string              `json:"verifyCommand,omitempty"`
}

func (p *successorPlan) hash() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}

// successorResp 是 POST /api/orchestrations/:id/successors 的 data（施工包 §4.1 字段序）。
type successorResp struct {
	OrchestrationID string `json:"orchestrationId"`
	Status          string `json:"status"`
	ReplanOf        string `json:"replanOf"`
	ReplanDepth     int    `json:"replanDepth"`
}

// normalizeSuccessorCommand 做不碰数据库的纯校验并产出规范字段。
func normalizeSuccessorCommand(cmd SuccessorCommand) (GoalAssessment, string, string, []string, []handoffReuseEntry, error) {
	outcome := strings.TrimSpace(cmd.GoalAssessment.Outcome)
	if outcome != successorOutcomeFeasible {
		return GoalAssessment{}, "", "", nil, nil, &successorError{
			fmt.Sprintf("目标可行性判定必须是 feasible 才能创建继任组（当前 %q）", outcome)}
	}
	evidence := strings.TrimSpace(cmd.GoalAssessment.Evidence)
	if evidence == "" {
		return GoalAssessment{}, "", "", nil, nil, &successorError{"目标可行性证据必填"}
	}
	if utf8Len(evidence) > successorTextMaxRunes {
		return GoalAssessment{}, "", "", nil, nil, &successorError{
			fmt.Sprintf("目标可行性证据超过 %d 字", successorTextMaxRunes)}
	}
	reason := strings.TrimSpace(cmd.Reason)
	if reason == "" {
		return GoalAssessment{}, "", "", nil, nil, &successorError{"重规划原因必填"}
	}
	if utf8Len(reason) > successorTextMaxRunes {
		return GoalAssessment{}, "", "", nil, nil, &successorError{
			fmt.Sprintf("重规划原因超过 %d 字", successorTextMaxRunes)}
	}
	successorReason := strings.TrimSpace(cmd.SuccessorReason)
	if successorReason == "" {
		return GoalAssessment{}, "", "", nil, nil, &successorError{"创建继任组的原因必填"}
	}
	if utf8Len(successorReason) > successorTextMaxRunes {
		return GoalAssessment{}, "", "", nil, nil, &successorError{
			fmt.Sprintf("创建继任组的原因超过 %d 字", successorTextMaxRunes)}
	}
	if len(cmd.NoReplay) > successorNoReplayMax {
		return GoalAssessment{}, "", "", nil, nil, &successorError{
			fmt.Sprintf("不得重放的动作最多 %d 条", successorNoReplayMax)}
	}
	seenNoReplay := make(map[string]bool, len(cmd.NoReplay))
	noReplay := make([]string, 0, len(cmd.NoReplay))
	for _, s := range cmd.NoReplay {
		s = strings.TrimSpace(s)
		if s == "" {
			return GoalAssessment{}, "", "", nil, nil, &successorError{"不得重放的动作不能为空行"}
		}
		if utf8Len(s) > successorNoReplayMaxRunes {
			return GoalAssessment{}, "", "", nil, nil, &successorError{
				fmt.Sprintf("不得重放的动作超过 %d 字：%s", successorNoReplayMaxRunes, truncatePlain(s, 40))}
		}
		if seenNoReplay[s] {
			continue
		}
		seenNoReplay[s] = true
		noReplay = append(noReplay, s)
	}
	sort.Strings(noReplay)
	if len(cmd.Reuse) > successorReuseMax {
		return GoalAssessment{}, "", "", nil, nil, &successorError{
			fmt.Sprintf("复用结果最多 %d 条", successorReuseMax)}
	}
	seenReuse := make(map[handoffReuseEntry]bool, len(cmd.Reuse))
	reuse := make([]handoffReuseEntry, 0, len(cmd.Reuse))
	for _, r := range cmd.Reuse {
		taskID := strings.TrimSpace(r.TaskID)
		if taskID == "" || r.ResultRevision < 1 {
			return GoalAssessment{}, "", "", nil, nil, &successorError{
				"复用条目必须同时给出 taskId 与不小于 1 的 resultRevision"}
		}
		entry := handoffReuseEntry{TaskID: taskID, ResultRevision: r.ResultRevision}
		if seenReuse[entry] {
			continue
		}
		seenReuse[entry] = true
		reuse = append(reuse, entry)
	}
	sort.Slice(reuse, func(i, j int) bool {
		if reuse[i].TaskID != reuse[j].TaskID {
			return reuse[i].TaskID < reuse[j].TaskID
		}
		return reuse[i].ResultRevision < reuse[j].ResultRevision
	})
	return GoalAssessment{Outcome: successorOutcomeFeasible, Evidence: evidence},
		reason, successorReason, noReplay, reuse, nil
}

// successorFromPlanErr 把计划／工作区配置校验错误翻成 117115（原因原样带出）。
// 继任方案的长度、DAG、预算问题都属于「继任方案不合法」，与普通创建的 117101 分开。
func successorFromPlanErr(err error) error {
	var pe *planError
	if errors.As(err, &pe) {
		return &successorError{pe.reason}
	}
	var wce *workspaceConfigError
	if errors.As(err, &wce) {
		return &successorError{wce.reason}
	}
	return err
}

// successorWorkspaceConfig 解析工作区配置：两项都省略 = 继承原组；否则按既有规则规范化。
func successorWorkspaceConfig(orig AgentOrchestration, cmd SuccessorCommand) (string, string, error) {
	if strings.TrimSpace(cmd.WorkspaceMode) == "" && strings.TrimSpace(cmd.VerifyCommand) == "" {
		return orchestrationStorageMode(orig), orig.VerifyCommand, nil
	}
	mode, verify, err := normalizeWorkspaceConfig(cmd.WorkspaceMode, cmd.VerifyCommand)
	if err != nil {
		return "", "", successorFromPlanErr(err)
	}
	return mode, verify, nil
}

// orchestrationStorageMode 把组行上的模式读成词表值（空串按 isolated，与 newGroupHandle 一致）。
func orchestrationStorageMode(o AgentOrchestration) string {
	if o.WorkspaceMode == "" {
		return workspaceModeIsolated
	}
	return o.WorkspaceMode
}

// orchestrationTerminal：succeeded／failed／cancelled 是组终态。
func orchestrationTerminal(status string) bool {
	switch status {
	case orchStatusSucceeded, orchStatusFailed, orchStatusCancelled:
		return true
	}
	return false
}

// groupContainerDirFor 按 tenant/workspace/orch ID 算组容器目录（新组尚未入 PG 时用）。
func (s *Store) groupContainerDirFor(tenantID, workspaceID, orchID string) (string, error) {
	container, err := projectContainerDir(s.workspaceRoot, tenantID, workspaceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(container, projectRuntimeDirName, "orchestrations", orchID), nil
}

// visibleOrchestration 按属主链读原组行（非本人与不存在同形）。
func (s *Store) visibleOrchestration(ctx context.Context, p principal, id string) (AgentOrchestration, error) {
	var rows []AgentOrchestration
	if err := s.db.WithContext(ctx).Raw(`
SELECT o.* FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?`, id, p.TenantID, p.UserID).
		Scan(&rows).Error; err != nil {
		return AgentOrchestration{}, err
	}
	if len(rows) == 0 {
		return AgentOrchestration{}, errOrchNotFound
	}
	return rows[0], nil
}

// successorByKey 按 (tenant, 来源会话, 规范化键) 读已存在的继任组。
func successorByKey(tx *gorm.DB, tenantID string, sessionID SessionID, key string) (*AgentOrchestration, error) {
	var rows []AgentOrchestration
	if err := tx.Where("tenant_id = ? AND source_session_id = ? AND idempotency_key = ?",
		tenantID, sessionID, key).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// successorRowsByPrefix 列一个原组已产生的全部继任组（幂等键前缀）。
func successorRowsByPrefix(tx *gorm.DB, tenantID, origID string) ([]AgentOrchestration, error) {
	var rows []AgentOrchestration
	err := tx.Where("tenant_id = ? AND idempotency_key LIKE ?", tenantID,
		successorKeyPrefix+origID+":%").Order("created_at, id").Find(&rows).Error
	return rows, err
}

// resolvedReuse 是一条复用请求解析后的实体：来源组、组句柄、Task 与不可变结果。
type resolvedReuse struct {
	group  AgentOrchestration
	handle *GroupHandle
	task   AgentTask
	imm    ImmutableResult
}

// resolveSuccessorReuse 校验复用条目并解析来源（施工包 §4.1/§5.3）：
// 必须属于原组或它的重规划链、属于已成功的 Task、版本就是请求的版本、结果与声明产物可读。
// 跨组、跨项目的条目都允许（链上任意一代），但同一租户；可见性已由原组的属主链保证。
func (s *Store) resolveSuccessorReuse(ctx context.Context, tenantID string, orig AgentOrchestration,
	refs []handoffReuseEntry) ([]resolvedReuse, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	chain, err := s.replanChain(ctx, orig)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.TaskID)
	}
	var tasks []AgentTask
	if err := s.db.WithContext(ctx).
		Where("id IN ? AND tenant_id = ?", ids, tenantID).Find(&tasks).Error; err != nil {
		return nil, err
	}
	groupOf := make(map[string]AgentOrchestration, len(chain))
	for _, g := range chain {
		groupOf[g.ID] = g
	}
	taskByID := make(map[string]AgentTask, len(tasks))
	for _, t := range tasks {
		if _, ok := groupOf[t.OrchestrationID]; ok {
			taskByID[t.ID] = t
		}
	}
	out := make([]resolvedReuse, 0, len(refs))
	for _, ref := range refs {
		t, ok := taskByID[ref.TaskID]
		if !ok {
			return nil, &successorError{fmt.Sprintf(
				"复用条目 %s 不属于原组或它的重规划链", ref.TaskID)}
		}
		if t.Status != agentTaskStatusSucceeded || t.ResultRevision < 1 {
			return nil, &successorError{fmt.Sprintf(
				"复用条目 %s 的 Task 还没有成功的结果（当前 %s）", ref.TaskID, t.Status)}
		}
		if t.ResultRevision != ref.ResultRevision {
			return nil, &successorError{fmt.Sprintf(
				"复用条目 %s 请求的是第 %d 版结果，当前是第 %d 版", ref.TaskID, ref.ResultRevision, t.ResultRevision)}
		}
		g := groupOf[t.OrchestrationID]
		h, err := s.workspaceGroupHandle(g)
		if err != nil {
			return nil, err
		}
		imm, err := readImmutableResult(h, ResultRef{TaskID: ref.TaskID, ResultRevision: ref.ResultRevision})
		if err != nil {
			return nil, &successorError{fmt.Sprintf(
				"复用结果 %s/%d 不可读：%v", ref.TaskID, ref.ResultRevision, err)}
		}
		for _, a := range imm.Artifacts {
			if _, err := readResultArtifact(h, ResultRef{TaskID: ref.TaskID, ResultRevision: ref.ResultRevision}, a.Path); err != nil {
				return nil, &successorError{fmt.Sprintf(
					"复用结果 %s/%d 的产物 %s 不可读：%v", ref.TaskID, ref.ResultRevision, a.Path, err)}
			}
		}
		out = append(out, resolvedReuse{group: g, handle: h, task: t, imm: imm})
	}
	return out, nil
}

// materializeSuccessorReuse 把旧组结果逐字复制进继任组自己的结果区（施工包 §3 裁定 3）：
// 旧项目随后被删除不影响继任组。目标已存在且可读 = 上次已复制（幂等），跳过；存在但损坏 = 拒绝。
func (s *Store) materializeSuccessorReuse(ctx context.Context, tenantID string, resolved []resolvedReuse, newHandle *GroupHandle) error {
	if len(resolved) == 0 {
		return nil
	}
	var adding int64
	missing := make([]resolvedReuse, 0, len(resolved))
	for _, r := range resolved {
		ref := ResultRef{TaskID: r.task.ID, ResultRevision: r.task.ResultRevision}
		dst := newHandle.resultDir(ref.TaskID, ref.ResultRevision)
		if _, err := os.Stat(dst); err == nil {
			if _, verr := readImmutableResult(newHandle, ref); verr != nil {
				return fmt.Errorf("复用结果 %s/%d 的目标副本已存在但不可读：%w",
					ref.TaskID, ref.ResultRevision, verr)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		adding += directoryBytes(r.handle.resultDir(ref.TaskID, ref.ResultRevision))
		missing = append(missing, r)
	}
	if len(missing) == 0 {
		return nil
	}
	release, err := s.workspaceReserve()(ctx, tenantID, adding)
	if err != nil {
		return err
	}
	defer release()
	for _, r := range missing {
		ref := ResultRef{TaskID: r.task.ID, ResultRevision: r.task.ResultRevision}
		if err := copyImmutableResult(r.handle, newHandle, ref); err != nil {
			return err
		}
	}
	return nil
}

// reuseEntriesFrom 把解析结果落成 reuse.json 的形状（引擎与 Provider 之间只传 taskId+revision，
// 来源组 ID 与 Provider 引用只留在组目录，供崩溃恢复与对账）。
func reuseEntriesFrom(resolved []resolvedReuse) []groupReuseEntry {
	out := make([]groupReuseEntry, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, groupReuseEntry{
			SourceOrchestrationID: r.group.ID,
			TaskID:                r.task.ID,
			ResultRevision:        r.task.ResultRevision,
			Provider:              r.imm.Provider,
			Reference:             r.imm.Reference,
			Artifacts:             r.imm.Artifacts,
		})
	}
	return out
}

// successorConfigNote 渲染程序记录的工作区配置变更（继承时空串）。
func successorConfigNote(orig AgentOrchestration, plan *orchPlan, targetWS string) string {
	var parts []string
	if targetWS != orig.WorkspaceID {
		parts = append(parts, "目标项目改为 "+targetWS)
	}
	if plan.storageMode() != orchestrationStorageMode(orig) {
		parts = append(parts, "工作区模式改为 "+plan.storageMode())
	}
	if plan.VerifyCommand != orig.VerifyCommand {
		if plan.VerifyCommand == "" {
			parts = append(parts, "验证命令被移除")
		} else {
			parts = append(parts, "验证命令改为 "+plan.VerifyCommand)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "程序记录的工作区配置变更（其余配置沿用原组）：\n- " + strings.Join(parts, "\n- ")
}

// successorWriteFiles 落 handoff、reuse.json 与复用副本（PG 插入之前；只有不可执行文件）。
func (s *Store) successorWriteFiles(ctx context.Context, p principal, orig AgentOrchestration, succ AgentOrchestration,
	depth int, assessment GoalAssessment, reason, successorReason, configNote string, noReplay []string,
	reuseRefs []handoffReuseEntry, resolved []resolvedReuse) error {
	container, err := s.groupContainerDir(succ)
	if err != nil {
		return err
	}
	entries := reuseEntriesFrom(resolved)
	content, err := renderHandoff(handoffDoc{
		Kind: handoffKind, OrchestrationID: succ.ID, ReplanOf: orig.ID,
		ReplanDepth: depth, Reuse: reuseRefs,
	}, reason, successorReason, assessment.Evidence, configNote, noReplay, reuseArtifactsByTask(entries))
	if err != nil {
		return err
	}
	if err := writeHandoff(container, content); err != nil {
		return err
	}
	if err := writeGroupReuse(container, groupReuseFile{
		OrchestrationID: succ.ID, ReplanOf: orig.ID, Reuse: entries,
	}); err != nil {
		return err
	}
	newHandle, err := s.workspaceGroupHandle(succ)
	if err != nil {
		return err
	}
	return s.materializeSuccessorReuse(ctx, p.TenantID, resolved, newHandle)
}

// ensureSuccessorMark 在原组容器里写 successor.json（组锁内，幂等）。标记不是唯一性权威，
// 写失败只记日志：PG 的幂等键前缀已经挡下第二个继任组，对账会补标记。
func (s *Store) ensureSuccessorMark(ctx context.Context, orig AgentOrchestration, succID, creationKey string) error {
	container, err := s.groupContainerDir(orig)
	if err != nil {
		return err
	}
	mark, err := readSuccessorMark(container)
	if err != nil {
		return err
	}
	if mark != nil {
		if mark.SuccessorOrchestrationID != succID {
			return errSuccessorRefused
		}
		return nil
	}
	h, err := s.workspaceGroupHandle(orig)
	if err != nil {
		return err
	}
	rel, err := h.Locker.Lock(ctx, []string{h.groupLockKey()})
	if err != nil {
		return err
	}
	defer rel()
	if mark, err = readSuccessorMark(container); err != nil {
		return err
	}
	if mark != nil {
		if mark.SuccessorOrchestrationID != succID {
			return errSuccessorRefused
		}
		return nil
	}
	return writeSuccessorMark(container, successorMark{
		OrchestrationID: orig.ID, SuccessorOrchestrationID: succID,
		CreationKey: creationKey, CreatedAt: nowUTC(),
	})
}

// finalizeSuccessor 把继任组从 preparing 推进到最终状态（施工包 §7.2 的后三步）：
// 原组未终态先走既有停止链，再按停止后的真实状态决定 waiting_previous 还是 active。
// 激活与「提升无父 pending」在同一事务，且只有真的翻转状态才发事件。
func (s *Store) finalizeSuccessor(ctx context.Context, succID, origID string) (string, []RunID, error) {
	var orig AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", origID).Take(&orig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil, fmt.Errorf("继任原组 %s 不存在", origID)
		}
		return "", nil, err
	}
	var succ AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", succID).Take(&succ).Error; err != nil {
		return "", nil, err
	}
	if succ.Status != orchStatusPreparing && succ.Status != orchStatusWaitingPrevious {
		return succ.Status, nil, nil // 已被停止或已激活
	}
	var runIDs []RunID
	if !orchestrationTerminal(orig.Status) && succ.Status == orchStatusPreparing {
		stopped, runs, _, err := s.stopOrchestrationCore(ctx, origID)
		if err != nil {
			return succ.Status, nil, err
		}
		_ = stopped
		runIDs = runs
		if err := s.db.WithContext(ctx).Where("id = ?", origID).Take(&orig).Error; err != nil {
			return succ.Status, runIDs, err
		}
	}
	next := orchStatusWaitingPrevious
	if orchestrationTerminal(orig.Status) {
		next = orchStatusActive
	}
	var tenantID string
	var promoted []string
	changed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", succID).Take(&row).Error; err != nil {
			return err
		}
		tenantID = row.TenantID
		if row.Status != orchStatusPreparing && row.Status != orchStatusWaitingPrevious {
			return nil
		}
		if row.Status == next {
			return nil
		}
		// 激活前在事务内重读原组状态：原组可能刚被停止收口，不能只看事务外的快照。
		if next == orchStatusActive {
			var st string
			if err := tx.Raw(`SELECT status FROM agent_orchestrations WHERE id = ?`, origID).Scan(&st).Error; err != nil {
				return err
			}
			if !orchestrationTerminal(st) {
				next = orchStatusWaitingPrevious
			}
		}
		if row.Status == next {
			return nil
		}
		res := tx.Model(&AgentOrchestration{}).
			Where("id = ? AND status = ?", succID, row.Status).Update("status", next)
		if res.Error != nil {
			return res.Error
		}
		changed = res.RowsAffected > 0
		if !changed || next != orchStatusActive {
			return nil
		}
		// 激活那一刻才把无父的 pending 提成 ready（继任组出生时全部是 pending）。
		var rows []struct {
			ID string `gorm:"column:id"`
		}
		if err := tx.Raw(`
UPDATE agent_tasks t SET status = ?, updated_at = now()
 WHERE t.orchestration_id = ? AND t.status = ?
   AND NOT EXISTS (SELECT 1 FROM agent_task_links l WHERE l.child_task_id = t.id)
RETURNING t.id`, agentTaskStatusReady, succID, agentTaskStatusPending).Scan(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			promoted = append(promoted, r.ID)
		}
		return nil
	})
	if err != nil {
		return succ.Status, runIDs, err
	}
	if changed {
		s.PublishEvent(ctx, tenantID, eventAgentOrchestrationsChanged, succID)
		for _, id := range promoted {
			s.PublishEvent(ctx, tenantID, eventAgentTasksChanged, id)
		}
		if next == orchStatusActive {
			s.PublishTaskWake(ctx, tenantID, "successor_active", succID)
		}
	}
	return next, runIDs, nil
}

// successorView 把继任组行翻成响应；重规划关系从 handoff 现读（PG 不存这两列）。
func (s *Store) successorView(o AgentOrchestration) successorResp {
	replanOf, depth, err := s.handoffReplanOf(o)
	if err != nil {
		log.Printf("successor view %s: 读 handoff 失败: %v", o.ID, err)
	}
	return successorResp{OrchestrationID: o.ID, Status: o.Status, ReplanOf: replanOf, ReplanDepth: depth}
}

// CreateSuccessorOrchestration 创建一个继任编排组（施工包 §7.2 的顺序）。
func (s *Store) CreateSuccessorOrchestration(ctx context.Context, p principal, originalID string,
	cmd SuccessorCommand, env planEnv) (successorResp, error) {
	assessment, reason, successorReason, noReplay, reuseRefs, err := normalizeSuccessorCommand(cmd)
	if err != nil {
		return successorResp{}, err
	}
	orig, err := s.visibleOrchestration(ctx, p, originalID)
	if err != nil {
		return successorResp{}, err
	}
	switch orig.Status {
	case orchStatusPreparing, orchStatusWaitingPrevious:
		return successorResp{}, errSuccessorRefused
	}

	targetWS := strings.TrimSpace(cmd.WorkspaceID)
	if targetWS == "" {
		targetWS = orig.WorkspaceID
	}
	if _, err := s.VisibleWorkspace(ctx, targetWS, p.TenantID, p.UserID); err != nil {
		return successorResp{}, errCreateWorkspace
	}
	mode, verify, err := successorWorkspaceConfig(orig, cmd)
	if err != nil {
		return successorResp{}, err
	}
	plan, err := normalizeOrchestrationPlan(CreateOrchestrationCommand{
		IdempotencyKey: cmd.IdempotencyKey, WorkspaceID: targetWS,
		SourceSessionID: orig.SourceSessionID, SourceRunID: cmd.SourceRunID,
		Goal: cmd.Goal, Tasks: cmd.Tasks, Links: cmd.Links,
		WorkspaceMode: mode, VerifyCommand: verify,
	})
	if err != nil {
		return successorResp{}, successorFromPlanErr(err)
	}
	if err := s.validatePlanAgents(ctx, p, plan); err != nil {
		return successorResp{}, err
	}
	for _, t := range plan.Tasks {
		ref := strings.TrimSpace(t.ModelRef)
		if ref == "" || isInheritRef(ref) || env.resolveModel == nil {
			continue
		}
		if err := env.resolveModel(ctx, ref); err != nil {
			return successorResp{}, fmt.Errorf("%w: %s: %v", errCreateModel, ref, err)
		}
	}

	_, origDepth, err := s.handoffReplanOf(orig)
	if err != nil {
		return successorResp{}, err
	}
	depth := origDepth + 1
	if depth > successorMaxDepth {
		return successorResp{}, &successorError{fmt.Sprintf(
			"重规划链已有 %d 层，超过上限 %d；继续重规划必须先问用户", origDepth, successorMaxDepth)}
	}

	normalizedKey := successorKeyPrefix + orig.ID + ":" + strings.TrimSpace(cmd.IdempotencyKey)
	if len(normalizedKey) > idempotencyMaxBytes {
		return successorResp{}, &successorError{fmt.Sprintf(
			"idempotencyKey 与规范化前缀合计超过 %d 字节", idempotencyMaxBytes)}
	}

	// 原项目与目标项目都必须已完成目录迁移；未迁移回 117116，不提供旁路。
	if _, _, err := ensureProjectLayout(s.workspaceRoot, p.TenantID, orig.WorkspaceID); err != nil {
		return successorResp{}, err
	}
	if _, _, err := ensureProjectLayout(s.workspaceRoot, p.TenantID, targetWS); err != nil {
		return successorResp{}, err
	}

	planHash := &successorPlan{
		ReplanOf: orig.ID, GoalAssessment: assessment,
		Reason: reason, SuccessorReason: successorReason,
		NoReplay: noReplay, Reuse: reuseRefs, WorkspaceID: targetWS,
		Goal: plan.Goal, Tasks: plan.Tasks, Links: plan.Links,
	}
	if plan.WorkspaceMode == workspaceModeIsolated {
		planHash.WorkspaceMode = plan.WorkspaceMode
		planHash.VerifyCommand = plan.VerifyCommand
	}
	hash, err := planHash.hash()
	if err != nil {
		return successorResp{}, err
	}

	// 早查幂等与已有的继任者：同键同方案直接回读，不同方案 117102，别的键 117114。
	if existing, err := successorByKey(s.db.WithContext(ctx), p.TenantID, orig.SourceSessionID, normalizedKey); err != nil {
		return successorResp{}, err
	} else if existing != nil {
		if existing.PlanHash != hash {
			return successorResp{}, errPlanConflict
		}
		return s.finishExistingSuccessor(ctx, *existing, orig.ID)
	}
	container, err := s.groupContainerDir(orig)
	if err != nil {
		return successorResp{}, err
	}
	if mark, err := readSuccessorMark(container); err != nil {
		return successorResp{}, err
	} else if mark != nil && mark.CreationKey != normalizedKey {
		return successorResp{}, errSuccessorRefused
	}

	resolved, err := s.resolveSuccessorReuse(ctx, p.TenantID, orig, reuseRefs)
	if err != nil {
		return successorResp{}, err
	}

	succID, err := newRowID("or_")
	if err != nil {
		return successorResp{}, err
	}
	taskIDs := make(map[string]string, len(plan.Tasks))
	rows := make([]AgentTask, 0, len(plan.Tasks))
	for _, t := range plan.Tasks {
		id, err := newRowID("at_")
		if err != nil {
			return successorResp{}, err
		}
		taskIDs[t.Key] = id
		rows = append(rows, AgentTask{
			ID: id, OrchestrationID: succID, TaskKey: t.Key,
			TenantID: p.TenantID, WorkspaceID: targetWS,
			Content: t.Content, AgentName: t.Agent, ModelRef: t.ModelRef,
			// 继任组出生时全部 pending；激活那一刻才按有无父节点提升。
			Status: agentTaskStatusPending, PlanCheckpoint: t.PlanCheckpoint,
		})
	}
	succ := AgentOrchestration{
		ID: succID, TenantID: p.TenantID, WorkspaceID: targetWS,
		SourceSessionID: orig.SourceSessionID, SourceRunID: cmd.SourceRunID,
		Goal: plan.Goal, Status: orchStatusPreparing,
		FinalTaskID:    taskIDs[plan.finalKey()],
		IdempotencyKey: normalizedKey, PlanHash: hash,
		WorkspaceMode: plan.storageMode(), VerifyCommand: plan.VerifyCommand,
	}
	succContainer, err := s.groupContainerDir(succ)
	if err != nil {
		return successorResp{}, err
	}
	cleanup := func() {
		if err := os.RemoveAll(succContainer); err != nil {
			log.Printf("successor cleanup %s: %v", succContainer, err)
		}
	}
	if err := s.successorWriteFiles(ctx, p, orig, succ, depth, assessment,
		reason, successorReason, successorConfigNote(orig, plan, targetWS), noReplay, reuseRefs, resolved); err != nil {
		cleanup()
		return successorResp{}, err
	}

	// PG 插入：原组行 FOR UPDATE 作为「同一原组只产生一个继任组」的串行点。
	// 这里不持组文件锁——发布路径先取 PG advisory 再取组锁，持锁跨 PG 事务会反向等待。
	var existingID string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked AgentOrchestration
		res := tx.Raw(`
SELECT o.* FROM agent_orchestrations o
  WHERE o.id = ? AND o.tenant_id = ?
  FOR UPDATE`, orig.ID, p.TenantID).Scan(&locked)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errOrchNotFound
		}
		if locked.Status == orchStatusPreparing || locked.Status == orchStatusWaitingPrevious {
			return errSuccessorRefused
		}
		prior, err := successorRowsByPrefix(tx, p.TenantID, orig.ID)
		if err != nil {
			return err
		}
		for _, row := range prior {
			if row.IdempotencyKey == normalizedKey {
				if row.PlanHash != hash {
					return errPlanConflict
				}
				existingID = row.ID
				return nil
			}
		}
		if len(prior) > 0 {
			return errSuccessorRefused
		}
		if err := tx.Create(&succ).Error; err != nil {
			if isUniqueViolation(err) {
				row, rerr := successorByKey(tx, p.TenantID, orig.SourceSessionID, normalizedKey)
				if rerr != nil {
					return rerr
				}
				if row == nil {
					return errSuccessorRefused
				}
				if row.PlanHash != hash {
					return errPlanConflict
				}
				existingID = row.ID
				return nil
			}
			return err
		}
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		if len(plan.Links) > 0 {
			links := make([]AgentTaskLink, 0, len(plan.Links))
			for _, l := range plan.Links {
				links = append(links, AgentTaskLink{
					ParentTaskID: taskIDs[l.Parent], ChildTaskID: taskIDs[l.Child],
					TenantID: p.TenantID, OrchestrationID: succID,
				})
			}
			if err := tx.Create(&links).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		cleanup()
		return successorResp{}, err
	}
	if existingID != "" && existingID != succID {
		// 并发同键：另一个请求先落库。丢掉本次预写的不可执行文件，回读胜者。
		cleanup()
		var winner AgentOrchestration
		if err := s.db.WithContext(ctx).Where("id = ?", existingID).Take(&winner).Error; err != nil {
			return successorResp{}, err
		}
		return s.finishExistingSuccessor(ctx, winner, orig.ID)
	}

	if err := s.ensureSuccessorMark(ctx, orig, succID, normalizedKey); err != nil {
		if errors.Is(err, errSuccessorRefused) {
			// 标记指向另一个继任组：PG 行保留（对账会发现两处不一致并人工处理），本次如实报冲突。
			return successorResp{}, err
		}
		log.Printf("successor mark %s: %v", succID, err)
	}

	status, runIDs, ferr := s.finalizeSuccessor(ctx, succID, orig.ID)
	if ferr != nil {
		// 创建已提交：preparing 组等对账推进，不回滚，也不谎报已停止原组。
		log.Printf("successor finalize %s: %v", succID, ferr)
	}
	_ = runIDs
	s.PublishEvent(ctx, p.TenantID, eventAgentOrchestrationsChanged, succID)
	for _, r := range rows {
		s.PublishEvent(ctx, p.TenantID, eventAgentTasksChanged, r.ID)
	}
	s.PublishTaskWake(ctx, p.TenantID, "successor", succID)
	if status == "" {
		status = orchStatusPreparing
	}
	replanOf, replanDepth, _ := s.handoffReplanOf(succ)
	return successorResp{
		OrchestrationID: succID, Status: status,
		ReplanOf: replanOf, ReplanDepth: replanDepth,
	}, nil
}

// finishExistingSuccessor 是幂等重打：不建第二组，把已有组推进到当前可达到的状态。
func (s *Store) finishExistingSuccessor(ctx context.Context, existing AgentOrchestration, origID string) (successorResp, error) {
	status, _, err := s.finalizeSuccessor(ctx, existing.ID, origID)
	if err != nil {
		return successorResp{}, err
	}
	if status != "" {
		existing.Status = status
	}
	replanOf, replanDepth, herr := s.handoffReplanOf(existing)
	if herr != nil {
		log.Printf("successor retry %s: 读 handoff 失败: %v", existing.ID, herr)
	}
	return successorResp{
		OrchestrationID: existing.ID, Status: existing.Status,
		ReplanOf: replanOf, ReplanDepth: replanDepth,
	}, nil
}

// reconcileSuccessors 是调度轮的继任对账：preparing／waiting_previous 逐组补齐标记与快照，
// 再按原组状态推进（施工包 §7.2「PG 已插入但后继步骤失败」的恢复路径）。
func (ts *TaskScheduler) reconcileSuccessors(ctx context.Context) {
	var rows []AgentOrchestration
	if err := ts.store.db.WithContext(ctx).
		Where("status IN ?", []string{orchStatusPreparing, orchStatusWaitingPrevious}).
		Order("created_at, id").Limit(64).Find(&rows).Error; err != nil {
		log.Printf("task scheduler successor list: %v", err)
		return
	}
	for _, row := range rows {
		if err := ts.store.reconcileSuccessor(ctx, row); err != nil {
			log.Printf("task scheduler successor %s: %v", row.ID, err)
		}
	}
}

// reconcileSuccessor 补齐一个继任组的文件事实并推进状态。
func (s *Store) reconcileSuccessor(ctx context.Context, succ AgentOrchestration) error {
	container, err := s.groupContainerDir(succ)
	if err != nil {
		return err
	}
	doc, _, err := readHandoff(container, succ.ID)
	if err != nil {
		return err
	}
	if doc == nil {
		return fmt.Errorf("继任组缺少完整的 handoff，需要人工处理")
	}
	var orig AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", doc.ReplanOf).Take(&orig).Error; err != nil {
		return err
	}
	// 快照：按 handoff 里的复用清单重新解析并补齐缺失的副本（已存在且可读的跳过）。
	resolved, err := s.resolveSuccessorReuse(ctx, succ.TenantID, orig, doc.Reuse)
	if err != nil {
		return err
	}
	newHandle, err := s.workspaceGroupHandle(succ)
	if err != nil {
		return err
	}
	if err := s.materializeSuccessorReuse(ctx, succ.TenantID, resolved, newHandle); err != nil {
		return err
	}
	if err := writeGroupReuse(container, groupReuseFile{
		OrchestrationID: succ.ID, ReplanOf: orig.ID, Reuse: reuseEntriesFrom(resolved),
	}); err != nil {
		return err
	}
	if err := s.ensureSuccessorMark(ctx, orig, succ.ID, succ.IdempotencyKey); err != nil {
		return err
	}
	if _, _, err := s.finalizeSuccessor(ctx, succ.ID, orig.ID); err != nil {
		return err
	}
	return nil
}
