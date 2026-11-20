package runengine

// 本文件是历史对话混合检索的 outbox 核心（doc/84 §4.2/§4.3）：目标任务表
// dialogue_index_jobs 与版本指针 dialogue_retrieval_pointer 的模型、登记、领取、写回。
//
// 与 memory_jobs（memory_store.go）同一套纪律，三个不变量全部落在数据库：
//
//	登记唯一：UNIQUE(event_id,target) + UNIQUE(run_id,kind,index_version,target)，
//	          重复登记回读原行——插入失败本身就是闸，不靠进程内锁；
//	领取互斥：FOR UPDATE SKIP LOCKED 领取到期 pending 与租约过期的 running，
//	          两个副本同时领同一行只有一个成功；
//	写回守卫：完成/失败/续租必须匹配 id + claim_epoch，旧代次的写一行都改不了。
//
// 一个业务事件（run 完成 / 项目删除 / 会话移动）登记**两条**目标任务（target=es 与
// target=milvus），共用 event_id，两边独立领取、独立重试、独立完成；两个目标都 done
// 后事件才算业务完成（单边成功只补另一边——relay 对 published_at 缺失的行重新投递）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 任务词表（doc/84 §4.2；CHECK 约束在 autoMigrateLocked 落库，词表固定）。
const (
	DialogueJobStatusPending = "pending"
	DialogueJobStatusRunning = "running"
	DialogueJobStatusDone    = "done"
	DialogueJobStatusFailed  = "failed"

	DialogueTargetES     = "es"
	DialogueTargetMilvus = "milvus"

	DialogueJobKindUpsert = "upsert"
	DialogueJobKindDelete = "delete"
	DialogueJobKindMove   = "move"
)

// 任务节奏（doc/84 §4.2/§6.2，写死不是配置）：租约 2 分钟（处理中续约），
// 退避 1/5/30 分钟，第三次失败进 failed 持久死信。
var (
	dialogueJobLease     = 2 * time.Minute
	dialogueMaxAttempts  = 3
	dialogueRetryBackoff = [3]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}
)

// DialogueIndexJob = `dialogue_index_jobs`（doc/84 §4.2）：一个业务事件的**单目标**任务。
// workspace_id 存项目 id（归属真源）；dialogue_chunks.workspace 存引擎作业根路径——
// 两者语义不同，消费者回读归属一律以任务行为准，不读 chunk 行的 workspace 列。
type DialogueIndexJob struct {
	ID      string `gorm:"primaryKey;size:64"`
	EventID string `gorm:"not null;size:64;uniqueIndex:dialogue_index_jobs_event_target,priority:1"`
	// target 与 (run_id,kind,index_version) 同在一个复合唯一索引里：
	// 同一版本、同一目标的重复登记回读原任务（doc/84 §4.2 不变量）。
	Target       string `gorm:"not null;size:16;uniqueIndex:dialogue_index_jobs_event_target,priority:2;uniqueIndex:dialogue_index_jobs_run_kind_ver_target,priority:4"`
	RunID        string `gorm:"not null;size:64;uniqueIndex:dialogue_index_jobs_run_kind_ver_target,priority:1"`
	Kind         string `gorm:"not null;size:16;uniqueIndex:dialogue_index_jobs_run_kind_ver_target,priority:2"`
	IndexVersion int64  `gorm:"not null;uniqueIndex:dialogue_index_jobs_run_kind_ver_target,priority:3"`
	SessionID    string `gorm:"not null;default:'';size:64"`
	TenantID     string `gorm:"not null;default:'';size:64"`
	UserID       string `gorm:"not null;default:'';size:64"`
	WorkspaceID  string `gorm:"not null;default:'';size:64"`
	// embedding_model：本任务模型；delete 任务可为空（不产生向量）。
	EmbeddingModel string `gorm:"not null;default:'';size:256"`
	Status         string `gorm:"not null;default:pending;size:16;index:dialogue_index_jobs_claim,priority:1"`
	Attempts       int    `gorm:"not null;default:0"`
	ClaimEpoch     int64  `gorm:"not null;default:0"`
	LeaseUntil     *time.Time
	NextAttemptAt  time.Time `gorm:"not null;index:dialogue_index_jobs_claim,priority:2"`
	PublishedAt    *time.Time
	DoneAt         *time.Time
	// last_error 只存机器可读错误分类（如 es_unavailable），不含正文与密钥（施工包禁令）。
	LastError   string `gorm:"not null;default:''"`
	TraceParent string `gorm:"not null;default:''"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (DialogueIndexJob) TableName() string { return "dialogue_index_jobs" }

// DialogueRetrievalPointer = `dialogue_retrieval_pointer`（doc/84 §4.3）：单例行 id=1，
// 当前生效的 index_version 与 ES index / Milvus collection。全量重建（P3）先建新版本再
// 一笔事务切指针；登记与消费都以它为代次真源。
type DialogueRetrievalPointer struct {
	ID               int `gorm:"primaryKey"`
	IndexVersion     int64
	EsIndex          string `gorm:"not null;default:'';size:128"`
	MilvusCollection string `gorm:"not null;default:'';size:128"`
	UpdatedAt        time.Time
}

func (DialogueRetrievalPointer) TableName() string { return "dialogue_retrieval_pointer" }

// EnsureDialogueRetrievalPointer 装配期播种/对齐指针行（hybrid 启动时调用，幂等）：
// 行不存在以 version=1 播种；已存在则把 es_index/milvus_collection 对齐到当前配置
// （索引名是部署配置，版本号是代次真源——只在显式重建/清空时递增，P3 的活）。
func (s *Store) EnsureDialogueRetrievalPointer(ctx context.Context, esIndex, milvusCollection string) (int64, error) {
	if err := s.db.WithContext(ctx).Exec(
		`INSERT INTO dialogue_retrieval_pointer (id, index_version, es_index, milvus_collection, updated_at)
		 VALUES (1, 1, ?, ?, now()) ON CONFLICT (id) DO NOTHING`,
		esIndex, milvusCollection).Error; err != nil {
		return 0, err
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE dialogue_retrieval_pointer SET es_index = ?, milvus_collection = ?, updated_at = now() WHERE id = 1`,
		esIndex, milvusCollection).Error; err != nil {
		return 0, err
	}
	ptr, err := s.dialoguePointer(ctx)
	if err != nil {
		return 0, err
	}
	return ptr.IndexVersion, nil
}

// dialoguePointer 读指针行；行不存在是装配错误（hybrid 启动必先 EnsureDialogueRetrievalPointer）。
func (s *Store) dialoguePointer(ctx context.Context) (*DialogueRetrievalPointer, error) {
	var ptr DialogueRetrievalPointer
	if err := s.db.WithContext(ctx).Raw(
		`SELECT * FROM dialogue_retrieval_pointer WHERE id = 1`).Scan(&ptr).Error; err != nil {
		return nil, err
	}
	if ptr.ID == 0 {
		return nil, errors.New("dialogue_retrieval_pointer 未初始化（hybrid 装配未执行？）")
	}
	return &ptr, nil
}

// SwitchDialogueRetrievalPointer 版本化重建的收尾切换（doc/84 §6.4 第 4 拍）：
// 一笔事务里一条 UPDATE 换三个字段（index_version + es_index + milvus_collection）。
// 中途进程被 kill -9，事务未提交 → 指针要么旧要么新，不存在半新半旧（幂等恢复窗口）。
// 旧版本索引/collection 不删（保留回滚），由人工按打印的示例语句清理。
func (s *Store) SwitchDialogueRetrievalPointer(ctx context.Context, version int64, esIndex, milvusCollection string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return switchDialoguePointerRow(tx, version, esIndex, milvusCollection)
	})
}

// switchDialoguePointerRow 是切换的实质语句（拆出来让测试能在自己的事务里模拟
// 「语句已执行但事务回滚」的 kill -9 窗口）。
func switchDialoguePointerRow(db *gorm.DB, version int64, esIndex, milvusCollection string) error {
	res := db.Exec(
		`UPDATE dialogue_retrieval_pointer
		    SET index_version = ?, es_index = ?, milvus_collection = ?, updated_at = now()
		  WHERE id = 1`, version, esIndex, milvusCollection)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("dialogue_retrieval_pointer 未初始化，无法切换（hybrid 装配未执行？）")
	}
	return nil
}

// DialogueIndexEventSpec 是一个业务事件的登记规格；一次登记产出 target=es 与 target=milvus
// 两条目标任务（共用 event_id）。
type DialogueIndexEventSpec struct {
	RunID          RunID
	SessionID      SessionID
	TenantID       string
	UserID         string
	WorkspaceID    string // 项目 id（归属真源）；未挂项目 = ""
	Kind           string // upsert | delete | move
	EmbeddingModel string
	TraceParent    string
}

// RegisterDialogueIndexJobs 在独立连接上登记（测试用）；生产登记一律走事务句柄
// （indexDialogueRun / DeleteWorkspaceCascade / SetSessionWorkspace 传 tx）。
func (s *Store) RegisterDialogueIndexJobs(ctx context.Context, spec DialogueIndexEventSpec) (string, error) {
	return registerDialogueIndexJobs(ctx, s.db, spec)
}

// registerDialogueIndexJobs 在**调用方的事务里**登记两条目标任务（doc/84 §4.2）：
// 业务写（chunk 落库 / 项目删除 / 会话移动）与 outbox 登记同生同灭。
//
// 冲突语义（UNIQUE 撞行时回读原任务）：
//   - upsert / delete：纯回读——幂等，重登记不产生新行、不改状态；
//   - move：workspace_id 变了才动行——更新归属；若已终态（done/failed）则重置为
//     pending 并清 published_at（relay 会重新投递，处理 A→B→A 的来回移动）。
func registerDialogueIndexJobs(ctx context.Context, db *gorm.DB, spec DialogueIndexEventSpec) (string, error) {
	switch spec.Kind {
	case DialogueJobKindUpsert, DialogueJobKindDelete, DialogueJobKindMove:
	default:
		return "", fmt.Errorf("dialogue index job kind %q 不在词表内", spec.Kind)
	}
	ptr, err := dialoguePointerTx(ctx, db)
	if err != nil {
		return "", err
	}
	eventID := ""
	for _, target := range []string{DialogueTargetES, DialogueTargetMilvus} {
		eid, err := ensureDialogueIndexJob(ctx, db, spec, target, ptr.IndexVersion, eventID)
		if err != nil {
			return "", err
		}
		if eventID == "" {
			eventID = eid
		}
	}
	return eventID, nil
}

// dialoguePointerTx 与 dialoguePointer 同读，但接受事务句柄（登记在调用方事务里跑）。
func dialoguePointerTx(ctx context.Context, db *gorm.DB) (*DialogueRetrievalPointer, error) {
	var ptr DialogueRetrievalPointer
	if err := db.WithContext(ctx).Raw(
		`SELECT * FROM dialogue_retrieval_pointer WHERE id = 1`).Scan(&ptr).Error; err != nil {
		return nil, err
	}
	if ptr.ID == 0 {
		return nil, errors.New("dialogue_retrieval_pointer 未初始化（hybrid 装配未执行？）")
	}
	return &ptr, nil
}

// ensureDialogueIndexJob 保证某个 target 的任务行存在并返回它的 event_id。
// preferEventID 是第一个目标回读/生成的 event_id——两个目标的行必须共用它。
func ensureDialogueIndexJob(ctx context.Context, db *gorm.DB, spec DialogueIndexEventSpec, target string, version int64, preferEventID string) (string, error) {
	job, err := readDialogueIndexJob(ctx, db, spec, target, version)
	if err != nil {
		return "", err
	}
	if job != nil {
		// 冲突回读。move 是唯一带修正语义的 kind：归属变了要更新任务行
		// （终态行重置回 pending，relay 会重新投递）；upsert/delete 纯幂等回读。
		if spec.Kind == DialogueJobKindMove && job.WorkspaceID != spec.WorkspaceID {
			if err := applyDialogueMoveCorrection(ctx, db, job, spec.WorkspaceID); err != nil {
				return "", err
			}
		}
		return job.EventID, nil
	}
	eventID := preferEventID
	if eventID == "" {
		id, err := newRowID("evt_")
		if err != nil {
			return "", err
		}
		eventID = id
	}
	now := time.Now()
	row := DialogueIndexJob{
		ID:             "",
		EventID:        eventID,
		Target:         target,
		RunID:          string(spec.RunID),
		Kind:           spec.Kind,
		IndexVersion:   version,
		SessionID:      string(spec.SessionID),
		TenantID:       spec.TenantID,
		UserID:         spec.UserID,
		WorkspaceID:    spec.WorkspaceID,
		EmbeddingModel: spec.EmbeddingModel,
		Status:         DialogueJobStatusPending,
		NextAttemptAt:  now,
		TraceParent:    spec.TraceParent,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	jobID, err := newRowID("dij_")
	if err != nil {
		return "", err
	}
	row.ID = jobID
	if err := db.WithContext(ctx).Create(&row).Error; err != nil {
		if !isUniqueViolation(err) {
			return "", err
		}
		// 并发登记撞唯一约束（另一副本先插了）：回读原任务按冲突语义处理。
		raced, rerr := readDialogueIndexJob(ctx, db, spec, target, version)
		if rerr != nil {
			return "", rerr
		}
		if raced == nil {
			return "", fmt.Errorf("dialogue index job %s/%s 插入冲突但回读不到原行", spec.RunID, target)
		}
		return raced.EventID, nil
	}
	return eventID, nil
}

// readDialogueIndexJob 按 (run_id,kind,index_version,target) 回读已登记的任务行；
// 无行返回 (nil, nil)。
func readDialogueIndexJob(ctx context.Context, db *gorm.DB, spec DialogueIndexEventSpec, target string, version int64) (*DialogueIndexJob, error) {
	var job DialogueIndexJob
	err := db.WithContext(ctx).Raw(
		`SELECT * FROM dialogue_index_jobs
		  WHERE run_id = ? AND kind = ? AND index_version = ? AND target = ?`,
		string(spec.RunID), spec.Kind, version, target).Scan(&job).Error
	if err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, nil
	}
	return &job, nil
}

// applyDialogueMoveCorrection 处理 move 冲突：归属变了就更新任务行的 workspace_id；
// 已终态（done/failed）的行重置回 pending 并清 published_at（relay 重新投递），
// 清 attempts 让移动重试不受上一代次耗尽的次数拖累。
func applyDialogueMoveCorrection(ctx context.Context, db *gorm.DB, job *DialogueIndexJob, workspaceID string) error {
	return db.WithContext(ctx).Exec(
		`UPDATE dialogue_index_jobs SET
		   workspace_id = ?,
		   status = CASE WHEN status IN ('done','failed') THEN 'pending' ELSE status END,
		   attempts = CASE WHEN status IN ('done','failed') THEN 0 ELSE attempts END,
		   next_attempt_at = CASE WHEN status IN ('done','failed') THEN now() ELSE next_attempt_at END,
		   published_at = NULL,
		   lease_until = NULL,
		   done_at = NULL,
		   last_error = '',
		   updated_at = now()
		 WHERE id = ?`, workspaceID, job.ID).Error
}

// ── 消费者与 relay 用的存储原语 ──────────────────────────────────────────────

// getDialogueJob 读一条目标任务（无行返回 nil，不是错误）。
func (s *Store) getDialogueJob(ctx context.Context, id string) (*DialogueIndexJob, error) {
	var job DialogueIndexJob
	if err := s.db.WithContext(ctx).Raw(`SELECT * FROM dialogue_index_jobs WHERE id = ?`, id).Scan(&job).Error; err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, nil
	}
	return &job, nil
}

// getDialogueJobByEvent 按 (event_id,target) 读任务行——消费者拿到消息后回读的唯一坐标。
func (s *Store) getDialogueJobByEvent(ctx context.Context, eventID, target string) (*DialogueIndexJob, error) {
	var job DialogueIndexJob
	if err := s.db.WithContext(ctx).Raw(
		`SELECT * FROM dialogue_index_jobs WHERE event_id = ? AND target = ?`,
		eventID, target).Scan(&job).Error; err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, nil
	}
	return &job, nil
}

// claimDialogueJobsByIDs 认领一批**指定**的任务（消费者拿到消息批后调）。
//
// 认领条件与 memory_jobs 相同：pending 已到期，或 running 租约已过期——
// 不满足的行不在 RETURNING 里（重复消息与并行副本在这里被挡住）。
// 子查询带 FOR UPDATE SKIP LOCKED：两个副本的批次有重叠行时各捡各的、互不阻塞
// （闸放数据库，与 ClaimMemoryJob 同一机制）。
// 认领动作：status=running、attempts+1、claim_epoch+1、写 lease_until。
// 返回带新 claim_epoch 的快照，后续写回必须带同一个 epoch。
func (s *Store) claimDialogueJobsByIDs(ctx context.Context, ids []string, lease time.Duration) ([]*DialogueIndexJob, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var jobs []*DialogueIndexJob
	err := s.db.WithContext(ctx).Raw(
		`UPDATE dialogue_index_jobs SET status = 'running', attempts = attempts + 1,
		        claim_epoch = claim_epoch + 1,
		        lease_until = now() + make_interval(secs => ?), updated_at = now()
		  WHERE id IN (
		    SELECT id FROM dialogue_index_jobs
		     WHERE id IN ? AND (
		           (status = 'pending' AND next_attempt_at <= now()) OR
		           (status = 'running' AND lease_until IS NOT NULL AND lease_until < now()))
		     FOR UPDATE SKIP LOCKED)
		 RETURNING id, event_id, run_id, session_id, tenant_id, user_id, workspace_id, target, kind,
		           index_version, embedding_model, status, attempts, claim_epoch, lease_until,
		           next_attempt_at, published_at, done_at, last_error, trace_parent, created_at, updated_at`,
		lease.Seconds(), ids).Scan(&jobs).Error
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// dueDialogueJobs 对账扫描（只读不改）：target 下到期 pending + 租约过期的 running。
// 补投只发叫醒消息；能不能处理由认领条件与 claim_epoch 裁决。
func (s *Store) dueDialogueJobs(ctx context.Context, target string, limit int) ([]DialogueIndexJob, error) {
	var jobs []DialogueIndexJob
	err := s.db.WithContext(ctx).Raw(
		`SELECT id, event_id, run_id, session_id, tenant_id, user_id, workspace_id, target, kind,
		        index_version, trace_parent
		  FROM dialogue_index_jobs
		 WHERE target = ? AND (
		       (status = 'pending' AND next_attempt_at <= now()) OR
		       (status = 'running' AND lease_until IS NOT NULL AND lease_until < now()))
		 ORDER BY next_attempt_at, created_at
		 LIMIT ?`, target, limit).Scan(&jobs).Error
	return jobs, err
}

// completeDialogueJob 把一条 running 任务翻成 done。
// WHERE 带 claim_epoch：旧代次（被接管过的迟到 attempt）翻不了状态。
func (s *Store) completeDialogueJob(ctx context.Context, id string, claimEpoch int64) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE dialogue_index_jobs SET status = 'done', done_at = now(), lease_until = NULL,
		        last_error = '', updated_at = now()
		  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
		id, claimEpoch).Error
}

// failDialogueJob 记一次失败：瞬时错误按 1/5/30 分钟退避（第三次进 failed 死信）；
// 永久错误（维度不符、非法响应、超限）直接 failed 持久死信，不再重试（doc/84 §6.2）。
// 两个分支都带 claim_epoch 守卫——旧代次既翻不了状态也覆盖不了 last_error。
func (s *Store) failDialogueJob(ctx context.Context, id string, claimEpoch int64, class string, permanent bool) error {
	if permanent {
		return s.db.WithContext(ctx).Exec(
			`UPDATE dialogue_index_jobs SET status = 'failed', lease_until = NULL, last_error = ?,
			        updated_at = now()
			  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
			class, id, claimEpoch).Error
	}
	if len(dialogueRetryBackoff) != dialogueMaxAttempts {
		panic("dialogueRetryBackoff 必须是 1/5/30 分钟三档（failDialogueJob 的 SQL 按下标取）")
	}
	return s.db.WithContext(ctx).Exec(
		`UPDATE dialogue_index_jobs
		    SET status = CASE WHEN attempts >= ? THEN 'failed' ELSE 'pending' END,
		        lease_until = NULL,
		        last_error = ?,
		        next_attempt_at = CASE WHEN attempts >= ? THEN next_attempt_at
		                              ELSE now() + make_interval(secs =>
		                                    (ARRAY[?::double precision, ?::double precision, ?::double precision])[attempts]) END,
		        updated_at = now()
		  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
		dialogueMaxAttempts, class, dialogueMaxAttempts,
		dialogueRetryBackoff[0].Seconds(), dialogueRetryBackoff[1].Seconds(), dialogueRetryBackoff[2].Seconds(),
		id, claimEpoch).Error
}

// renewDialogueJobLease 续租：租约顺延。返回 false = 已不归本持有者（被接管/已终结）。
func (s *Store) renewDialogueJobLease(ctx context.Context, id string, claimEpoch int64, lease time.Duration) (bool, error) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE dialogue_index_jobs SET lease_until = now() + make_interval(secs => ?), updated_at = now()
		  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
		lease.Seconds(), id, claimEpoch)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// unpublishedDialogueJobs relay 的扫描：已提交但还没投递成功（published_at 缺失）的行。
// 至少一次投递：投递成功才写 published_at；写失败下轮重投，允许重复消息（doc/84 §6.1）。
func (s *Store) unpublishedDialogueJobs(ctx context.Context, limit int) ([]DialogueIndexJob, error) {
	var jobs []DialogueIndexJob
	err := s.db.WithContext(ctx).Raw(
		`SELECT id, event_id, run_id, session_id, tenant_id, user_id, workspace_id, target, kind,
		        index_version, trace_parent
		  FROM dialogue_index_jobs
		 WHERE published_at IS NULL
		 ORDER BY created_at, id
		 LIMIT ?`, limit).Scan(&jobs).Error
	return jobs, err
}

// markDialogueJobPublished 投递成功后写 published_at（幂等：已写的行不动）。
func (s *Store) markDialogueJobPublished(ctx context.Context, eventID, target string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE dialogue_index_jobs SET published_at = now(), updated_at = now()
		  WHERE event_id = ? AND target = ? AND published_at IS NULL`,
		eventID, target).Error
}

// dialogueIndexSource 一次外部索引的输入：chunk 行 + run 定稿时间。
// 归属（tenant/user/workspace_id）不在这里——消费者一律以任务行为准。
type dialogueIndexSource struct {
	RunID      string
	SessionID  string
	Passage    string
	Runes      int
	Truncated  bool
	IndexedAt  time.Time
	FinishedAt *time.Time
}

// dialogueIndexSources 批量回读 chunk + run 定稿时间（消费者写 ES/Milvus 前调）。
// 缺 chunk 的 run 不在返回 map 里——upsert 语义下缺块 = 外部副本应收敛删除。
func (s *Store) dialogueIndexSources(ctx context.Context, runIDs []string) (map[string]*dialogueIndexSource, error) {
	if len(runIDs) == 0 {
		return map[string]*dialogueIndexSource{}, nil
	}
	var rows []dialogueIndexSource
	err := s.db.WithContext(ctx).Raw(
		`SELECT c.run_id, c.session_id, c.passage, c.runes, c.truncated, c.indexed_at,
		        r.finished_at
		  FROM dialogue_chunks c
		  LEFT JOIN runs r ON r.id = c.run_id
		 WHERE c.run_id IN ?`, runIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]*dialogueIndexSource, len(rows))
	for i := range rows {
		out[rows[i].RunID] = &rows[i]
	}
	return out, nil
}
