package runengine

// 本文件：记忆层的**存量迁移**（doc/65 §6.1；施工包 §7.3）。
//
// 迁什么、为什么迁：
//
//	notes 表（旧的常驻笔记线）  → memory_items 的 resident 层
//	notes 里 scope='tenant' 的  → 租户 runtime config 的 teamInstructions（不进记忆轴）
//	旧的 memory_items 行        → layer='recall'、scope='workspace'、workspace 填项目 id
//	旧的 kind='episode' 的行    → state='archived'（episode 不再参与召回）
//	旧的 kind/topic 列与 notes 表 → 复制完成后删掉
//
// 三条硬要求：
//
//  1. **顺序**：先加新列 → 复制 → 才删旧列。旧列是迁移的**输入**（靠 kind 认出 episode 行），
//     提前删了就再也认不出来。删列在复制之后（这里）。
//  2. **可重入**：第二步跑，memory_items 的行数不变、teamInstructions 的内容**完全一致**
//     （不翻倍）。做法是「每一步都以『输入还在不在』为闸」——输入没了（表已删 / 列已删），
//     那一步就整步跳过；输入还在的那一步必须逐行幂等（notes 复制保留原 id → 主键挡住重复）。
//  3. **带迁移锁**：多副本同时启动时只有一个跑迁移（PG advisory lock），别的等。
//
// 迁移锁用**会话级** advisory lock：它挂在 PG 后端会话上，连接断了 PG 自动放锁，
// 进程被 kill -9 也不会留下死锁（锁随会话一起去死）。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// memoryMigrateLockKey 是迁移锁的逻辑键（哈希后进 PG advisory lock）。
const memoryMigrateLockKey = "tako-memory-migrate"

// MigrateMemorySchema 是记忆层的存量迁移入口（启动装配期调一次）。
//
// 它**不是**「run 里隐式搬家」（doc/65 §6.2）——只在启动装配期跑，跑的是数据搬运，
// 不改变运行模式。模式互切 = 改 TAKO_MEMORY_MODE 后重启引擎：双向的搬家都按新模式
// 在启动期跑（markdown→PG 见 migrateMarkdownToPipeline，PG→markdown 见 migratePipelineToMarkdown）。
func (s *Store) MigrateMemorySchema(ctx context.Context) error {
	release, err := s.acquireMigrationLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	if err := s.migrateLegacyMemoryItems(ctx); err != nil {
		return fmt.Errorf("迁移存量 memory_items：%w", err)
	}
	if err := s.migrateLegacyNotes(ctx); err != nil {
		return fmt.Errorf("迁移存量 notes：%w", err)
	}
	if err := s.dropLegacyMemorySchema(ctx); err != nil {
		return fmt.Errorf("清理旧结构：%w", err)
	}
	return nil
}

// acquireMigrationLock 取迁移锁（会话级 PG advisory lock）。
//
// 独占连接 + 会话级锁：连接断 → PG 自动放锁（进程被 kill -9 也不会留死锁）。
// 绝不把还持锁的连接还回连接池（那会让后来者一直等不到锁）。
func (s *Store) acquireMigrationLock(ctx context.Context) (func(), error) {
	sqlDB, err := s.db.DB()
	if err != nil {
		return nil, fmt.Errorf("取数据库连接池失败：%w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("取独占连接失败：%w", err)
	}
	var locked bool
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, memoryMigrateLockKey).Scan(&locked); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("取迁移锁失败：%w", err)
	}
	if !locked {
		// 别的副本正在跑迁移：等它跑完（轮询 + 30 秒上限），别并发搬同一批数据。
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			var ok bool
			if err := conn.QueryRowContext(ctx,
				`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, memoryMigrateLockKey).Scan(&ok); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("等迁移锁失败：%w", err)
			}
			if ok {
				locked = true
				break
			}
		}
		if !locked {
			_ = conn.Close()
			return nil, errors.New("等迁移锁超时（另一个副本正在迁移记忆表）")
		}
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		var released bool
		if err := conn.QueryRowContext(context.Background(),
			`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, memoryMigrateLockKey).Scan(&released); err != nil {
			log.Printf("memory migrate: 解锁失败，断开连接：%v", err)
			discardMigrationConn(conn)
			return
		}
		_ = conn.Close()
	}, nil
}

// discardMigrationConn 让连接真的断开（不还回池子）。
func discardMigrationConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// tableExists 判断一张表在不在（迁移的每一步都以「输入还在不在」为闸）。
func tableExists(ctx context.Context, db *gorm.DB, name string) (bool, error) {
	var reg string
	err := db.WithContext(ctx).Raw(`SELECT COALESCE(to_regclass($1)::text, '')`, name).Scan(&reg).Error
	if err != nil {
		return false, err
	}
	return reg != "", nil
}

// columnExists 判断一列在不在。
func columnExists(ctx context.Context, db *gorm.DB, table, column string) (bool, error) {
	var n int
	err := db.WithContext(ctx).Raw(
		`SELECT count(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?`,
		table, column).Scan(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// migrateLegacyMemoryItems 把旧结构的 memory_items 行迁到新结构。
//
// 旧行的样子：kind/topic 两列分类 + 正文去重唯一索引 + workspace 列装的是**引擎工作区根路径**。
// 新行：layer/scope/state + 无去重 + workspace 列是**项目 id**。
//
// 怎么认出「这是旧行」：**kind 列非空**——新结构没有这一列，迁移窗口内它是旧行的精确标记
// （这正是「先不删旧列」的理由：提前删了就没法区分旧行，也认不出 episode 行）。
//
// 幂等：步骤以「kind 列还在」为闸；列一删，这一步整步跳过。行级更新是幂等的（只改列不改行数）。
func (s *Store) migrateLegacyMemoryItems(ctx context.Context) error {
	hasKind, err := columnExists(ctx, s.db, "memory_items", "kind")
	if err != nil {
		return err
	}
	if !hasKind {
		return nil // 已经是新结构（或全新库）：没什么可迁
	}
	// 旧行的 workspace 是引擎根路径 → 换成**来源 run 挂的项目 id**（查不到就退到该租户的
	// 默认项目；再查不到就留空并记账——留空 = 这行不可见，比塞一个错的项目 id 好）。
	if err := s.backfillLegacyWorkspace(ctx); err != nil {
		return err
	}
	// 旧行一律是召回层、项目可见域（doc/65 §6.1：存量 memory_items 迁入 layer='recall'、scope='workspace'）。
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE memory_items
		    SET layer = 'recall', scope = 'workspace', state = CASE WHEN state = '' THEN 'active' ELSE state END
		  WHERE kind IS NOT NULL`).Error; err != nil {
		return err
	}
	// episode 行归档：不再参与召回（doc/65 §6.1）。left_active_at 只在首次离开 active 时写。
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE memory_items
		    SET state = 'archived', left_active_at = COALESCE(left_active_at, now())
		  WHERE kind = 'episode' AND state = 'active'`).Error; err != nil {
		return err
	}
	var n int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM memory_items WHERE kind IS NOT NULL`).Scan(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		log.Printf("memory migrate: 存量 memory_items %d 行已迁为 recall/workspace", n)
	}
	return nil
}

// backfillLegacyWorkspace 把旧行的 workspace（引擎工作区根路径）换成项目 id。
//
// 取值顺序：来源 run 挂的项目 → 该租户的默认项目 → 空。
// 「来源 run 的项目」最准（旧行是从哪场 run 抽出来的，那场 run 知道自己在哪个项目）。
func (s *Store) backfillLegacyWorkspace(ctx context.Context) error {
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE memory_items m
		    SET workspace = COALESCE(
		          (SELECT r.workspace_id FROM runs r WHERE r.id = m.source_run_id AND r.workspace_id <> ''),
		          (SELECT w.id FROM workspaces w
		             WHERE w.tenant_id = m.tenant_id AND w.is_default
		             ORDER BY w.created_at, w.id LIMIT 1),
		          '')
		  WHERE m.kind IS NOT NULL`).Error; err != nil {
		return err
	}
	var blank int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM memory_items WHERE kind IS NOT NULL AND workspace = ''`).Scan(&blank).Error; err != nil {
		return err
	}
	if blank > 0 {
		log.Printf("memory migrate: %d 行旧记忆查不到所属项目（来源 run 已删且无默认项目）——保留为空，这些行在项目视图里不可见", blank)
	}
	return nil
}

// migrateLegacyNotes 把旧 notes 表搬进新结构。
//
//   - scope='user' 的行 → memory_items 的 resident 层（**保留原 id** → 主键挡住重复复制）；
//   - scope='tenant' 的行 → 租户 runtime config 的 teamInstructions（不进记忆轴：
//     团队规范不是某个人或某个项目的记忆，它是租户系统提示材料）。
//
// 幂等：notes 表在 → 复制（逐行 ON CONFLICT DO NOTHING）；不在 → 整步跳过。
// teamInstructions 的追加按**内容比对**（已在里面就不重复加）。
func (s *Store) migrateLegacyNotes(ctx context.Context) error {
	exists, err := tableExists(ctx, s.db, "notes")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	type legacyNote struct {
		ID           string
		Workspace    string
		Kind         string
		Content      string
		Quote        string
		SourceRunID  string
		SourceSeq    int64
		SupersededBy *string
		TenantID     string
		UserID       string
		Scope        string
	}
	var rows []legacyNote
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id, workspace, kind, content, quote, source_run_id, source_seq,
		        superseded_by, tenant_id, user_id, scope
		   FROM notes ORDER BY created_at, id`).Scan(&rows).Error; err != nil {
		return err
	}
	teamByTenant := map[string][]string{}
	migrated := 0
	for _, n := range rows {
		content := strings.TrimSpace(n.Content)
		if content == "" {
			continue
		}
		if n.Scope == "tenant" {
			// 团队规范 → 租户配置（不进记忆轴）。
			if n.TenantID == "" {
				continue
			}
			teamByTenant[n.TenantID] = append(teamByTenant[n.TenantID], content)
			continue
		}
		// 个人/项目记忆 → resident 层。
		item := MemoryItem{
			ID:          n.ID,
			TenantID:    n.TenantID,
			UserID:      n.UserID,
			Workspace:   legacyWorkspaceOf(ctx, s, n.SourceRunID, n.TenantID),
			Layer:       MemoryLayerResident,
			Scope:       MemoryScopeUser,
			Content:     content,
			Quote:       n.Quote,
			State:       MemoryStateActive,
			Source:      MemorySourceUser,
			SourceRunID: RunID(n.SourceRunID),
			SourceSeq:   n.SourceSeq,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if n.SupersededBy != nil && *n.SupersededBy != "" {
			sb := *n.SupersededBy
			item.SupersededBy = &sb
			item.State = MemoryStateSuperseded
			item.LeftActiveAt = &item.CreatedAt
		}
		// 保留原 id → 重复复制撞主键 → DO NOTHING（幂等）。
		if err := s.db.WithContext(ctx).Exec(
			`INSERT INTO memory_items (id, tenant_id, user_id, workspace, layer, scope, content, quote,
			        state, superseded_by, source, source_session_id, source_run_id, source_seq,
			        created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, now(), now())
			 ON CONFLICT (id) DO NOTHING`,
			item.ID, item.TenantID, item.UserID, item.Workspace, item.Layer, item.Scope,
			item.Content, item.Quote, item.State, item.SupersededBy, item.Source,
			item.SourceRunID, item.SourceSeq).Error; err != nil {
			return err
		}
		migrated++
	}
	if migrated > 0 {
		log.Printf("memory migrate: notes 表 %d 行已迁入 resident 层", migrated)
	}
	// 团队规范 → 租户配置。
	for tenantID, lines := range teamByTenant {
		if err := s.appendTeamInstructions(ctx, tenantID, lines); err != nil {
			return err
		}
	}
	return nil
}

// legacyWorkspaceOf 查旧 note 所属的项目 id（来源 run 挂的项目 → 租户默认项目 → 空）。
func legacyWorkspaceOf(ctx context.Context, s *Store, sourceRunID, tenantID string) string {
	if sourceRunID != "" {
		var wid string
		if err := s.db.WithContext(ctx).Raw(
			`SELECT workspace_id FROM runs WHERE id = ?`, sourceRunID).Scan(&wid).Error; err == nil && wid != "" {
			return wid
		}
	}
	if tenantID != "" {
		var wid string
		if err := s.db.WithContext(ctx).Raw(
			`SELECT id FROM workspaces WHERE tenant_id = ? AND is_default ORDER BY created_at, id LIMIT 1`,
			tenantID).Scan(&wid).Error; err == nil && wid != "" {
			return wid
		}
	}
	return ""
}

// appendTeamInstructions 把团队规范追加进租户 runtime config 的 teamInstructions。
//
// 幂等：**逐条比对**——已经在里面的内容不重复加。所以第二次迁移（notes 表还在但内容已加过）
// 结果与第一次完全一致。
func (s *Store) appendTeamInstructions(ctx context.Context, tenantID string, lines []string) error {
	if tenantID == "" || len(lines) == 0 {
		return nil
	}
	cfg, err := s.GetRuntimeConfig(ctx, tenantID)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, l := range strings.Split(cfg.TeamInstructions, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			existing[l] = true
		}
	}
	added := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || existing[l] {
			continue
		}
		existing[l] = true
		added++
		if cfg.TeamInstructions == "" {
			cfg.TeamInstructions = l
		} else {
			cfg.TeamInstructions = cfg.TeamInstructions + "\n" + l
		}
	}
	if added == 0 {
		return nil
	}
	// 迁移没有登录身份：用租户归属者记 updated_by（审计列不许空）。
	owner := ""
	_ = s.db.WithContext(ctx).Raw(
		`SELECT user_id FROM tenant_members WHERE tenant_id = ? AND role = 'owner' ORDER BY created_at LIMIT 1`,
		tenantID).Scan(&owner).Error
	if err := s.PutRuntimeConfig(ctx, tenantID, owner, cfg); err != nil {
		return err
	}
	log.Printf("memory migrate: 租户 %s 的 %d 条团队规范已并入 teamInstructions", tenantID, added)
	return nil
}

// dropLegacyMemorySchema 复制完成后清掉旧结构。
//
// 顺序有讲究（每一步都有依赖关系）：
//  1. **先删两个视图**——视图是 `SELECT * FROM memory_items`，它对表的**行类型**有依赖，
//     列还在时不能删列（PG：cannot drop column ... because other objects depend on it）。
//     视图由 ensureMemoryViews 重建（EnsureMemoryIndexes 在启动装配期会再调一次）。
//  2. 摘掉 memory_jobs.run_id 的唯一索引（EndRun 已改走 dedup_key，索引留着反而挡着
//     「一个 run 多条任务」这条要求）。
//  3. 删依赖 kind/topic 的非唯一索引，再删 kind/topic 两列。
//  4. 删 notes 表。
//
// 每步都以「还在不在」为闸（IF EXISTS / to_regclass），所以第二次跑整步跳过 = 幂等。
func (s *Store) dropLegacyMemorySchema(ctx context.Context) error {
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS resident_notes`,
		`DROP VIEW IF EXISTS recall_items`,
		`DROP INDEX IF EXISTS idx_memory_jobs_run_id`,
		`DROP INDEX IF EXISTS memory_items_recall`,
		`DROP INDEX IF EXISTS memory_items_topic`,
		`ALTER TABLE memory_items DROP COLUMN IF EXISTS kind`,
		`ALTER TABLE memory_items DROP COLUMN IF EXISTS topic`,
		`DROP TABLE IF EXISTS notes`,
	} {
		if err := s.db.WithContext(ctx).Exec(ddl).Error; err != nil {
			return fmt.Errorf("%s: %w", ddl, err)
		}
	}
	// 视图刚被删掉（删列的前置），**当场建回来**——迁移跑完这一轮启动就要能读视图，
	// 不能等下一次启动的 EnsureMemoryIndexes。
	if err := ensureMemoryViews(s.db.WithContext(ctx)); err != nil {
		return fmt.Errorf("重建记忆视图：%w", err)
	}
	return nil
}

// ── 模式互切（doc/65 §6.2；施工包 §12.2）─────────────────────────────────────
//
// 离线命令：`go run ./cmd/yktako -memory-migrate-to=markdown|pipeline`。
// 不起 HTTP、Worker、消费者或定时器；只开 PG（不需要 Redis 与模型配置）。
// 操作员必须先停所有引擎副本——命令会检查无活 run、无 pending/running 任务，
// 但「停了没有」最终靠操作员保证（检查只是兜底，不是并发控制）。
//
// 文件与 PG 不能跨存储全原子，所以两个方向都按「可重放顺序」走：
//
//	pipeline→markdown：PG 是真源 → 预检全部文件 → 逐个原子写 → 一笔事务归档+记批次。
//	  中途崩了：PG 没动，重跑同命令从 PG 安全重生成（文件被同名覆盖，内容一致）。
//	markdown→pipeline：文件是真源 → 全部读解析 → 域冲突检查 → 一笔事务插行+记批次。
//	  中途崩了：事务没提交就是没导入；重放靠「批次 + 全部源文件 SHA-256」认出已完成。
//
// 批次表（memory_migration_batches）只记互切审计与幂等，**不是第二份记忆正文**：
// 行存在 = 那笔事务已提交（批次与状态变化同事务插入）。

// 互切方向（flag 取值）。
const (
	MemoryMigrateToMarkdown = "markdown"
	MemoryMigrateToPipeline = "pipeline"
)

// memoryMigrationBatchTable 的 DDL：行存在即完成（与状态变化同事务插入，无中间态）。
const memoryMigrationBatchDDL = `
CREATE TABLE IF NOT EXISTS memory_migration_batches (
	id          text PRIMARY KEY,
	direction   text NOT NULL,
	files_hash  text NOT NULL,
	file_count  integer NOT NULL,
	item_count  integer NOT NULL,
	created_at  timestamptz NOT NULL DEFAULT now()
)`

// RunMemoryModeMigration 是离线互切命令的入口。target 取 markdown 或 pipeline。
//
// 先把库收敛到当前结构（MigrateMemorySchema 幂等），再在迁移锁内做互切——
// 另一副本此时启动会等迁移锁，等不到就启动失败，不会并行写。
func RunMemoryModeMigration(ctx context.Context, target, pgDSN, root string) error {
	if target != MemoryMigrateToMarkdown && target != MemoryMigrateToPipeline {
		return fmt.Errorf("-memory-migrate-to 只能是 markdown 或 pipeline， got %q", target)
	}
	db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{TranslateError: true})
	if err != nil {
		return fmt.Errorf("pg：%w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("pg：%w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("pg 不可达：%w", err)
	}
	if err := autoMigrate(db); err != nil {
		return fmt.Errorf("pg migrate：%w", err)
	}
	s := &Store{db: db}
	s.ConfigureMemory(MemoryConfig{Workspace: root})
	if err := s.MigrateMemorySchema(ctx); err != nil {
		return fmt.Errorf("存量迁移：%w", err)
	}
	if err := db.WithContext(ctx).Exec(memoryMigrationBatchDDL).Error; err != nil {
		return fmt.Errorf("建迁移批次表：%w", err)
	}
	release, err := s.acquireMigrationLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	// 文件写锁与引擎同一条链（PG advisory + 本机 flock）：就算有副本没停干净，
	// 它的 markdown 写入也会与本命令串行，不会互相覆盖。
	ctx = WithFileLocker(ctx, newChainFileLocker(newPGFileLocker(db, 0), newFlockFileLocker(root)))
	if err := s.migrationPrechecks(ctx, target); err != nil {
		return err
	}
	if target == MemoryMigrateToMarkdown {
		return s.exportResidentsToMarkdown(ctx, root)
	}
	return s.importMarkdownToResidents(ctx, root)
}

// migrationPrechecks 互切前的三道检查（施工包 §12.2）：
// 无活 run、无 pending/running 任务；pipeline→markdown 还有 failed 任务时列出并阻止。
func (s *Store) migrationPrechecks(ctx context.Context, target string) error {
	var live []struct {
		ID        string
		SessionID string
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id, session_id FROM runs WHERE status = 'running' AND lease_until > now() ORDER BY id`).Scan(&live).Error; err != nil {
		return fmt.Errorf("查活 run：%w", err)
	}
	if len(live) > 0 {
		lines := make([]string, 0, len(live))
		for _, r := range live {
			lines = append(lines, "  run "+r.ID+"（session "+r.SessionID+"）")
		}
		return fmt.Errorf("还有 %d 场活 run——先停所有引擎副本并等 run 收口：\n%s", len(live), strings.Join(lines, "\n"))
	}
	var jobs []struct {
		ID     string
		Kind   string
		Status string
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id, kind, status FROM memory_jobs WHERE status IN ('pending','running') ORDER BY id`).Scan(&jobs).Error; err != nil {
		return fmt.Errorf("查记忆任务：%w", err)
	}
	if len(jobs) > 0 {
		lines := make([]string, 0, len(jobs))
		for _, j := range jobs {
			lines = append(lines, "  job "+j.ID+"（"+j.Kind+"，"+j.Status+"）")
		}
		return fmt.Errorf("还有 %d 条 pending/running 记忆任务——等消费者收口后再切：\n%s", len(jobs), strings.Join(lines, "\n"))
	}
	if target == MemoryMigrateToMarkdown {
		var failed []struct {
			ID        string
			Kind      string
			LastError string
		}
		if err := s.db.WithContext(ctx).Raw(
			`SELECT id, kind, last_error FROM memory_jobs WHERE status = 'failed' ORDER BY id`).Scan(&failed).Error; err != nil {
			return fmt.Errorf("查失败任务：%w", err)
		}
		if len(failed) > 0 {
			lines := make([]string, 0, len(failed))
			for _, j := range failed {
				lines = append(lines, "  job "+j.ID+"（"+j.Kind+"）："+j.LastError)
			}
			return fmt.Errorf("还有 %d 条 failed 记忆任务——切到 markdown 后没有消费者能再处理它们，先人工裁决（重登记或确认放弃）：\n%s", len(failed), strings.Join(lines, "\n"))
		}
	}
	return nil
}

// memoryMigrationFile 是互切中的一份目标/来源文件。
type memoryMigrationFile struct {
	relPath     string // 相对 root 的路径（哈希与报告用）
	path        string
	content     []byte // 导出=生成的字节；导入=读到的原始字节
	lockKey     string
	tenantID    string
	userID      string // user 文件
	workspaceID string // workspace 文件
	entries     []string
	ids         []string // 导出时的来源行 id（归档用）
	cap         int
}

// migrationFilesHash 算全部源文件的身份：按相对路径排序，逐文件拼「路径\0内容SHA-256」。
// 内容或文件集合任一变化 → 哈希变 → 不会被当成同一批重放。
func migrationFilesHash(files []memoryMigrationFile) string {
	sorted := make([]memoryMigrationFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].relPath < sorted[j].relPath })
	h := sha256.New()
	for _, f := range sorted {
		ch := sha256.Sum256(f.content)
		fmt.Fprintf(h, "%s\x00%x\n", f.relPath, ch)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// exportResidentsToMarkdown 做 pipeline→markdown（施工包 §12.2 的五步）。
//
// 导出口径与常驻读契约一致：state='active' 且未过期（过期行不是当前态，
// 搬进文件等于让它复活）。recall、历史链、removed 留痕不搬。
func (s *Store) exportResidentsToMarkdown(ctx context.Context, root string) error {
	var rows []MemoryItem
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, user_id, workspace, scope, content
		   FROM memory_items
		  WHERE layer = 'resident' AND state = 'active'
		    AND (expires_at IS NULL OR expires_at > now())
		  ORDER BY tenant_id, scope, user_id, workspace, created_at, id`).Scan(&rows).Error; err != nil {
		return fmt.Errorf("读 active resident：%w", err)
	}
	if len(rows) == 0 {
		log.Printf("memory migrate: 没有 active resident 可导出，一个文件也没动")
		return nil
	}
	// 按 user/workspace 分组。同内容精确重复只留第一条（与 markdown add 的 no-op 同口径）。
	type groupKey struct{ tenant, scope, subject string }
	groups := map[groupKey][]MemoryItem{}
	var keys []groupKey
	for _, r := range rows {
		var subject string
		switch r.Scope {
		case MemoryScopeUser:
			subject = r.UserID
		case MemoryScopeWorkspace:
			subject = r.Workspace
		default:
			return fmt.Errorf("resident 行 %s 的 scope=%q 不在词表里（user|workspace）", r.ID, r.Scope)
		}
		if r.TenantID == "" || subject == "" {
			return fmt.Errorf("resident 行 %s 缺租户或归属（tenant=%q scope=%s subject=%q），定位不到文件", r.ID, r.TenantID, r.Scope, subject)
		}
		k := groupKey{r.TenantID, r.Scope, subject}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		return a.tenant+"\x00"+a.scope+"\x00"+a.subject < b.tenant+"\x00"+b.scope+"\x00"+b.subject
	})
	var files []memoryMigrationFile
	for _, k := range keys {
		f := memoryMigrationFile{tenantID: k.tenant, cap: memoryWorkspaceCap}
		var dir string
		var err error
		if k.scope == MemoryScopeUser {
			f.userID, f.lockKey, f.cap = k.subject, "user:"+k.subject, memoryUserCap
			dir, err = userDir(root, k.tenant, k.subject)
			f.path = filepath.Join(dir, memoryUserFileName)
		} else {
			f.workspaceID, f.lockKey = k.subject, "workspace:"+k.subject
			dir, err = workspaceDir(root, k.tenant, k.subject)
			f.path = filepath.Join(dir, memoryWorkspaceFileName)
		}
		if err != nil {
			return fmt.Errorf("租户 %s 的 %s id %q 不合法，定位不到文件：%w", k.tenant, k.scope, k.subject, err)
		}
		f.relPath, _ = filepath.Rel(root, f.path)
		seen := map[string]bool{}
		for _, r := range groups[k] {
			// 归档口径覆盖组内全部行：精确重复行的内容已在文件里（跟着第一条去了），
			// 留着它 active 会让重跑导出再生成一份缺条目的退化文件。
			f.ids = append(f.ids, r.ID)
			if seen[r.Content] {
				continue
			}
			seen[r.Content] = true
			f.entries = append(f.entries, r.Content)
		}
		f.content = []byte(strings.Join(f.entries, memoryEntrySep))
		files = append(files, f)
	}
	// 全部文件预检（格式 + 预算）：任一不过 → 一个文件也不改。
	var problems []string
	for _, f := range files {
		back, err := parseMemoryEntries(f.content)
		// 逐条比对而不是比 join 后的字符串：条目含分隔符时 join 结果相同但条目数不同。
		if err != nil || !slices.Equal(back, f.entries) {
			problems = append(problems, fmt.Sprintf("  %s：条目内容过不了规范形态往返（含分隔符或首尾空白）", f.relPath))
			continue
		}
		if used := memoryEntriesRunes(f.entries); used > f.cap {
			problems = append(problems, fmt.Sprintf("  %s：%d/%d rune 超预算", f.relPath, used, f.cap))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("预检不过，一个文件也没改（PG 未动）：\n%s", strings.Join(problems, "\n"))
	}
	// 逐个原子写。中途失败如实报告已写几个——PG 仍是真源，重跑同命令安全重生成。
	for i, f := range files {
		fctx := withPrincipal(ctx, principal{TenantID: f.tenantID, UserID: f.userID})
		if f.userID == "" {
			fctx = withWorkspace(ctx, WorkspaceRef{TenantID: f.tenantID, Root: root, WorkspaceID: f.workspaceID})
		}
		release, err := lockMemoryFile(fctx, root, f.lockKey)
		if err != nil {
			return fmt.Errorf("文件已写 %d/%d，PG 事务未提交（PG 仍是真源，重跑同命令可安全重生成）：%w", i, len(files), err)
		}
		err = os.MkdirAll(filepath.Dir(f.path), 0o755)
		if err == nil {
			err = atomicWriteFile(f.path, f.content, 0o644)
		}
		release()
		if err != nil {
			return fmt.Errorf("文件已写 %d/%d，PG 事务未提交（PG 仍是真源，重跑同命令可安全重生成）：%s：%w", i, len(files), f.relPath, err)
		}
	}
	// 文件全部成功 → 一笔事务：归档已导出行 + 记批次。
	batchID, err := newRowID("mm_")
	if err != nil {
		return err
	}
	var ids []string
	for _, f := range files {
		ids = append(ids, f.ids...)
	}
	hash := migrationFilesHash(files)
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&MemoryItem{}).
			Where("id IN ? AND state = 'active'", ids).
			Updates(map[string]any{
				"state":          MemoryStateArchived,
				"left_active_at": gorm.Expr("COALESCE(left_active_at, now())"),
				"updated_at":     time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != int64(len(ids)) {
			return fmt.Errorf("归档行数 %d ≠ 导出行数 %d（有行在导出后变了状态）", res.RowsAffected, len(ids))
		}
		return tx.Exec(
			`INSERT INTO memory_migration_batches (id, direction, files_hash, file_count, item_count) VALUES (?, ?, ?, ?, ?)`,
			batchID, "pipeline_to_markdown", hash, len(files), len(ids)).Error
	}); err != nil {
		return fmt.Errorf("文件已写 %d/%d，PG 事务未提交（PG 仍是真源，重跑同命令可安全重生成）：%w", len(files), len(files), err)
	}
	log.Printf("memory migrate: pipeline→markdown 完成：批次 %s，%d 份文件，%d 行 resident 已归档", batchID, len(files), len(ids))
	log.Printf("memory migrate: 请把 TAKO_MEMORY_MODE 改成 markdown 后再启动引擎")
	return nil
}

// importMarkdownToResidents 做 markdown→pipeline（施工包 §12.2 的五步）。
//
// 文件没有历史，不伪造历史版本：每条条目插一行 source=model 的 active resident，
// quote/证据指针留空。文件保留作来源，新模式下不再写它们。
func (s *Store) importMarkdownToResidents(ctx context.Context, root string) error {
	paths, err := filepath.Glob(filepath.Join(root, "tenants", "*", "users", "*", memoryUserFileName))
	if err != nil {
		return err
	}
	wsPaths, err := filepath.Glob(filepath.Join(root, "tenants", "*", "workspaces", "*", memoryWorkspaceFileName))
	if err != nil {
		return err
	}
	paths = append(paths, wsPaths...)
	sort.Strings(paths)
	if len(paths) == 0 {
		log.Printf("memory migrate: 没找到 USER.md/MEMORY.md，无导入")
		return nil
	}
	var files []memoryMigrationFile
	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 5 || parts[0] != "tenants" {
			return fmt.Errorf("记忆文件路径形状不对：%s", rel)
		}
		f := memoryMigrationFile{relPath: filepath.ToSlash(rel), path: p, tenantID: parts[1], cap: memoryWorkspaceCap}
		if parts[2] == "users" {
			f.userID, f.lockKey, f.cap = parts[3], "user:"+parts[3], memoryUserCap
			if _, err := userDir(root, f.tenantID, f.userID); err != nil {
				return fmt.Errorf("%s：用户 id 不合法：%w", rel, err)
			}
		} else {
			f.workspaceID, f.lockKey = parts[3], "workspace:"+parts[3]
			if _, err := workspaceDir(root, f.tenantID, f.workspaceID); err != nil {
				return fmt.Errorf("%s：项目 id 不合法：%w", rel, err)
			}
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("读 %s：%w", rel, err)
		}
		f.content = raw
		f.entries, err = parseMemoryEntries(raw)
		if err != nil {
			return fmt.Errorf("解析 %s（一个字节没动，库也没动）：%w", rel, err)
		}
		files = append(files, f)
	}
	hash := migrationFilesHash(files)
	// 重放识别：同一批文件已导入过（且之后没有更新的导出批次）→ 直接退出。
	// 「之后没有导出」这个限定让「导出→再导入」的往返不被误判成重放。
	var done string
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id FROM memory_migration_batches
		  WHERE direction = 'markdown_to_pipeline' AND files_hash = ?
		    AND created_at > (SELECT COALESCE(MAX(created_at), 'epoch'::timestamptz)
		                        FROM memory_migration_batches WHERE direction = 'pipeline_to_markdown')
		  LIMIT 1`, hash).Scan(&done).Error; err != nil {
		return fmt.Errorf("查迁移批次：%w", err)
	}
	if done != "" {
		log.Printf("memory migrate: 这批文件已导入过（批次 %s），库未动", done)
		return nil
	}
	// 域冲突：目标域还有 active 未过期 resident（= 没被上次导出归档）→ 拒绝，避免双份真相。
	var conflicts []string
	for _, f := range files {
		if len(f.entries) == 0 {
			continue
		}
		var n int64
		q := `SELECT count(*) FROM memory_items
		       WHERE layer = 'resident' AND state = 'active' AND tenant_id = ?
		         AND (expires_at IS NULL OR expires_at > now())`
		args := []any{f.tenantID}
		if f.userID != "" {
			q += ` AND scope = 'user' AND user_id = ?`
			args = append(args, f.userID)
		} else {
			q += ` AND scope = 'workspace' AND workspace = ?`
			args = append(args, f.workspaceID)
		}
		if err := s.db.WithContext(ctx).Raw(q, args...).Scan(&n).Error; err != nil {
			return fmt.Errorf("查域冲突：%w", err)
		}
		if n > 0 {
			conflicts = append(conflicts, fmt.Sprintf("  %s：该域还有 %d 行 active resident（先跑 -memory-migrate-to=markdown 导出归档，或人工确认）", f.relPath, n))
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("域冲突，一行也没插：\n%s", strings.Join(conflicts, "\n"))
	}
	// 一笔事务：插行 + 记批次。同文件内精确重复只留第一条（与导出同口径）。
	batchID, err := newRowID("mm_")
	if err != nil {
		return err
	}
	itemCount := 0
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, f := range files {
			scope := MemoryScopeWorkspace
			if f.userID != "" {
				scope = MemoryScopeUser
			}
			seen := map[string]bool{}
			for _, e := range f.entries {
				if seen[e] {
					continue
				}
				seen[e] = true
				id, err := newRowID("mem_")
				if err != nil {
					return err
				}
				if err := tx.Exec(
					`INSERT INTO memory_items (id, tenant_id, user_id, workspace, layer, scope, content, quote,
					        state, source, source_session_id, source_run_id, source_seq, created_at, updated_at)
					 VALUES (?, ?, ?, ?, 'resident', ?, ?, '', 'active', 'model', '', '', 0, now(), now())`,
					id, f.tenantID, f.userID, f.workspaceID, scope, e).Error; err != nil {
					return err
				}
				itemCount++
			}
		}
		return tx.Exec(
			`INSERT INTO memory_migration_batches (id, direction, files_hash, file_count, item_count) VALUES (?, ?, ?, ?, ?)`,
			batchID, "markdown_to_pipeline", hash, len(files), itemCount).Error
	}); err != nil {
		return fmt.Errorf("导入事务未提交（库没动）：%w", err)
	}
	log.Printf("memory migrate: markdown→pipeline 完成：批次 %s，%d 份文件，%d 行 resident（source=model）", batchID, len(files), itemCount)
	log.Printf("memory migrate: 请把 TAKO_MEMORY_MODE 改回 pipeline 后再启动；文件保留作来源，不在新模式下继续写")
	return nil
}
