package runengine

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestLastOwnerAndTransfer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	hash, err := bcrypt.GenerateFromPassword([]byte("strong-pass-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	alice, _, _, err := s.Signup(ctx, "alice@example.com", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	bob, _, _, err := s.Signup(ctx, "bob@example.com", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	team, _, err := s.CreateTeamTenant(ctx, alice.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	actor := principal{UserID: alice.ID, Role: "owner", TenantID: team.ID}
	if _, err := s.AddMemberByEmail(ctx, team.ID, "bob@example.com", "member", actor); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveTenant(ctx, team.ID, alice.ID, actor); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("leave last owner = %v", err)
	}
	if err := s.TransferOwner(ctx, team.ID, bob.ID, actor); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveTenant(ctx, team.ID, alice.ID, principal{UserID: alice.ID, Role: "admin", TenantID: team.ID}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMembers(ctx, team.ID)
	if err != nil || len(rows) != 1 || rows[0].UserID != bob.ID || rows[0].Role != "owner" {
		t.Fatalf("members = %+v %v", rows, err)
	}
	if err := s.SetSystemAdmin(ctx, alice.ID, true, actor); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSystemAdmin(ctx, alice.ID, false, actor); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("revoke last admin = %v", err)
	}
	logs, err := s.ListAudit(ctx, team.ID, false, 20)
	if err != nil || len(logs) == 0 {
		t.Fatalf("audit = %d %v", len(logs), err)
	}
}
