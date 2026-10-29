// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestPasswordLoginDisabledRefusesLoginAndItsBearer pins the central-Auth-only
// switch on the Operations Center: POST /auth/login refuses, and a bearer that
// login minted BEFORE the switch is refused by every consumer (the auth
// middleware, task creation, which sits outside it, and upload), while the
// admin API key keeps working.
func TestPasswordLoginDisabledRefusesLoginAndItsBearer(t *testing.T) {
	h, _ := setupTest(t)

	create := httptest.NewRequest("POST", "/users", bytes.NewBufferString(`{"username": "ops@x.com", "password": "testpassword123", "role": "client"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.CreateUser(w, create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}

	login := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(`{"username": "ops@x.com", "password": "testpassword123"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.Login(w, r)
		return w
	}
	w = login()
	if w.Code != http.StatusOK {
		t.Fatalf("login with passwords on: %d %s", w.Code, w.Body.String())
	}
	var resp models.LoginResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Token == "" {
		t.Fatalf("login response: %v %+v", err, resp)
	}
	bearer := "Bearer " + resp.Token

	reached := false
	protected := h.AdminOrUserAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	viaMiddleware := func(header, value string) int {
		reached = false
		r := httptest.NewRequest("GET", "/tasks", nil)
		r.Header.Set(header, value)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, r)
		return w.Code
	}
	if code := viaMiddleware("Authorization", bearer); code != http.StatusNoContent || !reached {
		t.Fatalf("bearer with passwords on: %d", code)
	}

	h.config.PasswordLoginDisabled = true

	if w := login(); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Sign in through SSO") {
		t.Errorf("login with passwords off: %d %s", w.Code, w.Body.String())
	}
	if code := viaMiddleware("Authorization", bearer); code != http.StatusUnauthorized || reached {
		t.Errorf("middleware accepted an earlier password bearer: %d", code)
	}
	if code := viaMiddleware("X-API-Key", "admin-key"); code != http.StatusNoContent || !reached {
		t.Errorf("admin key refused with passwords off: %d", code)
	}

	task := httptest.NewRequest("POST", "/tasks", bytes.NewBufferString(`{"name": "t", "prompt": "p"}`))
	task.Header.Set("Content-Type", "application/json")
	task.Header.Set("Authorization", bearer)
	w = httptest.NewRecorder()
	h.CreateTask(w, task)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("task creation accepted an earlier password bearer: %d %s", w.Code, w.Body.String())
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("file", "brief.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("hello"))
	_ = mw.Close()
	up := httptest.NewRequest("POST", "/upload", bytes.NewReader(body.Bytes()))
	up.Header.Set("Content-Type", mw.FormDataContentType())
	up.Header.Set("Authorization", bearer)
	w = httptest.NewRecorder()
	h.HandleUpload(w, up)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("upload accepted an earlier password bearer: %d %s", w.Code, w.Body.String())
	}
}

// Where Auth manages accounts, the admin-key POST /users (public as /v1/users)
// creates no Operations Center identity: one named like a Chat account would
// give it an Ops role Auth never granted.
func TestAccountsManagedByAuthRefusesOpsUserCreation(t *testing.T) {
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
}
