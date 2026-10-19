package runengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestSignupPersonalTenantAndDuplicate(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("strong-pass-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	alice, tenant, member, err := s.Signup(ctx, "alice@example.com", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	if tenant.Kind != "personal" || tenant.Name != "alice" || member.Role != "owner" {
		t.Fatalf("tenant=%+v member=%+v", tenant, member)
	}
	if alice.DefaultTenantID == nil || *alice.DefaultTenantID != tenant.ID {
		t.Fatalf("default=%v tenant=%s", alice.DefaultTenantID, tenant.ID)
	}
	if _, _, _, err := s.Signup(ctx, "alice@example.com", string(hash)); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate err=%v", err)
	}
	var tenants, users int64
	if err := s.db.Model(&Tenant{}).Count(&tenants).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&User{}).Count(&users).Error; err != nil {
		t.Fatal(err)
	}
	if tenants != 1 || users != 1 {
		t.Fatalf("tenants=%d users=%d", tenants, users)
	}
	bob, bobTenant, _, err := s.Signup(ctx, "bob@example.com", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	if bobTenant.ID == tenant.ID {
		t.Fatal("bob reused alice tenant")
	}
	team, _, err := s.CreateTeamTenant(ctx, alice.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueRefreshToken(ctx, bob.ID, bobTenant.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SwitchDefaultTenant(ctx, bob.ID, team.ID, hashRefresh(raw)); !errors.Is(err, ErrNotMember) {
		t.Fatalf("switch err=%v", err)
	}
	reloaded, err := s.GetUserByID(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DefaultTenantID == nil || *reloaded.DefaultTenantID != bobTenant.ID {
		t.Fatalf("bob default changed: %v", reloaded.DefaultTenantID)
	}
}

func TestRefreshRotation(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("strong-pass-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	user, _, _, err := s.Signup(ctx, "alice@example.com", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueRefreshToken(ctx, user.ID, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RotateRefreshToken(ctx, hashRefresh(raw), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RotateRefreshToken(ctx, hashRefresh(raw), time.Hour); !errors.Is(err, ErrRefreshInvalid) {
		t.Fatalf("reuse err=%v", err)
	}
	var row RefreshToken
	if err := s.db.Where("token_hash = ?", hashRefresh(raw)).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RevokedAt == nil || row.ReplacedBy == "" || row.TokenHash == raw {
		t.Fatalf("row=%+v", row)
	}
}
