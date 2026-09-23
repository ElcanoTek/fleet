package store

import (
	"context"
	"errors"
	"testing"
)

func TestApplyExternalAccessIsOrderedAndNonDestructive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const email = "alice@example.com"

	user, err := s.CreateUser(ctx, email, "password-long-enough")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	originalCreatedAt := user.CreatedAt
	role, team := RoleViewer, "growth"
	user, err = s.SetUserRoleTeam(ctx, email, &role, &team)
	if err != nil {
		t.Fatalf("SetUserRoleTeam: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, email, "Retained work", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	grant := ExternalAccessState{
		Issuer: "https://auth.example.com", Subject: "account-123", Email: email,
		Version: 2, Allowed: true, EventID: "grant-2", ChatRole: RoleMember,
		OpsRole: "none", IssuedAt: 1_000,
	}
	if got, changed, err := s.ApplyExternalAccess(ctx, grant); err != nil || !changed || !got.Allowed {
		t.Fatalf("grant = (%+v, %v, %v), want applied allowed", got, changed, err)
	}
	if got, err := s.GetUser(ctx, email); err != nil || got.Role != RoleMember || got.CreatedAt != originalCreatedAt {
		t.Fatalf("granted user = (%+v, %v), want same member account", got, err)
	}

	revoke := grant
	revoke.Version = 3
	revoke.Allowed = false
	revoke.EventID = "revoke-3"
	if _, changed, err := s.ApplyExternalAccess(ctx, revoke); err != nil || !changed {
		t.Fatalf("revoke changed=%v err=%v", changed, err)
	}
	if _, err := s.GetUser(ctx, email); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("disabled GetUser err=%v, want ErrUserNotFound", err)
	}
	var retainedEmail, retainedRole, retainedTeam string
	var retainedCreatedAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT email, role, team_id, created_at FROM users WHERE email = $1`, email).
		Scan(&retainedEmail, &retainedRole, &retainedTeam, &retainedCreatedAt); err != nil {
		t.Fatalf("read disabled user: %v", err)
	}
	if retainedEmail != email || retainedRole != RoleMember || retainedTeam != "growth" || retainedCreatedAt != user.CreatedAt {
		t.Fatalf("disabled row = (%q,%q,%q,%d), want identity/data retained", retainedEmail, retainedRole, retainedTeam, retainedCreatedAt)
	}

	if _, changed, err := s.ApplyExternalAccess(ctx, grant); err != nil || changed {
		t.Fatalf("stale grant changed=%v err=%v, want ignored", changed, err)
	}
	if _, err := s.GetUser(ctx, email); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("stale grant re-enabled user: %v", err)
	}

	regrant := grant
	regrant.Version = 4
	regrant.EventID = "grant-4"
	if _, changed, err := s.ApplyExternalAccess(ctx, regrant); err != nil || !changed {
		t.Fatalf("regrant changed=%v err=%v", changed, err)
	}
	if got, err := s.GetUser(ctx, email); err != nil || got.CreatedAt != user.CreatedAt || got.TeamID != "growth" {
		t.Fatalf("regranted user = (%+v, %v), want preserved identity/team", got, err)
	}
	if got, err := s.Get(ctx, email, conversation.ID); err != nil || got == nil || got.Title != "Retained work" {
		t.Fatalf("retained conversation = (%+v, %v)", got, err)
	}
}
