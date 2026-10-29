// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// On a Fleet that signs in through central Auth only, chat-server checks no
// password (even a correct one, from a caller that skipped the gateway).
func TestPasswordLoginDisabledRefusesPasswordVerify(t *testing.T) {
	s := memberFixture(t, "carol@x.com")
	h := s.Routes()
	verify := func() (int, string) {
		w := do(t, h, http.MethodPost, "/auth/verify", map[string]string{"email": "carol@x.com", "password": "test-password-123"}, "carol@x.com")
		return w.Code, w.Body.String()
	}
	if code, body := verify(); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("verify with passwords on: %d %s", code, body)
	}
	s.cfg.PasswordLoginDisabled = true
	if code, body := verify(); code != http.StatusForbidden || strings.Contains(body, `"ok":true`) || !strings.Contains(body, "Sign in through SSO") {
		t.Errorf("verify with passwords off: %d %s", code, body)
	}
}

// Where central Auth manages accounts, every account write chat-server serves
// to Settings → Admin → Users and Settings → Team is refused and changes
// nothing: create, role/team edit, delete, password reset, team rename and a
// person's own team. That holds in ordinary SSO operation and with the
// break-glass on (passwords back) alike. Reads still work.
func TestAccountsManagedByAuthRefuseAccountWrites(t *testing.T) {
	for _, breakGlass := range []bool{false, true} {
		t.Run(map[bool]string{false: "sso", true: "break-glass"}[breakGlass], func(t *testing.T) {
			testAccountWritesRefused(t, breakGlass)
		})
	}
}

func testAccountWritesRefused(t *testing.T, breakGlass bool) {
	s := memberFixture(t, "boss@x.com", "carol@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	setRole(t, s, "carol@x.com", "member", "blue")
	WithOpsAdmins(&fakeOpsAdmins{})(s)
	h := s.Routes()
	st := s.concreteStore(t)
	s.cfg.AccountsManagedByAuth = true
	s.cfg.PasswordLoginDisabled = !breakGlass

	cases := []struct {
		name, method, path string
		body               any
		user, want         string
	}{
		{"create", http.MethodPost, "/admin/users", map[string]any{"email": "dave@x.com", "password": "dave-pw-1234"}, "boss@x.com", "Add people in Auth"},
		{"role and team", http.MethodPatch, "/admin/users/carol@x.com", map[string]any{"role": "viewer", "team_id": "red"}, "boss@x.com", "Change roles and teams there"},
		{"delete", http.MethodDelete, "/admin/users/carol@x.com", nil, "boss@x.com", "fleet admin rm"},
		{"password reset", http.MethodPut, "/admin/users/carol@x.com/password", map[string]string{"password": "new-pw-12345"}, "boss@x.com", "Reset the password in Auth"},
		{"team rename", http.MethodPost, "/admin/teams/rename", map[string]string{"from": "blue", "to": "green"}, "boss@x.com", "Rename the team there"},
		{"own team", http.MethodPut, "/me/team", map[string]string{"team_id": ""}, "carol@x.com", "Your team is set in Auth"},
		{"own team (PATCH)", http.MethodPatch, "/me/team", map[string]string{"team_id": "red"}, "carol@x.com", "Your team is set in Auth"},
	}
	for _, c := range cases {
		w := do(t, h, c.method, c.path, c.body, c.user)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}

	ctx := context.Background()
	if ok, err := st.IsUser(ctx, "dave@x.com"); err != nil || ok {
		t.Errorf("create made an account: %v %v", ok, err)
	}
	carol, err := st.GetUser(ctx, "carol@x.com")
	if err != nil {
		t.Fatalf("carol is gone: %v", err)
	}
	if carol.Role != "member" || carol.TeamID != "blue" {
		t.Errorf("carol changed: role %q team %q", carol.Role, carol.TeamID)
	}
	if breakGlass {
		if code := do(t, h, http.MethodPost, "/auth/verify", map[string]string{"email": "carol@x.com", "password": "test-password-123"}, "carol@x.com").Code; code != http.StatusOK {
			t.Errorf("break-glass password sign-in: %d", code)
		}
	}
	if w := do(t, h, http.MethodGet, "/admin/users", nil, "boss@x.com"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "carol@x.com") {
		t.Errorf("list: %d", w.Code)
	}

	// Managed locally again: the same edit goes through.
	s.cfg.AccountsManagedByAuth = false
	if w := do(t, h, http.MethodPatch, "/admin/users/carol@x.com", map[string]any{"role": "viewer"}, "boss@x.com"); w.Code != http.StatusOK {
		t.Errorf("role edit when Fleet manages accounts: %d %s", w.Code, w.Body.String())
	}
}

// Where Auth manages accounts, Fleet Admin comes from Auth alone: an
// ADMIN_EMAILS entry grants nothing.
func TestAccountsManagedByAuthIgnoreAdminEmails(t *testing.T) {
	s := memberFixture(t, "carol@x.com")
	s.cfg.AdminEmails = []string{"carol@x.com"}
	h := s.Routes()
	if w := do(t, h, http.MethodGet, "/admin/users", nil, "carol@x.com"); w.Code != http.StatusOK {
		t.Fatalf("ADMIN_EMAILS when Fleet manages accounts: %d", w.Code)
	}
	s.cfg.AccountsManagedByAuth = true
	if w := do(t, h, http.MethodGet, "/admin/users", nil, "carol@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("ADMIN_EMAILS while Auth manages accounts: %d", w.Code)
	}
}

// Central Auth's team rename reaches chat-server with the shared secret and no
// person: 204 once applied (and again for a redelivery, which changes
// nothing), 400 for a malformed one, 403 without the secret.
func TestExternalTeamRenameRoute(t *testing.T) {
	s := memberFixture(t, "ann@x.com", "ben@x.com")
	setRole(t, s, "ann@x.com", "member", "Trading")
	setRole(t, s, "ben@x.com", "member", "Ops")
	h := s.Routes()
	body := map[string]any{"event_id": "evt-1", "issuer": "https://auth.example.com", "version": 1, "from": "Trading", "to": "Research"}
	if w := do(t, h, http.MethodPost, "/auth/external-team-rename", body, ""); w.Code != http.StatusNoContent {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	ann, err := s.concreteStore(t).GetUser(context.Background(), "ann@x.com")
	if err != nil || ann.TeamID != "Research" {
		t.Fatalf("ann after rename: %+v %v", ann, err)
	}
	// A redelivery of version 1 carrying different names changes nothing.
	body["from"], body["to"] = "Ops", "Research"
	if w := do(t, h, http.MethodPost, "/auth/external-team-rename", body, ""); w.Code != http.StatusNoContent {
		t.Fatalf("redelivery: %d", w.Code)
	}
	if ben, _ := s.concreteStore(t).GetUser(context.Background(), "ben@x.com"); ben.TeamID != "Ops" {
		t.Fatalf("a redelivered version moved ben to %q", ben.TeamID)
	}
	if w := do(t, h, http.MethodPost, "/auth/external-team-rename", map[string]any{"event_id": "e", "issuer": "https://auth.example.com", "version": 2, "from": "", "to": "x"}, ""); w.Code != http.StatusBadRequest {
		t.Errorf("malformed: %d", w.Code)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/external-team-rename", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("without the shared secret: %d", w.Code)
	}
}

// A push may carry the provider's last team rename next to the team; it must
// be a non-negative number, and only ever next to a team.
func TestExternalAccessTeamRenameSetting(t *testing.T) {
	s := memberFixture(t, "ann@x.com")
	h := s.Routes()
	push := func(version int, settings map[string]string) int {
		t.Helper()
		return do(t, h, http.MethodPost, "/auth/external-access", map[string]any{
			"event_id": "e", "issuer": "https://auth.example.com", "subject": "acct-ann", "action": "grant",
			"version": version, "issued_at": 1000, "settings": settings,
		}, "ann@x.com").Code
	}
	if code := push(1, map[string]string{"chat_role": "member", "ops_role": "none", "team": "Trading", "team_rename": "0"}); code != http.StatusNoContent {
		t.Fatalf("with team_rename: %d", code)
	}
	for name, settings := range map[string]map[string]string{
		"without a team": {"chat_role": "member", "ops_role": "none", "team_rename": "5"},
		"not a number":   {"chat_role": "member", "ops_role": "none", "team": "Trading", "team_rename": "five"},
		"negative":       {"chat_role": "member", "ops_role": "none", "team": "Trading", "team_rename": "-1"},
	} {
		if code := push(2, settings); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
}
