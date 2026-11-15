// 本文件是项目删除的磁盘清理与补偿（方案真源 = doc/76 §6.4/§5.4）。
//
// 删除的 PG 事务只做能回滚的事：软删会话、清项目记忆与记忆任务、写 project_cleanups.pending、
// 删项目行。物理目录（整个项目容器 + 关联会话私有目录）在事务提交之后删——删不掉也不回滚
// 业务删除，清理记录留 pending 由调度器持续补偿。
//
// 清理只认 ID 推导的路径：项目容器由 tenant/workspace ID 算，会话目录由库里仍保留的
// sessions.workspace_id 查出来再按 session ID 算。不认识任何外部传入的路径。
package runengine

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
)

// 项目清理记录的状态词表（doc/76 §5.4）。
const (
	projectCleanupPending = "pending"
	projectCleanupDone    = "done"
)

// ProjectCleanup = `project_cleanups` 表：一次项目删除的物理清理进度。
// 只记通用 ID 与状态，不记 Provider 路径或 Git 字段；补偿靠 sessions.workspace_id 反查会话目录。
type ProjectCleanup struct {
	WorkspaceID string    `gorm:"primaryKey"`
	TenantID    string    `gorm:"not null;index"`
	Status      string    `gorm:"not null;default:pending"`
	Error       string    `gorm:"type:text"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null;default:now()"`
}

func (ProjectCleanup) TableName() string { return "project_cleanups" }

// CleanupProjectStorage 删除一个项目的磁盘资源并收口清理记录（幂等）：
// 整个项目容器（content／runtime／编排组工作区）与所有挂在该项目上的会话私有目录。
// 任一步失败 → 记录留 pending 并带回错误；调用方（删除端点或调度器补偿）只决定怎么报。
func (s *Store) CleanupProjectStorage(ctx context.Context, tenantID, workspaceID string) error {
	if s.workspaceRoot == "" {
		return fmt.Errorf("引擎未配置作业根，无法清理项目磁盘")
	}
	var failure error
	keep := func(err error) {
		if err != nil && failure == nil {
			failure = err
		}
	}
	container, err := projectContainerDir(s.workspaceRoot, tenantID, workspaceID)
	if err != nil {
		keep(err)
	} else if err := os.RemoveAll(container); err != nil {
		keep(fmt.Errorf("删除项目容器 %s：%w", container, err))
	}
	// 会话行即使已软删仍保留 workspace_id，所以补偿每次都能重新找到它们。
	var sessionIDs []string
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id FROM sessions WHERE workspace_id = ? AND tenant_id = ?`, workspaceID, tenantID).
		Scan(&sessionIDs).Error; err != nil {
		keep(fmt.Errorf("查关联会话：%w", err))
	} else {
		for _, sid := range sessionIDs {
			dir, derr := sessionDir(s.workspaceRoot, tenantID, SessionID(sid))
			if derr != nil {
				keep(derr)
				continue
			}
			if rerr := os.RemoveAll(dir); rerr != nil {
				keep(fmt.Errorf("删除会话目录 %s：%w", dir, rerr))
			}
		}
	}
	status, msg := projectCleanupDone, ""
	if failure != nil {
		status, msg = projectCleanupPending, failure.Error()
	}
	res := s.db.WithContext(ctx).Exec(
		`UPDATE project_cleanups SET status = ?, error = ?, updated_at = now() WHERE workspace_id = ?`,
		status, msg, workspaceID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("项目清理记录不存在：%s", workspaceID)
	}
	return failure
}

// compensateProjectCleanups 是调度慢路径：继续清理上次没删干净的项目目录。
// 只扫 pending；标 done 后发 workspace_deleted，让前端把「磁盘清理稍后完成」收掉。
// 单轮最多 32 项，避免一个坏目录拖住整轮调度。
func (ts *TaskScheduler) compensateProjectCleanups(ctx context.Context) {
	var rows []ProjectCleanup
	if err := ts.store.db.WithContext(ctx).
		Where("status = ?", projectCleanupPending).
		Order("updated_at, workspace_id").Limit(32).Find(&rows).Error; err != nil {
		log.Printf("task scheduler project cleanup list: %v", err)
		return
	}
	for _, row := range rows {
		if err := ts.store.CleanupProjectStorage(ctx, row.TenantID, row.WorkspaceID); err != nil {
			log.Printf("task scheduler project cleanup %s: %v", row.WorkspaceID, err)
			continue
		}
		ts.store.PublishEvent(ctx, row.TenantID, eventWorkspaceDeleted, row.WorkspaceID)
	}
}
