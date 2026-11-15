// 本文件是隔离组工作区摘要的读面（方案真源 = doc/74 §4.3/§5.2）。
//
// 详情与列表里的 workspace 摘要只读三处：组级配置与发布状态来自 PG 通用列；
// Provider 类型现读组目录 state.json（尚未准备 = pending）；验证状态现读
// verification/<taskId>.json 证据文件。不读完整 patch、结果文件或验证输出——
// 列表接口与详情共用这一个轻量摘要。
package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// orchWorkspaceDir 是组工作区目录（doc/74 §5.2，doc/76 §6.1 起挂在项目运行资源区）：
// <workspace>/tenants/<tid>/workspaces/<wid>/runtime/orchestrations/<orchID>/workspace/。
// 路径全部由 tenant/workspace/orchestration ID 推导，模型输入不能指定内部路径。
func orchWorkspaceDir(root, tenantID, workspaceID, orchID string) string {
	if root == "" || tenantID == "" || workspaceID == "" || orchID == "" {
		return ""
	}
	return filepath.Join(filepath.Join(root, "tenants", tenantID, "workspaces", workspaceID, projectRuntimeDirName, "orchestrations"), orchID, "workspace")
}

// workspaceGroupState 是组目录 state.json 的形状（doc/74 §5.2）。
// Provider 类型在第一次准备时固定；之后必须一致，不允许项目形态变化时静默换 Provider。
type workspaceGroupState struct {
	FormatVersion int       `json:"formatVersion"`
	WorkspaceMode string    `json:"workspaceMode"`
	Provider      string    `json:"provider"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// workspaceVerificationEvidence 是 verification/<taskId>.json 的形状（doc/74 §4.3）。
// 读面取 status；证据收尾（passed/failed）时退出状态与输出位置也进摘要，供发布面板展示。
type workspaceVerificationEvidence struct {
	TaskID     string     `json:"taskId"`
	Status     string     `json:"status"` // running / passed / failed
	ExitCode   int        `json:"exitCode"`
	OutputPath string     `json:"outputPath"` // 相对组目录的输出留存位置
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

// workspaceSummary 组装一个隔离组的 workspace 读面；shared 组返回 nil（响应不出现该键）。
// taskID 决定读哪份验证证据：Task 详情读自己的，组详情读最终 Task 的。
// 组目录或证据文件不存在 = 尚未准备/未运行（pending / ""），不是错误。
func (s *Store) workspaceSummary(o AgentOrchestration, taskID string) *taskWorkspaceView {
	if o.WorkspaceMode != workspaceModeIsolated {
		return nil
	}
	v := &taskWorkspaceView{
		Mode:     workspaceModeIsolated,
		Provider: workspaceProviderPending,
		Verification: workspaceVerificationView{
			Configured: o.VerifyCommand != "",
		},
		Publication: workspacePublicationView{
			Status:    o.PublicationStatus,
			ErrorCode: o.PublicationErrorCode,
			Epoch:     o.PublicationEpoch,
		},
	}
	dir := orchWorkspaceDir(s.workspaceRoot, o.TenantID, o.WorkspaceID, o.ID)
	if dir == "" {
		return v
	}
	if st, err := readWorkspaceGroupState(dir); err == nil && st != nil &&
		(st.Provider == workspaceProviderGit || st.Provider == workspaceProviderDirectory) {
		v.Provider = st.Provider
	}
	if taskID != "" {
		if b, err := os.ReadFile(filepath.Join(dir, "verification", taskID+".json")); err == nil {
			var ev workspaceVerificationEvidence
			if json.Unmarshal(b, &ev) == nil {
				switch ev.Status {
				case verificationStatusRunning, verificationStatusPassed, verificationStatusFailed:
					v.Verification.Status = ev.Status
				}
				// 收尾证据才带退出状态与输出位置（running 时 ExitCode 还没意义）。
				if ev.Status == verificationStatusPassed || ev.Status == verificationStatusFailed {
					code := ev.ExitCode
					v.Verification.ExitCode = &code
					v.Verification.OutputPath = ev.OutputPath
				}
			}
		}
	}
	return v
}

// orchRowsByID 一笔读出若干组的行（读面组装 workspace 摘要用）。可见性已由调用方的
// Task 属主链保证——这里只按 ID 取配置，不再判属主。
func (s *Store) orchRowsByID(ctx context.Context, ids []string) (map[string]AgentOrchestration, error) {
	out := make(map[string]AgentOrchestration, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}
