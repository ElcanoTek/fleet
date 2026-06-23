// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// mintElcano signs a token the way the auth service does:
// base64url(payloadJSON) + "." + base64url(ed25519 sig over the body string).
func mintElcano(t *testing.T, priv ed25519.PrivateKey, claims map[string]interface{}) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestParseElcanoPubKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if ParseElcanoPubKey(base64.StdEncoding.EncodeToString(pub)) == nil {
		t.Error("valid 32-byte key should parse")
	}
	if ParseElcanoPubKey("") != nil {
		t.Error("empty should be nil (disabled)")
	}
	if ParseElcanoPubKey("not valid base64!!") != nil {
		t.Error("malformed base64 should be nil")
	}
	if ParseElcanoPubKey(base64.StdEncoding.EncodeToString([]byte("too-short"))) != nil {
		t.Error("wrong-size key should be nil")
	}
}

func TestVerifyElcanoToken(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	future := time.Now().Add(time.Hour).Unix()

	t.Run("valid token, email lowercased", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "Alice@Elcanotek.com", "tenant": "elcanotek.com", "exp": future})
		s := verifyElcanoToken(pub, tok)
		if s == nil || s.Email != "alice@elcanotek.com" || s.Tenant != "elcanotek.com" {
			t.Fatalf("got %+v", s)
		}
	})

	t.Run("foreign key rejected", func(t *testing.T) {
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		tok := mintElcano(t, priv, map[string]interface{}{"email": "a@b.com", "exp": future})
		if verifyElcanoToken(otherPub, tok) != nil {
			t.Error("a token signed by a different key must be rejected")
		}
	})

	t.Run("tampered body rejected", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "a@b.com", "exp": future})
		b := []byte(tok)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		if verifyElcanoToken(pub, string(b)) != nil {
			t.Error("tampered body must be rejected")
		}
	})

	t.Run("expired rejected", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "a@b.com", "exp": time.Now().Add(-time.Minute).Unix()})
		if verifyElcanoToken(pub, tok) != nil {
			t.Error("expired token must be rejected")
		}
	})

	t.Run("empty email rejected", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "", "exp": future})
		if verifyElcanoToken(pub, tok) != nil {
			t.Error("empty email must be rejected")
		}
	})

	t.Run("malformed rejected", func(t *testing.T) {
		for _, bad := range []string{"", "nodot", ".", "a.", ".b", "a.b"} {
			if verifyElcanoToken(pub, bad) != nil {
				t.Errorf("malformed token %q must be rejected", bad)
			}
		}
	})

	t.Run("wrong-size key fails closed (no panic)", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "a@b.com", "exp": future})
		if verifyElcanoToken(ed25519.PublicKey("short"), tok) != nil {
			t.Error("wrong-size key must be rejected")
		}
	})
}

func TestElcanoLogout(t *testing.T) {
	h := &Handlers{config: Config{ElcanoCookieName: "elcano_auth", ElcanoCookieDomain: "elcanotek.com", AuthLoginURL: "https://auth.example"}}
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

// newCookieAuthHandler builds a Handlers wired for the cookie path with an
// injected membership lookup (no database).
func newCookieAuthHandler(t *testing.T) (*Handlers, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	member := &models.User{ID: uuid.New(), Username: "alice@elcanotek.com", Role: "client"}
	h := &Handlers{
		config: Config{
			AdminAPIKey:      "admin-secret",
			ElcanoPubKey:     pub,
			ElcanoCookieName: "elcano_auth",
			AuthLoginURL:     "https://auth.example",
		},
		memberLookup: func(_ context.Context, email string) (*models.User, error) {
			if email == "alice@elcanotek.com" {
				return member, nil
			}
			return nil, sql.ErrNoRows
		},
	}
	return h, priv
}

func TestAdminOrUserAuthMiddleware_Cookie(t *testing.T) {
	h, priv := newCookieAuthHandler(t)
	future := time.Now().Add(time.Hour).Unix()

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := "(none)"
		if u := GetUserFromContext(r.Context()); u != nil {
			email = u.Username
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(email))
	})
	handler := h.AdminOrUserAuthMiddleware(final)

	do := func(h http.Handler, setup func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
		if setup != nil {
			setup(req)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	withCookie := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "elcano_auth", Value: tok}) }
	}

	t.Run("known member -> 200 with identity", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "alice@elcanotek.com", "exp": future})
		rr := do(handler, withCookie(tok))
		if rr.Code != http.StatusOK || rr.Body.String() != "alice@elcanotek.com" {
			t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
		}
	})

	t.Run("valid cookie, unknown email -> 403 not_a_member", func(t *testing.T) {
		tok := mintElcano(t, priv, map[string]interface{}{"email": "stranger@elcanotek.com", "exp": future})
		rr := do(handler, withCookie(tok))
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "not_a_member") {
			t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
		}
	})

	t.Run("forged-key cookie -> 401", func(t *testing.T) {
		_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
		tok := mintElcano(t, otherPriv, map[string]interface{}{"email": "alice@elcanotek.com", "exp": future})
		rr := do(handler, withCookie(tok))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("forged cookie should be 401, got %d", rr.Code)
		}
	})

	t.Run("no credentials -> 401", func(t *testing.T) {
		if rr := do(handler, nil); rr.Code != http.StatusUnauthorized {
			t.Fatalf("got %d", rr.Code)
		}
	})

	t.Run("admin API key still works (automation unaffected)", func(t *testing.T) {
		rr := do(handler, func(r *http.Request) { r.Header.Set("X-API-Key", "admin-secret") })
		if rr.Code != http.StatusOK {
			t.Fatalf("admin key got %d", rr.Code)
		}
	})

	t.Run("cookie path disabled when pubkey nil", func(t *testing.T) {
		h2 := *h
		h2.config.ElcanoPubKey = nil
		handler2 := h2.AdminOrUserAuthMiddleware(final)
		tok := mintElcano(t, priv, map[string]interface{}{"email": "alice@elcanotek.com", "exp": future})
		if rr := do(handler2, withCookie(tok)); rr.Code != http.StatusUnauthorized {
			t.Fatalf("disabled cookie path should 401, got %d", rr.Code)
		}
	})
}
