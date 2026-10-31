// skillrestore_test.go 钉住 D35 的装配期恢复：看板里建过任务的技能，盒子在 run 起手时就有它的根；
// 禁用/失效/盘上没了的逐项跳过，装配不失败。
package runengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreSkillRoots(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDSN(t, testLease, testTTL)
	const tenant, session = "ten_restore", SessionID("s_restore")
	prev, cur := RunID("r_restore_prev"), RunID("r_restore_cur")

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(entry, []byte("---\nname: demo\ndescription: d\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Create(&SkillRow{
		ID: "sk_restore_demo", Scope: "tenant", TenantID: tenant, Name: "demo",
		Path: entry, Description: "d", Requires: "[]", Valid: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, tenant, prev, session, "跑 demo 的脚本", "", "", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, tenant, prev, session, "普通任务", "", "", ""); err != nil {
		t.Fatal(err)
	}

	w := &Worker{store: s}
	box := newSkillBox()
	rctx := withSkillBox(withScope(ctx, dataScope{TenantID: tenant, Scope: "tenant"}), box)
	w.restoreSkillRoots(rctx, session, cur, false)

	got := box.list()
	if len(got) != 1 || got[0].Name != "demo" || got[0].Root != root {
		t.Fatalf("恢复后盒子 = %+v, want demo@%s", got, root)
	}

	// 禁用 → 不补（禁用绝不靠恢复绕过 116107）。
	if err := s.db.WithContext(ctx).Create(&SkillDisable{TenantID: tenant, Name: "demo"}).Error; err != nil {
		t.Fatal(err)
	}
	box2 := newSkillBox()
	w.restoreSkillRoots(withSkillBox(withScope(ctx, dataScope{TenantID: tenant, Scope: "tenant"}), box2), session, cur, false)
	if len(box2.list()) != 0 {
		t.Fatalf("禁用后仍补了 %+v", box2.list())
	}

	// 盘上目录没了 → 跳过、装配不失败（row 还在）。
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND name = ?", tenant, "demo").Delete(&SkillDisable{}).Error; err != nil {
		t.Fatal(err)
	}
	box3 := newSkillBox()
	w.restoreSkillRoots(withSkillBox(withScope(ctx, dataScope{TenantID: tenant, Scope: "tenant"}), box3), session, cur, false)
	if len(box3.list()) != 0 {
		t.Fatalf("目录没了仍补了 %+v", box3.list())
	}
}
