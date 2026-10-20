package runengine

import (
	"context"
	"errors"
	"testing"
)

func TestSessionAccessPrivateAndTenant(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_acc"), RunID("r_acc")
	s.mustAcquire(t, sid, rid, "m_acc", "hi")

	alice := testOwner
	bob := principal{UserID: "u_bob", TenantID: "t_test", Role: "member"}
	carol := principal{UserID: "u_carol", TenantID: "t_other", Role: "owner"}

	if _, kind, err := s.SessionAccess(ctx, sid, alice); err != nil || kind != accessOwner {
		t.Fatalf("owner = %v %v, want owner", kind, err)
	}
	if _, _, err := s.SessionAccess(ctx, sid, bob); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("同租户 private = %v, want not found", err)
	}
	if _, _, err := s.SessionAccess(ctx, sid, carol); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("跨租户 = %v, want not found", err)
	}
	if _, _, _, _, exists, err := s.SessionStatus(ctx, sid, bob); err != nil || exists {
		t.Fatalf("bob status exists=%v err=%v, want false", exists, err)
	}
	if err := s.SetSessionVisibility(ctx, sid, bob, "tenant"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("bob 改可见性 = %v, want not found", err)
	}
	if err := s.SetSessionVisibility(ctx, sid, alice, "nope"); !errors.Is(err, ErrBadVisibility) {
		t.Fatalf("非法 visibility = %v", err)
	}
	if err := s.SetSessionVisibility(ctx, sid, alice, "tenant"); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := s.SessionAccess(ctx, sid, bob); err != nil || kind != accessRead {
		t.Fatalf("tenant-visible = %v %v, want read", kind, err)
	}
	ok, err := s.AcquireRun(ctx, sid, RunID("r_acc2"), Row{RunID: "r_acc2", Role: "user", MessageID: "m2", Text: "x"}, bob, "")
	if ok || !errors.Is(err, ErrSessionForbidden) {
		t.Fatalf("bob 写入 = (ok %v, err %v), want forbidden", ok, err)
	}
	rows, err := s.ListSessions(ctx, bob, "own", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("bob own = %d %v", len(rows), err)
	}
	rows, err = s.ListSessions(ctx, bob, "tenant", "")
	if err != nil || len(rows) != 1 || rows[0].ID != sid {
		t.Fatalf("bob tenant list = %+v %v", rows, err)
	}
}
