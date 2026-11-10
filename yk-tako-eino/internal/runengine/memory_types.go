package runengine

import "time"

// 本文件是 pipeline 记忆的**新行结构**（doc/65 §4.1–§4.3；施工包 §7.1）。
//
// 与旧结构（doc/29 线一）的关系：
//   - 旧结构只有 `kind`/`topic` 两个分类维度，且靠 `memory_items_dedup` 唯一索引
//     （tenant,user,workspace,kind,content）做「同内容只留一行」去重。
//   - 新结构把分类换成 `layer`（读在哪一层）+ `scope`（谁能看）+ `state`（历史位置），
//     **不保留 kind/topic**，也**不做正文去重**——真实的 A→B→A 改口必须能产生新历史行。
//   - 历史行只进不复活：active → superseded | removed | archived；content/quote 不原地改。
//
// 词表常量也在这里：它们进了状态机与可见性谓词，写死成常量而不是配置，
// 是为了让「历史行」与「新行」永远用同一套词（配置化会让旧行按新词表失效）。

// 记忆层：resident 每场全量进系统提示（当场冻结）；recall 用到才见。
const (
	MemoryLayerResident = "resident"
	MemoryLayerRecall   = "recall"
)

// 可见域：user 跟这个人走（跨项目可见）；workspace 只在本项目可见。
const (
	MemoryScopeUser      = "user"
	MemoryScopeWorkspace = "workspace"
)

// 记忆行状态：只进不复活（doc/65 §4.3）。
const (
	MemoryStateActive     = "active"
	MemoryStateSuperseded = "superseded"
	MemoryStateRemoved    = "removed"
	MemoryStateArchived   = "archived"
)

// 记忆来源：user=用户明说（程序整理不能合并/替代/淘汰）；extracted=普通推断行；
// model=直接工具写入的存量迁移行。
const (
	MemorySourceUser      = "user"
	MemorySourceExtracted = "extracted"
	MemorySourceModel     = "model"
)

// 记忆任务种类：inrun=场中登记；postrun=run 定稿后的兜底抽取；tidy=整理。
const (
	MemoryJobKindInrun   = "inrun"
	MemoryJobKindPostrun = "postrun"
	MemoryJobKindTidy    = "tidy"
)

// 记忆任务状态。
const (
	MemoryJobStatusPending = "pending"
	MemoryJobStatusRunning = "running"
	MemoryJobStatusDone    = "done"
	MemoryJobStatusFailed  = "failed"
)

// MemoryItem = `memory_items`（doc/65 §4.2）：一条记忆行。
//
// **没有 kind、没有 topic、没有 embedding、没有 pinned。** 旧的两条唯一索引
// （memory_items_dedup 的正文去重、memory_items_episode_once 的 episode 唯一）
// 随旧结构一起退役（见 pgstore.go 的显式 DDL）。
//
// Workspace 在新结构里是**项目 id**（WorkspaceRef.WorkspaceID），不是引擎工作区根路径：
// scope='workspace' 的行用它判可见；scope='user' 的行只拿它记「来源项目」，
// 不因此变成项目所有（doc/65 §4.2）。
type MemoryItem struct {
	ID        string `gorm:"primaryKey"` // mem_<ULID>
	TenantID  string `gorm:"not null;index:memory_items_tenant_user_state,priority:1;index:memory_items_tenant_workspace_state,priority:1"`
	UserID    string `gorm:"not null;default:'';index:memory_items_tenant_user_state,priority:2"`
	Workspace string `gorm:"not null;default:'';index:memory_items_tenant_workspace_state,priority:2"`
	Layer     string `gorm:"not null;default:'recall'"`
	// Scope 是可见域：user（跨项目可见）/ workspace（只在本项目可见）。
	Scope string `gorm:"not null;default:'user'"`
	// Content 是这条记忆的正文。改口时插新行，历史行的 content 不原地改。
	Content string `gorm:"not null"`
	// Quote 是用户原话：能引用则填，推断条目可空。
	Quote string `gorm:"not null;default:''"`
	// State 是状态机位置：只进不复活。
	State string `gorm:"not null;default:'active';index:memory_items_tenant_user_state,priority:3;index:memory_items_tenant_workspace_state,priority:3"`
	// SupersededBy 指向替代它的新行；ConflictWith 指向与它冲突的另一行
	// （冲突不自动选赢家，两行互写，召回时成对标记，交给用户裁决）。
	SupersededBy *string `gorm:"index:memory_items_superseded_by"`
	ConflictWith *string
	// Source 是来源：user=用户明说（程序整理不得替代/淘汰），extracted=推断，model=工具写入。
	Source string `gorm:"not null;default:'extracted'"`
	// 证据指针：source_run_id + source_seq 定位原话现场；证据时间由
	// runlog_entries.created_at 推导（不取 runs.finished_at，doc/65 §4.2）。
	SourceSessionID SessionID `gorm:"not null;default:''"`
	SourceRunID     RunID     `gorm:"not null;default:''"`
	SourceSeq       int64     `gorm:"not null;default:0"`
	// CreatedAt/UpdatedAt 是提交时间，**不能**用来判「哪句话先说」。
	CreatedAt time.Time `gorm:"not null;index:memory_items_created_at"`
	UpdatedAt time.Time `gorm:"not null"`
	// LeftActiveAt 只在首次离开 active 时写一次（普通元数据更新不覆盖它）。
	LeftActiveAt *time.Time
	// RemovedAt 只在用户明确删除时写。
	RemovedAt *time.Time
	// ValidFrom/ExpiresAt 是这条记忆的有效期；默认读只认未过期（expires_at IS NULL OR > now()）。
	ValidFrom *time.Time
	ExpiresAt *time.Time
}

// TableName 是 `memory_items`。
func (MemoryItem) TableName() string { return "memory_items" }

// MemoryJob = `memory_jobs`（doc/65 §4.5）：记忆任务（抽取/整理的派工单）。
//
// run_id **不再全局唯一**：一个 run 可以同时有 inrun / postrun / tidy 几条任务，
// 幂等由 dedup_key 保证（见 RegisterMemoryJob）。
type MemoryJob struct {
	ID        string    `gorm:"primaryKey"`                     // mj_<ULID>
	RunID     RunID     `gorm:"not null;index:memory_jobs_run"` // 一个 run 可有多条任务
	SessionID SessionID `gorm:"not null;default:''"`
	TenantID  string    `gorm:"not null;default:''"`
	UserID    string    `gorm:"not null;default:''"`
	Workspace string    `gorm:"not null;default:''"`
	// Kind 是任务种类：inrun / postrun / tidy。
	Kind string `gorm:"not null;default:'postrun';index:memory_jobs_kind"`
	// TargetLayer 是目标任务层：resident / recall。postrun 固定 recall（后台兜底不晋升层），
	// tidy 可空。EndRun 与 memory 工具派工时都显式写。
	TargetLayer *string
	// Quote 是这条任务对应的用户原话。
	Quote string `gorm:"not null;default:''"`
	// SourceSeq 是原话在 runlog 里的 seq（与 SourceRunID 一起定位证据）。
	SourceSeq int64 `gorm:"not null;default:0"`
	// TraceParent 是 trace 透传串（消息只带 job_id 与它，任务正文真源在库里）。
	TraceParent string `gorm:"not null;default:''"`
	// Status 是任务状态：pending / running / done / failed。
	Status string `gorm:"not null;default:'pending';index:memory_jobs_claim,priority:1"`
	// Attempts 是已尝试次数；claim 时 +1。
	Attempts int `gorm:"not null;default:0"`
	// LeaseUntil 是 running 租约：进程崩了别的副本按它捡回。
	LeaseUntil *time.Time
	// NextAttemptAt 是下次可捡时间（失败退避后推后）。
	NextAttemptAt time.Time `gorm:"not null;index:memory_jobs_claim,priority:2"`
	// ClaimEpoch 是本次认领的代号：claim 时 +1，只有持有当前 epoch 的 attempt 能写回状态。
	ClaimEpoch int64 `gorm:"not null;default:0"`
	// BatchID/BatchPayload/BatchCommittedAt 记这一轮产出的批次：同 job 重放时
	// batch_committed_at 已非空 → 不重新分析、不新增行。
	BatchID          string `gorm:"not null;default:''"`
	BatchPayload     string `gorm:"not null;default:''"`
	BatchCommittedAt *time.Time
	// DedupKey 是幂等键（可空、唯一）：同一 dedup_key 重复登记复用同一行。
	DedupKey *string `gorm:"uniqueIndex:memory_jobs_dedup_key,where:dedup_key IS NOT NULL"`
	// LastError 是最近一次失败原因。
	LastError string    `gorm:"not null;default:''"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 是 `memory_jobs`。
func (MemoryJob) TableName() string { return "memory_jobs" }
