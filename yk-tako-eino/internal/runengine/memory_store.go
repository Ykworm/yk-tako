package runengine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 本文件是 pipeline 记忆的**数据层核心**（doc/65 §4.1–§4.3）：
//
//   - 可见性谓词：统一一处（memoryVisibilitySQL），普通查询不许各自散落 workspace 条件。
//   - 读口：只打视图（resident_notes / recall_items），不许直接查基表挑 layer。
//   - 写口：统一 Store 方法（WriteMemoryItems / Supersede / Remove / Archive），
//     状态只进不复活，历史行的 content/quote 不原地改。
//   - 证据时间：按 source_run_id + source_seq 查 runlog_entries.created_at，
//     **不取 runs.finished_at**（inrun 写入时 run 尚未完成；并发 run 的完成顺序
//     也不等于用户说话顺序，doc/65 §4.2）。
//   - 索引与视图：EnsureMemoryIndexes 在启动装配期建（main.go 调用）。
//
// 写口的生产调用方是 pipeline 抽取器（memory_pipeline.go 的 applyActions）。

// memoryVisibilitySQL 是可见性谓词的**唯一出处**（doc/65 §0）：
//
//	tenant_id = :tenant AND ((scope = 'user' AND user_id = :user)
//	                      OR (scope = 'workspace' AND workspace = :workspace))
//
// :workspace 取 WorkspaceRef.WorkspaceID（**项目 id**，不是引擎工作区根路径）。
// scope='user' 的行跨项目可见（只按 user_id 判），scope='workspace' 的行只在本项目可见。
//
// 返回的 SQL 片段带三个占位符，参数顺序固定：tenant、user、workspace。
func memoryVisibilitySQL() string {
	return `(tenant_id = ? AND ((scope = 'user' AND user_id = ?) OR (scope = 'workspace' AND workspace = ?)))`
}

// memoryVisibilityArgs 按 principal 与工作区取出可见性谓词的三个参数。
// 没有身份（单测/旧路径）时 tenant/user 为空串——上层据此决定是否加过滤。
func memoryVisibilityArgs(ctx context.Context) (tenantID, userID, workspaceID string) {
	p, _ := principalFromCtx(ctx)
	tenantID, userID = p.TenantID, p.UserID
	// workspaceFrom 的 ok 只看 Root；这里要的是 WorkspaceID（项目 id），所以拿 ref 本身。
	if ref, _ := workspaceFrom(ctx); ref.WorkspaceID != "" {
		workspaceID = ref.WorkspaceID
	}
	return tenantID, userID, workspaceID
}

// ListVisibleMemoryItems 是**应用读口**：按层读视图（resident_notes / recall_items），
// 只返回 state='active' 且未过期的行，按 created_at DESC、id DESC 排序。
//
// layer 取 "resident" 或 "recall"；空值 = 两层都读（UNION ALL，一次查询）。
// 可见性谓词统一走 memoryVisibilitySQL——普通查询不许自己写 workspace 条件。
//
// 常驻段渲染（notes.go 的 renderResidentMemorySnapshot）与 BM25 召回都以它为入口。
func (s *Store) ListVisibleMemoryItems(ctx context.Context, layer string, limit int) ([]MemoryItem, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	tenantID, userID, workspaceID := memoryVisibilityArgs(ctx)
	vis := memoryVisibilitySQL()

	// 两层各打自己的视图；WHERE 片段对两层一样，只拼一次。
	const activeClause = ` AND state = 'active' AND (expires_at IS NULL OR expires_at > now())`
	orderBy := ` ORDER BY created_at DESC, id DESC LIMIT ?`

	switch layer {
	case MemoryLayerResident:
		var rows []MemoryItem
		err := s.db.WithContext(ctx).Raw(
			`SELECT * FROM resident_notes WHERE `+vis+activeClause+orderBy,
			tenantID, userID, workspaceID, limit).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		return rows, nil
	case MemoryLayerRecall:
		var rows []MemoryItem
		err := s.db.WithContext(ctx).Raw(
			`SELECT * FROM recall_items WHERE `+vis+activeClause+orderBy,
			tenantID, userID, workspaceID, limit).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		return rows, nil
	default:
		// 两层都要：UNION ALL 一次查完，排序在合并后做。
		var rows []MemoryItem
		err := s.db.WithContext(ctx).Raw(
			`SELECT * FROM (
			   SELECT * FROM resident_notes WHERE `+vis+activeClause+`
			   UNION ALL
			   SELECT * FROM recall_items WHERE `+vis+activeClause+`
			 ) m ORDER BY created_at DESC, id DESC LIMIT ?`,
			tenantID, userID, workspaceID, tenantID, userID, workspaceID, limit).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		return rows, nil
	}
}

// MemoryWriteResult 是一次写入的结果：落库条数、被拒条数与写进去的 id（按写入序）。
type MemoryWriteResult struct {
	Written  int
	Rejected int
	IDs      []string
}

// WriteMemoryItems 是记忆行的**写入口**（抽取器与工具写入都走这里；写基表，不打视图）。
//
// 规则（doc/65 §4.2/§4.3）：
//   - 不变量 3（doc/65 §4.3 收窄版）：**禁物理删、禁就地改正文**——改口插新行并建立
//     superseded_by 指针，删除写 state=removed 留痕；state 列迁移不算改写；
//   - **不做正文去重**：真实的 A→B→A 改口必须能产生新历史行；
//   - 初始 state='active'，created_at/updated_at 取提交时间；
//   - left_active_at 此刻为空——它只在**首次**离开 active 时写一次。
//
// 入库前两道机械闸（都不依赖模型判断）：
//   - PII（doc/41 §3.2）：密钥类（api_key / secret）**整条拒收**，证件类打码后入库；
//   - 隐形字符与上下文块标记：记忆正文会被注入模型提示，含零宽字符或
//     <memory-context> 标记的行一律拒收（不让记忆块伪造边界）。
func (s *Store) WriteMemoryItems(ctx context.Context, items []MemoryItem) (MemoryWriteResult, error) {
	var res MemoryWriteResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		res, err = s.writeMemoryItemsWith(ctx, tx, items)
		return err
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// writeMemoryItemsWith 是写口的本体（**可以跑在调用方的事务里**）。
//
// 抽取器的 writeMemory 事务要把「写新行 + 把旧行标 superseded + 记批次」放在**同一笔事务**里
// （doc/65 §4.8：批内动作全部成功或全部回滚），所以写口要能接受外部事务句柄。
func (s *Store) writeMemoryItemsWith(ctx context.Context, tx *gorm.DB, items []MemoryItem) (MemoryWriteResult, error) {
	var res MemoryWriteResult
	{
		for _, item := range items {
			if scanNoteInvisible(item.Content) != "" || scanNoteInvisible(item.Quote) != "" {
				res.Rejected++
				continue
			}
			if strings.Contains(item.Content, "<memory-context") || strings.Contains(item.Content, "</memory-context") ||
				strings.Contains(item.Content, "<dialogue-context") {
				res.Rejected++
				continue
			}
			masked, hits := MaskPII(item.Content)
			if hasSecretPII(hits) {
				res.Rejected++
				continue
			}
			item.Content = masked
			if item.Quote != "" {
				qmasked, qhits := MaskPII(item.Quote)
				if hasSecretPII(qhits) {
					res.Rejected++
					continue
				}
				item.Quote = qmasked
			}
			if item.ID == "" {
				id, err := newRowID("mem_")
				if err != nil {
					return res, err
				}
				item.ID = id
			}
			if item.State == "" {
				item.State = MemoryStateActive
			}
			if item.Layer == "" {
				item.Layer = MemoryLayerRecall
			}
			if item.Scope == "" {
				item.Scope = MemoryScopeUser
			}
			if item.Source == "" {
				item.Source = MemorySourceExtracted
			}
			now := time.Now()
			if item.CreatedAt.IsZero() {
				item.CreatedAt = now
			}
			item.UpdatedAt = now
			if err := tx.Create(&item).Error; err != nil {
				return res, err
			}
			res.Written++
			res.IDs = append(res.IDs, item.ID)
		}
	}
	return res, nil
}

// SupersedeMemoryItem 把一行 active 记忆标成 superseded，并指向替代它的新行。
//
// 只进不复活：只有 state='active' 的行能被改；已经 superseded/removed/archived 的行不动。
// left_active_at 只在这里写一次（COALESCE：已写过的不覆盖）。
// source='user' 的行**不**被程序整理替代（doc/65 §4.2）：这类行由用户自己改口，
// 程序不得合并它——所以这里对 source='user' 不生效（0 行受影响）。
func (s *Store) SupersedeMemoryItem(ctx context.Context, id, supersededBy string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_items
		    SET state = 'superseded', superseded_by = ?,
		        left_active_at = COALESCE(left_active_at, now()), updated_at = now()
		  WHERE id = ? AND state = 'active' AND source <> 'user'`,
		supersededBy, id).Error
}

// RemoveMemoryItem 把一行 active 记忆标成 removed（用户明确删除）。
//
// removed_at 只服务用户明确删除这一条路径；left_active_at 与别处一样只写一次。
// source='user' 的行**可以**被删——用户自己说要删，程序不拦。
func (s *Store) RemoveMemoryItem(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_items
		    SET state = 'removed', removed_at = now(),
		        left_active_at = COALESCE(left_active_at, now()), updated_at = now()
		  WHERE id = ? AND state = 'active'`,
		id).Error
}

// ArchiveMemoryItem 把一行 active 记忆标成 archived（过期或程序整理退场）。
//
// 同样只进不复活；source='user' 的行不被程序整理淘汰（同 SupersedeMemoryItem）。
func (s *Store) ArchiveMemoryItem(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_items
		    SET state = 'archived',
		        left_active_at = COALESCE(left_active_at, now()), updated_at = now()
		  WHERE id = ? AND state = 'active' AND source <> 'user'`,
		id).Error
}

// MemoryEvidenceTime 查一条记忆的**证据时间**：source_run_id + source_seq
// 对应的 runlog_entries.created_at。
//
// 不取 runs.finished_at：inrun 写入时来源 run 还没完成（finished_at 为空），
// 且并发 run 的完成顺序不等于用户说话顺序。同一 run 内用 source_seq 排序。
//
// 查不到行时返回零值与 false——调用方据此退回 created_at（提交时间），
// 但那只能当兜底，不能反推「哪句话先说」。
func (s *Store) MemoryEvidenceTime(ctx context.Context, sourceRunID RunID, sourceSeq int64) (time.Time, bool, error) {
	return memoryEvidenceTimeDB(s.db.WithContext(ctx), sourceRunID, sourceSeq)
}

// memoryEvidenceTimeDB 是 MemoryEvidenceTime 的**事务内版本**：写事务里的证据新旧
// 校验必须与行变化同快照（db 传事务句柄或 *gorm.DB 均可）。
func memoryEvidenceTimeDB(db *gorm.DB, sourceRunID RunID, sourceSeq int64) (time.Time, bool, error) {
	if sourceRunID == "" || sourceSeq <= 0 {
		return time.Time{}, false, nil
	}
	var t time.Time
	err := db.Raw(
		`SELECT created_at FROM runlog_entries
		  WHERE run_id = ? AND seq = ? ORDER BY created_at LIMIT 1`,
		sourceRunID, sourceSeq).Scan(&t).Error
	if err != nil {
		return time.Time{}, false, err
	}
	if t.IsZero() {
		return time.Time{}, false, nil
	}
	return t, true, nil
}

// CompareMemoryEvidence 比较两行记忆的**证据先后**：返回 1 表示 a 比 b 新，-1 表示 b 更新，
// 0 表示无法判断（证据缺失或同一时刻）。
//
// 判据是证据时间（runlog_entries.created_at），**不是提交时间 created_at**：
// 并发 run 的完成顺序不等于用户说话顺序（doc/65 §4.2）。
// 同一 run 内用 source_seq 排序。证据缺失时退回提交时间并按 id 兜底——
// 那是兜底，不是「谁先说」的结论。
func (s *Store) CompareMemoryEvidence(ctx context.Context, a, b MemoryItem) (int, error) {
	at, aok, err := s.MemoryEvidenceTime(ctx, a.SourceRunID, a.SourceSeq)
	if err != nil {
		return 0, err
	}
	bt, bok, err := s.MemoryEvidenceTime(ctx, b.SourceRunID, b.SourceSeq)
	if err != nil {
		return 0, err
	}
	switch {
	case aok && bok:
		if at.After(bt) {
			return 1, nil
		}
		if bt.After(at) {
			return -1, nil
		}
		if a.SourceRunID == b.SourceRunID && a.SourceSeq != b.SourceSeq {
			if a.SourceSeq > b.SourceSeq {
				return 1, nil
			}
			return -1, nil
		}
		return 0, nil
	case aok != bok:
		// 只有一边有证据：有证据的那边更可信（它的说话时刻是实证的）。
		if aok {
			return 1, nil
		}
		return -1, nil
	default:
		// 两边都没证据：退回提交时间，再按 id 兜底。
		if a.CreatedAt.After(b.CreatedAt) {
			return 1, nil
		}
		if b.CreatedAt.After(a.CreatedAt) {
			return -1, nil
		}
		if a.ID > b.ID {
			return 1, nil
		}
		if a.ID < b.ID {
			return -1, nil
		}
		return 0, nil
	}
}

// MemoryJobSpec 是登记一条记忆任务的入参（EndRun 与 memory 工具派工共用）。
type MemoryJobSpec struct {
	RunID       RunID
	SessionID   SessionID
	TenantID    string
	UserID      string
	Workspace   string
	Kind        string  // inrun | postrun | tidy
	TargetLayer string  // resident | recall（postrun 固定 recall，tidy 可空）
	Quote       string  // 用户原话
	SourceSeq   int64   // 原话在 runlog 里的 seq
	TraceParent string  // trace 透传串
	DedupKey    *string // 幂等键（可空；同键重复登记复用同一行）
}

// RegisterMemoryJob 登记一条记忆任务（memory_jobs 写口）。
//
// dedup_key 可空、唯一：同一 dedup_key 重复登记返回**已有的 job id**（不新增行），
// 调用方据此判定「这单已经登记过了」。dedup_key 为空时每次都新登记一行。
//
// kind 取 inrun / postrun / tidy；target_layer 只对 inrun 有意义
// （postrun 固定 recall，tidy 不晋升层）。
func (s *Store) RegisterMemoryJob(ctx context.Context, spec MemoryJobSpec) (string, error) {
	return s.registerMemoryJob(ctx, s.db, spec)
}

// registerMemoryJob 是登记写口的本体。db 可以是**事务句柄**——EndRun 的 postrun 登记
// 要求「与 run 定稿同一笔事务」（doc/65 §4.5：run 定稿 ⇒ 待抽任务一定存在；
// 进程在 COMMIT 后崩溃也有补偿线索），所以那里不能走独立连接。
func (s *Store) registerMemoryJob(ctx context.Context, db *gorm.DB, spec MemoryJobSpec) (string, error) {
	job := MemoryJob{
		RunID:       spec.RunID,
		SessionID:   spec.SessionID,
		TenantID:    spec.TenantID,
		UserID:      spec.UserID,
		Workspace:   spec.Workspace,
		Kind:        spec.Kind,
		Quote:       spec.Quote,
		SourceSeq:   spec.SourceSeq,
		TraceParent: spec.TraceParent,
		DedupKey:    spec.DedupKey,
	}
	if spec.TargetLayer != "" {
		tl := spec.TargetLayer
		job.TargetLayer = &tl
	}
	if job.ID == "" {
		id, err := newRowID("mj_")
		if err != nil {
			return "", err
		}
		job.ID = id
	}
	if job.Status == "" {
		job.Status = MemoryJobStatusPending
	}
	if job.Kind == "" {
		job.Kind = MemoryJobKindPostrun
	}
	if job.NextAttemptAt.IsZero() {
		job.NextAttemptAt = time.Now()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	job.UpdatedAt = job.CreatedAt

	// dedup_key 有值时走 ON CONFLICT：重复登记不新增行，返回原行 id。
	// 索引是部分唯一索引（WHERE dedup_key IS NOT NULL），所以 ON CONFLICT 的推断条件
	// **必须带上同一个谓词**——PG 不会用不带谓词的形式去推断部分索引（本机实测报 42P10）。
	if job.DedupKey != nil && *job.DedupKey != "" {
		err := db.WithContext(ctx).Exec(
			`INSERT INTO memory_jobs (id, run_id, session_id, tenant_id, user_id, workspace,
			    kind, target_layer, quote, source_seq, trace_parent, status, attempts,
			    next_attempt_at, claim_epoch, dedup_key, last_error, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, '', ?, ?)
			 ON CONFLICT (dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING`,
			job.ID, job.RunID, job.SessionID, job.TenantID, job.UserID, job.Workspace,
			job.Kind, job.TargetLayer, job.Quote, job.SourceSeq, job.TraceParent, job.Status,
			job.NextAttemptAt, *job.DedupKey, job.CreatedAt, job.UpdatedAt).Error
		if err != nil {
			return "", err
		}
		var existing string
		err = db.WithContext(ctx).Raw(
			`SELECT id FROM memory_jobs WHERE dedup_key = ?`, *job.DedupKey).Scan(&existing).Error
		if err != nil {
			return "", err
		}
		if existing != "" {
			return existing, nil
		}
		return job.ID, nil
	}

	err := db.WithContext(ctx).Create(&job).Error
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// ClaimMemoryJob 领取一条待处理任务：pending → running。
//
// claim 时 claim_epoch+1、attempts+1，并写 lease_until（崩溃后别的副本按它捡回）。
// 返回带 claim_epoch 的任务快照——**后续写回必须带同一个 claim_epoch**：
// 旧 epoch 的写一律写不进去（见 CompleteMemoryJob / FailMemoryJob）。
func (s *Store) ClaimMemoryJob(ctx context.Context, kind string) (*MemoryJob, error) {
	var job MemoryJob
	err := s.db.WithContext(ctx).Raw(
		`UPDATE memory_jobs SET status = 'running', attempts = attempts + 1,
		        claim_epoch = claim_epoch + 1,
		        lease_until = now() + make_interval(secs => ?), updated_at = now()
		  WHERE id = (
		    SELECT id FROM memory_jobs
		     WHERE status = 'pending' AND next_attempt_at <= now()
		       AND (? = '' OR kind = ?)
		     ORDER BY next_attempt_at, created_at
		     LIMIT 1 FOR UPDATE SKIP LOCKED)
		 RETURNING id, run_id, session_id, tenant_id, user_id, workspace, kind, target_layer,
		           quote, source_seq, trace_parent, status, attempts, claim_epoch, lease_until,
		           next_attempt_at, dedup_key, created_at, updated_at`,
		memoryJobLease.Seconds(), kind, kind).Scan(&job).Error
	if err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, nil
	}
	return &job, nil
}

// CompleteMemoryJob 把一条 running 任务翻成 done。
//
// **旧 claim_epoch 不能翻 done**：WHERE 里带 claim_epoch = ?，
// 对不上就一行都不改（被别的副本重新 claim 过的旧 attempt 迟到不改状态）。
func (s *Store) CompleteMemoryJob(ctx context.Context, id string, claimEpoch int64) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs SET status = 'done', lease_until = NULL, last_error = '', updated_at = now()
		  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
		id, claimEpoch).Error
}

// FailMemoryJob 记一次失败：running → pending（回退重试，写 last_error 与 next_attempt_at）
// 或 running → failed（attempts 用尽）。
//
// 退避按**本行的 attempts** 走 1 / 5 / 30 分钟（doc/65 §4.6）：attempts=1（第一次失败）
// → 1 分钟，attempts=2 → 5 分钟，attempts=3 → failed。用 SQL 数组按下标取，
// 不需要调用方传次数（调用方手里的 attempts 可能已过期）。
//
// **旧 claim_epoch 不能覆盖错误**：同 CompleteMemoryJob，WHERE 带 claim_epoch = ?——
// 对不上的旧 attempt 既翻不了状态，也覆盖不了 last_error。
func (s *Store) FailMemoryJob(ctx context.Context, id string, claimEpoch int64, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if len(memoryRetryBackoff) != 3 {
		panic("memoryRetryBackoff 必须是 1/5/30 分钟三档（FailMemoryJob 的 SQL 按下标取）")
	}
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs
		    SET status = CASE WHEN attempts >= ? THEN 'failed' ELSE 'pending' END,
		        lease_until = NULL,
		        last_error = ?,
		        next_attempt_at = CASE WHEN attempts >= ? THEN next_attempt_at
		                              ELSE now() + make_interval(secs =>
		                                    (ARRAY[?::double precision, ?::double precision, ?::double precision])[attempts]) END,
		        updated_at = now()
		  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
		memoryMaxAttempts, msg, memoryMaxAttempts,
		memoryRetryBackoff[0].Seconds(), memoryRetryBackoff[1].Seconds(), memoryRetryBackoff[2].Seconds(),
		id, claimEpoch).Error
}

// memorySearchIndexDDL 是 memory_items 的 BM25 索引（doc/65 §4.4）。
//
// 逐字按主 Agent 2026-11-09 在本机 PG（pg_search 0.26.2）上实跑验证过的语法：
// 用 **paradedb**（不是 bm25）访问方法；键列 id 必须写在第一位；
// text_fields 指定 chinese_lindera 分词器（中文按 Lindera 切）。
// 仓库既有先例：dialogue.go 的 dialogueSearchIndexDDL。
const memorySearchIndexDDL = `CREATE INDEX IF NOT EXISTS memory_items_search_idx
ON memory_items USING paradedb (id, content)
WITH (text_fields = '{"content": {"tokenizer": {"type": "chinese_lindera"}}}')`

// memoryTokenizerJSON 是查询侧（BM25 召回、词集校验）的分词器声明——必须与上面索引 DDL
// 里的一致，否则查询切词与索引切词不同、词面命中失真。
const memoryTokenizerJSON = `{"type": "chinese_lindera"}`

// ensureMemoryViews 建两个读视图（doc/65 §4.1）：
//
//	resident_notes = layer='resident' 的行；recall_items = layer='recall' 的行。
//
// 普通应用读取只打视图，不直接查基表挑 layer；写事务由统一 Store 方法访问基表。
// 幂等：先 DROP 再 CREATE（CREATE OR REPLACE 在列集变化时会失败）。
func ensureMemoryViews(db *gorm.DB) error {
	for _, v := range []string{"resident_notes", "recall_items"} {
		if err := db.Exec(`DROP VIEW IF EXISTS ` + v).Error; err != nil {
			return err
		}
	}
	if err := db.Exec(`CREATE VIEW resident_notes AS SELECT * FROM memory_items WHERE layer = 'resident'`).Error; err != nil {
		return err
	}
	return db.Exec(`CREATE VIEW recall_items AS SELECT * FROM memory_items WHERE layer = 'recall'`).Error
}

// EnsureMemoryIndexes 在启动装配期建 pipeline 记忆的视图与索引（doc/65 §4.1/§4.4）。
//
// 由 cmd/yktako/main.go 在启动装配期调用（与 EnsureDialogueIndex 同一层）。建的是：
//   - 两个视图：resident_notes（layer='resident'）、recall_items（layer='recall'）；
//   - 普通索引：常驻快照读取、状态筛选（可见域与 superseded_by 由 GORM tag 建）。
//   - BM25 索引：memory_items_search_idx（pg_search 的 paradedb 访问方法）。
//
// BM25 索引需要 pg_search 扩展已安装；缺失时返回错误由调用方决定退出
// （与 EnsureDialogueIndex 的 fail fast 一致，但这里不自己 log.Fatal）。
func (s *Store) EnsureMemoryIndexes(ctx context.Context) error {
	db := s.db.WithContext(ctx)

	if err := ensureMemoryViews(db); err != nil {
		return err
	}

	// 普通索引（幂等）：
	//   - 常驻快照读取：常驻层按 tenant + user 读，按 created_at 倒序取。
	//   - 状态筛选：默认读只认 active。
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS memory_items_resident_read ON memory_items (tenant_id, user_id, created_at DESC) WHERE layer = 'resident'`,
		`CREATE INDEX IF NOT EXISTS memory_items_state_idx ON memory_items (state)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			return err
		}
	}

	// BM25 索引：pg_search 未预加载 / 扩展缺失 → 明确报错（调用方 fail fast）。
	// 错误文案不建议 TAKO_RAG=off——那只控制 dialogue_chunks，与记忆索引无关（施工包 §5.1）。
	var preload string
	if err := db.Raw(`SELECT current_setting('shared_preload_libraries')`).Scan(&preload).Error; err != nil {
		return err
	}
	loaded := false
	for _, p := range strings.Split(preload, ",") {
		if strings.TrimSpace(p) == "pg_search" {
			loaded = true
			break
		}
	}
	if !loaded {
		return fmt.Errorf("pg_search 未预加载（shared_preload_libraries=%q）：给 PG 加 shared_preload_libraries='pg_search' 并重启", preload)
	}
	var ext int
	if err := db.Raw(`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&ext).Error; err != nil {
		return err
	}
	if ext == 0 {
		return fmt.Errorf("pg_search 扩展未安装：psql -p <port> -d <db> -c 'CREATE EXTENSION pg_search CASCADE'（依赖 vector）")
	}
	return db.Exec(memorySearchIndexDDL).Error
}
