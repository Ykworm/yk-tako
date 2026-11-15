// 本文件是存量项目容器的离线布局迁移（方案真源 = doc/76 §8；施工包 §6.3）。
//
// 存量容器的形状（doc/76 之前）：
//
//	tenants/<tid>/workspaces/<wid>/
//	├── 用户文件…                  （直接躺在容器根）
//	├── .media/                    （入口图）
//	├── .tako/{bash,fetch,search}/（全文留存）
//	├── MEMORY.md                  （markdown 模式的项目记忆）
//	tenants/<tid>/.tako/orchestrations/<orchID>/workspace/  （编排组工作现场）
//
// 新形状把内容与运行资源分开（content/ 与 runtime/，见 project_layout.go），
// 编排组工作现场搬进所属项目的 runtime/orchestrations/。迁移是**显式离线命令**：
// 引擎停在旧代码上跑它，或至少没有该项目的新 run／发布会开始（迁移全程持有
// `workspace-pub:<wid>` 的会话级 advisory lock，与 AcquireRun／发布／删除取同一把）。
//
// 崩溃安全的分段：
//
//	建 staging（<容器>.migrating，先写身份标记）
//	→ 复制 + 写清单（migration.json，含每个文件的源/目标/哈希）
//	→ 逐文件复核 staging 的哈希
//	→ 写 layout.json
//	→ 旧容器改名为 <容器>.migrate-backup（写身份标记）
//	→ staging 原子改名为正式容器
//	→ 修 Git worktree 的相对登记
//	→ 按身份标记清 backup 与租户旧编排组目录
//
// 任一步崩溃后再次运行：按 staging／backup／清单三者的身份与阶段对账，能续做就续做，
// 认不出身份的一律报错、不删（doc/76 §8「不把未知目录删了；backup 清理只认本次迁移身份」）。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const (
	// projectMigrateBatchLimit 是单次命令最多处理的项目数（doc/76 §8「有限批量」）。
	projectMigrateBatchLimit = 32
	projectStagingSuffix     = ".migrating"
	projectBackupSuffix      = ".migrate-backup"
	// projectMigrationManifestName 是迁移清单（写在 staging 根，切换后留在新容器根作审计记录）。
	projectMigrationManifestName = "migration.json"
	// projectMigrationMarkerName 是身份标记：staging 与 backup 各一份，认证「这个目录是本次迁移的产物」。
	projectMigrationMarkerName = ".tako-migration.json"
	projectMigrationMarkerKind = "project-layout-migrate"
	projectMigrationFormat     = 1
)

// ProjectLayoutMigrationResult 是一个项目的迁移结果。Status：
//
//	initialized —— 空容器就地建新布局（没有旧字节要搬）
//	migrated    —— 复制、切换完成
//	recovered   —— 上次中断的迁移被对账续完
//	skipped     —— 已是新布局
//	failed      —— 该项目未切换，旧容器仍可用
type ProjectLayoutMigrationResult struct {
	WorkspaceID string
	TenantID    string
	Status      string
	Files       int
	Orches      int
	Warnings    []string
	Err         error
}

// ProjectLayoutMigrationReport 是整批结果；Failed = 任一项目有错误。
type ProjectLayoutMigrationReport struct {
	Results []ProjectLayoutMigrationResult
}

func (r ProjectLayoutMigrationReport) Failed() bool {
	for _, res := range r.Results {
		if res.Status == "failed" || res.Err != nil {
			return true
		}
	}
	return false
}

// projectMigrationMarker 是 staging／backup 的身份标记。
type projectMigrationMarker struct {
	FormatVersion int       `json:"formatVersion"`
	Kind          string    `json:"kind"`
	WorkspaceID   string    `json:"workspaceId"`
	TenantID      string    `json:"tenantId"`
	Container     string    `json:"container"` // 正式容器的作业根相对路径
	CreatedAt     time.Time `json:"createdAt"`
}

// projectMigrationFile 是清单里的一个文件：源、目标、哈希。路径全部相对作业根（报告与
// 清单里都不出现绝对路径，doc/76 §9.7）。
type projectMigrationFile struct {
	Path   string `json:"path"`             // 目标：相对正式容器根
	Source string `json:"source"`           // 源：相对作业根
	SHA256 string `json:"sha256,omitempty"` // 普通文件的字节哈希
	Size   int64  `json:"size,omitempty"`
	Mode   uint32 `json:"mode"`
	Link   string `json:"link,omitempty"` // 符号链接目标（有它就不算普通文件）
}

// projectMigrationOrch 是本项目迁入的一组编排组工作现场。
type projectMigrationOrch struct {
	ID     string `json:"id"`
	Target string `json:"target"` // 相对正式容器根
	Source string `json:"source"` // 相对作业根
}

// projectMigrationManifest 是 migration.json：复制清单 + 完成状态（崩溃后对账的依据）。
type projectMigrationManifest struct {
	FormatVersion int                    `json:"formatVersion"`
	Phase         string                 `json:"phase"` // copied / switched
	WorkspaceID   string                 `json:"workspaceId"`
	TenantID      string                 `json:"tenantId"`
	Container     string                 `json:"container"` // 相对作业根
	Staging       string                 `json:"staging"`
	Backup        string                 `json:"backup"`
	CreatedAt     time.Time              `json:"createdAt"`
	FinishedAt    *time.Time             `json:"finishedAt,omitempty"`
	Files         []projectMigrationFile `json:"files"`
	Orches        []projectMigrationOrch `json:"orches"`
}

type projectMigrationRow struct {
	ID       string `gorm:"column:id"`
	TenantID string `gorm:"column:tenant_id"`
}

// RunProjectLayoutMigration 是离线迁移入口（main 的 -migrate-project-layout）：
// 只连 PG 与工作区，不起 HTTP / Worker / 消费者 / 定时器。
// onlyWorkspaceID 非空时只迁那一个项目；项目不存在是错误（不静默返回空成功）。
func RunProjectLayoutMigration(ctx context.Context, pgDSN, root, onlyWorkspaceID string) (ProjectLayoutMigrationReport, error) {
	var report ProjectLayoutMigrationReport
	abs, err := filepath.Abs(root)
	if err != nil {
		return report, fmt.Errorf("workspace %q 绝对化: %w", root, err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return report, fmt.Errorf("workspace %q 不可用：%v", abs, err)
	}
	db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{TranslateError: true})
	if err != nil {
		return report, fmt.Errorf("pg：%w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return report, fmt.Errorf("pg：%w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return report, fmt.Errorf("pg 不可达：%w", err)
	}
	if err := autoMigrate(db); err != nil {
		return report, fmt.Errorf("pg migrate：%w", err)
	}

	query := `SELECT id, tenant_id FROM workspaces`
	var args []any
	if onlyWorkspaceID != "" {
		query += ` WHERE id = ?`
		args = append(args, onlyWorkspaceID)
	}
	query += ` ORDER BY tenant_id, id LIMIT ?`
	args = append(args, projectMigrateBatchLimit)
	var rows []projectMigrationRow
	if err := db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return report, fmt.Errorf("列项目：%w", err)
	}
	if onlyWorkspaceID != "" && len(rows) == 0 {
		return report, fmt.Errorf("项目不存在：%s", onlyWorkspaceID)
	}

	runner := &projectMigrationRunner{
		db:     db,
		root:   abs,
		locker: newChainFileLocker(newPGFileLocker(db, 0), newFlockFileLocker(abs)),
	}
	for _, row := range rows {
		report.Results = append(report.Results, runner.migrateOne(ctx, row))
	}
	return report, nil
}

type projectMigrationRunner struct {
	db     *gorm.DB
	root   string
	locker FileLocker
}

func (r *projectMigrationRunner) migrateOne(ctx context.Context, p projectMigrationRow) ProjectLayoutMigrationResult {
	res := ProjectLayoutMigrationResult{WorkspaceID: p.ID, TenantID: p.TenantID}
	fail := func(err error) ProjectLayoutMigrationResult {
		res.Status, res.Err = "failed", err
		return res
	}
	container, err := projectContainerDir(r.root, p.TenantID, p.ID)
	if err != nil {
		return fail(err)
	}
	staging, backup := container+projectStagingSuffix, container+projectBackupSuffix

	// 与 AcquireRun／发布／项目删除同一把锁（workspace-pub:<wid>）：
	// 迁移期间该项目开不了新 run、发布也进不来。同一项目第二个迁移者同样等这把锁。
	releaseSession, err := acquireWorkspaceMigrationLock(ctx, r.db, p.ID)
	if err != nil {
		return fail(fmt.Errorf("取项目迁移锁：%w", err))
	}
	defer releaseSession()

	if err := r.assertNoOpenWork(ctx, p); err != nil {
		return fail(err)
	}
	orchIDs, err := r.orchestrationIDs(ctx, p)
	if err != nil {
		return fail(err)
	}
	// 组工作现场与 markdown 项目记忆的写者也走同一套文件锁（跨副本 + 本机）。
	lockCtx := withWorkspace(ctx, WorkspaceRef{
		TenantID:   p.TenantID,
		TenantRoot: tenantStorageRoot(r.root, p.TenantID),
	})
	keys := []string{"workspace:" + p.ID}
	for _, id := range orchIDs {
		keys = append(keys, "orch-workspace:"+id)
	}
	releaseFiles, err := r.locker.Lock(lockCtx, keys)
	if err != nil {
		return fail(fmt.Errorf("取项目文件锁：%w", err))
	}
	defer releaseFiles()

	recovered, err := r.reconcile(p, container, staging, backup)
	if err != nil {
		return fail(err)
	}
	if recovered {
		res.Status = "recovered"
		// 对账可能刚完成切换；下面按「已就绪」路径做收尾，不重复迁移。
	}

	ready, err := projectLayoutReady(r.root, p.TenantID, p.ID)
	if err != nil {
		return fail(err)
	}
	if ready {
		if res.Status == "" {
			res.Status = "skipped"
		}
		// 修 worktree 也可能在切换后、清理 backup 后才失败：重跑本命令时在这里补做，
		// 所以 repair 失败是警告而不是死局（bare 仓与 worktree 都在新容器里，不需要 backup）。
		if err := r.repairWorktrees(ctx, container); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("Git worktree 修复失败：%v（修好后重跑本命令）", err))
		}
		r.cleanContainerMarker(container, p)
		return res
	}

	// 现在才看陈旧运行：对账完成切换的项目不该再被这里挡住。
	if err := r.assertNoOpenWork(ctx, p); err != nil {
		return fail(err)
	}
	entries, err := os.ReadDir(container)
	if err != nil && !os.IsNotExist(err) {
		return fail(err)
	}
	if len(entries) == 0 {
		if _, _, err := ensureProjectLayout(r.root, p.TenantID, p.ID); err != nil {
			return fail(err)
		}
		if res.Status == "" {
			res.Status = "initialized"
		}
		return res
	}

	files, orches, warnings, err := r.migrateContainer(ctx, p, container, staging, backup, orchIDs)
	res.Files, res.Orches, res.Warnings = files, orches, warnings
	if err != nil {
		return fail(err)
	}
	if res.Status == "" {
		res.Status = "migrated"
	}
	return res
}

// assertNoOpenWork 是迁移准入：有活 run／未收口组／进行中的发布／没落终态的 Task
// 就拒迁（doc/76 §8「检查无活 run／未收口组／发布」）。迁移期间新 run 由会话级锁挡住。
func (r *projectMigrationRunner) assertNoOpenWork(ctx context.Context, p projectMigrationRow) error {
	db := r.db.WithContext(ctx)
	check := []struct {
		label string
		query string
	}{
		{"活 run", `SELECT COUNT(*) FROM runs WHERE workspace_id = ? AND status = 'running' AND lease_until > now()`},
		{"未收口编排组", `SELECT COUNT(*) FROM agent_orchestrations WHERE workspace_id = ? AND status IN ('active', 'cancelling', 'preparing', 'waiting_previous')`},
		{"进行中的发布", `SELECT COUNT(*) FROM agent_orchestrations WHERE workspace_id = ? AND publication_status IN ('publishing', 'blocked')`},
		{"未落终态的 Task", `SELECT COUNT(*) FROM agent_tasks WHERE workspace_id = ? AND status IN ('pending', 'ready', 'running', 'publishing')`},
	}
	for _, c := range check {
		var n int64
		if err := db.Raw(c.query, p.ID).Scan(&n).Error; err != nil {
			return fmt.Errorf("查%s：%w", c.label, err)
		}
		if n > 0 {
			return fmt.Errorf("项目 %s 还有 %d 个%s，先等它收口再迁移", p.ID, n, c.label)
		}
	}
	return nil
}

// orchestrationIDs 列出属于该项目的编排组 id（旧工作现场按它们从租户目录搬到项目容器）。
func (r *projectMigrationRunner) orchestrationIDs(ctx context.Context, p projectMigrationRow) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).Raw(
		`SELECT id FROM agent_orchestrations WHERE tenant_id = ? AND workspace_id = ? ORDER BY id`,
		p.TenantID, p.ID).Scan(&ids).Error
	if err != nil {
		return nil, fmt.Errorf("列编排组：%w", err)
	}
	return ids, nil
}

// ── 崩溃对账 ────────────────────────────────────────────────────────────────

// reconcile 处理上次中断留下的 staging／backup。返回 true = 本次对账已经完成迁移。
// 认不出身份的目录一律报错，不删——「未知目录」可能是用户自己的东西。
func (r *projectMigrationRunner) reconcile(p projectMigrationRow, container, staging, backup string) (bool, error) {
	containerRel, err := relToRoot(r.root, container)
	if err != nil {
		return false, err
	}
	stagingExists := dirExists(staging)
	backupExists := dirExists(backup)

	// backup：旧容器已改名。新容器已有 layout = 切换已完成，按身份清 backup；
	// 新容器不在 = 崩在「改名」与「staging 改名」之间，恢复旧容器后重新迁移。
	if backupExists {
		marker, err := readMigrationMarker(backup)
		if err != nil {
			return false, fmt.Errorf("backup 目录 %s 身份标记不可读（不删未知目录）：%w", filepath.Base(backup), err)
		}
		if marker == nil || marker.Kind != projectMigrationMarkerKind ||
			marker.WorkspaceID != p.ID || marker.TenantID != p.TenantID || marker.Container != containerRel {
			return false, fmt.Errorf("backup 目录 %s 不是本项目本次迁移的产物（不删未知目录）", filepath.Base(backup))
		}
		if dirExists(container) {
			if err := os.RemoveAll(backup); err != nil {
				return false, fmt.Errorf("清理 backup：%w", err)
			}
			return false, nil
		}
		if err := os.Remove(filepath.Join(backup, projectMigrationMarkerName)); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		if err := os.Rename(backup, container); err != nil {
			return false, fmt.Errorf("恢复旧容器：%w", err)
		}
		backupExists = false
	}

	if !stagingExists {
		return false, nil
	}
	marker, err := readMigrationMarker(staging)
	if err != nil {
		return false, fmt.Errorf("staging 目录身份标记不可读（不删未知目录）：%w", err)
	}
	if marker == nil || marker.Kind != projectMigrationMarkerKind ||
		marker.WorkspaceID != p.ID || marker.TenantID != p.TenantID || marker.Container != containerRel {
		return false, fmt.Errorf("staging 目录 %s 不是本项目本次迁移的产物（不删未知目录）", filepath.Base(staging))
	}
	manifest, err := readProjectMigrationManifest(staging)
	if err != nil {
		return false, err
	}
	if manifest == nil {
		// 复制阶段崩了：清单未落，staging 只是半成品（身份已验证），清掉重来。
		if err := os.RemoveAll(staging); err != nil {
			return false, fmt.Errorf("清理半成品 staging：%w", err)
		}
		return false, nil
	}
	if !dirExists(container) {
		return false, fmt.Errorf("staging 清单已就绪但正式容器与 backup 都不在（状态无法解释），请人工检查 %s", filepath.Base(staging))
	}
	// 正式容器还在（没写过 layout）：切换从未开始，staging 可安全重来。
	if err := os.RemoveAll(staging); err != nil {
		return false, fmt.Errorf("清理上次 staging：%w", err)
	}
	return false, nil
}

// ── 容器迁移主体 ────────────────────────────────────────────────────────────

func (r *projectMigrationRunner) migrateContainer(ctx context.Context, p projectMigrationRow,
	container, staging, backup string, orchIDs []string) (files, orches int, warnings []string, err error) {

	// reconcile 已经把可识别的残留处理完；这里还有冲突目录 = 不能碰。
	if dirExists(staging) || dirExists(backup) {
		return 0, 0, nil, fmt.Errorf("staging/backup 目录仍存在且无法对账，拒绝迁移")
	}
	containerRel, err := relToRoot(r.root, container)
	if err != nil {
		return 0, 0, nil, err
	}
	manifest := projectMigrationManifest{
		FormatVersion: projectMigrationFormat,
		Phase:         "copied",
		WorkspaceID:   p.ID,
		TenantID:      p.TenantID,
		Container:     containerRel,
		Staging:       containerRel + projectStagingSuffix,
		Backup:        containerRel + projectBackupSuffix,
		CreatedAt:     time.Now().UTC(),
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return 0, 0, nil, err
	}
	// 身份标记最先写：崩溃后只剩一个半截 staging 时，只有认得出它是我们建的才允许删。
	if err := writeMigrationMarker(staging, projectMigrationMarker{
		FormatVersion: projectMigrationFormat,
		Kind:          projectMigrationMarkerKind,
		WorkspaceID:   p.ID,
		TenantID:      p.TenantID,
		Container:     containerRel,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		return 0, 0, nil, err
	}
	abort := func(err error) (int, int, []string, error) {
		// 任何失败都清掉自己建的 staging（身份已核对），旧容器原样不动。
		_ = os.RemoveAll(staging)
		return 0, 0, nil, err
	}
	contentDir := filepath.Join(staging, projectContentDirName)
	runtimeDir := filepath.Join(staging, projectRuntimeDirName)
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return abort(err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return abort(err)
	}

	// 1) 容器根：用户文件 → content/；.media → runtime/media；.tako/{bash,fetch,search} →
	//    runtime/tmp/{kind}；MEMORY.md → runtime/memory。
	entries, err := os.ReadDir(container)
	if err != nil {
		return abort(err)
	}
	for _, e := range entries {
		name := e.Name()
		src := filepath.Join(container, name)
		switch {
		case name == projectLayoutFileName:
			return abort(fmt.Errorf("存量容器里已有 %s（不该走迁移）", projectLayoutFileName))
		case name == memoryWorkspaceFileName:
			entry, cerr := r.copyPath(src, staging, filepath.Join(projectRuntimeDirName, "memory", name))
			if cerr != nil {
				return abort(cerr)
			}
			manifest.Files = append(manifest.Files, entry...)
		case name == ".media":
			entry, cerr := r.copyPath(src, staging, filepath.Join(projectRuntimeDirName, projectMediaDirName))
			if cerr != nil {
				return abort(cerr)
			}
			manifest.Files = append(manifest.Files, entry...)
		case name == ".tako":
			subs, rerr := os.ReadDir(src)
			if rerr != nil {
				return abort(rerr)
			}
			for _, sub := range subs {
				kind := sub.Name()
				switch kind {
				case "bash", "fetch", "search":
					// 旧逻辑路径 .tako/<kind> 对应新 runtime/tmp/<kind>（project_layout.go 的映射）。
					entry, cerr := r.copyPath(filepath.Join(src, kind), staging,
						filepath.Join(projectRuntimeDirName, "tmp", kind))
					if cerr != nil {
						return abort(cerr)
					}
					manifest.Files = append(manifest.Files, entry...)
				default:
					return abort(fmt.Errorf("容器里发现未知运行时目录 .tako/%s，不迁移（不删未知内容）", kind))
				}
			}
		default:
			entry, cerr := r.copyPath(src, staging, filepath.Join(projectContentDirName, name))
			if cerr != nil {
				return abort(cerr)
			}
			manifest.Files = append(manifest.Files, entry...)
		}
	}

	// 2) 编排组工作现场：租户级 .tako/orchestrations/<orchID>/workspace → 本项目 runtime/orchestrations/。
	tenantRoot := tenantStorageRoot(r.root, p.TenantID)
	for _, orchID := range orchIDs {
		src := filepath.Join(tenantRoot, ".tako", "orchestrations", orchID, "workspace")
		if !dirExists(src) {
			continue
		}
		target := filepath.Join(projectRuntimeDirName, "orchestrations", orchID, "workspace")
		entry, cerr := r.copyPath(src, staging, target)
		if cerr != nil {
			return abort(cerr)
		}
		manifest.Files = append(manifest.Files, entry...)
		srcRel, rerr := relToRoot(r.root, src)
		if rerr != nil {
			return abort(rerr)
		}
		manifest.Orches = append(manifest.Orches, projectMigrationOrch{
			ID: orchID, Target: target, Source: srcRel,
		})
	}

	if err := writeProjectMigrationManifest(staging, &manifest); err != nil {
		return abort(err)
	}
	if err := verifyProjectMigrationManifest(staging, &manifest); err != nil {
		return abort(fmt.Errorf("校验迁移清单：%w", err))
	}
	if err := writeProjectLayoutAfterMigration(staging, manifest.CreatedAt); err != nil {
		return abort(err)
	}

	// 3) 切换：旧容器 → backup（写身份标记）→ staging → 正式容器。
	if err := os.Rename(container, backup); err != nil {
		return abort(fmt.Errorf("旧容器改名为 backup：%w", err))
	}
	if err := writeMigrationMarker(backup, projectMigrationMarker{
		FormatVersion: projectMigrationFormat,
		Kind:          projectMigrationMarkerKind,
		WorkspaceID:   p.ID,
		TenantID:      p.TenantID,
		Container:     containerRel,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		// backup 标记写不出来 = 以后无法证明它属于本次迁移；恢复旧容器，不冒险。
		if rerr := os.Rename(backup, container); rerr != nil {
			return 0, 0, nil, fmt.Errorf("写 backup 标记失败且恢复旧容器失败：%v / %v", err, rerr)
		}
		_ = os.RemoveAll(staging)
		return 0, 0, nil, fmt.Errorf("写 backup 标记：%w", err)
	}
	if err := os.Rename(staging, container); err != nil {
		// 切换失败：旧容器换回原位（先摘掉标记），staging 留着等下一次对账清。
		_ = os.Remove(filepath.Join(backup, projectMigrationMarkerName))
		if rerr := os.Rename(backup, container); rerr != nil {
			return 0, 0, nil, fmt.Errorf("staging 切换失败且恢复旧容器失败：%v / %v", err, rerr)
		}
		return 0, 0, nil, fmt.Errorf("staging 切换为正式容器：%w", err)
	}
	_ = os.Remove(filepath.Join(container, projectMigrationMarkerName))
	now := time.Now().UTC()
	manifest.Phase, manifest.FinishedAt = "switched", &now
	if err := writeProjectMigrationManifest(container, &manifest); err != nil {
		warnings = append(warnings, fmt.Sprintf("清单未更新为 switched：%v", err))
	}

	// 4) 先清租户旧编排组目录（字节已复制并校验过）：旧的 repo.git 还在时，
	//    `git worktree repair` 会认为 worktree 的 .git 指过去的 gitdir 依然有效，
	//    只修 bare 仓那一侧；旧路径先消失，两侧才会一起改到新位置。
	for _, o := range manifest.Orches {
		src := filepath.Join(r.root, filepath.FromSlash(o.Source))
		if err := os.RemoveAll(src); err != nil {
			warnings = append(warnings, fmt.Sprintf("旧编排组目录未清掉 %s：%v", o.Source, err))
			continue
		}
		// workspace 搬走后空的 <orchestrations>/<orchID> 壳也收掉（非空不动）。
		_ = os.Remove(filepath.Dir(src))
	}
	// 空的 .tako/orchestrations 壳也收掉（非空不动）。
	_ = os.Remove(filepath.Join(tenantRoot, ".tako", "orchestrations"))

	// 5) Git worktree 的绝对登记随目录改名失效，切完按新路径修复（只认 provider/repo.git 存在时）。
	if err := r.repairWorktrees(ctx, container); err != nil {
		warnings = append(warnings, fmt.Sprintf("Git worktree 修复失败：%v（修好后重跑本命令）", err))
	}

	// 6) 清 backup（复制与校验都已通过、worktree 也修过了；修失败时 backup 也不含可用登记，
	//    重跑命令会按新容器再修一次）。
	if err := os.RemoveAll(backup); err != nil {
		warnings = append(warnings, fmt.Sprintf("backup 未清掉：%v", err))
	}

	return len(manifest.Files), len(manifest.Orches), warnings, nil
}

// repairWorktrees 修 Git 组工作现场的登记：bare 仓与 worktree 一起搬了目录，
// `.git` 文件与 repo.git/worktrees/* 里的绝对路径都过期。`git worktree repair <新路径…>`
// 会双向重建连接（doc/76 §8 的「Git worktree 修复」）。
func (r *projectMigrationRunner) repairWorktrees(ctx context.Context, container string) error {
	orchRoot := filepath.Join(container, projectRuntimeDirName, "orchestrations")
	orches, err := os.ReadDir(orchRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, o := range orches {
		if !o.IsDir() {
			continue
		}
		dir := filepath.Join(orchRoot, o.Name(), "workspace")
		if !dirExists(filepath.Join(dir, "provider", "repo.git")) {
			continue // Directory Provider 没有 Git 登记
		}
		workRoot := filepath.Join(dir, "work")
		entries, rerr := os.ReadDir(workRoot)
		if os.IsNotExist(rerr) {
			continue
		}
		if rerr != nil {
			return rerr
		}
		paths := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() && fileExists(filepath.Join(workRoot, e.Name(), ".git")) {
				paths = append(paths, filepath.Join(workRoot, e.Name()))
			}
		}
		if len(paths) == 0 {
			continue
		}
		args := append([]string{"--git-dir=" + filepath.Join(dir, "provider", "repo.git"), "worktree", "repair"}, paths...)
		if _, gerr := runGit(ctx, "", args...); gerr != nil {
			return fmt.Errorf("组 %s：%w", o.Name(), gerr)
		}
	}
	return nil
}

// cleanContainerMarker 收掉已经切换完成但没来得及删的身份标记（只在身份匹配时删）。
func (r *projectMigrationRunner) cleanContainerMarker(container string, p projectMigrationRow) {
	marker, err := readMigrationMarker(container)
	if err != nil || marker == nil {
		return
	}
	containerRel, relErr := relToRoot(r.root, container)
	if relErr != nil {
		return
	}
	if marker.Kind == projectMigrationMarkerKind && marker.WorkspaceID == p.ID &&
		marker.TenantID == p.TenantID && marker.Container == containerRel {
		_ = os.Remove(filepath.Join(container, projectMigrationMarkerName))
	}
}

// ── 复制与校验 ──────────────────────────────────────────────────────────────

// copyPath 把 src（文件、目录或符号链接）复制到 staging 下的 destRel。
// 目录递归；符号链接按链接复制（不跟随），其余特殊文件拒绝迁移。
func (r *projectMigrationRunner) copyPath(src, staging, destRel string) ([]projectMigrationFile, error) {
	var out []projectMigrationFile
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		targetRel := destRel
		if rel != "." {
			targetRel = filepath.Join(destRel, rel)
		}
		target := filepath.Join(staging, targetRel)
		srcRel, rerr := relToRoot(r.root, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		entry := projectMigrationFile{
			Path:   filepath.ToSlash(targetRel),
			Source: srcRel,
			Mode:   uint32(info.Mode().Perm()),
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			entry.Link = link
			out = append(out, entry)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("不支持迁移的文件类型（%s）：%s", info.Mode().Type(), srcRel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		sum, size, cerr := copyRegularFile(path, target, info.Mode().Perm())
		if cerr != nil {
			return cerr
		}
		entry.SHA256, entry.Size = sum, size
		out = append(out, entry)
		return nil
	})
	return out, err
}

// copyRegularFile 复制普通文件并回读目标算哈希（写出去再读回来才算校验过目标字节）。
func copyRegularFile(src, dst string, mode os.FileMode) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return "", 0, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	// 保留可执行位等权限（umask 会削掉部分位）。
	if err := os.Chmod(dst, mode); err != nil {
		return "", 0, err
	}
	return hashFile(dst)
}

// verifyProjectMigrationManifest 复核 staging：清单里每个文件的哈希/大小/链接一致，
// 且 staging 里没有清单之外的条目（多出来的字节说明复制逻辑漏了映射）。
// layout.json、清单本身与身份标记是生成物，不参与条目比对。
func verifyProjectMigrationManifest(staging string, manifest *projectMigrationManifest) error {
	want := make(map[string]projectMigrationFile, len(manifest.Files))
	for _, f := range manifest.Files {
		want[f.Path] = f
	}
	seen := make(map[string]bool, len(want))
	err := filepath.WalkDir(staging, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(staging, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		switch rel {
		case projectLayoutFileName, projectMigrationManifestName, projectMigrationMarkerName:
			return nil
		}
		f, ok := want[rel]
		if !ok {
			return fmt.Errorf("staging 里有清单之外的条目：%s", rel)
		}
		seen[rel] = true
		if f.Link != "" {
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return fmt.Errorf("读符号链接 %s：%w", rel, lerr)
			}
			if link != f.Link {
				return fmt.Errorf("符号链接 %s 目标不一致：%q != %q", rel, link, f.Link)
			}
			return nil
		}
		sum, size, herr := hashFile(path)
		if herr != nil {
			return herr
		}
		if size != f.Size || sum != f.SHA256 {
			return fmt.Errorf("文件 %s 校验不一致（size %d/%d）", rel, size, f.Size)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(want) {
		for p := range want {
			if !seen[p] {
				return fmt.Errorf("清单里的文件不存在：%s", p)
			}
		}
	}
	return nil
}

// ── 清单与标记的读写 ────────────────────────────────────────────────────────

func writeProjectMigrationManifest(container string, m *projectMigrationManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(container, projectMigrationManifestName), b, 0o644)
}

func readProjectMigrationManifest(container string) (*projectMigrationManifest, error) {
	b, err := os.ReadFile(filepath.Join(container, projectMigrationManifestName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m projectMigrationManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("迁移清单损坏：%w", err)
	}
	return &m, nil
}

func writeMigrationMarker(dir string, m projectMigrationMarker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, projectMigrationMarkerName), b, 0o644)
}

func readMigrationMarker(dir string) (*projectMigrationMarker, error) {
	b, err := os.ReadFile(filepath.Join(dir, projectMigrationMarkerName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m projectMigrationMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("身份标记损坏：%w", err)
	}
	return &m, nil
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func relToRoot(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("路径不在作业根下：%s", path)
	}
	return filepath.ToSlash(rel), nil
}

// acquireWorkspaceMigrationLock 取项目级迁移锁：与 AcquireRun／发布／项目删除的
// pg_advisory_xact_lock('workspace-pub:'||wid) 同一把锁（PG 的 advisory 锁空间不分
// 会话级与事务级）。迁移全程持有 → 新 run 与发布进不来；第二个迁移者同样排队。
func acquireWorkspaceMigrationLock(ctx context.Context, db *gorm.DB, workspaceID string) (func(), error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("取数据库连接池：%w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("取独占连接：%w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var ok bool
		if err := conn.QueryRowContext(ctx,
			`SELECT pg_try_advisory_lock(hashtextextended('workspace-pub:' || $1, 0))`, workspaceID).Scan(&ok); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			_ = conn.Close()
			return nil, errors.New("等项目迁移锁超时（另一个迁移者或发布还在跑）")
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		var released bool
		_ = conn.QueryRowContext(context.Background(),
			`SELECT pg_advisory_unlock(hashtextextended('workspace-pub:' || $1, 0))`, workspaceID).Scan(&released)
		_ = conn.Close()
	}, nil
}
