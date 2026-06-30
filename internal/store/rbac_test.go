package store

import (
	"context"
	"errors"
	"testing"
)

// strptr is a small helper for the *string PATCH args of SetUserRoleTeam.
func strptr(s string) *string { return &s }

func TestGetUser_DefaultsAndNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "Dana@Example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	u, err := s.GetUser(ctx, "dana@example.com")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	// A freshly-created user defaults to the 'member' role with no team.
	if u.Role != RoleMember {
		t.Errorf("default role = %q, want %q", u.Role, RoleMember)
	}
	if u.TeamID != "" {
		t.Errorf("default team_id = %q, want empty", u.TeamID)
	}
	// Lookup is case-insensitive (normalized).
	if _, err := s.GetUser(ctx, "DANA@Example.com"); err != nil {
		t.Errorf("case-insensitive GetUser failed: %v", err)
	}

	// Unknown / empty → ErrUserNotFound.
	if _, err := s.GetUser(ctx, "ghost@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: got %v want ErrUserNotFound", err)
	}
	if _, err := s.GetUser(ctx, "   "); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("empty email: got %v want ErrUserNotFound", err)
	}
}

func TestSetUserRoleTeam(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "ed@x.com", "password123"); err != nil {
		t.Fatal(err)
	}

	// Set role only — team untouched (nil pointer).
	u, err := s.SetUserRoleTeam(ctx, "ed@x.com", strptr(RoleAdmin), nil)
	if err != nil {
		t.Fatalf("set role: %v", err)
	}
	if u.Role != RoleAdmin || u.TeamID != "" {
		t.Errorf("after role set: role=%q team=%q", u.Role, u.TeamID)
	}

	// Set team only — role untouched.
	u, err = s.SetUserRoleTeam(ctx, "ed@x.com", nil, strptr("growth"))
	if err != nil {
		t.Fatalf("set team: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("role should be untouched, got %q", u.Role)
	}
	if u.TeamID != "growth" {
		t.Errorf("team = %q, want growth", u.TeamID)
	}

	// Clear the team with an empty string (→ NULL), role untouched.
	u, err = s.SetUserRoleTeam(ctx, "ed@x.com", nil, strptr(""))
	if err != nil {
		t.Fatalf("clear team: %v", err)
	}
	if u.TeamID != "" {
		t.Errorf("team should be cleared, got %q", u.TeamID)
	}
	if u.Role != RoleAdmin {
		t.Errorf("role should survive a team clear, got %q", u.Role)
	}

	// Invalid role is rejected without touching the row.
	if _, err := s.SetUserRoleTeam(ctx, "ed@x.com", strptr("superuser"), nil); err == nil {
		t.Error("invalid role should be rejected")
	}

	// Unknown user → ErrUserNotFound.
	if _, err := s.SetUserRoleTeam(ctx, "nobody@x.com", strptr(RoleViewer), nil); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: got %v want ErrUserNotFound", err)
	}
}

func TestTeamVisibleConversations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Two teammates (team "blue") and one outsider.
	for _, e := range []string{"alice@x.com", "bob@x.com", "carol@x.com"} {
		if _, err := s.CreateUser(ctx, e, "password123"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []string{"alice@x.com", "bob@x.com"} {
		if _, err := s.SetUserRoleTeam(ctx, e, nil, strptr("blue")); err != nil {
			t.Fatalf("set team for %s: %v", e, err)
		}
	}
	// carol is on a different team.
	if _, err := s.SetUserRoleTeam(ctx, "carol@x.com", nil, strptr("red")); err != nil {
		t.Fatal(err)
	}

	// Alice has two conversations; she shares only one with the team.
	shared, err := s.CreateConversation(ctx, "alice@x.com", "shared", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := s.CreateConversation(ctx, "alice@x.com", "private", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Before any opt-in, the team view is empty for Bob.
	list, err := s.ListTeamConversations(ctx, "bob@x.com")
	if err != nil {
		t.Fatalf("ListTeamConversations (pre-share): %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("team view should be empty before opt-in, got %d", len(list))
	}

	// Alice opts the "shared" conversation into team visibility.
	if err := s.SetConversationTeamVisible(ctx, "alice@x.com", shared.ID, true); err != nil {
		t.Fatalf("SetConversationTeamVisible: %v", err)
	}

	// Bob (same team) now sees exactly the shared one — never the private one.
	list, err = s.ListTeamConversations(ctx, "bob@x.com")
	if err != nil {
		t.Fatalf("ListTeamConversations: %v", err)
	}
	if len(list) != 1 || list[0].ID != shared.ID {
		t.Fatalf("team view = %v, want only %s", list, shared.ID)
	}
	if list[0].ID == priv.ID {
		t.Error("private conversation leaked into team view")
	}

	// Carol (different team) sees nothing.
	if list, err := s.ListTeamConversations(ctx, "carol@x.com"); err != nil || len(list) != 0 {
		t.Errorf("cross-team view = (%v, %v), want empty", list, err)
	}

	// Un-sharing removes it from the team view again.
	if err := s.SetConversationTeamVisible(ctx, "alice@x.com", shared.ID, false); err != nil {
		t.Fatalf("un-share: %v", err)
	}
	if list, err := s.ListTeamConversations(ctx, "bob@x.com"); err != nil || len(list) != 0 {
		t.Errorf("after un-share = (%v, %v), want empty", list, err)
	}
}

func TestListTeamConversations_NoTeam(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "loner@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListTeamConversations(ctx, "loner@x.com"); !errors.Is(err, ErrNoTeam) {
		t.Errorf("no-team caller: got %v want ErrNoTeam", err)
	}
}

func TestSetConversationTeamVisible_OwnershipGate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, e := range []string{"owner@x.com", "intruder@x.com"} {
		if _, err := s.CreateUser(ctx, e, "password123"); err != nil {
			t.Fatal(err)
		}
	}
	conv, err := s.CreateConversation(ctx, "owner@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// A non-owner cannot flip the flag — the WHERE user_email gate yields
	// "conversation not found" rather than mutating someone else's row.
	if err := s.SetConversationTeamVisible(ctx, "intruder@x.com", conv.ID, true); err == nil {
		t.Error("non-owner should not be able to share another user's conversation")
	}
	// And the owner's view confirms it was never shared.
	if list, _ := s.ListTeamConversations(ctx, "intruder@x.com"); len(list) != 0 {
		t.Errorf("intruder somehow has a team view: %v", list)
	}
}
