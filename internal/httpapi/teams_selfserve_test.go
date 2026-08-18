package httpapi

// Self-serve team membership (#1157) — the HTTP surface of GET /me and
// PUT /me/team, plus the two admin-Users-tab regressions that made teams (and
// therefore team projects) unreachable on a fresh box.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// decodeMe reads the shared {email, role, team_id, admin} response body.
func decodeMe(t *testing.T, body []byte) meResponse {
	t.Helper()
	var out meResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode /me: %v (%s)", err, body)
	}
	return out
}

// TestEnvAdminCanSetOwnTeam is the #1157 regression. The Users tab PATCHes role
// and team together, and an ADMIN_EMAILS bootstrap admin has the default
// users.role = 'member', so the old blanket self-demote guard rejected every
// attempt that admin made to set their OWN team — leaving the box with no way
// to create a team at all, and "Share with my team" permanently broken.
func TestEnvAdminCanSetOwnTeam(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	s.cfg.AdminEmails = []string{"boss@x.com"} // admin by env only; DB role stays member
	h := s.Routes()

	w := do(t, h, http.MethodPatch, "/admin/users/boss@x.com",
		map[string]any{"role": "member", "team_id": "platform"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("self PATCH role+team: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	var got adminUser
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if got.TeamID != "platform" {
		t.Errorf("team_id = %q, want %q", got.TeamID, "platform")
	}
	if got.Role != "member" {
		t.Errorf("role = %q, want member (unchanged)", got.Role)
	}
}

// TestSelfDemoteStillRefused proves the narrowed guard still guards: a DB-role
// admin with an empty ADMIN_EMAILS cannot clear its own only grant, but the
// same request that leaves the role alone goes through.
func TestSelfDemoteStillRefused(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	w := do(t, h, http.MethodPatch, "/admin/users/boss@x.com",
		map[string]any{"role": "member"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("self demote: status %d want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "refusing to demote") {
		t.Errorf("self demote body = %q", w.Body.String())
	}

	// Same row, role unchanged: allowed.
	w = do(t, h, http.MethodPatch, "/admin/users/boss@x.com",
		map[string]any{"role": "admin", "team_id": "platform"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("self team edit: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
}

// TestSelfServeTeam covers /me + /me/team end to end: read your team, create
// one, get 409 trying to walk into someone else's, leave yours, and (as an
// admin) join an existing one.
func TestSelfServeTeam(t *testing.T) {
	s := memberFixture(t, "ann@x.com", "bo@x.com", "root@x.com")
	setRole(t, s, "root@x.com", "admin", "")
	h := s.Routes()

	// GET /me: teamless member.
	w := do(t, h, http.MethodGet, "/me", nil, "ann@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me: status %d (body %s)", w.Code, w.Body.String())
	}
	me := decodeMe(t, w.Body.Bytes())
	if me.Email != "ann@x.com" || me.TeamID != "" || me.Role != "member" || me.Admin {
		t.Fatalf("GET /me = %+v, want teamless non-admin member", me)
	}

	// Create a team — no admin involved.
	w = do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": "platform"}, "ann@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create team: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if me = decodeMe(t, w.Body.Bytes()); me.TeamID != "platform" {
		t.Errorf("after create, team_id = %q", me.TeamID)
	}

	// A second member cannot join it by name.
	w = do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": "platform"}, "bo@x.com")
	if w.Code != http.StatusConflict {
		t.Fatalf("join existing team: status %d want 409 (body %s)", w.Code, w.Body.String())
	}
	// ...and is still teamless afterwards.
	w = do(t, h, http.MethodGet, "/me/team", nil, "bo@x.com")
	if me = decodeMe(t, w.Body.Bytes()); me.TeamID != "" {
		t.Errorf("refused join leaked a team: %q", me.TeamID)
	}

	// An admin may join any team.
	w = do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": "platform"}, "root@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("admin join: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if me = decodeMe(t, w.Body.Bytes()); me.TeamID != "platform" || !me.Admin {
		t.Errorf("admin join = %+v", me)
	}

	// Leaving is always allowed.
	w = do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": ""}, "ann@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("leave team: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if me = decodeMe(t, w.Body.Bytes()); me.TeamID != "" {
		t.Errorf("after leave, team_id = %q", me.TeamID)
	}

	// A missing team_id is a 400, not an accidental "leave".
	w = do(t, h, http.MethodPut, "/me/team", map[string]any{}, "bo@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status %d want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestViewerCannotSetOwnTeam keeps the read-only role read-only: /me/team sits
// behind rejectViewerWrites, so a viewer reads its team but cannot change it.
func TestViewerCannotSetOwnTeam(t *testing.T) {
	s := memberFixture(t, "eyes@x.com")
	setRole(t, s, "eyes@x.com", "viewer", "audit")
	h := s.Routes()

	w := do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": "platform"}, "eyes@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT /me/team: status %d want 403 (body %s)", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodGet, "/me/team", nil, "eyes@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("viewer GET /me/team: status %d want 200", w.Code)
	}
	if me := decodeMe(t, w.Body.Bytes()); me.TeamID != "audit" {
		t.Errorf("viewer team_id = %q, want audit", me.TeamID)
	}
}

// TestProjectSharingAfterSelfServeTeam is the user-visible payoff: the same
// "Share with my team" project write that 400s for a teamless caller succeeds
// once they have created a team themselves.
func TestProjectSharingAfterSelfServeTeam(t *testing.T) {
	s := memberFixture(t, "ann@x.com")
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/projects",
		map[string]any{"name": "Roadmap", "team_shared": true}, "ann@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("teamless team_shared: status %d want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Settings → Team") {
		t.Errorf("teamless error should point at the self-serve fix, got %q", w.Body.String())
	}

	if w = do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": "platform"}, "ann@x.com"); w.Code != http.StatusOK {
		t.Fatalf("create team: status %d (body %s)", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodPost, "/projects",
		map[string]any{"name": "Roadmap", "team_shared": true}, "ann@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("shared project: status %d want 200 (body %s)", w.Code, w.Body.String())
	}
	var p struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode project: %v (%s)", err, w.Body.String())
	}
	if p.TeamID != "platform" {
		t.Errorf("project team_id = %q, want platform", p.TeamID)
	}
}
