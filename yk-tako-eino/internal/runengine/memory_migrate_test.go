package runengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 离线模式互切（doc/65 §6.2 / 施工包 §12.2）的确定性回归：
// 导出分组与归档口径、预检全有或全无、导入域冲突/解析失败不动库、批次重放不重复插、
// 导出→导入→再导出→再导入的往返不被误判成重放。
// 全部走 RunMemoryModeMigration 公开入口（含 autoMigrate + 存量迁移 + 迁移锁），不打模型。

// seedResident 插一行 resident。state/scope 等由调用方给；CreatedAt 显式给，保证导出顺序可断言。
func seedResident(t *testing.T, s *Store, id, tenant, user, workspace, scope, content, state string, created time.Time) {
	t.Helper()
	row := MemoryItem{
		ID: id, TenantID: tenant, UserID: user, Workspace: workspace,
		Layer: MemoryLayerResident, Scope: scope, Content: content, State: state,
		Source: MemorySourceModel, CreatedAt: created, UpdatedAt: created,
	}
	if err := s.db.Create(&row).Error; err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func migrationBatchCount(t *testing.T, s *Store, direction string) int64 {
	t.Helper()
	var n int64
	if err := s.db.Raw(`SELECT count(*) FROM memory_migration_batches WHERE direction = ?`, direction).Scan(&n).Error; err != nil {
		t.Fatalf("查批次: %v", err)
	}
	return n
}

func residentCount(t *testing.T, s *Store, state string) int64 {
	t.Helper()
	var n int64
	if err := s.db.Raw(`SELECT count(*) FROM memory_items WHERE layer = 'resident' AND state = ?`, state).Scan(&n).Error; err != nil {
		t.Fatalf("查 resident: %v", err)
	}
	return n
}

func readMigrationFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("读 %s: %v", rel, err)
	}
	return string(raw)
}

// 导出：按 user/workspace 分组、同内容精确重复只留第一条；recall/历史/removed/过期不搬；
// 已导出行 archived + left_active_at；批次落表；重跑（已无 active resident）不动文件。
func TestMemoryModeMigrationExportGroupsAndArchives(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	base := time.Now().UTC().Add(-time.Hour)

	seedResident(t, s, "m_u1a", "t1", "u1", "", MemoryScopeUser, "用户喜欢茶", "active", base)
	seedResident(t, s, "m_u1b", "t1", "u1", "", MemoryScopeUser, "用户喜欢茶", "active", base.Add(time.Minute)) // 精确重复：内容随第一条进文件，行一起归档
	seedResident(t, s, "m_u1c", "t1", "u1", "", MemoryScopeUser, "用户在杭州", "active", base.Add(2*time.Minute))
	seedResident(t, s, "m_u2", "t1", "u2", "", MemoryScopeUser, "二号用户", "active", base)
	seedResident(t, s, "m_w1", "t1", "", "w1", MemoryScopeWorkspace, "项目用 Go", "active", base)
	// 不该搬的四种：recall 层、superseded 历史、removed 留痕、已过期 active。
	seedResident(t, s, "m_recall", "t1", "u1", "", MemoryScopeUser, "召回层不搬", "active", base)
	if err := s.db.Model(&MemoryItem{}).Where("id = ?", "m_recall").Update("layer", MemoryLayerRecall).Error; err != nil {
		t.Fatal(err)
	}
	seedResident(t, s, "m_old", "t1", "u1", "", MemoryScopeUser, "历史不搬", "superseded", base)
	seedResident(t, s, "m_rm", "t1", "u1", "", MemoryScopeUser, "删除留痕不搬", "removed", base)
	seedResident(t, s, "m_exp", "t1", "u1", "", MemoryScopeUser, "过期不搬", "active", base)
	past := base.Add(-time.Minute)
	if err := s.db.Model(&MemoryItem{}).Where("id = ?", "m_exp").Update("expires_at", past).Error; err != nil {
		t.Fatal(err)
	}

	if err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root); err != nil {
		t.Fatalf("导出: %v", err)
	}
	if got := readMigrationFile(t, root, "tenants/t1/users/u1/USER.md"); got != "用户喜欢茶\n§\n用户在杭州" {
		t.Fatalf("u1 文件 = %q", got)
	}
	if got := readMigrationFile(t, root, "tenants/t1/users/u2/USER.md"); got != "二号用户" {
		t.Fatalf("u2 文件 = %q", got)
	}
	if got := readMigrationFile(t, root, "tenants/t1/workspaces/w1/MEMORY.md"); got != "项目用 Go" {
		t.Fatalf("w1 文件 = %q", got)
	}
	if n := residentCount(t, s, "archived"); n != 5 {
		t.Fatalf("archived = %d, want 5（精确重复行随内容一起归档）", n)
	}
	var left int64
	if err := s.db.Raw(`SELECT count(*) FROM memory_items WHERE state = 'archived' AND left_active_at IS NOT NULL`).Scan(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 5 {
		t.Fatalf("left_active_at 写入 = %d, want 5", left)
	}
	// recall、历史、removed、过期都原样不动。
	for _, id := range []string{"m_recall", "m_old", "m_rm", "m_exp"} {
		var state string
		if err := s.db.Raw(`SELECT state FROM memory_items WHERE id = ?`, id).Scan(&state).Error; err != nil {
			t.Fatal(err)
		}
		want := "active"
		switch id {
		case "m_old":
			want = "superseded"
		case "m_rm":
			want = "removed"
		}
		if state != want {
			t.Fatalf("%s state = %s, want %s（不该被动）", id, state, want)
		}
	}
	if n := migrationBatchCount(t, s, "pipeline_to_markdown"); n != 1 {
		t.Fatalf("导出批次 = %d, want 1", n)
	}
	var fc, ic int
	if err := s.db.Raw(`SELECT file_count, item_count FROM memory_migration_batches WHERE direction = 'pipeline_to_markdown'`).Row().Scan(&fc, &ic); err != nil {
		t.Fatal(err)
	}
	if fc != 3 || ic != 5 {
		t.Fatalf("批次 file_count=%d item_count=%d, want 3/5", fc, ic)
	}
	// 重跑：已无 active resident → 短路，文件不变、不再记批次。
	before := readMigrationFile(t, root, "tenants/t1/users/u1/USER.md")
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root); err != nil {
		t.Fatalf("重跑导出: %v", err)
	}
	if got := readMigrationFile(t, root, "tenants/t1/users/u1/USER.md"); got != before {
		t.Fatalf("重跑后文件变了: %q", got)
	}
	if n := migrationBatchCount(t, s, "pipeline_to_markdown"); n != 1 {
		t.Fatalf("重跑后导出批次 = %d, want 仍 1", n)
	}
}

// 导出预检全有或全无：任一文件超预算 → 一个文件也不写、PG 一行也不动。
func TestMemoryModeMigrationExportPrecheckAllOrNothing(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	base := time.Now().UTC().Add(-time.Hour)
	seedResident(t, s, "m_ok", "t1", "u_ok", "", MemoryScopeUser, "正常条目", "active", base)
	seedResident(t, s, "m_big", "t1", "u_big", "", MemoryScopeUser, strings.Repeat("好", memoryUserCap+1), "active", base)

	err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root)
	if err == nil || !strings.Contains(err.Error(), "预检不过") || !strings.Contains(err.Error(), "超预算") {
		t.Fatalf("err = %v, want 预检不过+超预算", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "tenants")); !os.IsNotExist(statErr) {
		t.Fatalf("预检失败但文件写了: %v", statErr)
	}
	if n := residentCount(t, s, "active"); n != 2 {
		t.Fatalf("active = %d, want 2（PG 未动）", n)
	}
	if n := migrationBatchCount(t, s, "pipeline_to_markdown"); n != 0 {
		t.Fatalf("预检失败但记了批次: %d", n)
	}
}

// 导出预检格式：条目含分隔符 → 规范形态往返失败 → 全部拒写。
func TestMemoryModeMigrationExportRejectsSeparatorInContent(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	seedResident(t, s, "m_sep", "t1", "u1", "", MemoryScopeUser, "上半\n§\n下半", "active", time.Now().UTC())
	err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root)
	if err == nil || !strings.Contains(err.Error(), "预检不过") {
		t.Fatalf("err = %v, want 预检不过", err)
	}
	if n := residentCount(t, s, "active"); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
}

// 互切前置检查：活 run / pending job 双向都挡；failed job 只挡 →markdown。
func TestMemoryModeMigrationPrechecks(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	// EndRun 只在 Extract=on 时登记 job（store.go），前置检查要有真 job 可查。
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: root, Extract: true, Recall: true})

	// 活 run：AcquireRun 出生即 running + lease 未到期。
	s.mustAcquire(t, "s_live", "r_live", "m_live", "hi")
	err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root)
	if err == nil || !strings.Contains(err.Error(), "活 run") || !strings.Contains(err.Error(), "r_live") {
		t.Fatalf("err = %v, want 列出活 run r_live", err)
	}
	// run 收口 → 登记 pending job → 双向都挡。
	if _, _, ok, err := s.EndRun(ctx, "s_live", "r_live", 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun(r_live): ok=%v err=%v", ok, err)
	}
	for _, target := range []string{MemoryMigrateToMarkdown, MemoryMigrateToPipeline} {
		err := RunMemoryModeMigration(ctx, target, dsn, root)
		if err == nil || !strings.Contains(err.Error(), "pending/running") {
			t.Fatalf("target=%s err = %v, want pending/running 阻止", target, err)
		}
	}
	// job 置 failed：→markdown 列出并阻止；→pipeline 不受 failed 影响（没有文件 → 无导入正常返回）。
	if err := s.db.Exec(`UPDATE memory_jobs SET status = 'failed', last_error = '抽取超时' WHERE run_id = 'r_live'`).Error; err != nil {
		t.Fatal(err)
	}
	err = RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root)
	if err == nil || !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "抽取超时") {
		t.Fatalf("err = %v, want 列出 failed job 与 last_error", err)
	}
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root); err != nil {
		t.Fatalf("→pipeline 不该被 failed job 挡: %v", err)
	}
}

// 导入：域冲突拒绝且一行不插；解析失败不动库；正常导入 source=model；同批重放不重复。
func TestMemoryModeMigrationImportConflictParseAndReplay(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	userFile := filepath.Join(root, "tenants/t1/users/u1/USER.md")
	wsFile := filepath.Join(root, "tenants/t1/workspaces/w1/MEMORY.md")
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 解析失败（条目首尾带空格，非规范形态）→ 报错且库没动。
	writeFile(userFile, " 条目一\n§\n条目二")
	err := RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root)
	if err == nil || !strings.Contains(err.Error(), "解析") {
		t.Fatalf("err = %v, want 解析失败", err)
	}
	if n := residentCount(t, s, "active"); n != 0 {
		t.Fatalf("解析失败但插了行: %d", n)
	}

	// 域冲突：文件改好，但目标域还有 active resident → 拒绝，一行也不插。
	writeFile(userFile, "条目一\n§\n条目二")
	writeFile(wsFile, "项目条目")
	seedResident(t, s, "m_live", "t1", "u1", "", MemoryScopeUser, "库里已有的当前态", "active", time.Now().UTC())
	err = RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root)
	if err == nil || !strings.Contains(err.Error(), "域冲突") || !strings.Contains(err.Error(), "USER.md") {
		t.Fatalf("err = %v, want 域冲突并点名文件", err)
	}
	if n := residentCount(t, s, "active"); n != 1 {
		t.Fatalf("域冲突但插了行: active = %d, want 仍 1", n)
	}
	if n := migrationBatchCount(t, s, "markdown_to_pipeline"); n != 0 {
		t.Fatalf("域冲突但记了批次: %d", n)
	}

	// 清掉冲突行 → 正常导入：source=model、scope 按文件、quote/证据指针为空。
	if err := s.db.Exec(`DELETE FROM memory_items WHERE id = 'm_live'`).Error; err != nil {
		t.Fatal(err)
	}
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root); err != nil {
		t.Fatalf("导入: %v", err)
	}
	var rows []MemoryItem
	if err := s.db.Where("layer = 'resident' AND state = 'active'").Order("scope, content").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("导入行数 = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.Source != MemorySourceModel || r.Quote != "" || r.SourceRunID != "" || r.SourceSeq != 0 {
			t.Fatalf("导入行 %s 伪造了来源/历史: %+v", r.ID, r)
		}
	}
	if rows[0].Scope != MemoryScopeUser || rows[0].UserID != "u1" || rows[0].Workspace != "" {
		t.Fatalf("user 行归属错: %+v", rows[0])
	}
	if rows[2].Scope != MemoryScopeWorkspace || rows[2].Workspace != "w1" {
		t.Fatalf("workspace 行归属错: %+v", rows[2])
	}
	if n := migrationBatchCount(t, s, "markdown_to_pipeline"); n != 1 {
		t.Fatalf("导入批次 = %d, want 1", n)
	}

	// 同命令重放：看到已完成批次直接退出，不重复插。
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root); err != nil {
		t.Fatalf("重放: %v", err)
	}
	if n := residentCount(t, s, "active"); n != 3 {
		t.Fatalf("重放后 active = %d, want 仍 3", n)
	}
	if n := migrationBatchCount(t, s, "markdown_to_pipeline"); n != 1 {
		t.Fatalf("重放后批次 = %d, want 仍 1", n)
	}
	// 文件保留作来源，导入不删不改。
	if got := readMigrationFile(t, root, "tenants/t1/users/u1/USER.md"); got != "条目一\n§\n条目二" {
		t.Fatalf("导入动了源文件: %q", got)
	}
}

// 往返：导出→导入→再导出→再导入。第二次导入不被当成重放（上次导入早于最近的导出批次），
// 也不报域冲突（行已被第二次导出归档）。
func TestMemoryModeMigrationRoundTripNotReplay(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	seedResident(t, s, "m_a", "t1", "u1", "", MemoryScopeUser, "喜欢茶", "active", time.Now().UTC().Add(-time.Hour))

	if err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root); err != nil {
		t.Fatalf("导出1: %v", err)
	}
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root); err != nil {
		t.Fatalf("导入1: %v", err)
	}
	if n := residentCount(t, s, "active"); n != 1 {
		t.Fatalf("导入1后 active = %d, want 1", n)
	}
	// 再导出：把导入的行归档，文件内容不变（同一份当前态）。
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToMarkdown, dsn, root); err != nil {
		t.Fatalf("导出2: %v", err)
	}
	if n := residentCount(t, s, "active"); n != 0 {
		t.Fatalf("导出2后 active = %d, want 0", n)
	}
	// 再导入：哈希与导入1相同，但导入1早于导出2 → 不是重放 → 正常插一行。
	if err := RunMemoryModeMigration(ctx, MemoryMigrateToPipeline, dsn, root); err != nil {
		t.Fatalf("导入2: %v", err)
	}
	if n := residentCount(t, s, "active"); n != 1 {
		t.Fatalf("导入2后 active = %d, want 1", n)
	}
	if n := migrationBatchCount(t, s, "markdown_to_pipeline"); n != 2 {
		t.Fatalf("导入批次 = %d, want 2", n)
	}
}

// 非法 target 在连库之前就拒绝。
func TestMemoryModeMigrationRejectsBadTarget(t *testing.T) {
	err := RunMemoryModeMigration(context.Background(), "bogus", "", "")
	if err == nil || !strings.Contains(err.Error(), "markdown 或 pipeline") {
		t.Fatalf("err = %v, want 非法 target", err)
	}
}
