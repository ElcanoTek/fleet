// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// TestPasswordBearerIsRetired pins #2049 M4 (ADR-0105): the Operations
// Center's own password bearer is gone. A bearer — even the shape of one the
// old POST /auth/login minted, for a real account — authenticates nowhere: not
// the auth middleware, not task creation (outside it), not upload. The admin
// API key keeps working.
func TestPasswordBearerIsRetired(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)
	seedOpsUser(t, store, uuid.New(), "pat@x.com", "admin", "")
	const bearer = "Bearer a-token-the-retired-login-would-have-minted"

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", bearer)
	w := httptest.NewRecorder()
	h.AdminOrUserAuthMiddleware(ok).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("auth middleware accepted a bearer: %d", w.Code)
	}

	req = httptest.NewRequest("POST", "/tasks", strings.NewReader(`{}`))
	req.Header.Set("Authorization", bearer)
	w = httptest.NewRecorder()
	if _, _, authorized := h.authorizeTaskCreator(w, req); authorized || w.Code != http.StatusUnauthorized {
		t.Errorf("task creation accepted a bearer: authorized=%v %d", authorized, w.Code)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "a.txt")
	_, _ = part.Write([]byte("x"))
	_ = mw.Close()
	up := httptest.NewRequest("POST", "/upload", &body)
	up.Header.Set("Content-Type", mw.FormDataContentType())
	up.Header.Set("Authorization", bearer)
	w = httptest.NewRecorder()
	h.HandleUpload(w, up)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("upload accepted a bearer: %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w = httptest.NewRecorder()
	h.AdminOrUserAuthMiddleware(ok).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("admin key refused: %d", w.Code)
	}
}

// Where Auth manages accounts, the admin-key POST /users (public as /v1/users)
// creates no Operations Center identity: one named like a Chat account would
// give it an Ops role Auth never granted.
func TestAccountsManagedByAuthRefusesOpsUserCreation(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)
	h.config.AccountsManagedByAuth = true
	r := httptest.NewRequest("POST", "/users", bytes.NewBufferString(`{"username": "ann@x.com", "password": "testpassword123", "role": "admin"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.CreateUser(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "managed in Auth") {
		t.Fatalf("create with Auth managing accounts: %d %s", w.Code, w.Body.String())
	}
	if u, _ := store.GetUserByUsername("ann@x.com"); u != nil {
		t.Fatalf("an Ops identity was created: %+v", u)
	}
	h.config.AccountsManagedByAuth = false
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/users", bytes.NewBufferString(`{"username": "ann@x.com", "password": "testpassword123", "role": "admin"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-API-Key", "admin-key")
	h.CreateUser(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create when Fleet manages accounts: %d %s", w.Code, w.Body.String())
	}
	// One identity table (#2049 M4): role admin is the Fleet admin role, so
	// the new account is an admin in chat too, with full Operations Center
	// access; and a second create of the same account is a conflict.
	acct, err := store.GetIdentity(t.Context(), "ann@x.com")
	if err != nil || acct.Role != "admin" || acct.LegacyOpsRole() != "admin" {
		t.Fatalf("created account = %+v, %v", acct, err)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/users", bytes.NewBufferString(`{"username": "ANN@x.com", "password": "testpassword123", "role": "client"}`))
	r.Header.Set("X-API-Key", "admin-key")
	h.CreateUser(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("second create: %d %s", w.Code, w.Body.String())
	}
	// A client create is a viewer with ops_scope run.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/users", bytes.NewBufferString(`{"username": "cli@x.com", "password": "testpassword123", "role": "client"}`))
	r.Header.Set("X-API-Key", "admin-key")
	h.CreateUser(w, r)
	acct, err = store.GetIdentity(t.Context(), "cli@x.com")
	if w.Code != http.StatusCreated || err != nil || acct.Role != "viewer" || acct.OpsScope != "run" {
		t.Fatalf("client create: %d %s / %+v %v", w.Code, w.Body.String(), acct, err)
	}
}

// POST /users judges the parsed grant, not the raw string: "none" in any
// spelling is refused, and padded/re-cased roles are invalid rather than
// trimmed past the check (Codex 4504695158).
func TestCreateUserRefusesNoneAndNonCanonicalRoles(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)
	for i, role := range []string{"none", " none ", "none ", "\tnone", " client ", "Admin", "view ", ""} {
		email := fmt.Sprintf("pad%d@x.com", i)
		body := fmt.Sprintf(`{"username": %q, "password": "testpassword123", "role": %q}`, email, role)
		r := httptest.NewRequest("POST", "/users", bytes.NewBufferString(body))
		r.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.CreateUser(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("role %q: status %d %s, want 400", role, w.Code, w.Body.String())
		}
		if acct, err := store.GetIdentity(t.Context(), email); err == nil && acct != nil {
			t.Errorf("role %q created an account: %+v", role, acct)
		}
	}
	// The new scope names are accepted as written.
	r := httptest.NewRequest("POST", "/users", bytes.NewBufferString(`{"username": "viewer@x.com", "password": "testpassword123", "role": "view"}`))
	r.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.CreateUser(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("role view: %d %s", w.Code, w.Body.String())
	}
}

// An email that is already a chat account (no Operations Center access) is
// promoted by POST /users — the e2e seed's `fleet sched user add` on its chat
// user, and any script that added an Operations Center user for a chat email
// before the one identity table — and then reaches the Operations Center
// through the session cookie (header trust). Its chat password is unchanged.
// One that already has access, or is disabled, is still a 409.
func TestCreateUserPromotesAnExistingChatAccount(t *testing.T) {
	t.Parallel()

	h, store := setupTest(t)
	ctx := t.Context()
	hash, err := identity.HashPassword("chat-password-placeholder", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Identity().CreateAccount(ctx, identity.NewAccount{Email: "chat@x.com", PasswordHash: hash, Role: identity.RoleMember}); err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	me := func(email string) int {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req = asSession(req, email)
		w := httptest.NewRecorder()
		h.AdminOrUserAuthMiddleware(ok).ServeHTTP(w, req)
		return w.Code
	}
	if code := me("chat@x.com"); code != http.StatusForbidden {
		t.Fatalf("a chat member without Operations Center access: /me = %d, want 403", code)
	}
	create := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/users", bytes.NewBufferString(body))
		r.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.CreateUser(w, r)
		return w
	}
	if w := create(`{"username": "chat@x.com", "password": "another-password-1", "role": "admin"}`); w.Code != http.StatusCreated {
		t.Fatalf("promote the chat account: %d %s", w.Code, w.Body.String())
	}
	acct, err := store.GetIdentity(ctx, "chat@x.com")
	if err != nil || acct.Role != identity.RoleAdmin || acct.LegacyOpsRole() != "admin" {
		t.Fatalf("promoted account = %+v, %v", acct, err)
	}
	if code := me("chat@x.com"); code != http.StatusOK {
		t.Fatalf("the promoted account: /me = %d, want 200", code)
	}
	var stored string
	if err := store.DB().Conn().QueryRowContext(ctx, `SELECT password_hash FROM core.users WHERE email = 'chat@x.com'`).Scan(&stored); err != nil || stored != hash {
		t.Errorf("the promotion changed the chat password (%v)", err)
	}
	// Access already there: 409, nothing changed.
	if w := create(`{"username": "chat@x.com", "password": "another-password-1", "role": "readonly"}`); w.Code != http.StatusConflict {
		t.Fatalf("second add: %d %s", w.Code, w.Body.String())
	}
	// A non-admin grant keeps the chat role.
	if _, err := store.Identity().CreateAccount(ctx, identity.NewAccount{Email: "viewer@x.com", Role: identity.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if w := create(`{"username": "viewer@x.com", "password": "another-password-1", "role": "client"}`); w.Code != http.StatusCreated {
		t.Fatalf("grant client: %d %s", w.Code, w.Body.String())
	}
	if acct, err := store.GetIdentity(ctx, "viewer@x.com"); err != nil || acct.Role != identity.RoleViewer || acct.OpsScope != identity.OpsRun {
		t.Fatalf("client grant = %+v, %v", acct, err)
	}
}
