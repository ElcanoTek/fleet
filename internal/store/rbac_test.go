package store

import (
	"context"
	"errors"
	"strings"
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

	// A chat can only be shared with the team from inside a project shared
	// with that team (ADR-0057) — the pairing is enforced by the store, so the
	// fixture files the chat first.
	proj, err := s.CreateProject(ctx, &Project{OwnerEmail: "alice@x.com", Name: "Blue", TeamID: "blue"})
	if err != nil {
		t.Fatal(err)
	}

	// Alice has two conversations; she shares only one with the team.
	shared, err := s.CreateConversation(ctx, "alice@x.com", "shared", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetConversationProject(ctx, "alice@x.com", shared.ID, proj.ID); err != nil {
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
	if stored, err := s.SetConversationTeamVisible(ctx, "alice@x.com", shared.ID, true); err != nil || !stored {
		t.Fatalf("SetConversationTeamVisible = (%v, %v), want (true, nil)", stored, err)
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

	// A public share token on the team-visible conversation must not
	// appear in the teammate listing (#1112).
	if err := s.SetShareToken(ctx, "alice@x.com", shared.ID, "cap-token-xyz", nil); err != nil {
		t.Fatalf("SetShareToken: %v", err)
	}
	list, err = s.ListTeamConversations(ctx, "bob@x.com")
	if err != nil {
		t.Fatalf("ListTeamConversations after share token: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("team view after share token = %d, want 1", len(list))
	}
	if list[0].ShareToken != "" {
		t.Errorf("teammate listing leaked share_token %q", list[0].ShareToken)
	}
	owner, err := s.Get(ctx, "alice@x.com", shared.ID)
	if err != nil || owner == nil || owner.ShareToken != "cap-token-xyz" {
		t.Fatalf("owner Get should still see the token: %+v %v", owner, err)
	}

	// Carol (different team) sees nothing.
	if list, err := s.ListTeamConversations(ctx, "carol@x.com"); err != nil || len(list) != 0 {
		t.Errorf("cross-team view = (%v, %v), want empty", list, err)
	}

	// Un-sharing removes it from the team view again.
	if stored, err := s.SetConversationTeamVisible(ctx, "alice@x.com", shared.ID, false); err != nil || stored {
		t.Fatalf("un-share = (%v, %v), want (false, nil)", stored, err)
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
	if _, err := s.SetConversationTeamVisible(ctx, "intruder@x.com", conv.ID, true); err == nil {
		t.Error("non-owner should not be able to share another user's conversation")
	}
	// And the owner's view confirms it was never shared.
	if list, _ := s.ListTeamConversations(ctx, "intruder@x.com"); len(list) != 0 {
		t.Errorf("intruder somehow has a team view: %v", list)
	}
}

// TestSetOwnTeam covers the self-serve team write (#1157): creating and leaving
// a team need no admin, but JOINING an existing trust group is refused with
// ErrTeamExists — a shared team_id is what exposes team-visible conversations
// and team-shared projects, so it cannot be claimable by typing a name.
func TestSetOwnTeam(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, e := range []string{"ann@x.com", "bo@x.com", "cy@x.com", "root@x.com"} {
		if _, err := s.CreateUser(ctx, e, "password123"); err != nil {
			t.Fatalf("CreateUser %s: %v", e, err)
		}
	}

	// Create: the first user into a name owns it.
	u, err := s.SetOwnTeam(ctx, "ann@x.com", " platform ", false)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if u.TeamID != "platform" { // trimmed
		t.Fatalf("team_id = %q, want %q", u.TeamID, "platform")
	}

	// Idempotent: re-stating your own team is a no-op, not a "join".
	if u, err = s.SetOwnTeam(ctx, "ann@x.com", "Platform", false); err != nil {
		t.Fatalf("restate own team: %v", err)
	}
	if u.TeamID != "platform" {
		t.Errorf("restate changed team_id to %q", u.TeamID)
	}

	// Join is refused — case-insensitively, so "Platform" cannot shadow it.
	if _, err = s.SetOwnTeam(ctx, "bo@x.com", "PLATFORM", false); !errors.Is(err, ErrTeamExists) {
		t.Fatalf("join existing team: got %v want ErrTeamExists", err)
	}
	// ...but an admin may add someone (allowExisting), the Users-tab path.
	if u, err = s.SetOwnTeam(ctx, "bo@x.com", "platform", true); err != nil {
		t.Fatalf("admin join: %v", err)
	}
	if u.TeamID != "platform" {
		t.Errorf("admin join team_id = %q", u.TeamID)
	}

	// A team-shared project keeps its name reserved after its last member
	// leaves, so its shared memory never becomes claimable.
	if _, err = s.CreateProject(ctx, &Project{OwnerEmail: "ann@x.com", Name: "Infra", TeamID: "orphan"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err = s.SetOwnTeam(ctx, "cy@x.com", "orphan", false); !errors.Is(err, ErrTeamExists) {
		t.Fatalf("claim a project-only team: got %v want ErrTeamExists", err)
	}

	// Leaving always works and clears the column.
	if u, err = s.SetOwnTeam(ctx, "bo@x.com", "  ", false); err != nil {
		t.Fatalf("leave team: %v", err)
	}
	if u.TeamID != "" {
		t.Errorf("after leaving, team_id = %q, want empty", u.TeamID)
	}

	// Guardrails: absurd names and unknown accounts.
	if _, err = s.SetOwnTeam(ctx, "cy@x.com", strings.Repeat("x", maxTeamNameLen+1), false); err == nil {
		t.Error("over-long team name accepted")
	}
	if _, err = s.SetOwnTeam(ctx, "ghost@x.com", "platform", true); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: got %v want ErrUserNotFound", err)
	}
}
