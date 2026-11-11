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
	// agentTaskStatusInterrupted 只在 API 视图出现（读侧派生），永不入库。
	agentTaskStatusInterrupted = "interrupted"
)

// 交付状态词表（doc/70 §9.3）。空串 = 没有可交付结果（组 failed / 单件通知）。
const (
	deliveryStatusPending    = "pending"
	deliveryStatusDelivering = "delivering"
	deliveryStatusDelivered  = "delivered"
	deliveryStatusFailed     = "failed"
)

// 协调通知词表（doc/70 §9.1）：只有这三类事件进模型；普通工具进度只进 run 账本。
const (
	notifKindTaskFailed      = "task_failed"
	notifKindTaskInterrupted = "task_interrupted"
	notifKindOrchFinished    = "orchestration_finished"
)

// 通知状态：pending（待消费）→ claimed（已认领、协调 run 在跑）→ consumed（已消费完）。
const (
	notifStatusPending  = "pending"
	notifStatusClaimed  = "claimed"
	notifStatusConsumed = "consumed"
)

// sessions.kind 词表：chat 是既有默认（所有普通会话）；agent_task 是 Task 执行会话——
// 不出现在普通聊天列表（doc/70 §5.5）。
const (
	sessionKindChat      = "chat"
	sessionKindAgentTask = "agent_task"
)

// agentTaskMaxCount 是一张编排组的节点数上限（施工包 §4.2）。
const agentTaskMaxCount = 32

// orchestrationConcurrency 是「同组同时最多在跑的 Task 数」（施工包 §2 第 11 条，固定常量）。
// interrupted 不占容量（租约已过期，不算 live run）。
const orchestrationConcurrency = 2

// 机器错误码（error_code 列 / API error.code）。展示文案与机器原因分开（doc/70 §4.3）。
const (
	taskErrDependencyFailed  = "dependency_failed"
	taskErrResultNotSubmit   = "result_not_submitted"
	taskErrRunError          = "run_error"
	taskErrAssemblyFailed    = "assembly_failed"
	taskErrSubmitStaleEpoch  = "stale_execution"
	taskErrSubmitNotRunning  = "not_running"
	taskErrArtifactBad       = "artifact_invalid"
	taskErrSubmitBad         = "result_invalid"
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
	CreatedAt      time.Time
	FinishedAt     *time.Time
	DeliveredAt    *time.Time
	UpdatedAt      time.Time
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
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	UpdatedAt      time.Time
}

func (AgentTask) TableName() string { return "agent_tasks" }

// AgentTaskLink = `agent_task_links` 表。复合主键 (parent, child) 同时就是唯一约束。
type AgentTaskLink struct {
	ParentTaskID   string `gorm:"primaryKey"`
	ChildTaskID    string `gorm:"primaryKey"`
	TenantID       string `gorm:"not null;index"`
	OrchestrationID string `gorm:"not null;index"`
	CreatedAt      time.Time
}

func (AgentTaskLink) TableName() string { return "agent_task_links" }

// AgentNotification = `agent_notifications` 表。dedup_key 唯一：
// 失败 / 中断 = task:run:epoch，整组完成 = 组:final revision——恢复同一代不重复写。
type AgentNotification struct {
	ID                string    `gorm:"primaryKey"` // an_<ULID>
	TenantID          string    `gorm:"not null;index"`
	OrchestrationID   string    `gorm:"not null;index"`
	TaskID            string    `gorm:"not null;default:''"`
	ReceiverSessionID SessionID `gorm:"not null"`
	Kind              string    `gorm:"not null"`
	DedupKey          string    `gorm:"not null;uniqueIndex"`
	Status            string    `gorm:"not null;default:pending"`
	ClaimRunID        string    `gorm:"not null;default:''"`
	ErrorCode         string    `gorm:"not null;default:''"`
	CreatedAt         time.Time
	ClaimedAt         *time.Time
	ConsumedAt        *time.Time
}

func (AgentNotification) TableName() string { return "agent_notifications" }

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
	TaskID          string      `json:"taskId"`
	OrchestrationID string      `json:"orchestrationId"`
	Key             string      `json:"key"`
	Content         string      `json:"content"`
	Agent           string      `json:"agent"`
	ModelRef        string      `json:"modelRef"`
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
}

// orchestrationBody 是编排组视图的组半。
type orchestrationBody struct {
	OrchestrationID string     `json:"orchestrationId"`
	Goal            string     `json:"goal"`
	WorkspaceID     string     `json:"workspaceId"`
	SourceSessionID string     `json:"sourceSessionId"`
	Status          string     `json:"status"`
	FinalTaskID     string     `json:"finalTaskId"`
	DeliveryStatus  string     `json:"deliveryStatus"`
	DeliveryRunID   string     `json:"deliveryRunId"`
	CreatedAt       time.Time  `json:"createdAt"`
	FinishedAt      *time.Time `json:"finishedAt"`
	DeliveredAt     *time.Time `json:"deliveredAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
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
