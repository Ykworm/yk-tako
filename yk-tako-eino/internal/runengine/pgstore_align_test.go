package runengine

// 本文件：存量库「先有列、后有 default 标签」与「同名索引形状过期」的补齐回归。
//
// 2026-11-13 dev 库事故的存储级孪生：memory_jobs 的批量列没有 DEFAULT ''，
// EndRun 登记时省略它们 → PG 23502 → 收尾事务回滚 → run 挂到租约过期。
// 新库走不到这条分支（AutoMigrate 建列时就带默认值），所以这里先用 DDL 造旧形态。

import (
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAutoMigrateAlignsLegacyMemoryShape(t *testing.T) {
	rowID, err := newRowID("t")
	if err != nil {
		t.Fatal(err)
	}
	dbName := "tako_test_" + strings.ToLower(rowID)
	admin, err := gorm.Open(postgres.Open(testPGAdminDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连 5433 失败（PG 需先起着）: %v", err)
	}
	if err := admin.Exec("CREATE DATABASE " + dbName).Error; err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP DATABASE " + dbName + " WITH (FORCE)").Error; err != nil {
			t.Errorf("DROP DATABASE %s: %v", dbName, err)
		}
		if sqlDB, err := admin.DB(); err == nil {
			sqlDB.Close()
		}
	})
	db, err := gorm.Open(postgres.Open(testPGDSN(dbName)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连临时库 %s: %v", dbName, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	// 旧形态：文本列都没有 DEFAULT ''、updated_at 可空、dialogue_chunks_scope 只有两列。
	for _, ddl := range []string{
		`CREATE TABLE memory_items (
			id text PRIMARY KEY, tenant_id text NOT NULL, user_id text NOT NULL,
			workspace text NOT NULL, layer text NOT NULL, scope text NOT NULL,
			content text NOT NULL, quote text NOT NULL, state text NOT NULL,
			source text NOT NULL, source_session_id text NOT NULL, source_run_id text NOT NULL,
			source_seq bigint NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz)`,
		`INSERT INTO memory_items (id, tenant_id, user_id, workspace, layer, scope, content, quote,
		        state, source, source_session_id, source_run_id, source_seq, created_at)
		 VALUES ('mem_legacy', 't1', 'u1', 'ws1', 'recall', 'workspace', '旧记忆', '旧原话',
		        'active', 'extracted', 's1', 'r1', 1, now())`,
		`CREATE TABLE memory_jobs (
			id text PRIMARY KEY, run_id text NOT NULL, session_id text NOT NULL,
			tenant_id text NOT NULL, user_id text NOT NULL, workspace text NOT NULL,
			kind text NOT NULL, target_layer text, quote text NOT NULL, source_seq bigint NOT NULL,
			trace_parent text NOT NULL, status text NOT NULL, attempts bigint NOT NULL,
			lease_until timestamptz, next_attempt_at timestamptz NOT NULL, claim_epoch bigint NOT NULL,
			batch_id text NOT NULL, batch_payload text NOT NULL, batch_committed_at timestamptz,
			dedup_key text, last_error text NOT NULL, created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL)`,
		`CREATE TABLE dialogue_chunks (
			run_id text PRIMARY KEY, session_id text NOT NULL, tenant_id text NOT NULL,
			workspace text NOT NULL, passage text NOT NULL, runes bigint NOT NULL,
			truncated boolean NOT NULL, indexed_at timestamptz NOT NULL)`,
		`CREATE INDEX dialogue_chunks_scope ON dialogue_chunks (tenant_id, workspace)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("造旧形态失败: %s: %v", ddl, err)
		}
	}

	if err := autoMigrate(db); err != nil {
		t.Fatalf("autoMigrate: %v", err)
	}

	mustBool := func(query string, args ...any) bool {
		t.Helper()
		var b bool
		if err := db.Raw(query, args...).Row().Scan(&b); err != nil {
			t.Fatalf("SQL %s: %v", query, err)
		}
		return b
	}

	// 1) 默认值补齐：省略这些列插 memory_jobs 不再 23502（dev 事故的直接孪生）。
	if err := db.Exec(
		`INSERT INTO memory_jobs (id, run_id, kind, status, attempts, next_attempt_at, created_at, updated_at)
		 VALUES ('mj_probe', 'r_probe', 'postrun', 'pending', 0, now(), now(), now())`).Error; err != nil {
		t.Fatalf("省略默认列插 memory_jobs: %v", err)
	}
	for _, c := range []struct{ table, col string }{
		{"memory_items", "user_id"}, {"memory_items", "workspace"}, {"memory_items", "quote"},
		{"memory_items", "source_session_id"}, {"memory_items", "source_run_id"},
		{"memory_jobs", "session_id"}, {"memory_jobs", "tenant_id"}, {"memory_jobs", "user_id"},
		{"memory_jobs", "workspace"}, {"memory_jobs", "quote"}, {"memory_jobs", "trace_parent"},
		{"memory_jobs", "batch_id"}, {"memory_jobs", "batch_payload"}, {"memory_jobs", "last_error"},
		{"dialogue_chunks", "user_id"},
	} {
		if !mustBool(
			`SELECT column_default IS NOT NULL FROM information_schema.columns
			  WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			c.table, c.col) {
			t.Errorf("%s.%s 仍缺 DEFAULT ''", c.table, c.col)
		}
	}

	// 2) updated_at：老行回填成 created_at，列收紧成 NOT NULL。
	if !mustBool(`SELECT (updated_at IS NOT NULL AND updated_at = created_at)
	                FROM memory_items WHERE id = 'mem_legacy'`) {
		t.Error("旧行 updated_at 应回填为 created_at")
	}
	if !mustBool(
		`SELECT is_nullable = 'NO' FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'memory_items' AND column_name = 'updated_at'`) {
		t.Error("memory_items.updated_at 应收紧成 NOT NULL")
	}

	// 3) 索引换成当前 model 声明的三列形状。
	var indexDef string
	if err := db.Raw(
		`SELECT indexdef FROM pg_indexes
		  WHERE schemaname = current_schema() AND indexname = 'dialogue_chunks_scope'`).
		Scan(&indexDef).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexDef, "(tenant_id, user_id, workspace)") {
		t.Errorf("dialogue_chunks_scope = %q, want (tenant_id, user_id, workspace)", indexDef)
	}

	// 4) 幂等：同一库再跑一遍启动迁移是空操作。
	if err := autoMigrate(db); err != nil {
		t.Fatalf("第二次 autoMigrate 应幂等: %v", err)
	}
}
