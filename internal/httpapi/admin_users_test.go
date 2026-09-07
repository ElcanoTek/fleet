package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOpsAdmins records the two-plane calls the admin Users endpoints make so
// the tests can assert grant-on-promote / revoke-on-demote-or-delete without a
// sched DB.
type fakeOpsAdmins struct {
	ensured, removed []string
	admins           []string
	// roles: explicit per-email sched-plane roles set via SetRole; admins
	// listed above are reported as role "admin" alongside.
	roles map[string]string
}

func (f *fakeOpsAdmins) Ensure(_ context.Context, email string) error {
	f.ensured = append(f.ensured, email)
	f.admins = append(f.admins, email)
	return nil
}

func (f *fakeOpsAdmins) Remove(_ context.Context, email string) error {
	f.removed = append(f.removed, email)
	out := f.admins[:0]
	for _, a := range f.admins {
		if !strings.EqualFold(a, email) {
			out = append(out, a)
		}
	}
	f.admins = out
	return nil
}

func (f *fakeOpsAdmins) List(context.Context) ([]string, error) {
	return append([]string(nil), f.admins...), nil
}

func (f *fakeOpsAdmins) SetRole(_ context.Context, email, role string) error {
	if f.roles == nil {
		f.roles = map[string]string{}
	}
	key := strings.ToLower(email)
	if role == "" || role == "none" {
		delete(f.roles, key)
		return f.Remove(context.Background(), email)
	}
	f.roles[key] = role
	if role == "admin" {
		f.admins = append(f.admins, email)
	}
	return nil
}

func (f *fakeOpsAdmins) Roles(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range f.admins {
		out[strings.ToLower(a)] = "admin"
	}
	for k, v := range f.roles {
		out[k] = v
	}
	return out, nil
}

// TestAdminUserLifecycle drives the full UI user-management surface end to
// end: create (with role → ops grant), duplicate → 409, password reset,
// demote → ops revoke, delete → ops revoke, and the self-demote/self-delete
// lockout guards.
func TestAdminUserLifecycle(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	ops := &fakeOpsAdmins{}
	WithOpsAdmins(ops)(s)
	h := s.Routes()

	// Create carol as admin → 201, ops grant, annotated response.
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
	if len(ops.ensured) != 1 || ops.ensured[0] != "carol@x.com" {
		t.Errorf("ops.ensured = %v, want [carol@x.com]", ops.ensured)
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

	// Demote carol → ops revoke.
	w = do(t, h, http.MethodPatch, "/admin/users/carol@x.com",
		map[string]any{"role": "member"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("demote: status %d (body %s)", w.Code, w.Body.String())
	}
	if len(ops.removed) != 1 || ops.removed[0] != "carol@x.com" {
		t.Errorf("ops.removed after demote = %v, want [carol@x.com]", ops.removed)
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

	// Delete carol → 204 + second ops revoke; a repeat delete → 404.
	w = do(t, h, http.MethodDelete, "/admin/users/carol@x.com", nil, "boss@x.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d want 204 (body %s)", w.Code, w.Body.String())
	}
	if len(ops.removed) != 2 {
		t.Errorf("ops.removed after delete = %v, want 2 entries", ops.removed)
	}
	w = do(t, h, http.MethodDelete, "/admin/users/carol@x.com", nil, "boss@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("repeat delete: status %d want 404", w.Code)
	}
}

// TestAdminUserEndpointsRequireAdmin verifies a plain member is 403 on every
// new mutating route (the adminMiddleware gate).
func TestAdminUserEndpointsRequireAdmin(t *testing.T) {
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

// TestAdminUserOpsRole locks in the split-permission semantics: ops_role
// grants client/readonly without touching the chat role; chat demote drops an
// implied ops-admin row but leaves an explicit client grant standing.
func TestAdminUserOpsRole(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	ops := &fakeOpsAdmins{}
	WithOpsAdmins(ops)(s)
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
	if got.Role != "member" || got.OpsCenterRole != "client" || got.OpsCenterAdmin {
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
