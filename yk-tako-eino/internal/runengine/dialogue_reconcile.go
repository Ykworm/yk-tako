package runengine

// 本文件是历史对话混合检索的定期对账器（doc/84 §6.4，施工包 §7 P3 第 1 条）：
// outbox + 至少一次投递保证「登记过的任务最终写出去」，但外部副本仍可能因
// 外部实例回退（文档被手工清掉、索引被重建）而缺——对账器周期性比较
// 「PG 应存在」（dialogue_chunks 按 run_id × 当前指针 index_version）与
// 「外部实际记录」（ES / Milvus 按 run_id 枚举），缺失走既有投递链自愈，
// 孤儿走既有 delete 链清理，并输出覆盖水位与消费 lag。
//
// 闸放数据库不放进程（AGENTS.md 部署形态）：对账重投不维护任何内存状态，
// 全部通过 PG 任务行的状态迁移（done→pending 重拨 / 无行→登记）表达，
// 与消费路径共用同一组唯一约束与领取互斥，多副本下天然安全。

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"
)

// 对账节奏：间隔可配（TAKO_DIALOGUE_RECONCILE_INTERVAL，缺省 5 分钟）。
const (
	dialogueCoverageIntervalEnv = "TAKO_DIALOGUE_RECONCILE_INTERVAL"
	dialogueCoverageInterval    = 5 * time.Minute

	dialogueReconcileListPage = 500    // 外部枚举分页大小
	dialogueReconcileMaxRuns  = 100000 // 单轮枚举上限（超限截断，防失控）
)

// DialogueCoverageStats 是 outbox 覆盖水位（doc/84 §6.4）：机器可读的任务面状态，
// 对账器每轮进日志，P4 的 status 端点原样透出。
type DialogueCoverageStats struct {
	Pending         int64      // status=pending 且 attempts=0：还没被消费过的未处理任务
	Retry           int64      // status=pending 且 attempts>0：退避中
	Running         int64      // 认领未归还（含租约未过期）
	Failed          int64      // 死信（永久失败；不无限重试）
	OldestPendingAt *time.Time // 未终态（pending/running）里 created_at 最老的一条
}

// DialogueCoverage 一次聚合算出全部水位（单条 SQL，无行锁）。
func (s *Store) DialogueCoverage(ctx context.Context) (DialogueCoverageStats, error) {
	var stats DialogueCoverageStats
	err := s.db.WithContext(ctx).Raw(`
		SELECT
		  count(*) FILTER (WHERE status = 'pending' AND attempts = 0)::bigint AS pending,
		  count(*) FILTER (WHERE status = 'pending' AND attempts > 0)::bigint AS retry,
		  count(*) FILTER (WHERE status = 'running')::bigint                   AS running,
		  count(*) FILTER (WHERE status = 'failed')::bigint                    AS failed,
		  min(created_at) FILTER (WHERE status IN ('pending', 'running'))      AS oldest_pending_at
		FROM dialogue_index_jobs`).
		Scan(&stats).Error
	return stats, err
}

// dialogueRunIDLister 是对账用的外部 run_id 枚举面（真实现 = ES / Milvus 客户端，
// 单测打桩）。CountDocs 与 ListAllRunIDs 分开：计数是独立查询，缺失/孤儿用集合差。
type dialogueRunIDLister interface {
	// ListAllRunIDs 枚举当前绑定索引/collection 的全部 run_id；超过 maxRuns 截断。
	ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error)
	// CountDocs 当前索引/collection 的文档数。
	CountDocs(ctx context.Context) (int64, error)
}

var (
	_ dialogueRunIDLister = (*dialogueESClient)(nil)
	_ dialogueRunIDLister = (*dialogueMilvusClient)(nil)
)

// dialogueReconciler 是对账器本体：每轮输出覆盖水位 + lag + PG↔外部差异，
// 并把缺失/孤儿收敛进既有投递链。指针绑定名与启动时不一致（另一副本切换了版本）
// 时暂停外部差异比对，只报水位——客户端绑定是启动期固定的，等重启重绑。
type dialogueReconciler struct {
	store   *Store
	esName  string // 启动期绑定的 ES 索引名
	esList  dialogueRunIDLister
	milName string // 启动期绑定的 Milvus collection 名
	milList dialogueRunIDLister
	esQ     dialogueQueue
	vecQ    dialogueQueue
	iter    time.Duration
}

func newDialogueReconciler(store *Store, es *dialogueESClient, mil *dialogueMilvusClient,
	esQ, vecQ dialogueQueue, interval time.Duration) *dialogueReconciler {
	return &dialogueReconciler{
		store:   store,
		esName:  es.index,
		esList:  es,
		milName: mil.coll,
		milList: mil,
		esQ:     esQ,
		vecQ:    vecQ,
		iter:    interval,
	}
}

// Run 是周期循环（挂进 StartDialogueRetrieval 的生命周期，ctx 取消即退）。
func (r *dialogueReconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.iter)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report := r.runOnce(ctx)
			log.Printf("dialogue reconcile: coverage pending=%d retry=%d running=%d failed=%d oldestPending=%s lagES=%d lagVector=%d%s",
				report.Coverage.Pending, report.Coverage.Retry, report.Coverage.Running, report.Coverage.Failed,
				dialogueReconcileOldest(report.Coverage.OldestPendingAt), report.LagES, report.LagMilvus,
				dialogueReconcileLagNote(report.LagNote))
			if report.PointerSkipped {
				log.Printf("dialogue reconcile: 指针已切换（es=%s mil=%s），外部对账暂停；副本重启后重绑",
					r.esName, r.milName)
				continue
			}
			log.Printf("dialogue reconcile: 覆盖 pgChunks=%d es=%d(esMissing=%d esOrphan=%d) milvus=%d(milMissing=%d milOrphan=%d) heal reset=%d registered=%d inflight=%d deadletter=%d orphanDelete=%d skippedOrphan=%d%s",
				report.PGChunks, report.ESDocs, report.ESMissing, report.ESOrphan,
				report.MilvusDocs, report.MilvusMissing, report.MilvusOrphan,
				report.Rescheduled, report.Registered, report.Inflight, report.DeadLetters,
				report.OrphanDeleted, report.SkippedOrphans, dialogueReconcileTrunc(report.Truncated))
		}
	}
}

// dialogueReconcileReport 是单轮对账的结果（进日志；单测直接断言字段）。
type dialogueReconcileReport struct {
	Coverage       DialogueCoverageStats
	LagES          int64
	LagMilvus      int64
	LagNote        string // 非空 = lag 拿不到的原因（水位记 0 不伪造）
	PGChunks       int64
	ESDocs         int64
	MilvusDocs     int64
	ESMissing      int
	ESOrphan       int
	MilvusMissing  int
	MilvusOrphan   int
	Rescheduled    int // done 但外部缺失 → 重拨 pending
	Registered     int // 无任务行 → 新登记
	Inflight       int // pending/running 在途，不动
	DeadLetters    int // failed 且外部缺失：死信契约不动，计数上报（人工或全量重建处理）
	OrphanDeleted  int // 孤儿（外部有 PG 无）登记的 delete 事件数
	SkippedOrphans int // 登记 delete 前复查发现 PG 已有 chunk 的假孤儿
	PointerSkipped bool
	Truncated      bool
}

func dialogueReconcileOldest(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func dialogueReconcileLagNote(note string) string {
	if note == "" {
		return ""
	}
	return " lagNote=\"" + note + "\""
}

func dialogueReconcileTrunc(truncated bool) string {
	if truncated {
		return " truncated=true"
	}
	return ""
}

// runOnce 跑一轮：先报覆盖水位与 lag（无论指针是否绑定），再做 PG↔外部差异比对与收敛。
func (r *dialogueReconciler) runOnce(ctx context.Context) dialogueReconcileReport {
	report := dialogueReconcileReport{}
	if cov, err := r.store.DialogueCoverage(ctx); err != nil {
		log.Printf("dialogue reconcile: 覆盖水位查询失败: %v", err)
	} else {
		report.Coverage = cov
	}
	report.LagES, report.LagMilvus, report.LagNote = r.queueLag(ctx)

	ptr, err := r.store.dialoguePointer(ctx)
	if err != nil {
		log.Printf("dialogue reconcile: 读指针失败: %v", err)
		report.PointerSkipped = true
		return report
	}
	if ptr.EsIndex != r.esName || ptr.MilvusCollection != r.milName {
		// 另一副本切换了版本，本副本客户端还绑旧名——旧名上的比对只会产生假差异。
		report.PointerSkipped = true
		return report
	}

	pgIDs, truncated, err := r.store.dialogueChunkRunIDs(ctx, dialogueReconcileMaxRuns)
	if err != nil {
		log.Printf("dialogue reconcile: PG chunk 枚举失败: %v", err)
		return report
	}
	report.Truncated = truncated
	report.PGChunks, err = r.store.dialogueChunkCount(ctx)
	if err != nil {
		log.Printf("dialogue reconcile: PG chunk 计数失败: %v", err)
		return report
	}

	esIDs, esTrunc, err := r.esList.ListAllRunIDs(ctx, dialogueReconcileMaxRuns)
	if err != nil {
		log.Printf("dialogue reconcile: ES run_id 枚举失败（跳过 ES 臂）: %v", err)
	} else {
		if esTrunc {
			report.Truncated = true
		}
		if cnt, cerr := r.esList.CountDocs(ctx); cerr != nil {
			log.Printf("dialogue reconcile: ES 计数失败: %v", cerr)
		} else {
			report.ESDocs = cnt
		}
		report.ESMissing, report.ESOrphan = dialogueReconcileDiff(pgIDs, esIDs)
		r.healMissing(ctx, ptr.IndexVersion, DialogueTargetES, dialogueReconcileSubset(pgIDs, esIDs), &report)
	}

	milIDs, milTrunc, err := r.milList.ListAllRunIDs(ctx, dialogueReconcileMaxRuns)
	if err != nil {
		log.Printf("dialogue reconcile: Milvus run_id 枚举失败（跳过 Milvus 臂）: %v", err)
	} else {
		if milTrunc {
			report.Truncated = true
		}
		if cnt, cerr := r.milList.CountDocs(ctx); cerr != nil {
			log.Printf("dialogue reconcile: Milvus 计数失败: %v", cerr)
		} else {
			report.MilvusDocs = cnt
		}
		report.MilvusMissing, report.MilvusOrphan = dialogueReconcileDiff(pgIDs, milIDs)
		r.healMissing(ctx, ptr.IndexVersion, DialogueTargetMilvus, dialogueReconcileSubset(pgIDs, milIDs), &report)
	}

	// 孤儿（外部有、PG 无）两条臂合并处理：delete 任务一次登记就是两条目标任务
	// （es + milvus 配对），重复登记被唯一约束回读吸收。
	if len(esIDs) > 0 || len(milIDs) > 0 {
		var orphans []string
		seen := map[string]bool{}
		for id := range esIDs {
			if !pgIDs[id] && !seen[id] {
				seen[id] = true
				orphans = append(orphans, id)
			}
		}
		for id := range milIDs {
			if !pgIDs[id] && !seen[id] {
				seen[id] = true
				orphans = append(orphans, id)
			}
		}
		registered, skipped, err := r.store.registerDialogueOrphanDeletes(ctx, orphans)
		if err != nil {
			log.Printf("dialogue reconcile: 孤儿 delete 登记失败: %v", err)
		}
		report.OrphanDeleted = registered
		report.SkippedOrphans = skipped
	}
	return report
}

// healMissing 对一组缺失 run_id 做收敛：无行登记 / done 重拨 / failed 死信计数 / 在途不动。
func (r *dialogueReconciler) healMissing(ctx context.Context, version int64, target string, missing []string, report *dialogueReconcileReport) {
	for _, runID := range missing {
		action, err := r.store.dialogueRescheduleMissing(ctx, runID, version, target)
		if err != nil {
			log.Printf("dialogue reconcile: run=%s target=%s 补投失败: %v", runID, target, err)
			continue
		}
		switch action {
		case dialogueHealReset:
			report.Rescheduled++
		case dialogueHealRegistered:
			report.Registered++
		case dialogueHealInflight:
			report.Inflight++
		case dialogueHealDeadLetter:
			report.DeadLetters++
		}
	}
}

// queueLag 查两个消费组的积压；拿不到记 0 并带说明（不伪造水位）。
func (r *dialogueReconciler) queueLag(ctx context.Context) (int64, int64, string) {
	var notes []string
	lagES, esNote := r.lagFor(ctx, r.esQ)
	lagVec, vecNote := r.lagFor(ctx, r.vecQ)
	if esNote != "" {
		notes = append(notes, dialogueESGroup+": "+esNote)
	}
	if vecNote != "" {
		notes = append(notes, dialogueVectorGroup+": "+vecNote)
	}
	return lagES, lagVec, strings.Join(notes, "; ")
}

func (r *dialogueReconciler) lagFor(ctx context.Context, q dialogueQueue) (int64, string) {
	if q == nil {
		return 0, "队列未装配"
	}
	lagger, ok := q.(dialogueQueueLagger)
	if !ok {
		return 0, "队列未实现 lag 查询"
	}
	n, err := lagger.ConsumerLag(ctx)
	if err != nil {
		return 0, "lag 查询失败: " + err.Error()
	}
	return n, ""
}

// dialogueReconcileDiff 返回 (PG 有外部无=缺失, 外部有 PG 无=孤儿) 的计数。
func dialogueReconcileDiff(pgIDs, extIDs map[string]bool) (int, int) {
	missing, orphan := 0, 0
	for id := range pgIDs {
		if !extIDs[id] {
			missing++
		}
	}
	for id := range extIDs {
		if !pgIDs[id] {
			orphan++
		}
	}
	return missing, orphan
}

// dialogueReconcileSubset 返回 pgIDs 有而 extIDs 没有的 run_id 列表（确定性排序）。
func dialogueReconcileSubset(pgIDs, extIDs map[string]bool) []string {
	var out []string
	for id := range pgIDs {
		if !extIDs[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ---- Store 侧的对账原语 ----

// dialogueChunkRunIDs 枚举 PG 侧应有的 run_id 集合（dialogue_chunks 全表，keyset 分页）。
// 超过 maxRuns 截断（truncated=true）。
func (s *Store) dialogueChunkRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error) {
	out := make(map[string]bool)
	after := ""
	for {
		var ids []string
		if err := s.db.WithContext(ctx).Raw(
			`SELECT run_id FROM dialogue_chunks WHERE run_id > ? ORDER BY run_id LIMIT 500`,
			after).Scan(&ids).Error; err != nil {
			return nil, false, err
		}
		for _, id := range ids {
			out[id] = true
			after = id
		}
		if len(ids) < 500 || len(out) >= maxRuns {
			break
		}
	}
	return out, len(out) >= maxRuns, nil
}

// dialogueChunkCount 计数 dialogue_chunks 总行数（PG 应存在的文档数）。
func (s *Store) dialogueChunkCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.WithContext(ctx).Raw(`SELECT count(*) FROM dialogue_chunks`).Scan(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// dialogueHealAction 是一次缺失收敛的动作分类（对账水位用）。
type dialogueHealAction string

const (
	dialogueHealReset      dialogueHealAction = "reset"      // done 但外部缺失 → 重拨 pending
	dialogueHealRegistered dialogueHealAction = "registered" // 无任务行 → 新登记
	dialogueHealInflight   dialogueHealAction = "inflight"   // pending/running 在途，不动
	dialogueHealDeadLetter dialogueHealAction = "deadletter" // failed 死信：契约不动，计数上报
	dialogueHealChunkGone  dialogueHealAction = "chunk_gone" // 复查时 chunk 已不在（转孤儿路径）
)

// dialogueRescheduleMissing 让「PG 有 chunk、外部无文档」的 run 重新走既有投递链：
//
//   - 无任务行（hybrid 开启前落库的 chunk / 登记丢失）：按 chunk+runs 归属登记
//     两条目标任务（registerDialogueIndexJobs 的唯一约束回读把兄弟 target 配到同一
//     event_id），行状态 pending → relay 投递 → 消费者领取，与业务登记同一条路；
//   - 行 done：任务完成过但外部副本回退了（文档被删/索引被重建）→ 重拨 pending
//     并清 published_at（relay 重新投递）；
//   - 行 pending/running：在途（消费中 / 退避中）→ 不动，交给既有重试收敛；
//   - 行 failed：永久死信。§6.2 的契约是「不无限重试」——对账每 5 分钟重拨会让
//     死信每轮白烧一次外部调用，破坏死信的可告警性，所以保持不动，只在水位里
//     计数（人工处理或走全量重建批量修复）。
func (s *Store) dialogueRescheduleMissing(ctx context.Context, runID string, version int64, target string) (dialogueHealAction, error) {
	job, err := readDialogueIndexJob(ctx, s.db, DialogueIndexEventSpec{
		RunID: RunID(runID), Kind: DialogueJobKindUpsert,
	}, target, version)
	if err != nil {
		return "", err
	}
	if job != nil {
		switch job.Status {
		case DialogueJobStatusDone:
			return dialogueHealReset, s.resetDialogueDoneJob(ctx, job.ID)
		case DialogueJobStatusFailed:
			return dialogueHealDeadLetter, nil
		default:
			return dialogueHealInflight, nil
		}
	}
	own, err := s.dialogueHealOwnership(ctx, runID)
	if err != nil {
		return "", err
	}
	if own == nil {
		// chunk 在枚举后到现在之间被删了：外部那份由孤儿路径清理，这里不登记。
		return dialogueHealChunkGone, nil
	}
	if _, err := registerDialogueIndexJobs(ctx, s.db, DialogueIndexEventSpec{
		RunID:          RunID(runID),
		SessionID:      SessionID(own.SessionID),
		TenantID:       own.TenantID,
		UserID:         own.UserID,
		WorkspaceID:    own.WorkspaceID,
		Kind:           DialogueJobKindUpsert,
		EmbeddingModel: s.dialogueRetrieval.EmbeddingModel,
	}); err != nil {
		return "", err
	}
	return dialogueHealRegistered, nil
}

// resetDialogueDoneJob 把 done 行重拨回 pending（清 published_at/done_at/attempts/
// last_error），relay 会重新投递。WHERE status='done' 守卫：与并发的写回竞争时
// 不动非 done 行（消费者可能刚好又处理了一遍）。
func (s *Store) resetDialogueDoneJob(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Exec(`UPDATE dialogue_index_jobs SET
		  status = 'pending', attempts = 0, next_attempt_at = now(),
		  published_at = NULL, done_at = NULL, lease_until = NULL, last_error = '', updated_at = now()
		WHERE id = ? AND status = 'done'`, id).Error
}

// dialogueHealOwnership 取补投登记所需的归属（chunk 行 + runs.workspace_id 项目 id）。
// 归属真源是业务行，绝不用 dialogue_chunks.workspace（那是引擎作业根路径）。
// chunk 已不在返回 nil。
func (s *Store) dialogueHealOwnership(ctx context.Context, runID string) (*dialogueOwnershipRow, error) {
	var rows []dialogueOwnershipRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT c.run_id, c.session_id, c.tenant_id, c.user_id,
		       COALESCE(r.workspace_id, '') AS workspace_id
		  FROM dialogue_chunks c
		  LEFT JOIN runs r ON r.id = c.run_id
		 WHERE c.run_id = ?`, runID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// dialogueOwnershipRow 是对账补投/重建回放共用的归属+正文行。
type dialogueOwnershipRow struct {
	RunID       string
	SessionID   string
	TenantID    string
	UserID      string
	WorkspaceID string
	Passage     string
	IndexedAt   time.Time
	FinishedAt  *time.Time
}

// registerDialogueOrphanDeletes 给孤儿（外部有、PG 无）按 run_id 登记 kind=delete
// 任务（两条目标任务配对），走既有 delete 路径：relay → 消费者 → 客户端删除。
// delete 任务的功能字段只有 run_id（消费者只按 run_id 删）；PG 正本已无，
// 归属字段留空。登记前复查一次 chunk 是否刚写入，避免误删真数据（枚举与
// 登记之间有时间窗）。
func (s *Store) registerDialogueOrphanDeletes(ctx context.Context, runIDs []string) (int, int, error) {
	registered, skipped := 0, 0
	for _, runID := range runIDs {
		var exists int64
		if err := s.db.WithContext(ctx).Raw(
			`SELECT count(*) FROM dialogue_chunks WHERE run_id = ?`, runID).Scan(&exists).Error; err != nil {
			return registered, skipped, err
		}
		if exists > 0 {
			skipped++
			continue
		}
		if _, err := registerDialogueIndexJobs(ctx, s.db, DialogueIndexEventSpec{
			RunID: RunID(runID),
			Kind:  DialogueJobKindDelete,
		}); err != nil {
			return registered, skipped, err
		}
		registered++
	}
	return registered, skipped, nil
}
