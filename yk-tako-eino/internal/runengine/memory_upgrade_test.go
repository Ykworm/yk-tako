package runengine

// 本文件：**升级路径**的回归测试（存量迁移在真实旧结构上的行为）。
//
// 为什么要有它：迁移的输入是「旧库的样子」（notes 表、memory_items 的 kind/topic 列、
// 老行没有 layer/scope/state）。用全新的空库测不出来——空库走不到迁移分支。
// 所以这里拿**开发库的模板副本**当真输入：dev 库在这次施工前就是旧结构，副本上跑一遍
// 迁移，验「行数不变但语义到位、旧结构清干净、视图当场建回、第二次跑幂等」。
//
// 副作用控制：副本库名固定（tako_upgrade_probe）、测试结束必删；开发库本身**只读**
// （迁移只发生在副本上）。开发库不存在或已是新结构时跳过（别把别的环境判成失败）。

import (
	"context"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUpgradePathOnRealLegacySchema(t *testing.T) {
	ctx := context.Background()
	admin, err := gorm.Open(postgres.Open(testPGAdminDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("连不上 PG：%v", err)
	}
	const probe = "tako_upgrade_probe"
	if err := admin.Exec(`DROP DATABASE IF EXISTS ` + probe + ` WITH (FORCE)`).Error; err != nil {
		t.Skipf("清旧探针库失败：%v", err)
	}
	if err := admin.Exec(`CREATE DATABASE ` + probe + ` TEMPLATE tako`).Error; err != nil {
		t.Skipf("从开发库复制失败（开发库可能不存在或连不上）：%v", err)
	}
	t.Cleanup(func() { _ = admin.Exec(`DROP DATABASE IF EXISTS ` + probe + ` WITH (FORCE)`).Error })

	store, err := OpenStore(ctx, "redis://127.0.0.1:6379/4", testPGDSN(probe), 0, 0)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	var n int64
	_ = store.db.Raw(`SELECT count(*) FROM memory_items`).Scan(&n).Error
	before := n
	_ = store.db.Raw(`SELECT count(*) FROM memory_items WHERE kind IS NOT NULL`).Scan(&n).Error
	legacy := n
	t.Logf("迁移前 memory_items 行数 = %d（带 kind 的旧行 %d）", before, legacy)
	if legacy == 0 {
		t.Skip("开发库已是新结构，没有旧行可迁（这条路径在别的环境由 TestMigrateMemorySchemaIsIdempotent 覆盖）")
	}

	if err := store.MigrateMemorySchema(ctx); err != nil {
		t.Fatalf("MigrateMemorySchema: %v", err)
	}

	// 行数不变（迁移是「改归属」，不是「重写数据」）。
	_ = store.db.Raw(`SELECT count(*) FROM memory_items`).Scan(&n).Error
	if n != before {
		t.Errorf("迁移后行数 = %d，want %d（不能丢行）", n, before)
	}
	// 旧行都归到 recall/workspace（doc/65 §6.1）。
	_ = store.db.Raw(`SELECT count(*) FROM memory_items WHERE layer='recall' AND scope='workspace'`).Scan(&n).Error
	if n != legacy {
		t.Errorf("迁成 recall/workspace 的行数 = %d，want %d（旧行全归这一档）", n, legacy)
	}
	// 旧结构清干净：notes 表没了、kind 列没了。
	var hasNotes, hasKind bool
	_ = store.db.Raw(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='notes')`).Scan(&hasNotes).Error
	if hasNotes {
		t.Error("notes 表还在")
	}
	_ = store.db.Raw(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='memory_items' AND column_name='kind')`).Scan(&hasKind).Error
	if hasKind {
		t.Error("memory_items.kind 列还在")
	}
	// 视图要当场建回来（删列的前置是删视图，迁移末尾必须重建）。
	var hasView bool
	_ = store.db.Raw(`SELECT EXISTS (SELECT 1 FROM information_schema.views WHERE table_name='resident_notes')`).Scan(&hasView).Error
	if !hasView {
		t.Error("resident_notes 视图没重建（迁移后这一轮启动就读不到视图）")
	}

	// 幂等：再跑一次，行数不变。
	if err := store.MigrateMemorySchema(ctx); err != nil {
		t.Fatalf("第二次迁移: %v", err)
	}
	_ = store.db.Raw(`SELECT count(*) FROM memory_items`).Scan(&n).Error
	if n != before {
		t.Errorf("第二次迁移后行数 = %d，want %d（不能重复迁）", n, before)
	}
	t.Logf("第二次迁移后 memory_items 行数 = %d（不变 ✓）", n)
}
