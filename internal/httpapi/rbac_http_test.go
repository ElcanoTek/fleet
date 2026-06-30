package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// setRole assigns a role (and optional team) to a seeded user through the real
// store, so the membership middleware enriches the request with it.
func setRole(t *testing.T, s *Server, email, role, team string) {
	t.Helper()
	var teamArg *string
	if team != "" {
		teamArg = &team
	}
	if _, err := s.concreteStore(t).SetUserRoleTeam(context.Background(), email, &role, teamArg); err != nil {
		t.Fatalf("set role %s=%s: %v", email, role, err)
	}
}

// createConv is a small helper that POSTs a new conversation and returns its id.
func createConv(t *testing.T, h http.Handler, user, title string) string {
	t.Helper()
	w := do(t, h, http.MethodPost, "/conversations", map[string]any{"title": title}, user)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("create conversation: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create resp: %v (%s)", err, w.Body.String())
	}
	if resp.ID == "" {
		t.Fatalf("create conversation: empty id (%s)", w.Body.String())
	}
	return resp.ID
}

// TestViewerCannotWrite verifies the read-only role gate (#237): a viewer keeps
// full read access but is 403 {"error":"read_only"} on any mutating method.
func TestViewerCannotWrite(t *testing.T) {
	s := memberFixture(t, "viewer@x.com", "member@x.com")
	setRole(t, s, "viewer@x.com", "viewer", "")
	h := s.Routes()

	// A member can create.
	createConv(t, h, "member@x.com", "by member")

	// A viewer is blocked from creating.
	w := do(t, h, http.MethodPost, "/conversations", map[string]any{"title": "nope"}, "viewer@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer POST: status %d want 403 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "read_only") {
		t.Errorf("viewer POST body = %q, want read_only", w.Body.String())
	}

	// But a viewer can still read (GET).
	w = do(t, h, http.MethodGet, "/conversations", nil, "viewer@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("viewer GET: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
}

// TestAdminGateHonorsDBRole verifies admin endpoints accept a users.role=admin
// account even with an empty ADMIN_EMAILS allowlist (#237), and reject a member.
func TestAdminGateHonorsDBRole(t *testing.T) {
	s := memberFixture(t, "boss@x.com", "peon@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	// DB-admin reaches the Users tab.
	w := do(t, h, http.MethodGet, "/admin/users", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("db-admin /admin/users: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Users []adminUser `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(resp.Users) != 2 {
		t.Errorf("listed %d users, want 2", len(resp.Users))
	}

	// A plain member is forbidden.
	w = do(t, h, http.MethodGet, "/admin/users", nil, "peon@x.com")
	if w.Code != http.StatusForbidden {
		t.Errorf("member /admin/users: status %d want 403", w.Code)
	}
}

// TestAdminUserPatch verifies PATCH /admin/users/{email} assigns role + team.
func TestAdminUserPatch(t *testing.T) {
	s := memberFixture(t, "boss@x.com", "target@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	w := do(t, h, http.MethodPatch, "/admin/users/target@x.com",
		map[string]any{"role": "viewer", "team_id": "growth"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("patch: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	var u adminUser
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.Role != "viewer" || u.TeamID != "growth" {
		t.Errorf("patched user = %+v, want viewer/growth", u)
	}

	// An invalid role is a 400, not a silent no-op.
	w = do(t, h, http.MethodPatch, "/admin/users/target@x.com",
		map[string]any{"role": "wizard"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid role: status %d want 400 (body %s)", w.Code, w.Body.String())
	}

	// Unknown user → 404.
	w = do(t, h, http.MethodPatch, "/admin/users/ghost@x.com",
		map[string]any{"role": "member"}, "boss@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown user patch: status %d want 404", w.Code)
	}
}

// TestTeamScopeEndpoint verifies GET /conversations?scope=team returns the
// conversations teammates shared (and only those), and POST .../share-with-team
// toggles the opt-in (#237).
func TestTeamScopeEndpoint(t *testing.T) {
	s := memberFixture(t, "alice@x.com", "bob@x.com", "loner@x.com")
	setRole(t, s, "alice@x.com", "member", "blue")
	setRole(t, s, "bob@x.com", "member", "blue")
	h := s.Routes()

	convID := createConv(t, h, "alice@x.com", "alice shared")

	// Before opt-in, Bob's team view is empty.
	w := do(t, h, http.MethodGet, "/conversations?scope=team", nil, "bob@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("team scope (pre-share): status %d body %s", w.Code, w.Body.String())
	}
	if n := countConversations(t, w.Body.Bytes()); n != 0 {
		t.Fatalf("pre-share team view has %d conversations, want 0", n)
	}

	// Alice opts the conversation into team visibility.
	w = do(t, h, http.MethodPost, "/conversations/"+convID+"/share-with-team",
		map[string]any{"visible": true}, "alice@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("share-with-team: status %d body %s", w.Code, w.Body.String())
	}

	// Bob now sees it.
	w = do(t, h, http.MethodGet, "/conversations?scope=team", nil, "bob@x.com")
	if n := countConversations(t, w.Body.Bytes()); n != 1 {
		t.Fatalf("post-share team view has %d conversations, want 1 (body %s)", n, w.Body.String())
	}

	// A caller with no team gets 400.
	w = do(t, h, http.MethodGet, "/conversations?scope=team", nil, "loner@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("no-team scope: status %d want 400", w.Code)
	}
}

func countConversations(t *testing.T, body []byte) int {
	t.Helper()
	var resp struct {
		Conversations []json.RawMessage `json:"conversations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode conversations: %v (%s)", err, string(body))
	}
	return len(resp.Conversations)
}
