package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/principal"
)

// The identity a handler acts for is the request principal, and nothing a
// caller writes into a header (#2050, ADR-0108).

// meAs drives GET /me with ctx and the given extra headers, returning the
// status and the email /me reports.
func meAs(ctx context.Context, t *testing.T, h http.Handler, headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/me", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Email
}

// A browser that adds identity headers to a request riding a real session is
// still the session's user: the gateway never forwards them, and the backend
// would not read them if it did.
func TestSpoof_SessionPrincipalOutranksIdentityHeaders(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "alice@x.com", "mallory@x.com")
	setRole(t, s, "mallory@x.com", "admin", "")
	h := s.Routes()

	code, email := meAs(sessionContext("alice@x.com", principal.Session{}), t, h, map[string]string{
		"X-User-Email":          "mallory@x.com",
		"X-User-Session-Epoch":  "anything",
		"X-User-Session-Source": "oidc",
		"X-External-Issuer":     "https://evil",
		"X-External-Subject":    "mallory",
	})
	if code != http.StatusOK || email != "alice@x.com" {
		t.Fatalf("session + spoofed headers: status %d email %q, want 200 alice@x.com", code, email)
	}
}

// Identity headers with no principal and no credential are refused, never
// believed.
func TestSpoof_HeadersAloneAreRefused(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "mallory@x.com")
	h := s.Routes()

	code, _ := meAs(context.Background(), t, h, map[string]string{"X-User-Email": "mallory@x.com"})
	if code != http.StatusUnauthorized {
		t.Fatalf("bare identity header: status %d, want 401", code)
	}
	// The retired service token is no better, whatever its value.
	code, _ = meAs(context.Background(), t, h, map[string]string{"X-User-Email": "mallory@x.com", "X-Chat-Server-Token": "guess"})
	if code != http.StatusUnauthorized {
		t.Fatalf("retired token: status %d, want 401", code)
	}
}

// The role a handler sees is the account row's, whatever the caller's
// principal claimed: membershipMiddleware replaces it with the resolved one.
func TestSpoof_PrincipalRoleIsResolvedFromTheRow(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "viewer@x.com")
	setRole(t, s, "viewer@x.com", "viewer", "")
	h := s.Routes()

	ctx := principal.With(context.Background(), principal.Principal{Email: "viewer@x.com", Role: "admin", AuthMethod: principal.MethodSession})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer claiming admin: status %d, want 403", w.Code)
	}
}

// The sign-in legs act on an email the caller names, so a user principal —
// even a real session — may not call them; only the gateway may.
func TestSpoof_SignInLegsAreGatewayOnly(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "alice@x.com")
	h := s.Routes()

	for _, path := range []string{"/auth/session-epoch", "/auth/verify", "/auth/external-access", "/auth/external-session-epoch", "/auth/external-session-revoke", "/auth/external-team-rename"} {
		method := http.MethodPost
		if path == "/auth/session-epoch" {
			method = http.MethodGet
		}
		req := httptest.NewRequestWithContext(sessionContext("alice@x.com", principal.Session{}), method, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s as a session: status %d, want 403", path, w.Code)
		}
	}
	if w := do(t, h, http.MethodGet, "/auth/session-epoch", nil, "alice@x.com"); w.Code != http.StatusOK {
		t.Fatalf("/auth/session-epoch as the gateway: status %d, want 200", w.Code)
	}
}

// The identity-less brand routes need a principal (the gateway's) but no
// user; nothing else reaches them.
func TestSpoof_BrandRoutesTakeTheGatewayPrincipal(t *testing.T) {
	t.Parallel()
	s := memberFixture(t)
	h := s.Routes()

	req := httptest.NewRequestWithContext(principal.With(context.Background(), principal.Principal{AuthMethod: principal.MethodGateway}), http.MethodGet, "/brand/meta", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/brand/meta as the gateway: status %d, want 200", w.Code)
	}
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/meta", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/brand/meta with nothing: status %d, want 401", w.Code)
	}
}
