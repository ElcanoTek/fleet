// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/websession"
)

func withPasswordLoginDisabled() envOpt {
	return func(c *Config, _ *Options) { c.PasswordLoginDisabled = true }
}

// On a Fleet that signs in through central Auth only, the password form is
// refused before anything is checked, and the login page is told to drop it.
func TestPasswordLoginDisabledRefusesTheForm(t *testing.T) {
	idp := &OIDCConfig{Issuer: "https://auth.example.com", ClientID: "c", ClientSecret: "s", ButtonLabel: "Sign in with SSO"}
	e := newTestEnv(t, withOrigin(testOrigin), withOIDC(idp), withPasswordLoginDisabled())
	e.chat.respond = loginBackend(200, true, "epoch-7")

	res := e.do(loginReq("email=bob%40x.com&password=hunter2"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/login?e=password_off" {
		t.Fatalf("login: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if e.chat.count() != 0 {
		t.Fatalf("the password reached chat-server: %d calls", e.chat.count())
	}
	if strings.Contains(setCookies(res), websession.CookieName+"=") {
		t.Fatal("minted a session")
	}
	// CSRF still comes first.
	res = e.do(req("POST", "/api/auth/login", "email=a@x.com&password=p", "Origin", "https://evil.example", "Content-Type", "application/x-www-form-urlencoded"))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("login CSRF: %d", res.StatusCode)
	}

	for _, q := range []string{"", "?manual=1", "?e=password_off"} {
		b := bootstrapOf(t, readBody(t, e.do(req("GET", "/login"+q, ""))))
		if b.Login.PasswordLoginEnabled || !b.Login.OIDCEnabled {
			t.Errorf("/login%s: %+v", q, b.Login)
		}
	}
	on := newTestEnv(t, withOIDC(idp))
	if b := bootstrapOf(t, readBody(t, on.do(req("GET", "/login", "")))); !b.Login.PasswordLoginEnabled {
		t.Errorf("passwords on: %+v", b.Login)
	}
}

// A session a password login minted before the switch stops authenticating
// every surface: the session probe, pages, chat routes and the Operations
// Center's cookie path. Central sessions and the elcano_auth cookie still
// work, and a browser holding a password cookie beside an elcano_auth one is
// signed in by the latter.
func TestPasswordLoginDisabledRefusesPasswordSessions(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin), withPasswordLoginDisabled())
	password := mintHMAC(t, "alice@x.com")
	central, err := websession.NewHMAC(testSecret).MintOIDC("dave@x.com", "ext-epoch", "https://auth.example.com", "sub-1")
	if err != nil {
		t.Fatal(err)
	}

	pw := func(r *http.Request) *http.Request { return withCookie(r, websession.CookieName, password) }
	if res := e.do(pw(req("GET", "/api/session", ""))); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/session: %d", res.StatusCode)
	}
	if res := e.do(pw(req("GET", "/chat", ""))); res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != testOrigin+"/login" {
		t.Errorf("/chat: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := e.do(pw(req("POST", "/api/conversations/c1/rename", `{"title":"t"}`, "Origin", testOrigin))); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("chat route: %d", res.StatusCode)
	}
	if res := e.do(pw(req("GET", "/api/orchestrator/tasks", ""))); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("orchestrator route: %d", res.StatusCode)
	}
	if e.chat.count() != 0 || e.orch.count() != 0 {
		t.Fatalf("a password session reached a backend: chat %d, orchestrator %d", e.chat.count(), e.orch.count())
	}
	// /login treats the browser as signed out, so it can sign in through SSO.
	if res := e.do(pw(req("GET", "/login", ""))); res.StatusCode != http.StatusOK {
		t.Errorf("/login with a refused cookie: %d", res.StatusCode)
	}

	if res := e.do(withCookie(req("GET", "/api/session", ""), websession.CookieName, central)); res.StatusCode != http.StatusOK || !strings.Contains(readBody(t, res), "dave@x.com") {
		t.Errorf("central session: %d", res.StatusCode)
	}
	both := withCookie(pw(req("GET", "/api/session", "")), websession.DefaultElcanoCookieName, e.mintElcano(t, "carol@x.com"))
	if res := e.do(both); res.StatusCode != http.StatusOK || !strings.Contains(readBody(t, res), "carol@x.com") {
		t.Errorf("password + elcano cookies: %d", res.StatusCode)
	}

	// Passwords on: the same cookie is a session.
	on := newTestEnv(t)
	if res := on.do(pw(req("GET", "/api/session", ""))); res.StatusCode != http.StatusOK {
		t.Errorf("passwords on: %d", res.StatusCode)
	}
}

// Where Auth manages accounts the bootstrap carries Auth's console URL, which
// the account pages link to; otherwise it carries nothing.
func TestBootstrapNamesAuthWhenItManagesAccounts(t *testing.T) {
	idp := &OIDCConfig{Issuer: "https://auth.example.com", ClientID: "c", ClientSecret: "s"}
	managed := newTestEnv(t, withOIDC(idp), func(c *Config, _ *Options) { c.AccountsManagedByAuth = true })
	b := bootstrapOf(t, readBody(t, managed.do(signedIn(t, req("GET", "/settings/admin/users", "")))))
	if b.CentralAuth == nil || b.CentralAuth.AdminURL != "https://auth.example.com/admin" {
		t.Fatalf("managed: %+v", b.CentralAuth)
	}
	local := newTestEnv(t, withOIDC(idp))
	if b := bootstrapOf(t, readBody(t, local.do(signedIn(t, req("GET", "/settings/admin/users", ""))))); b.CentralAuth != nil {
		t.Fatalf("not managed: %+v", b.CentralAuth)
	}
	for issuer, want := range map[string]string{
		"https://auth.example.com/":     "https://auth.example.com/admin",
		"https://example.com/auth":      "https://example.com/auth/admin",
		"javascript:alert(1)":           "",
		"not a url with no host at all": "",
	} {
		c := Config{OIDC: &OIDCConfig{Issuer: issuer}}
		if got := c.authAdminURL(); got != want {
			t.Errorf("authAdminURL(%q) = %q, want %q", issuer, got, want)
		}
	}
}
