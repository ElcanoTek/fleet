package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// opsOf reads the account's Operations Center access straight from its one
// identity row (#2049 M4): role, ops_scope and the legacy role they grant
// ("" for none). It reads the row whatever its enabled state.
func opsOf(t *testing.T, s *Server, email string) (role, scope, legacy string) {
	t.Helper()
	var u store.User
	users, err := s.concreteStore(t).ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range users {
		if x.Email == email {
			u = x
		}
	}
	if u.Email == "" {
		return "", "", ""
	}
	return u.Role, u.OpsScope, u.LegacyOpsRole()
}

// TestAdminUserLifecycle drives the full UI user-management surface end to
// end: create (role admin is full Operations Center access), duplicate → 409,
// password reset, demote (ends Operations Center admin), delete (the one row
// goes), and the self-demote/self-delete lockout guards.
func TestAdminUserLifecycle(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	// Create carol as admin → 201, Operations Center admin, annotated response.
	w := do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "carol@x.com", "password": "carol-pw-123", "role": "admin"}, "boss@x.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d want 201 (body %s)", w.Code, w.Body.String())
	}
	var created adminUser
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Role != "admin" || !created.OpsCenterAdmin {
		t.Errorf("create resp = %+v, want role=admin ops_center_admin=true", created)
	}
	if role, _, legacy := opsOf(t, s, "carol@x.com"); role != "admin" || legacy != "admin" {
		t.Errorf("carol's row = role %q ops %q, want admin/admin", role, legacy)
	}

	// Duplicate create → 409.
	w = do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "carol@x.com", "password": "carol-pw-123"}, "boss@x.com")
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate create: status %d want 409 (body %s)", w.Code, w.Body.String())
	}

	// Short password → 400.
	w = do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "dave@x.com", "password": "short"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("short-password create: status %d want 400 (body %s)", w.Code, w.Body.String())
	}

	// List annotates carol as ops-center admin.
	w = do(t, h, http.MethodGet, "/admin/users", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d (body %s)", w.Code, w.Body.String())
	}
	var list struct {
		Users []adminUser `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	foundCarol := false
	for _, u := range list.Users {
		if u.Email == "carol@x.com" {
			foundCarol = true
			if !u.OpsCenterAdmin {
				t.Error("list: carol should be annotated ops_center_admin")
			}
		}
	}
	if !foundCarol {
		t.Fatal("list: carol missing")
	}

	// Password reset: too short → 400; valid → 204 and the new password
	// authenticates through the real bcrypt path.
	w = do(t, h, http.MethodPut, "/admin/users/carol@x.com/password",
		map[string]any{"password": "short"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("short reset: status %d want 400", w.Code)
	}
	w = do(t, h, http.MethodPut, "/admin/users/carol@x.com/password",
		map[string]any{"password": "carol-pw-456"}, "boss@x.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("reset: status %d want 204 (body %s)", w.Code, w.Body.String())
	}
	if err := s.concreteStore(t).VerifyUser(context.Background(), "carol@x.com", "carol-pw-456"); err != nil {
		t.Errorf("new password does not verify: %v", err)
	}

	// Demote carol → Operations Center admin ends with it (scope none).
	w = do(t, h, http.MethodPatch, "/admin/users/carol@x.com",
		map[string]any{"role": "member"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("demote: status %d (body %s)", w.Code, w.Body.String())
	}
	if role, scope, legacy := opsOf(t, s, "carol@x.com"); role != "member" || scope != "none" || legacy != "" {
		t.Errorf("after demote carol = %s/%s/%q, want member/none/no access", role, scope, legacy)
	}

	// Self-demote and self-delete are refused.
	w = do(t, h, http.MethodPatch, "/admin/users/boss@x.com",
		map[string]any{"role": "member"}, "boss@x.com")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "demote your own") {
		t.Errorf("self-demote: status %d body %q, want 400 refusing", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodDelete, "/admin/users/boss@x.com", nil, "boss@x.com")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "delete your own") {
		t.Errorf("self-delete: status %d body %q, want 400 refusing", w.Code, w.Body.String())
	}

	// Delete carol → 204, the one row gone; a repeat delete → 404.
	w = do(t, h, http.MethodDelete, "/admin/users/carol@x.com", nil, "boss@x.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d want 204 (body %s)", w.Code, w.Body.String())
	}
	if role, _, _ := opsOf(t, s, "carol@x.com"); role != "" {
		t.Errorf("carol's row survived the delete (role %q)", role)
	}
	w = do(t, h, http.MethodDelete, "/admin/users/carol@x.com", nil, "boss@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("repeat delete: status %d want 404", w.Code)
	}
}

// TestAdminUserEndpointsRequireAdmin verifies a plain member is 403 on every
// new mutating route (the adminMiddleware gate).
func TestAdminUserEndpointsRequireAdmin(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "boss@x.com", "peon@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	cases := []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/admin/users", map[string]any{"email": "x@x.com", "password": "xxxxxxxx"}},
		{http.MethodDelete, "/admin/users/boss@x.com", nil},
		{http.MethodPut, "/admin/users/boss@x.com/password", map[string]any{"password": "yyyyyyyy"}},
	}
	for _, tc := range cases {
		w := do(t, h, tc.method, tc.path, tc.body, "peon@x.com")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s as member: status %d want 403", tc.method, tc.path, w.Code)
		}
	}
}

// TestAdminUserOpsRole locks in the Operations Center semantics over the one
// identity row (#2049 M4): ops_role grants client/readonly (ops_scope
// run/view) without touching the chat role; a chat demote ends Operations
// Center admin but leaves an explicit scope standing; admin wins over a
// narrower grant; ops_role admin without role admin (the split state) is a
// 400; ops_scope speaks the new names.
func TestAdminUserOpsRole(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	decode := func(w *httptest.ResponseRecorder) adminUser {
		t.Helper()
		var u adminUser
		if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
			t.Fatalf("decode: %v (body %s)", err, w.Body.String())
		}
		return u
	}

	w := do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "op@x.com", "password": "op-pw-12345", "ops_role": "client"}, "boss@x.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d (body %s)", w.Code, w.Body.String())
	}
	// Account creation can grant ops client without touching the chat role.
	got := decode(w)
	if got.Role != "member" || got.OpsCenterRole != "client" || got.OpsCenterAdmin || got.OpsScope != "run" {
		t.Fatalf("create with ops client grant: %+v", got)
	}

	// Invalid ops_role is rejected on both create and update.
	w = do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "bad@x.com", "password": "bad-pw-12345", "ops_role": "supreme"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid create ops_role: status %d", w.Code)
	}
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com",
		map[string]any{"ops_role": "supreme"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid ops_role: status %d", w.Code)
	}
	// Padded or re-cased grants are refused, not trimmed into a different
	// grant than the one validated (Codex 4504695158).
	for _, field := range []string{"ops_role", "ops_scope"} {
		for _, v := range []string{" none ", " run", "Admin", "client\n"} {
			w = do(t, h, http.MethodPost, "/admin/users",
				map[string]any{"email": "pad@x.com", "password": "pad-pw-12345", field: v}, "boss@x.com")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("create %s %q: status %d", field, v, w.Code)
			}
			w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{field: v}, "boss@x.com")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("patch %s %q: status %d", field, v, w.Code)
			}
		}
	}

	// chat promote → implied ops admin; chat demote → implied row dropped
	do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"role": "admin"}, "boss@x.com")
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"role": "member"}, "boss@x.com")
	if got = decode(w); got.OpsCenterRole == "admin" {
		t.Fatalf("chat demote left ops admin standing: %+v", got)
	}

	// explicit client grant survives a later chat-role change
	do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"ops_role": "client"}, "boss@x.com")
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"role": "viewer"}, "boss@x.com")
	if got = decode(w); got.OpsCenterRole != "client" {
		t.Fatalf("chat role change stomped explicit ops grant: %+v", got)
	}

	// none revokes
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"ops_role": "none"}, "boss@x.com")
	if got = decode(w); got.OpsCenterRole != "" {
		t.Fatalf("ops_role none did not revoke: %+v", got)
	}

	// Chat Admin in the same request wins over a narrower ops_role — on PATCH
	// exactly as on POST. The PATCH used to apply the implied ops-admin grant
	// and then overwrite it with the narrower role from the same body.
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com",
		map[string]any{"role": "admin", "ops_role": "readonly"}, "boss@x.com")
	if got = decode(w); got.Role != "admin" || got.OpsCenterRole != "admin" {
		t.Fatalf("PATCH role=admin + ops_role=readonly: want ops admin, got %+v", got)
	}
	w = do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "both@x.com", "password": "both-planes-pw", "role": "admin", "ops_role": "readonly"}, "boss@x.com")
	if got = decode(w); w.Code != http.StatusCreated || got.OpsCenterRole != "admin" {
		t.Fatalf("POST role=admin + ops_role=readonly: status %d, got %+v", w.Code, got)
	}

	// ops_role admin is the Fleet admin role: next to a non-admin role, or on
	// a non-admin with no role in the body, it is the split state ADR-0074
	// refuses — a 400, and nothing written.
	do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"role": "member", "ops_role": "none"}, "boss@x.com")
	for _, body := range []map[string]any{
		{"role": "member", "ops_role": "admin"},
		{"ops_role": "admin"},
	} {
		w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", body, "boss@x.com")
		if w.Code != http.StatusBadRequest {
			t.Errorf("PATCH %v: status %d want 400 (body %s)", body, w.Code, w.Body.String())
		}
	}
	if role, scope, _ := opsOf(t, s, "op@x.com"); role != "member" || scope != "none" {
		t.Errorf("a refused split-admin write changed the row: %s/%s", role, scope)
	}
	w = do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "split@x.com", "password": "placeholder-not-a-secret", "ops_role": "admin"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST ops_role=admin without role admin: status %d want 400", w.Code)
	}

	// ops_scope: the new names, and it wins over ops_role in the same body.
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"ops_scope": "view", "ops_role": "client"}, "boss@x.com")
	if got = decode(w); got.OpsScope != "view" || got.OpsCenterRole != "readonly" {
		t.Fatalf("PATCH ops_scope=view: %+v", got)
	}
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"ops_scope": "readonly"}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("PATCH ops_scope=readonly (a legacy name): status %d want 400", w.Code)
	}
	// ops_role accepts the new names too.
	w = do(t, h, http.MethodPatch, "/admin/users/op@x.com", map[string]any{"ops_role": "run"}, "boss@x.com")
	if got = decode(w); got.OpsScope != "run" || got.OpsCenterRole != "client" {
		t.Fatalf("PATCH ops_role=run: %+v", got)
	}
}

// failingRenameTeamStore is a chatStore whose RenameTeam fails with a plain
// (non-input) error — the seam for a transaction failure.
type failingRenameTeamStore struct{ chatStore }

func (failingRenameTeamStore) RenameTeam(context.Context, string, string) (int64, int64, error) {
	return 0, 0, errors.New("simulated postgres outage")
}

// POST /admin/teams/rename tells the caller's mistakes (blank or equal names,
// an unknown team → 400) apart from the server's (a failed transaction →
// 500). Every store error used to come back 400 with the raw driver text, so
// a Postgres outage read as the admin's own bad request.
func TestAdminTeamRenameErrorSplit(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "devops")
	h := s.Routes()

	for _, body := range []map[string]string{
		{"from": "", "to": "platform"},
		{"from": "devops", "to": "devops"},
		{"from": "no-such-team", "to": "platform"},
	} {
		w := do(t, h, http.MethodPost, "/admin/teams/rename", body, "boss@x.com")
		if w.Code != http.StatusBadRequest {
			t.Errorf("rename %v: status %d body=%q, want 400", body, w.Code, w.Body.String())
		}
	}
	w := do(t, h, http.MethodPost, "/admin/teams/rename", map[string]string{"from": "devops", "to": "platform"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("valid rename: status %d body=%q", w.Code, w.Body.String())
	}

	s.store = failingRenameTeamStore{s.store}
	h = s.Routes()
	w = do(t, h, http.MethodPost, "/admin/teams/rename", map[string]string{"from": "platform", "to": "infra"}, "boss@x.com")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("store failure: status %d body=%q, want 500", w.Code, w.Body.String())
	}
}
