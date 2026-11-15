// 本文件是隔离工作区的 Provider 接口、组句柄与组目录状态（方案真源 = doc/74 §5.2/§5.3）。
//
// 分工（doc/74 §3.1）：引擎裁决 run/epoch、结果版本、发布准入与 Task 状态；Provider 只负责
// 工作现场与不可变结果的文件/Git 操作，不决定 DAG、不直接改 Task 状态。
// shared 与 isolated 都走这里（doc/76 §4）：区别只在工作现场是「同组共用 work/shared」还是
// 「每件 Task 一个 work/<taskId>」；两种模式都不直接把中间产物写进用户内容根，
// 最终结果经结果接受与发布准入后才进 content。
//
// 组目录布局（共享持久卷，所有引擎副本可见；位于项目容器的运行资源区）：
//
//	<tenant>/workspaces/<workspaceId>/runtime/orchestrations/<orchestrationId>/workspace/
//	  state.json                        组状态：Provider 类型与模式第一次准备时固定
//	  provider/                         Provider 内部状态（Git bare 仓 / Directory 基线）
//	  work/<taskId>/                    isolated：每件 Task 的独立工作现场
//	  work/shared/                      shared：同组共用的工作现场
//	  results/<taskId>/<resultRevision>/  不可变结果（result.json + artifacts/）
//	  verification/<taskId>.json        验证证据
//	  publication/<publicationEpoch>.json 发布进度记录
package runengine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ResultRef 是引擎与 Provider 之间唯一的结果引用（doc/74 §5.3）：
// 只有通用 Task ID + resultRevision。Git commit、清单哈希等内部引用由 Provider 自行解析。
type ResultRef struct {
	TaskID         string
	ResultRevision int
}

// TaskWork 是 Provider 交给 Worker 的工作现场。
// Root 是 Worker 的唯一项目工作根；RuntimeRoot 是该组的运行资源区（bash/fetch/搜索全文落这里，
// 不写进任何 content）；ExtraWriteRoots 是 Provider 明确声明的沙箱附加根
// （如 Git 组的本组 repo.git）。PromptOverlay 只指导模型，程序判定不依赖它。
type TaskWork struct {
	Root            string
	RuntimeRoot     string
	PromptOverlay   string
	ExtraWriteRoots []string
	// Conflicts 是父子交接冲突的路径数（进 trace 的冲突数量，doc/74 §10.9）。
	Conflicts int
}

// ImmutableResult 是 results/<taskId>/<resultRevision>/result.json 的形状（doc/74 §5.2）。
// Reference 是 Provider 的不透明引用（Git = 结果引用/commit，Directory = 清单哈希），
// 引擎不解析其语义，只把它当同一性比对与对账的凭据。
type ImmutableResult struct {
	TaskID         string         `json:"taskId"`
	ResultRevision int            `json:"resultRevision"`
	Provider       string         `json:"provider"`
	Artifacts      []TaskArtifact `json:"artifacts"`
	CreatedAt      time.Time      `json:"createdAt"`
	Reference      string         `json:"reference"`
}

// PublicationPlan 是发布计划的引擎可见部分：只有人话摘要（进日志与 trace）。
// 逐路径/逐提交的计划明细是 Provider 内部状态，落 publication/<epoch>.json，引擎不读。
type PublicationPlan struct {
	Summary string
}

// PublishOutcome 的 Status 词表：published = 项目已写成；blocked = 需要执行者或用户处理；
// retry = 暂时性失败（锁竞争等），引擎稍后重试。blocked 的 ErrorCode 用通用三码
// （workspace_changed / workspace_dirty / publication_recovery_required）。
const (
	publishOutcomePublished = "published"
	publishOutcomeBlocked   = "blocked"
	publishOutcomeRetry     = "retry"
)

type PublishOutcome struct {
	Status    string
	ErrorCode string
	Detail    string // 人话原因，进日志与受阻通知；不含敏感 stderr 原文
}

// CleanupReport 是过窗清理的回执：删了什么、保留了什么（保留原因进日志）。
type CleanupReport struct {
	Removed []string
	Kept    []string
}

// WorkspaceProvider 是隔离工作区的内部接口（doc/74 §5.3，操作不可少）。
// 所有操作都在 PG 事务外执行；文件/Git 操作自带超时与输出上限。
// 引擎不把 PreparePublication 的计划当成功事实——只有 Publish/ReconcilePublication
// 的实际结果与 PG 条件更新共同决定发布状态。
type WorkspaceProvider interface {
	// PrepareGroup 第一次准备时固定 Provider 类型并建立组级状态（幂等）。
	PrepareGroup(ctx context.Context, g *GroupHandle) error
	// PrepareTask 领取时建立本件工作现场；parents 按 taskKey 排序由调用方保证。
	PrepareTask(ctx context.Context, g *GroupHandle, task AgentTask, parents []ResultRef) (TaskWork, error)
	// AcceptResult 验证工作现场并把结果固化成不可变结果；revision 是引擎算的候选版本。
	AcceptResult(ctx context.Context, g *GroupHandle, task AgentTask, revision int, result TaskResult, work TaskWork) (ImmutableResult, error)
	// ReadArtifact 读当前结果版本里声明的产物字节；path 必须逐字命中清单。
	ReadArtifact(ctx context.Context, g *GroupHandle, ref ResultRef, path string) ([]byte, error)
	// PreparePublication 计算并落发布计划（崩溃后对账的依据），不改原项目。
	PreparePublication(ctx context.Context, g *GroupHandle, final ImmutableResult) (PublicationPlan, error)
	// Publish 执行发布；调用前引擎已完成项目准入（advisory lock + 发布标记）。
	Publish(ctx context.Context, g *GroupHandle, final ImmutableResult, epoch int64) (PublishOutcome, error)
	// ReconcilePublication 崩溃/重启后按现场与记录判定：补收尾、继续或受阻。
	ReconcilePublication(ctx context.Context, g *GroupHandle, epoch int64) (PublishOutcome, error)
	// RecoverTaskWork 恢复已存在的工作现场（resume/新处理 run），不重新准备。
	RecoverTaskWork(ctx context.Context, g *GroupHandle, task AgentTask, revision int) (TaskWork, error)
	// Cleanup 过窗且组终态后清理；无法证明安全的现场保留并写进 Kept。
	Cleanup(ctx context.Context, g *GroupHandle, now time.Time, archiveTTL time.Duration) (CleanupReport, error)
}

// GroupHandle 是一场编排的组工作区句柄：路径全部由 tenant/orchestration ID 推导，
// 模型输入不能指定内部路径。Locker 是 PG advisory + 本机 flock 的串联锁（跨副本互斥）。
type GroupHandle struct {
	OrchestrationID string
	TenantID        string
	WorkspaceID     string
	WorkspaceMode   string // shared / isolated（空串按 isolated，供直接构造句柄的测试）
	VerifyCommand   string
	Dir             string // 组 workspace 目录（state.json 所在）
	ProjectRoot     string // 原项目根（发布目标；Git 执行沙箱不开放它）
	Locker          FileLocker
}

// newGroupHandle 装配组句柄。root 是引擎作业根（worker.Workspace()）。
func newGroupHandle(root string, o AgentOrchestration, projectRoot string, locker FileLocker) (*GroupHandle, error) {
	dir := orchWorkspaceDir(root, o.TenantID, o.WorkspaceID, o.ID)
	if dir == "" || projectRoot == "" || locker == nil {
		return nil, fmt.Errorf("组工作区句柄缺装配：tenant=%q orch=%q root=%q", o.TenantID, o.ID, root)
	}
	mode := o.WorkspaceMode
	if mode == "" {
		mode = workspaceModeIsolated
	}
	return &GroupHandle{
		OrchestrationID: o.ID,
		TenantID:        o.TenantID,
		WorkspaceID:     o.WorkspaceID,
		WorkspaceMode:   mode,
		VerifyCommand:   o.VerifyCommand,
		Dir:             dir,
		ProjectRoot:     projectRoot,
		Locker:          locker,
	}, nil
}

func (g *GroupHandle) statePath() string { return filepath.Join(g.Dir, "state.json") }
func (g *GroupHandle) providerDir() string {
	return filepath.Join(g.Dir, "provider")
}

// sharedMode 表示同组共用 work/shared（doc/76 §4.1）；空模式按 isolated，
// 让直接构造 GroupHandle 的测试保持旧语义。
func (g *GroupHandle) sharedMode() bool { return g.WorkspaceMode == workspaceModeShared }

func (g *GroupHandle) workDir(taskID string) string {
	if g.sharedMode() {
		return filepath.Join(g.Dir, "work", "shared")
	}
	return filepath.Join(g.Dir, "work", taskID)
}
func (g *GroupHandle) resultDir(taskID string, revision int) string {
	return filepath.Join(g.Dir, "results", taskID, strconv.Itoa(revision))
}
func (g *GroupHandle) verificationPath(taskID string) string {
	return filepath.Join(g.Dir, "verification", taskID+".json")
}
func (g *GroupHandle) publicationPath(epoch int64) string {
	return filepath.Join(g.Dir, "publication", strconv.FormatInt(epoch, 10)+".json")
}

// 锁的逻辑键：组一把、每件 Task 一把（doc/74 §9.2——锁只挡同 Task，不挡整组）。
// PG 侧由 pgLockKey 再加租户前缀；flock 侧锁目录本身按租户分。
func (g *GroupHandle) groupLockKey() string { return "orch-workspace:" + g.OrchestrationID }
func (g *GroupHandle) taskLockKey(taskID string) string {
	return g.groupLockKey() + "/task:" + taskID
}

// workspaceStateFormatVersion 是 state.json 的格式版本：形状变了才递增。
const workspaceStateFormatVersion = 1

// readWorkspaceGroupState 读组状态；文件不存在 = 尚未准备（nil, nil），损坏 = 错误。
func readWorkspaceGroupState(dir string) (*workspaceGroupState, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st workspaceGroupState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("组状态文件损坏：%w", err)
	}
	return &st, nil
}

// ensureGroupState 在组锁内固定 Provider 类型（doc/74 §5.2）：
// 已存在则必须一致——项目形态变化（如后来 git init）不允许静默换 Provider；
// 不存在则创建。写入走同目录临时文件 + 原子 rename。
func ensureGroupState(ctx context.Context, g *GroupHandle, provider string) error {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return err
	}
	defer rel()
	st, err := readWorkspaceGroupState(g.Dir)
	if err != nil {
		return err
	}
	mode := g.WorkspaceMode
	if mode == "" {
		mode = workspaceModeIsolated
	}
	if st != nil {
		if st.Provider != provider || st.WorkspaceMode != mode {
			return fmt.Errorf("组工作区已按 %s/%s 准备，与当前 %s/%s 不一致", st.WorkspaceMode, st.Provider, mode, provider)
		}
		return nil
	}
	if err := os.MkdirAll(g.Dir, 0o755); err != nil {
		return err
	}
	now := time.Now().UTC()
	b, err := json.Marshal(workspaceGroupState{
		FormatVersion: workspaceStateFormatVersion,
		WorkspaceMode: mode,
		Provider:      provider,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		return err
	}
	return atomicWriteFile(g.statePath(), b, 0o644)
}

// detectWorkspaceProvider 按真实项目形态选 Provider：有 .git（目录或文件）= Git，否则 Directory。
// 只在第一次准备时调用；之后以 state.json 固定的类型为准（ensureGroupState 校验一致性）。
func detectWorkspaceProvider(projectRoot string) string {
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err == nil {
		return workspaceProviderGit
	}
	return workspaceProviderDirectory
}
