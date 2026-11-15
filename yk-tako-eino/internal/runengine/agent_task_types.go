// 本文件是持久 Task 与编排组的持久 model 与 API DTO（方案真源 = doc/70 §5；施工包 §5）。
//
// 四张表各管一件持久事实：
//   - agent_orchestrations：编排组——一个用户目标、它的来源会话、创建幂等（idempotency_key +
//     plan_hash 唯一约束）与最终交付状态。所有 Task 都属于一个编排组（单件 Task = 单节点组）。
//   - agent_tasks：Task——一件可独立执行、恢复、提交结果的工作。run complete ≠ succeeded，
//     裁决在 agent_task_store.go 的 ReconcileRun。
//   - agent_task_links：依赖边——parent 成功后 child 才具备执行资格；(parent, child) 唯一。
//   - agent_notifications：持久协调通知——Task 失败 / 中断 / 整组完成才写（普通进度不进模型），
//     来源会话忙时保持 pending，进程重启后从 PG 继续消费。
//
// 「interrupted」不是存储状态：Task status='running' 且当前 run 租约过期时读侧派生，
// 与 runs 的读侧派生同一条规则（doc/30 A1）。
package runengine

import (
	"encoding/json"
	"time"
)

// 编排组状态词表（doc/70 §5.1）。cancelling 是「停止请求已持久化」：running 节点由现有
// 停止链路收尾，全部终态后调度循环把它落 cancelled。
const (
	orchStatusActive     = "active"
	orchStatusSucceeded  = "succeeded"
	orchStatusFailed     = "failed"
	orchStatusCancelling = "cancelling"
	orchStatusCancelled  = "cancelled"
	// orchStatusPreparing 是继任组的出生状态（doc/75 §4.2）：记录已存在，正在写 handoff／
	// 标记继任关系或停止原组。不可调度、不占模型容量，但阻止项目与会话删除。
	orchStatusPreparing = "preparing"
	// orchStatusWaitingPrevious：handoff 完整，但原组尚未终态。调度器确认原组终态后转 active。
	orchStatusWaitingPrevious = "waiting_previous"
)

// Task 状态词表（doc/70 §7）。没有 skipped（V1 拍定）；blocked 带 machine reason（error_code）。
const (
	agentTaskStatusPending   = "pending"
	agentTaskStatusReady     = "ready"
	agentTaskStatusRunning   = "running"
	agentTaskStatusSucceeded = "succeeded"
	agentTaskStatusFailed    = "failed"
	agentTaskStatusBlocked   = "blocked"
	agentTaskStatusCancelled = "cancelled"
	// agentTaskStatusPublishing 是隔离组最终 Task 的存储状态（doc/74 §4.6）：最终 run 已
	// complete、结果已接受，但发布仍 pending/publishing。不是 interrupted，不占模型执行容量。
	agentTaskStatusPublishing = "publishing"
	// agentTaskStatusInterrupted 只在 API 视图出现（读侧派生），永不入库。
	agentTaskStatusInterrupted = "interrupted"
)

// 工作区模式词表（doc/74 §4.1）：组级配置。shared 是既有正式行为（默认）；isolated 整组
// 使用 WorkspaceProvider 的独立工作现场。Task 与 DAG 不认识 Git——模式只有这两个通用值。
const (
	workspaceModeShared   = "shared"
	workspaceModeIsolated = "isolated"
)

// Provider 类型词表（doc/74 §4.3）：详情读面从组目录 state.json 现读，不从 PG 取。
// pending = 尚未准备（组目录或 state.json 还不存在）。
const (
	workspaceProviderPending   = "pending"
	workspaceProviderGit       = "git"
	workspaceProviderDirectory = "directory"
)

// 发布状态词表（doc/74 §5.1，agent_orchestrations.publication_status）：
// 空 = 无发布（shared 组恒空）；pending → publishing → published，或 blocked 等处理。
const (
	publicationStatusPending    = "pending"
	publicationStatusPublishing = "publishing"
	publicationStatusBlocked    = "blocked"
	publicationStatusPublished  = "published"
)

// 发布受阻的通用机器原因（doc/74 §4.5/§7.3/§8.3）：retry-publication 只认这三个。
const (
	taskErrWorkspaceChanged    = "workspace_changed"             // 项目已前进，旧结果不能覆盖
	taskErrWorkspaceDirty      = "workspace_dirty"               // 项目有未保存内容
	taskErrPublicationRecovery = "publication_recovery_required" // 现场无法解释，需人工检查
)

// 验证证据状态词表（doc/74 §4.3）："" = 未运行；证据文件在共享卷，不在 PG。
const (
	verificationStatusRunning = "running"
	verificationStatusPassed  = "passed"
	verificationStatusFailed  = "failed"
)

// 交付状态词表（doc/70 §9.3）。空串 = 没有可交付结果（组 failed / 单件通知）。
const (
	deliveryStatusPending    = "pending"
	deliveryStatusDelivering = "delivering"
	deliveryStatusDelivered  = "delivered"
	deliveryStatusFailed     = "failed"
)

// 协调通知词表（doc/70 §9.1；doc/72 §6.1；doc/82 §6.5）：进模型的事件；普通工具进度只进 run 账本。
// plan_review_required 是检查点——Task 成功把组推进 plan_hold，等一次计划决策。
// bash_job_finished 是后台命令到终态（doc/82）：orchestration 为空、job 必填、task_id 维持空串。
const (
	notifKindTaskFailed         = "task_failed"
	notifKindTaskInterrupted    = "task_interrupted"
	notifKindOrchFinished       = "orchestration_finished"
	notifKindPlanReviewRequired = "plan_review_required"
	notifKindBashJobFinished    = "bash_job_finished"
)

// 后台命令状态词表（doc/82 §6.1；施工包 §4-1/§4-2）。stopping 是中间态：停止请求已持久化、
// 执行者尚未确认进程组退出，仍算未收口；已终态重复停止回当前终态，仍 stopping 时重复停止 409 117118。
const (
	jobStatusRunning     = "running"
	jobStatusStopping    = "stopping"
	jobStatusSucceeded   = "succeeded"
	jobStatusFailed      = "failed"
	jobStatusStopped     = "stopped"
	jobStatusInterrupted = "interrupted"
)

// jobTerminal 报告一个 job 状态是否终态（stopping 不是终态）。
func jobTerminal(status string) bool {
	switch status {
	case jobStatusSucceeded, jobStatusFailed, jobStatusStopped, jobStatusInterrupted:
		return true
	}
	return false
}

// 通知状态（doc/73 §5.1）：pending（待消费）→ claimed（已认领、协调 run 在跑）→
// consumed（已消费完）；claimed 的 run 明确 error／stopped 后进 failed，只能由用户显式
// 重新处理（新 attempt）再回 claimed。failed 永不被普通 pending 消费查询捞起。
const (
	notifStatusPending  = "pending"
	notifStatusClaimed  = "claimed"
	notifStatusConsumed = "consumed"
	notifStatusFailed   = "failed"
)

// 通知消费失败与整组交付失败的机器原因（doc/73 §4.5）。Task 自身 error_code 只表示被通知的
// Task 为什么失败，不能拿来保存「交付没完成」。
const (
	deliveryErrRunError = "delivery_run_error" // 当前协调 run 以 error 结束
	deliveryErrStopped  = "delivery_stopped"   // 用户停止当前协调 run
	deliveryErrArtifact = "artifact_invalid"   // 当前结果产物已不可读取
)

// sessions.kind 词表：chat 是既有默认（所有普通会话）；agent_task 是 Task 执行会话——
// 不出现在普通聊天列表（doc/70 §5.5）。
const (
	sessionKindChat      = "chat"
	sessionKindAgentTask = "agent_task"
)

// agentTaskMaxCount 是一张编排组的节点数上限（施工包 §4.2）。
// 它也是修订预算：历史节点（含已取消）计入，取消不释放；修订本身的接受上限同为 32 版。
const agentTaskMaxCount = 32

// orchestrationConcurrency 是「同组同时最多在跑的 Task 数」（施工包 §2 第 11 条，固定常量）。
// interrupted 不占容量（租约已过期，不算 live run）。
const orchestrationConcurrency = 2

// 机器错误码（error_code 列 / API error.code）。展示文案与机器原因分开（doc/70 §4.3）。
const (
	taskErrDependencyFailed = "dependency_failed"
	taskErrResultNotSubmit  = "result_not_submitted"
	taskErrRunError         = "run_error"
	taskErrAssemblyFailed   = "assembly_failed"
	taskErrSubmitStaleEpoch = "stale_execution"
	taskErrSubmitNotRunning = "not_running"
	taskErrArtifactBad      = "artifact_invalid"
	taskErrSubmitBad        = "result_invalid"
	taskErrInputTimeout     = "input_timeout"
)

// AgentOrchestration = `agent_orchestrations` 表。
// 唯一约束 (tenant_id, source_session_id, idempotency_key) 就是创建幂等本身：
// 重试 INSERT 撞索引 → 回读比对 plan_hash（同 = 原组，异 = 117102）。
type AgentOrchestration struct {
	ID              string    `gorm:"primaryKey"` // or_<ULID>
	TenantID        string    `gorm:"not null;uniqueIndex:agent_orchestrations_idem,priority:1"`
	WorkspaceID     string    `gorm:"not null"`
	SourceSessionID SessionID `gorm:"not null;uniqueIndex:agent_orchestrations_idem,priority:2"`
	// SourceRunID 是模型工具创建时的当前 run（trace 对读用）；产品 API 手工创建为空串。
	SourceRunID string `gorm:"not null;default:''"`
	Goal        string `gorm:"not null"`
	Status      string `gorm:"not null;default:active"`
	// FinalTaskID 是全组唯一的最终 Task（出度为 0 的那一个；创建期校验恰好一个）。
	FinalTaskID string `gorm:"not null"`
	// FinalResultRevision 是最终 Task 提交结果时的 revision（交付通知 dedup 的一部分）。
	FinalResultRevision int `gorm:"not null;default:0"`
	// IdempotencyKey 是创建幂等键（调用方作用域 = 租户 × 来源会话）。
	IdempotencyKey string `gorm:"not null;uniqueIndex:agent_orchestrations_idem,priority:3;size:128"`
	// PlanHash 是规范化创建计划的 SHA-256 十六进制（agent_task_graph.go）；单件 Task 创建时同样计算。
	PlanHash string `gorm:"not null;default:''"`
	// DeliveryStatus：空 = 无可交付结果；pending → delivering → delivered（或 failed）。
	DeliveryStatus string `gorm:"not null;default:''"`
	DeliveryRunID  string `gorm:"not null;default:''"`
	// DeliveryErrorCode 只在 delivery_status=failed 时有值（doc/73 §4.5 词表）：
	// 它回答「交付为什么没完成」，与被通知 Task 的 error_code 不是一回事。
	DeliveryErrorCode string `gorm:"not null;default:''"`
	// PlanRevision 是图的接受版本（初始 0；每接受一次修订 +1）。它是修订 CAS 的版本号。
	PlanRevision int `gorm:"not null;default:0"`
	// PlanHold=true 表示检查点已触发：停止新领取与组收口，等一次 continue／revision。
	PlanHold bool `gorm:"not null;default:false"`
	// 工作区配置与发布状态（doc/74 §5.1）：PG 只存通用业务事实。
	// Provider 内部字段（branch/commit/repo/worktree/哈希）一律不进这张表。
	WorkspaceMode        string `gorm:"not null;default:shared"` // shared / isolated
	VerifyCommand        string `gorm:"not null;default:''"`
	PublicationStatus    string `gorm:"not null;default:''"` // '' / pending / publishing / blocked / published
	PublicationErrorCode string `gorm:"not null;default:''"`
	PublicationEpoch     int64  `gorm:"not null;default:0"`
	PublishedAt          *time.Time
	CreatedAt            time.Time
	FinishedAt           *time.Time
	DeliveredAt          *time.Time
	UpdatedAt            time.Time
}

func (AgentOrchestration) TableName() string { return "agent_orchestrations" }

// AgentTask = `agent_tasks` 表。
type AgentTask struct {
	ID              string `gorm:"primaryKey"` // at_<ULID>
	OrchestrationID string `gorm:"not null;uniqueIndex:agent_tasks_orch_key,priority:1;index"`
	TaskKey         string `gorm:"not null;uniqueIndex:agent_tasks_orch_key,priority:2"`
	TenantID        string `gorm:"not null;index"`
	WorkspaceID     string `gorm:"not null"`
	Content         string `gorm:"not null"`
	AgentName       string `gorm:"not null"`
	// ModelRef 空 = 继承（领取装配时按租户运行时配置解析）。
	ModelRef string `gorm:"not null;default:''"`
	Status   string `gorm:"not null;default:pending"`
	// ExecutionSessionID / CurrentRunID 可空直到领取；非空时各自唯一（领取事务保证）。
	ExecutionSessionID *string `gorm:"index"`
	CurrentRunID       *string `gorm:"index"`
	// Result 是提交的规范 JSON（summary/artifacts/metadata，TaskResult 形状）；空 = 未提交。
	// ResultRunID/ResultEpoch 证明这份结果是哪一代执行提交的：旧 worker 不能覆盖新代。
	Result         string `gorm:"not null;default:''"`
	ResultRevision int    `gorm:"not null;default:0"`
	ResultRunID    string `gorm:"not null;default:''"`
	ResultEpoch    int64  `gorm:"not null;default:0"`
	ErrorCode      string `gorm:"not null;default:''"`
	ErrorMessage   string `gorm:"not null;default:''"`
	// PlanCheckpoint=true：本节点成功后触发一次计划复核（组 plan_hold + 通知）。
	PlanCheckpoint bool `gorm:"not null;default:false"`
	// CreatedRevision 是节点被接受的图版本（初始节点 0；修订新增 = 该修订版本）。
	CreatedRevision int `gorm:"not null;default:0"`
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	UpdatedAt       time.Time
}

func (AgentTask) TableName() string { return "agent_tasks" }

// AgentTaskLink = `agent_task_links` 表。复合主键 (parent, child) 同时就是唯一约束。
type AgentTaskLink struct {
	ParentTaskID    string `gorm:"primaryKey"`
	ChildTaskID     string `gorm:"primaryKey"`
	TenantID        string `gorm:"not null;index"`
	OrchestrationID string `gorm:"not null;index"`
	CreatedAt       time.Time
}

func (AgentTaskLink) TableName() string { return "agent_task_links" }

// AgentNotification = `agent_notifications` 表。dedup_key 唯一：
// 失败 / 中断 = task:run:epoch，整组完成 = 组:final revision——恢复同一代不重复写。
type AgentNotification struct {
	ID       string `gorm:"primaryKey"` // an_<ULID>
	TenantID string `gorm:"not null;index"`
	// OrchestrationID 可空（doc/82 §6.4）：bash_job_finished 通知没有编排组，job 必填。
	// 空串 = 这条通知不来自编排组（bash 行）；属主链与跳转目标按 kind 二选一取。
	OrchestrationID string `gorm:"index"`
	TaskID          string `gorm:"not null;default:''"`
	// BackgroundJobID 指向后台命令（doc/82 §6.5）：bash_job_finished 通知按它找本 job 的 pending 行。
	// 其他 kind 为 NULL；唯一索引允许多个 NULL。
	BackgroundJobID   *string   `gorm:"uniqueIndex"`
	ReceiverSessionID SessionID `gorm:"not null"`
	Kind              string    `gorm:"not null"`
	DedupKey          string    `gorm:"not null;uniqueIndex"`
	Status            string    `gorm:"not null;default:pending"`
	ClaimRunID        string    `gorm:"not null;default:'';index"`
	// ClaimedInPlace=true：由来源 run 就地认领（ClaimNotificationInline，不铸 run、不写 attempt），
	// 与协调 run 的认领区分——CoordinationClaimForRun 与启动回填 attempt 的 SQL 都要排除它。
	ClaimedInPlace    bool   `gorm:"not null;default:false"`
	ErrorCode         string `gorm:"not null;default:''"`
	// ConsumeErrorCode 是最近一次协调 run 没完成消费的机器原因（doc/73 §4.5），
	// 只在 status=failed 时有值；它与被通知 Task 的 ErrorCode 分开。
	ConsumeErrorCode string `gorm:"not null;default:''"`
	CreatedAt        time.Time
	ClaimedAt        *time.Time
	FailedAt         *time.Time
	ConsumedAt       *time.Time
	UpdatedAt        time.Time `gorm:"not null;default:now()"`
}

func (AgentNotification) TableName() string { return "agent_notifications" }

// AgentNotificationAttempt = `agent_notification_attempts` 表（doc/73 §5.2）：同一通知先后由
// 哪些 run 处理。首次认领写 attempt=1；同 run 的崩溃 resume 只增 epoch，不增 attempt；
// 只有用户显式「重新处理」才 attempt+1。run 的执行状态与正文仍现读 runs／runlog，本表不复制。
type AgentNotificationAttempt struct {
	ID             string    `gorm:"primaryKey"` // ana_<ULID>
	TenantID       string    `gorm:"not null;index:agent_notification_attempts_tenant_notif,priority:1"`
	NotificationID string    `gorm:"not null;uniqueIndex:agent_notification_attempts_notif_attempt,priority:1;index:agent_notification_attempts_tenant_notif,priority:2"`
	Attempt        int       `gorm:"not null;uniqueIndex:agent_notification_attempts_notif_attempt,priority:2"`
	RunID          string    `gorm:"not null;uniqueIndex"`
	CreatedAt      time.Time `gorm:"not null"`
}

func (AgentNotificationAttempt) TableName() string { return "agent_notification_attempts" }

// AgentPlanRevision = `agent_plan_revisions` 表（doc/72 §5.2）：每一次被接受的计划修订一行。
// command_json 是规范化命令（数组已排序），command_hash 是它的 SHA-256——同键不同命令靠它区分。
type AgentPlanRevision struct {
	ID              string    `gorm:"primaryKey"` // pr_<ULID>
	TenantID        string    `gorm:"not null;uniqueIndex:agent_plan_revisions_idem,priority:1"`
	OrchestrationID string    `gorm:"not null;uniqueIndex:agent_plan_revisions_idem,priority:2;uniqueIndex:agent_plan_revisions_rev,priority:1;index:agent_plan_revisions_orch_rev,priority:1"`
	Revision        int       `gorm:"not null;uniqueIndex:agent_plan_revisions_rev,priority:2;index:agent_plan_revisions_orch_rev,priority:2"`
	BaseRevision    int       `gorm:"not null"`
	SourceRunID     string    `gorm:"not null;default:''"`
	IdempotencyKey  string    `gorm:"not null;uniqueIndex:agent_plan_revisions_idem,priority:3;size:128"`
	CommandHash     string    `gorm:"not null"`
	CommandJSON     string    `gorm:"not null;type:text"`
	Reason          string    `gorm:"not null;type:text"`
	CreatedAt       time.Time `gorm:"not null"`
}

func (AgentPlanRevision) TableName() string { return "agent_plan_revisions" }

// BackgroundJob = `background_jobs` 表（doc/82 §6.4）：一条离开前台窗口或显式转入后台的
// bash 命令的持久身份。前台窗口内完成的短命令也登记（Backgrounded=false）——按命令号回查
// 与停止请求都要求有行；默认列表靠 Backgrounded 过滤，所以短命令不刷满列表。
//
// 单 owner 语义：job 由启动它的副本监管（ExecutorID 指向该副本），Epoch+LeaseUntil 双保险。
// 幂等键 (SourceRunID, ToolCallID) 的唯一索引就是「一次工具调用一行」的闸。
type BackgroundJob struct {
	ID              string `gorm:"primaryKey"` // bj_ + newRowID 同款
	TenantID        string `gorm:"not null;index"`
	WorkspaceID     string `gorm:"not null;index"` // 没有项目的会话为空串
	SourceSessionID string `gorm:"not null;index"`
	SourceRunID     string `gorm:"not null;uniqueIndex:background_jobs_idem,priority:1"`
	ToolCallID      string `gorm:"not null;uniqueIndex:background_jobs_idem,priority:2"`
	SourceTaskID    string `gorm:"not null;default:''"`
	// SourceOrchestrationID 是来源 Task 所属编排组（聊天来源为空串）。
	SourceOrchestrationID string `gorm:"not null;default:''"`
	Command               string `gorm:"not null"`
	// Status：running/stopping/succeeded/failed/stopped/interrupted。
	Status        string `gorm:"not null;index"`
	ExitCode      *int
	StopRequested bool `gorm:"not null;default:false"`
	// Backgrounded：命令以「返回命令号继续跑」的形态回过调用方（前台超时转后台或显式后台）。
	// 只有它才写 bash_job_finished 通知，也才进默认命令列表。
	Backgrounded bool   `gorm:"not null;default:false"`
	ExecutorID   string `gorm:"not null;default:''"`
	// Epoch 从 1 起；对账收 interrupted 时 +1，旧 owner 的条件写不再命中。
	Epoch          int       `gorm:"not null"`
	ProcessGroupID *int64
	LeaseUntil     time.Time `gorm:"not null"`
	// 相对运行资源根的逻辑路径（无项目时以 .tako/bash/jobs/<bj>/ 开头）；不存绝对路径。
	StdoutRelPath string `gorm:"not null;default:''"`
	StderrRelPath string `gorm:"not null;default:''"`
	CreatedAt     time.Time
	StartedAt     *time.Time // 启动确认前为 null
	EndedAt       *time.Time
	UpdatedAt     time.Time `gorm:"not null;default:now()"`
}

func (BackgroundJob) TableName() string { return "background_jobs" }

// TaskArtifact 是结果里的一个产物引用：项目根的相对路径 + 种类标签。
type TaskArtifact struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// TaskResult 是 submit_task_result 的载荷（doc/70 §7.1）。
// 字段序 = 结果 JSON 的落库字节序（result 列存 marshal 后的原文）。
type TaskResult struct {
	Summary   string         `json:"summary"`
	Artifacts []TaskArtifact `json:"artifacts"`
	Metadata  map[string]any `json:"metadata"`
}

// taskError 是 API 视图里的错误形状：机器原因与展示文案分开。
type taskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// agentTaskView 是 GET 返回的完整 Task 视图。字段序 = 施工包 §4.3 的契约字节序：
// taskId → orchestrationId → key → content → agent → modelRef → workspaceId → status
// → currentRunId → result → error → resultRevision → createdAt → startedAt → finishedAt → updatedAt。
// Status 带 interrupted 派生：running + 当前 run 租约过期 → interrupted（存储仍是 running）。
type agentTaskView struct {
	TaskID          string `json:"taskId"`
	OrchestrationID string `json:"orchestrationId"`
	Key             string `json:"key"`
	Content         string `json:"content"`
	Agent           string `json:"agent"`
	ModelRef        string `json:"modelRef"`
	// 修订相关两列插在 modelRef 后（doc/72 §4.7）：旧字段相对顺序不变。
	PlanCheckpoint  bool        `json:"planCheckpoint"`
	CreatedRevision int         `json:"createdRevision"`
	WorkspaceID     string      `json:"workspaceId"`
	Status          string      `json:"status"`
	CurrentRunID    string      `json:"currentRunId"`
	Result          *TaskResult `json:"result"`
	Error           *taskError  `json:"error"`
	ResultRevision  int         `json:"resultRevision"`
	CreatedAt       time.Time   `json:"createdAt"`
	StartedAt       *time.Time  `json:"startedAt"`
	FinishedAt      *time.Time  `json:"finishedAt"`
	UpdatedAt       time.Time   `json:"updatedAt"`
	// Workspace 是隔离组的通用工作区摘要（doc/74 §4.3），追加在视图末尾；
	// shared 响应不出现这个键（omitempty + nil）。
	Workspace *taskWorkspaceView `json:"workspace,omitempty"`
}

// taskWorkspaceView 是隔离组详情的 workspace 读面（doc/74 §4.3）。
// 键序固定：mode → provider → verification → publication。
// provider 从组目录现读；verification 证据在共享卷；publication 来自通用 PG 状态。
type taskWorkspaceView struct {
	Mode         string                    `json:"mode"`
	Provider     string                    `json:"provider"`
	Verification workspaceVerificationView `json:"verification"`
	Publication  workspacePublicationView  `json:"publication"`
}

// workspaceVerificationView 键序：configured → status（→ exitCode → outputPath）。
// status：""/running/passed/failed。exitCode／outputPath 只在证据已收尾（passed/failed）
// 时追加，供发布面板展示退出状态与输出位置（施工包 §11.2）；未运行/running 不出现这两键，
// 无证据时的字节与既有契约完全一致。
type workspaceVerificationView struct {
	Configured bool   `json:"configured"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	OutputPath string `json:"outputPath,omitempty"`
}

// workspacePublicationView 键序：status → errorCode → epoch。
type workspacePublicationView struct {
	Status    string `json:"status"`
	ErrorCode string `json:"errorCode"`
	Epoch     int64  `json:"epoch"`
}

// orchestrationBody 是编排组视图的组半。
type orchestrationBody struct {
	OrchestrationID string `json:"orchestrationId"`
	Goal            string `json:"goal"`
	WorkspaceID     string `json:"workspaceId"`
	SourceSessionID string `json:"sourceSessionId"`
	Status          string `json:"status"`
	FinalTaskID     string `json:"finalTaskId"`
	// 修订相关两列插在 finalTaskId 后（doc/72 §4.7）：旧字段相对顺序不变。
	PlanRevision   int    `json:"planRevision"`
	PlanHold       bool   `json:"planHold"`
	DeliveryStatus string `json:"deliveryStatus"`
	DeliveryRunID  string `json:"deliveryRunId"`
	// DeliveryErrorCode 是交付失败的机器原因（doc/73 §4.4/§4.5）：插在 deliveryRunId 后，
	// 既有字段顺序不变；无失败时为空串。
	DeliveryErrorCode string     `json:"deliveryErrorCode"`
	CreatedAt         time.Time  `json:"createdAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	DeliveredAt       *time.Time `json:"deliveredAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	// Workspace 与 agentTaskView 同一形状（doc/74 §4.3），追加在组半末尾；shared 不出现。
	// 组摘要的验证状态取最终 Task 的证据文件。
	Workspace *taskWorkspaceView `json:"workspace,omitempty"`
	// 重规划关系（doc/75 §4.4）：只有继任组出现这两个键，追加在末尾；非继任组字节不变。
	// replanOf／replanDepth 从 handoff 现读，PG 不存这两列。
	ReplanOf    string `json:"replanOf,omitempty"`
	ReplanDepth int    `json:"replanDepth,omitempty"`
}

// taskLinkView 是依赖边视图：parent → child。
type taskLinkView struct {
	ParentTaskID string `json:"parentTaskId"`
	ChildTaskID  string `json:"childTaskId"`
}

// orchestrationView 是 GET /api/orchestrations/:id 的 data：组、节点、边。
type orchestrationView struct {
	Orchestration orchestrationBody `json:"orchestration"`
	Tasks         []agentTaskView   `json:"tasks"`
	Links         []taskLinkView    `json:"links"`
}

// parseTaskResult 把 result 列的 JSON 解回形状；空串 / 坏 JSON = 未提交（nil）。
func parseTaskResult(raw string) *TaskResult {
	if raw == "" {
		return nil
	}
	var r TaskResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil
	}
	// 落库字节保留提交原样（没带 artifacts 就是 null），但 API 视图必须回数组——
	// 契约示例里 artifacts 是数组，前端按 result.artifacts.length 渲染，null 会炸整个详情抽屉。
	if r.Artifacts == nil {
		r.Artifacts = []TaskArtifact{}
	}
	return &r
}
