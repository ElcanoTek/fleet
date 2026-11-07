// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestElcanoLogout(t *testing.T) {
	h := &Handlers{config: Config{ElcanoCookieName: "elcano_auth", ElcanoCookieDomain: "elcanotek.com"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	rr := httptest.NewRecorder()
	h.ElcanoLogout(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / (the server's own login)", loc)
	}
	// Must emit an expiring deletion cookie matching name + domain so the
	// browser actually drops the shared elcano_auth cookie.
	sc := rr.Header().Get("Set-Cookie")
	if !strings.Contains(sc, "elcano_auth=;") || !strings.Contains(sc, "Domain=elcanotek.com") || !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("Set-Cookie = %q, want an expired elcano_auth on Domain=elcanotek.com", sc)
	}
}

// TestLogoutEndsBearerSession pins that POST /auth/logout with an
// Authorization: Bearer token minted by Login ends that session server-side:
// the token authenticates before and is refused (401) after. A logout with
// some other bearer leaves the session alone.
func TestLogoutEndsBearerSession(t *testing.T) {
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	defer cleanup()
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0",
		ElcanoCookieName: "elcano_auth"}, store, keyMgr)
	r := chi.NewRouter()
	r.Post("/auth/logout", h.ElcanoLogout)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks", h.ListTasks)
	})
	addLogUser(t, store, "logout-user", "client", "logout-token")

	listWith := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	logoutWith := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if code := listWith("logout-token"); code != http.StatusOK {
		t.Fatalf("bearer before logout = %d, want 200", code)
	}
	if w := logoutWith("some-other-token"); w.Code != http.StatusSeeOther {
		t.Fatalf("logout with an unknown bearer = %d, want 303", w.Code)
	}
	if code := listWith("logout-token"); code != http.StatusOK {
		t.Fatalf("an unrelated logout must leave the session alone: got %d", code)
	}

	w := logoutWith("logout-token")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", w.Code)
	}
	if sc := w.Header().Get("Set-Cookie"); !strings.Contains(sc, "elcano_auth=;") {
		t.Errorf("logout must still delete the cookie, Set-Cookie = %q", sc)
	}
	if code := listWith("logout-token"); code != http.StatusUnauthorized {
		t.Fatalf("bearer after logout = %d, want 401", code)
	}
}
