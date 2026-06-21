// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestElcanoLogin covers the "Use Elcano email" handoff (GET /auth/elcano-login):
// when the public key is configured it 303s to the auth service's login carrying
// the server's own return URL; when it is not, it bounces back to the dashboard
// with a flag rather than trapping the user in a verify-fail redirect loop.
func TestElcanoLogin(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)

	t.Run("pubkey configured -> 303 to auth login with return_to", func(t *testing.T) {
		h := &Handlers{config: Config{
			ElcanoPubKey:    pub,
			AuthLoginURL:    "https://auth.elcanotek.com",
			OrchestratorURL: "https://moc.elcanotek.com",
		}}
		req := httptest.NewRequest(http.MethodGet, "/auth/elcano-login", nil)
		rr := httptest.NewRecorder()
		h.ElcanoLogin(rr, req)

		if rr.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rr.Code)
		}
		loc := rr.Header().Get("Location")
		if !strings.HasPrefix(loc, "https://auth.elcanotek.com/?return_to=") {
			t.Fatalf("Location = %q, want auth login with return_to", loc)
		}
		if !strings.Contains(loc, url.QueryEscape("https://moc.elcanotek.com/")) {
			t.Errorf("return_to should be the escaped orchestrator URL, got %q", loc)
		}
	})

	t.Run("pubkey unset -> 303 back to dashboard with flag", func(t *testing.T) {
		h := &Handlers{config: Config{ElcanoPubKey: nil, AuthLoginURL: "https://auth.elcanotek.com"}}
		req := httptest.NewRequest(http.MethodGet, "/auth/elcano-login", nil)
		rr := httptest.NewRecorder()
		h.ElcanoLogin(rr, req)

		if rr.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/?e=elcano_unavailable" {
			t.Errorf("Location = %q, want /?e=elcano_unavailable", loc)
		}
	})
}

// TestMocPublicURL covers both branches of the return-URL builder: the
// configured ORCHESTRATOR_URL (production) and the request-host fallback (dev).
func TestMocPublicURL(t *testing.T) {
	t.Run("uses configured OrchestratorURL", func(t *testing.T) {
		h := &Handlers{config: Config{OrchestratorURL: "https://moc.elcanotek.com"}}
		req := httptest.NewRequest(http.MethodGet, "/auth/elcano-login", nil)
		if got := h.mocPublicURL(req); got != "https://moc.elcanotek.com/" {
			t.Errorf("mocPublicURL = %q, want https://moc.elcanotek.com/", got)
		}
	})

	t.Run("trailing slash on OrchestratorURL is normalized", func(t *testing.T) {
		h := &Handlers{config: Config{OrchestratorURL: "https://moc.elcanotek.com/"}}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if got := h.mocPublicURL(req); got != "https://moc.elcanotek.com/" {
			t.Errorf("mocPublicURL = %q, want a single trailing slash", got)
		}
	})

	t.Run("falls back to request host (X-Forwarded-Proto) when unset", func(t *testing.T) {
		h := &Handlers{config: Config{}}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "localhost:8000"
		req.Header.Set("X-Forwarded-Proto", "https")
		if got := h.mocPublicURL(req); got != "https://localhost:8000/" {
			t.Errorf("mocPublicURL = %q, want https://localhost:8000/", got)
		}
	})
}

// TestGetCurrentUser covers the GET /api/me probe the SPA uses to detect a
// session (cookie users have no bearer token in localStorage).
func TestGetCurrentUser(t *testing.T) {
	h := &Handlers{}

	t.Run("user in context -> identity returned", func(t *testing.T) {
		user := &models.User{Username: "alice@elcanotek.com", Role: "client"}
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, user))
		rr := httptest.NewRecorder()
		h.GetCurrentUser(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["authenticated"] != true || body["username"] != "alice@elcanotek.com" || body["role"] != "client" {
			t.Errorf("body = %+v, want authenticated alice/client", body)
		}
	})

	t.Run("no user (API key) -> authenticated without identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		rr := httptest.NewRecorder()
		h.GetCurrentUser(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var body map[string]interface{}
		json.NewDecoder(rr.Body).Decode(&body)
		if body["authenticated"] != true {
			t.Errorf("expected authenticated:true, got %+v", body)
		}
		if _, hasUsername := body["username"]; hasUsername {
			t.Errorf("API-key session should not include a username, got %+v", body)
		}
	})
}
