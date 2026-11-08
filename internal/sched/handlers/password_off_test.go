// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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
	if _, authorized := h.authorizeTaskCreator(w, req); authorized || w.Code != http.StatusUnauthorized {
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
