package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSoftDeletedSessionHidden(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_soft"), RunID("r_soft")
	s.mustAcquire(t, sid, rid, "m_soft", "hi")
	if err := s.db.WithContext(ctx).Exec(`UPDATE sessions SET deleted_at = now() WHERE id = ?`, sid).Error; err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.SessionAccess(ctx, sid, testOwner); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SessionAccess = %v, want not found", err)
	}
	rows, err := s.ListSessions(ctx, testOwner, "own", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("ListSessions = %+v %v, want empty", rows, err)
	}
	exists, err := s.Exists(ctx, sid)
	if err != nil || exists {
		t.Fatalf("Exists = %v %v, want false", exists, err)
	}
	_, _, logExists, err := s.ReadLog(ctx, sid, 0, 20)
	if err != nil || logExists {
		t.Fatalf("ReadLog exists = %v %v, want false", logExists, err)
	}
	before := s.count(t, &Run{}, "session_id = ?", sid)
	ok, err := s.AcquireRun(ctx, sid, RunID("r_soft2"), Row{RunID: "r_soft2", Role: "user", MessageID: "m2", Text: "again"}, testOwner, "")
	if ok || !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AcquireRun = (ok %v, err %v), want not found", ok, err)
	}
	if after := s.count(t, &Run{}, "session_id = ?", sid); after != before {
		t.Fatalf("runs = %d, want %d", after, before)
	}
	if err := s.SetSessionVisibility(ctx, sid, testOwner, "tenant"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SetSessionVisibility = %v, want not found", err)
	}
	if err := s.SetSessionWorkspace(ctx, sid, testOwner, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SetSessionWorkspace = %v, want not found", err)
	}
	if s.sqlBool(t, `SELECT visibility = 'private' AND workspace_id IS NULL AND deleted_at IS NOT NULL FROM sessions WHERE id = ?`, sid) == false {
		t.Fatal("已删会话的可见性和挂载被改掉了")
	}
}

func TestDeleteWorkspaceCascade(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	if !s.sqlBool(t, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'sessions' AND indexname = 'sessions_owner_alive')`) {
		t.Fatal("sessions_owner_alive 没有建出来")
	}

	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "Q4")
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := SessionID("s_cas"), RunID("r_cas")
	ok, err := s.AcquireRun(ctx, sid, rid, Row{RunID: rid, Role: "user", MessageID: "m_cas", Text: "hi"}, testOwner, ws.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireRun = (ok %v, err %v)", ok, err)
	}
	if _, err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, ws.ID); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("活 run 删除 = %v, want busy", err)
	}
	if s.count(t, &Workspace{}, "id = ?", ws.ID) != 1 {
		t.Fatal("活 run 拒绝后项目行没了")
	}
	if s.sqlBool(t, `SELECT deleted_at IS NULL FROM sessions WHERE id = ?`, sid) == false {
		t.Fatal("活 run 拒绝后会话被标成已删")
	}

	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun = (ok %v, err %v)", ok, err)
	}
	bob := principal{UserID: "u_bob", TenantID: testOwner.TenantID, Role: "member"}
	if err := s.db.WithContext(ctx).Exec(
		`INSERT INTO sessions (id, title, tenant_id, user_id, visibility, agent_id, workspace_id, created_at)
		 VALUES ('s_cas_bob', 'b', ?, ?, 'private', '', ?, now())`,
		bob.TenantID, bob.UserID, ws.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`INSERT INTO sessions (id, title, tenant_id, user_id, visibility, agent_id, created_at)
		 VALUES ('s_cas_loose', 'loose', ?, ?, 'private', '', now())`,
		testOwner.TenantID, testOwner.UserID).Error; err != nil {
		t.Fatal(err)
	}
	runsBefore := s.count(t, &Run{}, "")

	if _, err := s.DeleteWorkspaceCascade(ctx, bob.TenantID, bob.UserID, ws.ID); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("成员删除 = %v, want not found", err)
	}
	def, err := s.EnsureDefaultWorkspace(ctx, testOwner.TenantID, testOwner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, def.ID); !errors.Is(err, ErrWorkspaceDefault) {
		t.Fatalf("默认工作区 = %v, want default", err)
	}
	if s.count(t, &Workspace{}, "id = ?", def.ID) != 1 {
		t.Fatal("默认工作区被删掉了")
	}

	ids, err := s.DeleteWorkspaceCascade(ctx, testOwner.TenantID, testOwner.UserID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[SessionID]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[sid] || !got["s_cas_bob"] {
		t.Fatalf("连带会话 = %v, want s_cas 与 s_cas_bob", ids)
	}
	if s.count(t, &Workspace{}, "id = ?", ws.ID) != 0 {
		t.Fatal("项目行还在")
	}
	if s.sqlBool(t, `SELECT deleted_at IS NOT NULL FROM sessions WHERE id = ?`, sid) == false {
		t.Fatal("创建者的会话没有 deleted_at")
	}
	if s.sqlBool(t, `SELECT deleted_at IS NOT NULL FROM sessions WHERE id = 's_cas_bob'`) == false {
		t.Fatal("成员的会话没有 deleted_at")
	}
	if s.sqlBool(t, `SELECT deleted_at IS NULL FROM sessions WHERE id = 's_cas_loose'`) == false {
		t.Fatal("游离会话被软删了")
	}
	if s.count(t, &Run{}, "") != runsBefore {
		t.Fatalf("runs = %d, want %d", s.count(t, &Run{}, ""), runsBefore)
	}
}

func TestDeleteSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_del"), RunID("r_del")
	s.mustAcquire(t, sid, rid, "m_del", "hi")

	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, sid); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("活 run 删除 = %v, want busy", err)
	}
	if s.sqlBool(t, `SELECT deleted_at IS NULL FROM sessions WHERE id = ?`, sid) == false {
		t.Fatal("活 run 拒绝后会话被标成已删")
	}

	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun = (ok %v, err %v)", ok, err)
	}
	bob := principal{UserID: "u_bob", TenantID: testOwner.TenantID, Role: "member"}
	if err := s.db.WithContext(ctx).Exec(
		`INSERT INTO sessions (id, title, tenant_id, user_id, visibility, agent_id, created_at)
		 VALUES ('s_bob_priv', 'b', ?, ?, 'private', '', now())`,
		bob.TenantID, bob.UserID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, "s_bob_priv"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("删别人的私有会话 = %v, want not found", err)
	}
	if err := s.db.WithContext(ctx).Exec(`UPDATE sessions SET visibility = 'tenant' WHERE id = ?`, sid).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, bob.TenantID, bob.UserID, sid); !errors.Is(err, ErrSessionForbidden) {
		t.Fatalf("成员删属主的可见会话 = %v, want forbidden", err)
	}
	if err := s.db.WithContext(ctx).Exec(`UPDATE sessions SET visibility = 'tenant' WHERE id = 's_bob_priv'`).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, "s_bob_priv"); !errors.Is(err, ErrSessionForbidden) {
		t.Fatalf("tenant owner 删成员会话 = %v, want forbidden", err)
	}
	if s.sqlBool(t, `SELECT deleted_at IS NULL FROM sessions WHERE id = 's_bob_priv'`) == false {
		t.Fatal("拒绝后成员会话被标成已删")
	}
	runsBefore := s.count(t, &Run{}, "")
	logsBefore := s.count(t, &RunlogEntry{}, "")

	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, sid); err != nil {
		t.Fatal(err)
	}
	if s.sqlBool(t, `SELECT deleted_at IS NOT NULL FROM sessions WHERE id = ?`, sid) == false {
		t.Fatal("属主删除后没有 deleted_at")
	}
	if s.count(t, &Session{}, "id = ?", sid) != 1 {
		t.Fatal("软删把会话行删掉了")
	}
	if s.count(t, &Run{}, "") != runsBefore || s.count(t, &RunlogEntry{}, "") != logsBefore {
		t.Fatalf("runs/runlog = %d/%d, want %d/%d", s.count(t, &Run{}, ""), s.count(t, &RunlogEntry{}, ""), runsBefore, logsBefore)
	}
	if err := s.DeleteSession(ctx, testOwner.TenantID, testOwner.UserID, sid); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("再删一次 = %v, want not found", err)
	}
	if _, _, err := s.SessionAccess(ctx, sid, testOwner); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("删除后 SessionAccess = %v", err)
	}
}

func TestRemoveSessionSystemArchives(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tenants", "t1", "sessions", "s1")
	if err := os.MkdirAll(filepath.Join(dir, ".tako", "search"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tako", "search", "spill.txt"), []byte("spill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deliverable.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeSessionSystemArchives(root, "t1", "s1")
	if b, err := os.ReadFile(filepath.Join(dir, "deliverable.md")); err != nil || string(b) != "keep" {
		t.Fatalf("产出 = %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".tako")); !os.IsNotExist(err) {
		t.Fatalf(".tako 还在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".media")); !os.IsNotExist(err) {
		t.Fatalf(".media 还在: %v", err)
	}
	removeSessionSystemArchives(root, "t1", "s1")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("会话目录不该被删掉")
	}
}
