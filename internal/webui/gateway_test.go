// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// The request gate, ported case-for-case from web/src/proxy.test.ts (the
// Next middleware it replaced). It must redirect unauthenticated page loads to
// /login, 401 unauthenticated /api calls, let public paths through, bounce
// signed-in users away from /login, gate BOTH views, and accept both cookie
// sessions AND a moc Bearer token.

func TestGateRedirectsUnauthenticatedPagesToLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, p := range []string{"/", "/chat", "/orchestrator", "/settings", "/help"} {
		res := e.do(req("GET", p, ""))
		if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/login" {
			t.Errorf("%s: %d %q", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

func TestGate401sUnauthenticatedAPI(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(req("GET", "/api/conversations", ""))
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", res.StatusCode)
	}
	if body := readBody(t, res); !strings.Contains(body, `"error":"Unauthorized"`) {
		t.Fatalf("body %q", body)
	}
	if e.chat.count() != 0 {
		t.Fatal("an unauthenticated request reached the backend")
	}
}

func TestGateAdmitsEitherCookie(t *testing.T) {
	e := newTestEnv(t)
	for name, r := range map[string]*http.Request{
		"hmac":   signedIn(t, req("GET", "/orchestrator", "")),
		"elcano": withCookie(req("GET", "/orchestrator", ""), "elcano_auth", e.mintElcano(t, "carol@example.com")),
	} {
		res := e.do(r)
		if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" {
			t.Errorf("%s: %d %q", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

func TestGateBouncesSignedInUserAwayFromLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	res := e.do(signedIn(t, req("GET", "/login", "")))
	if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/chat" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestGateServesLoginAndPublicRoutesWithoutSession(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/login", "/api/auth/elcano-login", "/api/orchestrator/auth/elcano-login", "/manifest.webmanifest", "/api/theme", "/api/brand/logo", "/api/brand/share-image"} {
		res := e.do(req("GET", p, ""))
		if res.StatusCode == http.StatusUnauthorized || strings.HasSuffix(res.Header.Get("Location"), "/login") {
			t.Errorf("%s was gated: %d %q", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
	// The moc orchestrator login must be reachable to obtain a bearer at all.
	res := e.do(req("POST", "/api/orchestrator/auth/login", `{"username":"u"}`, "Origin", "http://fleet.example.com"))
	if res.StatusCode == http.StatusUnauthorized {
		t.Fatal("orchestrator login gated")
	}
}

func TestGateAdmitsBearerOnly(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(req("GET", "/orchestrator", "", "Authorization", "Bearer moc-token-123"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page: %d", res.StatusCode)
	}
	res = e.do(req("GET", "/api/orchestrator/stats", "", "Authorization", "Bearer moc-token-123"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("api: %d", res.StatusCode)
	}
	got := e.orch.last(t)
	if got.Header.Get("Authorization") != "Bearer moc-token-123" || got.Header.Get("X-User-Email") != "" || got.Header.Get("X-Orchestrator-Server-Token") != "" {
		t.Fatalf("bearer not forwarded verbatim/alone: %v", got.Header)
	}
	// "Bearer" with nothing after it is not a credential.
	if res := e.do(req("GET", "/api/orchestrator/stats", "", "Authorization", "Bearer ")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty bearer admitted: %d", res.StatusCode)
	}
	// A bearer does not stand in for a session on a chat route.
	if res := e.do(req("GET", "/api/conversations", "", "Authorization", "Bearer x")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer admitted to chat: %d", res.StatusCode)
	}
}

func cspOf(res *http.Response) string { return res.Header.Get("Content-Security-Policy") }

func TestSharedViewIsPublicWithRestrictiveCSP(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(req("GET", "/shared/some-token", ""))
	if res.StatusCode == http.StatusTemporaryRedirect {
		t.Fatal("shared view gated")
	}
	csp := cspOf(res)
	for _, want := range []string{"default-src 'self'", "img-src 'self' data: blob:", "connect-src 'self'", "style-src 'self' 'unsafe-inline'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("missing %q in %q", want, csp)
		}
	}
	if strings.Contains(csp, "https:") {
		t.Errorf("shared CSP allows external hosts: %q", csp)
	}
	// Opening a share link while signed in is legitimate: not bounced.
	res = e.do(signedIn(t, req("GET", "/shared/some-token", "")))
	if res.StatusCode == http.StatusTemporaryRedirect {
		t.Fatal("signed-in viewer bounced from /shared")
	}
}

func TestBaselineCSPIsStrict(t *testing.T) {
	e := newTestEnv(t)
	csp := cspOf(e.do(signedIn(t, req("GET", "/chat", ""))))
	if !strings.Contains(csp, "img-src 'self' data: blob: https:") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("csp %q", csp)
	}
	// The SPA ships no inline script: unlike the Next tier, no 'unsafe-inline'
	// or 'unsafe-eval' for scripts.
	if m := regexp.MustCompile(`script-src [^;]*`).FindString(csp); m != "script-src 'self'" {
		t.Fatalf("script-src = %q", m)
	}
}

func TestFormActionAdmitsOnlyTheIssuerOrigin(t *testing.T) {
	cases := []struct{ issuer, want string }{
		{"https://auth.example.com/", "form-action 'self' https://auth.example.com"},
		{"https://auth.example.com/tenant/x", "form-action 'self' https://auth.example.com"},
		{"https://auth.example.com:8443/", "form-action 'self' https://auth.example.com:8443"},
		{"http://localhost:9000", "form-action 'self' http://localhost:9000"},
		{"not a url", "form-action 'self'"},
		{"javascript:alert(1)", "form-action 'self'"},
	}
	for _, c := range cases {
		e := newTestEnv(t, withOIDC(&OIDCConfig{Issuer: c.issuer, ClientID: "fleet", ClientSecret: "s"}))
		csp := cspOf(e.do(signedIn(t, req("GET", "/chat", ""))))
		got := regexp.MustCompile(`form-action [^;]*`).FindString(csp)
		if got != c.want {
			t.Errorf("issuer %q: %q, want %q", c.issuer, got, c.want)
		}
	}
	// Unset (or a partial config, which ConfigFromEnv turns into nil) stays 'self'.
	e := newTestEnv(t)
	if got := regexp.MustCompile(`form-action [^;]*`).FindString(cspOf(e.do(signedIn(t, req("GET", "/chat", ""))))); got != "form-action 'self'" {
		t.Fatalf("%q", got)
	}
}

func TestPartialOIDCEnvIsDisabled(t *testing.T) {
	t.Setenv("FLEET_OIDC_ISSUER", "https://auth.example.com/")
	t.Setenv("FLEET_OIDC_CLIENT_ID", "")
	t.Setenv("FLEET_OIDC_CLIENT_SECRET", "s")
	if ConfigFromEnv().OIDC != nil {
		t.Fatal("issuer without client id enabled OIDC")
	}
}

func TestSecurityHeadersOnEveryGateResponse(t *testing.T) {
	e := newTestEnv(t)
	for name, r := range map[string]*http.Request{
		"redirect": req("GET", "/chat", ""),
		"401":      req("GET", "/api/conversations", ""),
		"page":     signedIn(t, req("GET", "/chat", "")),
		"api":      signedIn(t, req("GET", "/api/conversations", "")),
	} {
		res := e.do(r)
		h := res.Header
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || h.Get("X-Frame-Options") != "DENY" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" ||
			h.Get("Strict-Transport-Security") == "" || h.Get(BuildIDHeader) != testBuildID {
			t.Errorf("%s: headers %v", name, h)
		}
	}
}

func TestSessionRefreshOnlyOnAuthenticatedPassThrough(t *testing.T) {
	e := newTestEnv(t)
	// A cookie minted over a minute ago is re-minted on the next authenticated
	// request (ADR-0064's idle limit).
	h := websession.NewHMAC(testSecret).WithClock(func() time.Time { return time.Now().Add(-5 * time.Minute) })
	tok, err := h.Mint("alice@example.com", "epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	res := e.do(withCookie(req("GET", "/api/conversations", ""), websession.CookieName, tok))
	if !strings.Contains(setCookies(res), websession.CookieName+"=") {
		t.Fatalf("no refresh on an authenticated API call: %q", setCookies(res))
	}
	// Redirects, 401s, public routes and bearer-only requests never touch it.
	for _, r := range []*http.Request{
		req("GET", "/chat", ""),
		req("GET", "/api/conversations", ""),
		req("GET", "/login", ""),
		req("GET", "/api/orchestrator/tasks", "", "Authorization", "Bearer moc"),
		withCookie(req("GET", "/login", ""), websession.CookieName, tok),
		withCookie(req("GET", "/shared/abc", ""), websession.CookieName, tok),
	} {
		if c := setCookies(e.do(r)); strings.Contains(c, websession.CookieName+"=") {
			t.Errorf("%s %s refreshed the cookie: %q", r.Method, r.URL.Path, c)
		}
	}
	// A fresh mint (under a minute) is not re-minted either.
	if c := setCookies(e.do(signedIn(t, req("GET", "/api/conversations", "")))); strings.Contains(c, websession.CookieName+"=") {
		t.Fatalf("re-minted a fresh cookie: %q", c)
	}
}

// ── team links (B24) ────────────────────────────────────────────────────────

func TestTeamLinkParkedOnTheWayToLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	res := e.do(req("GET", "/chat?team="+testTeamLinkID, ""))
	if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/login?team_link=1" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	c := setCookies(res)
	for _, want := range []string{"fleet_team_link=" + testTeamLinkID, "HttpOnly", "SameSite=Lax", "Path=/", "Max-Age=1800", "Secure"} {
		if !strings.Contains(c, want) {
			t.Errorf("cookie %q lacks %q", c, want)
		}
	}
	if !strings.Contains(cspOf(res), "default-src 'self'") || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing on the park redirect")
	}
}

func TestTeamLinkNotParkedForInvalidIDsOrOtherPages(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, target := range []string{"/chat?team=not-a-uuid", "/chat?team=" + testTeamLinkID + "/../x", "/chat?team=https%3A%2F%2Fevil.example%2F" + testTeamLinkID, "/chat?team=" + testTeamLinkID + "%3Bx%3D1", "/orchestrator?team=" + testTeamLinkID} {
		res := e.do(req("GET", target, ""))
		if res.Header.Get("Location") != testOrigin+"/login" || strings.Contains(setCookies(res), "fleet_team_link") {
			t.Errorf("%s: %q %q", target, res.Header.Get("Location"), setCookies(res))
		}
	}
}

func TestTeamLinkRedeemedOnSignedInLanding(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	for _, p := range []string{"/", "/chat", "/login"} {
		res := e.do(withCookie(signedIn(t, req("GET", p, "")), "fleet_team_link", testTeamLinkID))
		if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/chat?team="+testTeamLinkID {
			t.Errorf("%s: %d %q", p, res.StatusCode, res.Header.Get("Location"))
		}
		if !regexp.MustCompile(`fleet_team_link=;.*Max-Age=0`).MatchString(setCookies(res)) {
			t.Errorf("%s: parked cookie not cleared: %q", p, setCookies(res))
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers missing", p)
		}
	}
}

func TestTeamLinkOwnLinkWinsAndInvalidParkedValueIsDropped(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(withCookie(signedIn(t, req("GET", "/chat?team="+testTeamLinkID, "")), "fleet_team_link", "0f8fad5b-d9cb-469f-a165-000000000000"))
	if res.StatusCode != http.StatusOK || !regexp.MustCompile(`fleet_team_link=;.*Max-Age=0`).MatchString(setCookies(res)) {
		t.Fatalf("own link: %d %q", res.StatusCode, setCookies(res))
	}
	res = e.do(withCookie(signedIn(t, req("GET", "/chat", "")), "fleet_team_link", "../../evil"))
	if res.StatusCode != http.StatusOK || !strings.Contains(setCookies(res), "Max-Age=0") {
		t.Fatalf("invalid parked: %d %q", res.StatusCode, setCookies(res))
	}
	res = e.do(withCookie(signedIn(t, req("GET", "/orchestrator", "")), "fleet_team_link", testTeamLinkID))
	if res.StatusCode != http.StatusOK || strings.Contains(setCookies(res), "fleet_team_link") {
		t.Fatalf("other page touched the parked link: %q", setCookies(res))
	}
	if c := setCookies(e.do(signedIn(t, req("GET", "/chat", "")))); strings.Contains(c, "fleet_team_link") {
		t.Fatalf("no parked link, yet %q", c)
	}
}
