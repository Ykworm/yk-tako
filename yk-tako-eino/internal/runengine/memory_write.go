package runengine

// 本文件：抽取结果的**写库事务**（doc/65 §4.8；施工包 §11.3）。
//
// 事务的形状（顺序是硬要求）：
//
//	BEGIN
//	→ 取来源 workspace 级 PG advisory lock（多副本互斥）
//	→ 复核 job_id + 当前 claim_epoch（旧持有者写不进来）
//	→ 逐条复核候选（layer/state/证据新旧）
//	→ 写前校验（时间、merge 词集、删除抑制、预算）
//	→ 落所有行变化 + 记 batch_id/batch_payload/op_index
//	→ batch_committed_at + job done
//	COMMIT
//
// 失败语义（**整批全滚**）：
//   - 候选序号非法、目标状态已变、动作越权 → 整批不写，不降级成矛盾双写；
//   - 删除抑制查询失败 → 整 job 失败重试，**不能 fail-open**（fail-open = 把用户删掉的
//     事实又写回去，这是记忆系统最不可接受的失败）；
//   - 常驻超预算 → 不写，返回使用量与当前条目；
//   - merge 引入原文没有的新词 → 拒绝该动作（整批不写）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

// memoryAction 是模型给出的一个动作。
//
// Targets 是**候选序号**（不是数据库 id）——模型不碰数据库 id。
// Resident 只在 op=supersede_by_resident 时用：指向那条更新的常驻行。
type memoryAction struct {
	Op        string  `json:"op"`
	Targets   []int   `json:"targets"`
	Resident  int     `json:"resident"`
	Scope     string  `json:"scope"`
	Content   string  `json:"content"`
	Quote     string  `json:"quote"`
	ValidFrom *string `json:"valid_from"`
	ExpiresAt *string `json:"expires_at"`
}

// 动作词表（doc/65 §4.7）。
const (
	memoryOpInsert              = "insert"
	memoryOpReplace             = "replace"
	memoryOpMerge               = "merge"
	memoryOpConflict            = "conflict"
	memoryOpCoveredByResident   = "covered_by_resident"
	memoryOpSupersedeByResident = "supersede_by_resident"
	memoryOpRemove              = "remove"
)

// parseMemoryActions 解析最终动作列表。
//
// 单个动作字段非法 → **丢该动作**，其他动作继续（doc/65 §4.8 的失败语义表：
// 「单个候选字段非法 → 丢该候选，其他候选继续分析」）。
// 但**整体 JSON 不可解析** → 整 job 失败重试。
func parseMemoryActions(raw string) ([]memoryAction, error) {
	raw = stripCodeFence(raw)
	var out struct {
		Actions []memoryAction `json:"actions"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	kept := make([]memoryAction, 0, len(out.Actions))
	for _, a := range out.Actions {
		a.Op = strings.TrimSpace(a.Op)
		a.Scope = strings.TrimSpace(a.Scope)
		a.Content = strings.TrimSpace(a.Content)
		a.Quote = strings.TrimSpace(a.Quote)
		switch a.Op {
		case memoryOpInsert, memoryOpReplace, memoryOpMerge, memoryOpConflict,
			memoryOpCoveredByResident, memoryOpSupersedeByResident, memoryOpRemove:
		default:
			continue // 未知动作：丢掉，不猜
		}
		kept = append(kept, a)
	}
	return kept, nil
}

// applyActions 把动作落进 memory_items（**一笔事务**，doc/65 §4.8）。
//
// 返回真正写入的行数（insert/replace/merge 产生的新行）。
//
// 事务形状（顺序是硬要求）：
//
//	BEGIN
//	→ workspace 级 pg_advisory_xact_lock（跨 session 写同一项目在这里串行）
//	→ 复核 job_id + 当前 claim_epoch
//	→ 逐条动作（序号解析、准入、词集、时间、删除抑制、证据新旧）
//	→ 常驻预算
//	→ 行变化 + batch_id/batch_payload + batch_committed_at + job done
//	COMMIT
//
// 行变化与「已提交」标记在**同一笔**事务里：不存在「行写了、批次没记」的中间态。
// （先写行、后记批次的两笔形状，在两笔之间崩溃时会让重放重新分析、重复写行——
// 违反 doc/65 §4.8 的「每个 job 至多成功提交一个写批次」。）
func (e *memoryExtractor) applyActions(ctx context.Context, job *MemoryJob, candidates []memoryExtractCandidate, actions []memoryAction) (int, error) {
	// 候选序号 → 候选行（模型编序号 → 拒）。
	byIndex := make(map[int]memoryExtractCandidate, len(candidates))
	for _, c := range candidates {
		byIndex[c.Index] = c
	}
	acts := actions
	if acts == nil {
		acts = []memoryAction{}
	}
	payload, err := json.Marshal(acts)
	if err != nil {
		return 0, err
	}
	batchID := "batch_" + job.ID
	written := 0
	err = e.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 动作数上限在**事务内**复核（doc/65 §4.8：单批写动作数必须在事务内复核）。
		if len(actions) > memoryMaxActions {
			return fmt.Errorf("最终动作 %d 条，超过上限 %d 条", len(actions), memoryMaxActions)
		}
		// workspace 级 advisory lock：覆盖「跨 session 写同一项目」的竞争。
		// 事务级锁（pg_advisory_xact_lock）：COMMIT/ROLLBACK 自动释放，不用管连接；
		// 键加 memory: 前缀，与文件锁（filelock.go）的键空间分开。
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('memory:' || ?, 0))`, job.Workspace).Error; err != nil {
			return err
		}
		// 复核 job 仍是 running 且 epoch 没变（旧消费者写不进来）。
		if err := e.checkJobOwnership(tx, job); err != nil {
			return err
		}
		written = 0
		now := time.Now()
		for _, a := range actions {
			switch a.Op {
			case memoryOpCoveredByResident:
				// 常驻已覆盖：不写召回行（这是模型的判断，程序不推翻）。
				continue
			case memoryOpInsert:
				id, err := e.insertAction(ctx, tx, job, a, now)
				if err != nil {
					return err
				}
				if id != "" {
					written++
				}
			case memoryOpReplace:
				n, err := e.replaceAction(ctx, tx, job, a, byIndex, now)
				if err != nil {
					return err
				}
				written += n
			case memoryOpMerge:
				n, err := e.mergeAction(ctx, tx, job, a, byIndex, now)
				if err != nil {
					return err
				}
				written += n
			case memoryOpConflict:
				if err := e.conflictAction(tx, a, byIndex, now); err != nil {
					return err
				}
			case memoryOpSupersedeByResident:
				if err := e.supersedeByResidentAction(tx, a, byIndex, now); err != nil {
					return err
				}
			case memoryOpRemove:
				n, err := e.removeAction(tx, a, byIndex, now)
				if err != nil {
					return err
				}
				written += n
			}
		}
		// 常驻预算：写完再算，超了整笔回滚（不写半截）。
		if err := e.checkResidentBudget(ctx, tx, job); err != nil {
			return err
		}
		// 批次 + done：与行变化同笔提交/回滚（重放看到 batch_committed_at 非空直接收尾）。
		// last_error **不清**：行上可能记着 inrun_failed 警告（警告借这个格），done 不抹历史记录。
		return tx.Exec(
			`UPDATE memory_jobs SET batch_id = ?, batch_payload = ?, batch_committed_at = now(),
			        status = 'done', lease_until = NULL, updated_at = now()
			  WHERE id = ? AND claim_epoch = ?`,
			batchID, string(payload), job.ID, job.ClaimEpoch).Error
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// checkJobOwnership 复核 job 仍归当前持有者（id + status=running + claim_epoch 一致）。
//
// 旧消费者即使复活，也**不能**凭旧 epoch 写记忆、翻 done 或覆盖错误。
func (e *memoryExtractor) checkJobOwnership(tx *gorm.DB, job *MemoryJob) error {
	var cur MemoryJob
	if err := tx.Raw(`SELECT id, status, claim_epoch FROM memory_jobs WHERE id = ?`, job.ID).Scan(&cur).Error; err != nil {
		return err
	}
	if cur.ID == "" {
		return errors.New("任务已不存在（项目被删？）")
	}
	if cur.Status != MemoryJobStatusRunning {
		return fmt.Errorf("任务状态已是 %s（不是 running），本次写入作废", cur.Status)
	}
	if cur.ClaimEpoch != job.ClaimEpoch {
		return fmt.Errorf("claim_epoch 不匹配（当前 %d，本次 %d），写入作废", cur.ClaimEpoch, job.ClaimEpoch)
	}
	return nil
}

// insertAction 写一条新记忆。
func (e *memoryExtractor) insertAction(ctx context.Context, tx *gorm.DB, job *MemoryJob, a memoryAction, now time.Time) (string, error) {
	if a.Scope != MemoryScopeUser && a.Scope != MemoryScopeWorkspace {
		return "", fmt.Errorf("insert 的 scope 非法：%q", a.Scope)
	}
	if a.Content == "" {
		return "", errors.New("insert 的 content 是空的")
	}
	if utf8.RuneCountInString(a.Content) > memoryContentCap {
		return "", fmt.Errorf("insert 的 content %d 字超过上限 %d", utf8.RuneCountInString(a.Content), memoryContentCap)
	}
	if utf8.RuneCountInString(a.Quote) > memoryQuoteCap {
		return "", fmt.Errorf("insert 的 quote %d 字超过上限 %d", utf8.RuneCountInString(a.Quote), memoryQuoteCap)
	}
	// **删除抑制**：用户明确删掉过的事实，不许被后来的旧证据写回来。
	// 查询失败 → 整 job 失败（**不 fail-open**）。
	suppressed, err := isRemovedContentTx(tx, job, a.Scope, a.Content)
	if err != nil {
		return "", fmt.Errorf("删除抑制检查失败（不 fail-open）：%w", err)
	}
	if suppressed {
		return "", nil
	}
	item := MemoryItem{
		TenantID:        job.TenantID,
		UserID:          job.UserID,
		Workspace:       job.Workspace,
		Layer:           e.targetLayer(job),
		Scope:           a.Scope,
		Content:         a.Content,
		Quote:           a.Quote,
		State:           MemoryStateActive,
		Source:          MemorySourceExtracted,
		SourceSessionID: job.SessionID,
		SourceRunID:     job.RunID,
		SourceSeq:       job.SourceSeq,
	}
	if err := e.applyTimes(&item, a); err != nil {
		return "", err
	}
	res, err := e.store.writeMemoryItemsTx(ctx, tx, []MemoryItem{item})
	if err != nil {
		return "", err
	}
	if res.Rejected > 0 {
		// 写前准入拒了（隐形字符/PII）→ 这条不写（其它动作继续由调用方处理，
		// 但这里按「整批不写」的保守语义返回错误，让整 job 重试而不是静默少一条）。
		return "", errors.New("新记忆被写前准入拒收（隐形字符或 PII）")
	}
	if len(res.IDs) > 0 {
		return res.IDs[0], nil
	}
	return "", nil
}

// replaceAction：新行替代旧行（旧行 superseded_by 指新行）。
func (e *memoryExtractor) replaceAction(ctx context.Context, tx *gorm.DB, job *MemoryJob, a memoryAction, byIndex map[int]memoryExtractCandidate, now time.Time) (int, error) {
	targets, err := e.resolveTargets(tx, a.Targets, byIndex)
	if err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, errors.New("replace 没有有效目标")
	}
	if a.Content == "" {
		return 0, errors.New("replace 的 content 是空的")
	}
	// 证据新旧：旧证据不能覆盖较新证据（同 run 内用 source_seq，跨 run 用 runlog 时刻）。
	if err := e.checkEvidenceNotOlder(ctx, tx, job, targets); err != nil {
		return 0, err
	}
	item := MemoryItem{
		TenantID:        job.TenantID,
		UserID:          job.UserID,
		Workspace:       job.Workspace,
		Layer:           e.targetLayer(job),
		Scope:           e.scopeOf(targets[0]),
		Content:         a.Content,
		Quote:           a.Quote,
		State:           MemoryStateActive,
		Source:          MemorySourceExtracted,
		SourceSessionID: job.SessionID,
		SourceRunID:     job.RunID,
		SourceSeq:       job.SourceSeq,
	}
	if err := e.applyTimes(&item, a); err != nil {
		return 0, err
	}
	res, err := e.store.writeMemoryItemsTx(ctx, tx, []MemoryItem{item})
	if err != nil {
		return 0, err
	}
	if res.Rejected > 0 || len(res.IDs) == 0 {
		return 0, errors.New("替代行被写前准入拒收")
	}
	newID := res.IDs[0]
	for _, t := range targets {
		if err := e.store.supersedeMemoryItemTx(tx, t.Item.ID, newID, now); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

// mergeAction：多条合成一条（被合并的旧行都指向新行）。
//
// **merge 不得引入原文没有的新事实**：用 pg_search 的分词器算词集，
// 旧行词集里出现在新行之外的词 = 新引入的事实 → 拒绝。
// 分词能力不可用 → 拒绝（不降级 coexist）。
func (e *memoryExtractor) mergeAction(ctx context.Context, tx *gorm.DB, job *MemoryJob, a memoryAction, byIndex map[int]memoryExtractCandidate, now time.Time) (int, error) {
	targets, err := e.resolveTargets(tx, a.Targets, byIndex)
	if err != nil {
		return 0, err
	}
	if len(targets) < 2 {
		return 0, errors.New("merge 至少要两条目标")
	}
	if a.Content == "" {
		return 0, errors.New("merge 的 content 是空的")
	}
	// 词集校验：合成后的正文不能带来旧行没有的词。
	newWords, err := memoryWordSetTx(tx, a.Content)
	if err != nil {
		return 0, fmt.Errorf("merge 词集校验不可用（拒绝 merge，不降级）：%w", err)
	}
	for _, t := range targets {
		oldWords, err := memoryWordSetTx(tx, t.Item.Content)
		if err != nil {
			return 0, fmt.Errorf("merge 词集校验不可用（拒绝 merge，不降级）：%w", err)
		}
		for w := range oldWords {
			if !newWords[w] {
				return 0, fmt.Errorf("merge 引入了原文没有的新事实（词 %q）", w)
			}
		}
	}
	if err := e.checkEvidenceNotOlder(ctx, tx, job, targets); err != nil {
		return 0, err
	}
	item := MemoryItem{
		TenantID:        job.TenantID,
		UserID:          job.UserID,
		Workspace:       job.Workspace,
		Layer:           e.targetLayer(job),
		Scope:           e.scopeOf(targets[0]),
		Content:         a.Content,
		Quote:           a.Quote,
		State:           MemoryStateActive,
		Source:          MemorySourceExtracted,
		SourceSessionID: job.SessionID,
		SourceRunID:     job.RunID,
		SourceSeq:       job.SourceSeq,
	}
	if err := e.applyTimes(&item, a); err != nil {
		return 0, err
	}
	res, err := e.store.writeMemoryItemsTx(ctx, tx, []MemoryItem{item})
	if err != nil {
		return 0, err
	}
	if res.Rejected > 0 || len(res.IDs) == 0 {
		return 0, errors.New("合并行被写前准入拒收")
	}
	newID := res.IDs[0]
	for _, t := range targets {
		if err := e.store.supersedeMemoryItemTx(tx, t.Item.ID, newID, now); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

// conflictAction：两行互写 conflict_with（**不自动选赢家**）。
func (e *memoryExtractor) conflictAction(tx *gorm.DB, a memoryAction, byIndex map[int]memoryExtractCandidate, now time.Time) error {
	targets, err := e.resolveTargets(tx, a.Targets, byIndex)
	if err != nil {
		return err
	}
	if len(targets) < 2 {
		return errors.New("conflict 至少要两条目标")
	}
	for i := 0; i < len(targets); i++ {
		for j := i + 1; j < len(targets); j++ {
			if err := e.store.setMemoryConflictTx(tx, targets[i].Item.ID, targets[j].Item.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// supersedeByResidentAction：旧召回行指向较新的常驻行（**不机械级联**）。
func (e *memoryExtractor) supersedeByResidentAction(tx *gorm.DB, a memoryAction, byIndex map[int]memoryExtractCandidate, now time.Time) error {
	targets, err := e.resolveTargets(tx, a.Targets, byIndex)
	if err != nil {
		return err
	}
	resident, ok := byIndex[a.Resident]
	if !ok {
		return errors.New("supersede_by_resident 的 resident 序号非法")
	}
	if resident.Item.Layer != MemoryLayerResident {
		return errors.New("supersede_by_resident 指向的不是常驻行")
	}
	for _, t := range targets {
		if t.Item.Layer != MemoryLayerRecall {
			return errors.New("supersede_by_resident 的目标必须是召回行")
		}
		if err := e.store.supersedeMemoryItemTx(tx, t.Item.ID, resident.Item.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// removeAction：用户明确删除 → state=removed + removed_at。
//
// **source='user' 的行不被动**（用户自己说的那件事，用户自己删）。
func (e *memoryExtractor) removeAction(tx *gorm.DB, a memoryAction, byIndex map[int]memoryExtractCandidate, now time.Time) (int, error) {
	targets, err := e.resolveTargets(tx, a.Targets, byIndex)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range targets {
		if t.Item.Source == MemorySourceUser {
			// 用户自己说的行不删（程序不替用户删他自己的原话）。
			continue
		}
		if err := e.store.removeMemoryItemTx(tx, t.Item.ID, now); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// resolveTargets 把候选序号解析成目标行（**在事务里复核状态**）。
//
// 序号非法 / 目标已不是 active → 整批拒绝（不降级成矛盾双写）。
func (e *memoryExtractor) resolveTargets(tx *gorm.DB, idxs []int, byIndex map[int]memoryExtractCandidate) ([]memoryExtractCandidate, error) {
	if len(idxs) == 0 {
		return nil, errors.New("动作没有目标")
	}
	out := make([]memoryExtractCandidate, 0, len(idxs))
	seen := map[string]bool{}
	for _, i := range idxs {
		c, ok := byIndex[i]
		if !ok {
			return nil, fmt.Errorf("候选序号 %d 不在本任务的候选表里", i)
		}
		if seen[c.Item.ID] {
			continue
		}
		seen[c.Item.ID] = true
		// 事务里复核：这一行现在还在不在、是不是 active。
		var cur MemoryItem
		if err := tx.Raw(`SELECT * FROM memory_items WHERE id = ?`, c.Item.ID).Scan(&cur).Error; err != nil {
			return nil, err
		}
		if cur.ID == "" {
			return nil, fmt.Errorf("目标行 %s 已不存在", c.Item.ID)
		}
		if cur.State != MemoryStateActive {
			return nil, fmt.Errorf("目标行 %s 已是 %s，不是 active", cur.ID, cur.State)
		}
		c.Item = cur
		out = append(out, c)
	}
	return out, nil
}

// targetLayer 取本次写入的目标层。
//
// inrun 用 job 指定的层；postrun **永远 recall**（后台兜底不晋升常驻）；tidy 不晋升层。
func (e *memoryExtractor) targetLayer(job *MemoryJob) string {
	switch job.Kind {
	case MemoryJobKindInrun:
		if job.TargetLayer != nil && *job.TargetLayer != "" {
			return *job.TargetLayer
		}
		return MemoryLayerRecall
	case MemoryJobKindPostrun:
		return MemoryLayerRecall
	default:
		return MemoryLayerRecall
	}
}

// scopeOf 取目标行的可见域（replace/merge 沿用旧行的域，不让模型改归属）。
func (e *memoryExtractor) scopeOf(c memoryExtractCandidate) string {
	if c.Item.Scope == MemoryScopeWorkspace {
		return MemoryScopeWorkspace
	}
	return MemoryScopeUser
}

// applyTimes 填有效期（expires_at 必须在未来；valid_from 解析不了就清空）。
func (e *memoryExtractor) applyTimes(item *MemoryItem, a memoryAction) error {
	now := time.Now()
	if a.ExpiresAt != nil && strings.TrimSpace(*a.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*a.ExpiresAt))
		if err != nil {
			return fmt.Errorf("expires_at 解析不了：%q", *a.ExpiresAt)
		}
		if !t.After(now) {
			return fmt.Errorf("expires_at %s 不在未来", t)
		}
		item.ExpiresAt = &t
	}
	if a.ValidFrom != nil && strings.TrimSpace(*a.ValidFrom) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*a.ValidFrom))
		if err != nil {
			// valid_from 解析不了 → **清空该字段**（不猜一个时间）。
			item.ValidFrom = nil
		} else {
			if item.ExpiresAt != nil && !t.Before(*item.ExpiresAt) {
				return errors.New("valid_from 不早于 expires_at")
			}
			item.ValidFrom = &t
		}
	}
	if item.ValidFrom != nil && item.ExpiresAt != nil && !item.ValidFrom.Before(*item.ExpiresAt) {
		return errors.New("valid_from 不早于 expires_at")
	}
	return nil
}

// checkEvidenceNotOlder：旧证据不能覆盖较新证据。
//
// 判据是**证据时间**（runlog_entries.created_at），不是提交时间——
// 并发 run 的完成顺序不等于说话顺序。
func (e *memoryExtractor) checkEvidenceNotOlder(ctx context.Context, tx *gorm.DB, job *MemoryJob, targets []memoryExtractCandidate) error {
	newAt, ok, err := memoryEvidenceTimeDB(tx, job.RunID, job.SourceSeq)
	if err != nil {
		return err
	}
	if !ok {
		return nil // 取不到新证据时间 → 不判（不拿猜的时间否掉合法的改口）
	}
	for _, t := range targets {
		oldAt, ok, err := memoryEvidenceTimeDB(tx, t.Item.SourceRunID, t.Item.SourceSeq)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if newAt.Before(oldAt) {
			return fmt.Errorf("旧证据不能覆盖较新证据（本次 %s < 目标 %s）", newAt, oldAt)
		}
	}
	return nil
}

// checkResidentBudget：常驻层的 rune 总量不能超预算。
//
// 超了 → 整笔回滚（不写半截）；由调用方决定重试还是 failed。
func (e *memoryExtractor) checkResidentBudget(ctx context.Context, tx *gorm.DB, job *MemoryJob) error {
	capRunes := e.store.residentRuneCap(ctx, job.TenantID)
	var used int64
	if err := tx.Raw(
		`SELECT COALESCE(SUM(length(content)), 0) FROM memory_items WHERE layer = 'resident' AND state = 'active' AND tenant_id = ?`,
		job.TenantID).Scan(&used).Error; err != nil {
		return err
	}
	if used > int64(capRunes) {
		return fmt.Errorf("常驻记忆超预算：%d/%d rune", used, capRunes)
	}
	return nil
}

// ── store 侧的小工具（抽取器用）────────────────────────────────────────────────

// listActiveMemoryForExtraction 读候选：可见域全部 active 的 resident + recall。
func (s *Store) listActiveMemoryForExtraction(ctx context.Context, job *MemoryJob) ([]MemoryItem, error) {
	var items []MemoryItem
	err := s.db.WithContext(ctx).Raw(
		`SELECT * FROM memory_items
		  WHERE `+memoryVisibilitySQL()+`
		    AND state = 'active'
		    AND (expires_at IS NULL OR expires_at > now())`,
		job.TenantID, job.UserID, job.Workspace).Scan(&items).Error
	if err != nil {
		return nil, err
	}
	return items, nil
}

// hasUnfinishedInrun：同 session 还有未完成的 inrun 吗（pending 或 running）。
//
// failed **不算**未完成——它已经结束了，不能因此永久卡住后续 postrun。
func (s *Store) hasUnfinishedInrun(ctx context.Context, sessionID SessionID) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var n int64
	err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM memory_jobs
		  WHERE session_id = ? AND kind = 'inrun' AND status IN ('pending', 'running')`,
		sessionID).Scan(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// isRemovedContentTx：这条内容在同域里有没有被**明确删除**过（**跑在写事务里**，
// 与行变化同快照）。有 → 这次不写回（用户的删除优先）。查询失败 → 错误上抛（不 fail-open）。
func isRemovedContentTx(tx *gorm.DB, job *MemoryJob, scope, content string) (bool, error) {
	var n int64
	err := tx.Raw(
		`SELECT count(*) FROM memory_items
		  WHERE tenant_id = ? AND scope = ? AND content = ? AND state = 'removed'`,
		job.TenantID, scope, content).Scan(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// memoryWordSetTx 取一段文本的词集（pg_search 的 chinese_lindera 分词；**跑在写事务里**）。
//
// 纯标点与空格不算词（否则「改个路径标点」会被误判成「引入新事实」）。
// 分词不可用 → 错误上抛（merge 拒绝，不降级）。
func memoryWordSetTx(tx *gorm.DB, text string) (map[string]bool, error) {
	var agg string
	// 第二参数必须显式 ::text：不 cast 时参数以 unknown 进函数解析，PG 报
	// 「paradedb.tokenize(jsonb, unknown) does not exist」（2026-11-10 实测）。
	err := tx.Raw(
		`SELECT COALESCE(array_agg(DISTINCT (rec).token)::text, '') FROM (
		   SELECT paradedb.tokenize('`+memoryTokenizerJSON+`'::jsonb, ?::text) AS rec) t`,
		text).Scan(&agg).Error
	if err != nil {
		return nil, err
	}
	words := map[string]bool{}
	for _, w := range strings.Split(strings.Trim(agg, "{}"), ",") {
		w = strings.Trim(strings.TrimSpace(w), `"`)
		if w == "" || isPunctWord(w) {
			continue
		}
		words[w] = true
	}
	return words, nil
}

// isPunctWord：这个「词」只是标点或空白吗。
func isPunctWord(w string) bool {
	for _, r := range w {
		if !strings.ContainsRune(" \t\n　·.,;:!?、，。；：！？-_/\\()（）[]【】\"'“”‘’…—～@#$%^&*+=<>|", r) {
			return false
		}
	}
	return true
}

// writeMemoryItemsTx 是 WriteMemoryItems 的事务内版本（抽取器在同一个事务里写行）。
func (s *Store) writeMemoryItemsTx(ctx context.Context, tx *gorm.DB, items []MemoryItem) (MemoryWriteResult, error) {
	return s.writeMemoryItemsWith(ctx, tx, items)
}

// supersedeMemoryItemTx：旧行 superseded → state=superseded + superseded_by + left_active_at（只写一次）。
func (s *Store) supersedeMemoryItemTx(tx *gorm.DB, id, supersededBy string, now time.Time) error {
	return tx.Exec(
		`UPDATE memory_items
		    SET state = 'superseded', superseded_by = ?,
		        left_active_at = COALESCE(left_active_at, ?), updated_at = ?
		  WHERE id = ? AND state = 'active'`,
		supersededBy, now, now, id).Error
}

// removeMemoryItemTx：明确删除 → state=removed + removed_at。
func (s *Store) removeMemoryItemTx(tx *gorm.DB, id string, now time.Time) error {
	return tx.Exec(
		`UPDATE memory_items
		    SET state = 'removed', removed_at = ?, left_active_at = COALESCE(left_active_at, ?), updated_at = ?
		  WHERE id = ? AND state = 'active'`,
		now, now, now, id).Error
}

// setMemoryConflictTx：两行互写 conflict_with。
func (s *Store) setMemoryConflictTx(tx *gorm.DB, a, b string) error {
	if err := tx.Exec(`UPDATE memory_items SET conflict_with = ?, updated_at = now() WHERE id = ?`, b, a).Error; err != nil {
		return err
	}
	return tx.Exec(`UPDATE memory_items SET conflict_with = ?, updated_at = now() WHERE id = ?`, a, b).Error
}

// residentRuneCap 取该租户的常驻预算（缺省 2200；租户 runtime config 可调 500–10000）。
func (s *Store) residentRuneCap(ctx context.Context, tenantID string) int {
	const def = 2200
	if tenantID == "" {
		return def
	}
	cfg, err := s.GetRuntimeConfig(ctx, tenantID)
	if err != nil {
		return def
	}
	if cfg.Memory.ResidentRunes >= 500 && cfg.Memory.ResidentRunes <= 10000 {
		return cfg.Memory.ResidentRunes
	}
	return def
}
