// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// Ported from web/src/app/api/auth/{login,logout,elcano-login}/route.test.ts.

func loginBackend(verifyStatus int, verifyOK bool, epoch string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/verify":
			if verifyStatus != http.StatusOK {
				w.WriteHeader(verifyStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": verifyOK})
		case "/auth/session-epoch":
			if epoch == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"session_epoch": epoch})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func loginReq(form string) *http.Request {
	return req("POST", "/api/auth/login", form, "Origin", "https://fleet.example.com", "Content-Type", "application/x-www-form-urlencoded")
}

func TestLoginMintsAnEpochBoundSession(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	e.chat.respond = loginBackend(200, true, "epoch-7")
	res := e.do(loginReq("email=+Bob%40X.com+&password=hunter2"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var verify recordedCall
	for _, c := range e.chat.calls {
		if c.Path == "/auth/verify" {
			verify = c
		}
	}
	if verify.Body != `{"email":"bob@x.com","password":"hunter2"}` || verify.Header.Get("X-User-Email") != "bob@x.com" || verify.Header.Get("X-Chat-Server-Token") != testChatToken {
		t.Fatalf("verify call %q %v", verify.Body, verify.Header)
	}
	c := setCookies(res)
	for _, want := range []string{"elcano_session=", "HttpOnly", "SameSite=Lax", "Secure", "Path=/", "Max-Age=86400"} {
		if !strings.Contains(c, want) {
			t.Errorf("cookie %q lacks %q", c, want)
		}
	}
	tok := strings.TrimPrefix(strings.SplitN(c, ";", 2)[0], "elcano_session=")
	s := websession.NewHMAC(testSecret).Verify(tok)
	if s == nil || s.Email != "bob@x.com" || s.Epoch != "epoch-7" {
		t.Fatalf("minted %+v", s)
	}
}

func TestLoginErrorCodes(t *testing.T) {
	cases := []struct {
		name     string
		form     string
		backend  func(http.ResponseWriter, *http.Request)
		wantCode string
	}{
		{"missing fields", "email=&password=", loginBackend(200, true, "e"), "missing"},
		{"throttled", "email=a@x.com&password=p", loginBackend(http.StatusTooManyRequests, false, "e"), "throttled"},
		{"server error", "email=a@x.com&password=p", loginBackend(http.StatusInternalServerError, false, "e"), "server"},
		{"invalid", "email=a@x.com&password=p", loginBackend(200, false, "e"), "invalid"},
		{"no epoch", "email=a@x.com&password=p", loginBackend(200, true, ""), "server"},
	}
	for _, c := range cases {
		e := newTestEnv(t, withOrigin(testOrigin))
		e.chat.respond = c.backend
		res := e.do(loginReq(c.form))
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/login?e="+c.wantCode {
			t.Errorf("%s: %d %q", c.name, res.StatusCode, res.Header.Get("Location"))
		}
		if strings.Contains(setCookies(res), "elcano_session=") {
			t.Errorf("%s: minted a cookie", c.name)
		}
	}
	// CSRF first: a cross-site login is a 403 and never reaches verify.
	e := newTestEnv(t)
	res := e.do(req("POST", "/api/auth/login", "email=a@x.com&password=p", "Origin", "https://evil.example", "Content-Type", "application/x-www-form-urlencoded"))
	if res.StatusCode != 403 || e.chat.count() != 0 {
		t.Fatalf("login CSRF: %d", res.StatusCode)
	}
	// No secret configured: fail closed, no cookie.
	g, _ := New(Config{ChatToken: testChatToken}, Options{Chat: http.HandlerFunc(loginBackend(200, true, "e")), Bundle: testBundle()})
	rec := newRecorder()
	g.ServeHTTP(rec, loginReq("email=a@x.com&password=p"))
	if !strings.HasSuffix(rec.Header().Get("Location"), "/login?e=server") || strings.Contains(strings.Join(rec.Header().Values("Set-Cookie"), ""), "elcano_session=e") {
		t.Fatalf("no secret: %q", rec.Header().Get("Location"))
	}
}

func TestLogoutClearsEverythingAndLandsOnManualLogin(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	e.g.cfg.ElcanoCookieDomain = "example.com"
	res := e.do(req("POST", "/api/auth/logout", "", "Origin", testOrigin))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/login?manual=1" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %q %q", res.StatusCode, res.Header.Get("Location"), res.Header.Get("Cache-Control"))
	}
	cookies := res.Header.Values("Set-Cookie")
	want := []string{
		"elcano_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure",
		"elcano_auth=; Domain=example.com; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure",
		"elcano_auth=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure",
		"fleet_oidc_state=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure",
		"fleet_oidc_nonce=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure",
		"fleet_oidc_verifier=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax; Secure",
	}
	if strings.Join(cookies, "\n") != strings.Join(want, "\n") {
		t.Fatalf("cookies:\n%s", strings.Join(cookies, "\n"))
	}
}

func TestLogoutWithCentralAuthGoesToRPInitiatedLogout(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin), withOIDC(&OIDCConfig{Issuer: "https://auth.example.com/tenant", ClientID: "fleet app", ClientSecret: "s"}))
	res := e.do(req("POST", "/api/auth/logout", "", "Origin", testOrigin))
	if res.Header.Get("Location") != "https://auth.example.com/logout?client_id=fleet+app" {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
	// A malformed issuer must not turn logout into a 500 that keeps the cookies.
	e = newTestEnv(t, withOrigin(testOrigin), withOIDC(&OIDCConfig{Issuer: "::not a url", ClientID: "c", ClientSecret: "s"}))
	res = e.do(req("POST", "/api/auth/logout", "", "Origin", testOrigin))
	if res.Header.Get("Location") != testOrigin+"/login?manual=1" || !strings.Contains(setCookies(res), "elcano_session=;") {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
	// CSRF applies.
	if res := e.do(req("POST", "/api/auth/logout", "", "Origin", "https://evil.example")); res.StatusCode != 403 {
		t.Fatalf("logout CSRF: %d", res.StatusCode)
	}
}

func TestElcanoLoginHandoff(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	res := e.do(req("GET", "/api/auth/elcano-login", ""))
	want := "https://auth.elcanotek.com/?return_to=" + url.QueryEscape(testOrigin+"/")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != strings.ReplaceAll(want, "+", "%20") {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	res = e.do(req("GET", "/api/orchestrator/auth/elcano-login", ""))
	if !strings.HasSuffix(res.Header.Get("Location"), url.QueryEscape(testOrigin+"/orchestrator")) {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
	// AUTH_LOGIN_URL overrides, trailing slashes stripped.
	e.g.cfg.AuthLoginURL = "https://login.example.com//"
	res = e.do(req("GET", "/api/auth/elcano-login", ""))
	if !strings.HasPrefix(res.Header.Get("Location"), "https://login.example.com/?return_to=") {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
	// Without a public key the button must not loop through auth.
	e.g.cfg.ElcanoPubKeyB64 = ""
	res = e.do(req("GET", "/api/auth/elcano-login", ""))
	if res.Header.Get("Location") != testOrigin+"/login?e=elcano_unavailable" {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
}

func TestRedirectURLPrefersCanonicalOriginOverForwardedHeaders(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	r := req("GET", "/chat", "", "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "http")
	if got := e.g.redirectURL(r, "/login"); got != testOrigin+"/login" {
		t.Fatalf("%q", got)
	}
	if !e.g.secureRequest(r) {
		t.Fatal("canonical https origin not secure")
	}
	// A configured plain-http origin (loopback deploys) is insecure.
	e = newTestEnv(t, withOrigin("http://127.0.0.1:3000"))
	if e.g.secureRequest(req("GET", "/", "", "X-Forwarded-Proto", "https")) {
		t.Fatal("plain-http origin reported secure")
	}
	// No origin: forwarded headers, then the connection.
	e = newTestEnv(t)
	r = req("GET", "/chat", "", "X-Forwarded-Host", "proxy.example", "X-Forwarded-Proto", "https")
	if got := e.g.redirectURL(r, "/login"); got != "https://proxy.example/login" || !e.g.secureRequest(r) {
		t.Fatalf("%q", got)
	}
	if got := e.g.redirectURL(req("GET", "/chat", ""), "/login"); got != "http://fleet.example.com/login" {
		t.Fatalf("%q", got)
	}
	// An unparseable configured origin is treated as unset.
	if parseOrigin("not a url") != nil || parseOrigin("") != nil {
		t.Fatal("bad origin parsed")
	}
}

func TestSessionAndVersionEndpoints(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(signedIn(t, req("GET", "/api/session", "")))
	if b := readBody(t, res); !strings.Contains(b, `"email":"alice@example.com"`) {
		t.Fatalf("%q", b)
	}
	res = e.do(signedIn(t, req("GET", "/api/version", "")))
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"buildId":"`+testBuildID+`"`) || res.Header.Get(BuildIDHeader) != testBuildID || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%q %v", b, res.Header)
	}
}

func TestMCPOAuthCallback(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"name":"Linear"}`) }
	res := e.do(signedIn(t, req("GET", "/api/oauth/mcp/callback?code=c1&state=s1", "")))
	if res.Header.Get("Location") != testOrigin+"/settings/connections?connected=Linear" {
		t.Fatalf("%q", res.Header.Get("Location"))
	}
	if got := e.chat.last(t); got.Method != "POST" || got.Body != `{"code":"c1","state":"s1"}` {
		t.Fatalf("%s %q", got.Method, got.Body)
	}
	for target, want := range map[string]string{
		"/api/oauth/mcp/callback?error=access_denied": "?error=access_denied",
		"/api/oauth/mcp/callback?code=c1":             "?error=missing_code_or_state",
	} {
		if got := e.do(signedIn(t, req("GET", target, ""))).Header.Get("Location"); got != testOrigin+"/settings/connections"+want {
			t.Errorf("%s: %q", target, got)
		}
	}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"detail":"state expired"}`)
	}
	if got := e.do(signedIn(t, req("GET", "/api/oauth/mcp/callback?code=c&state=s", ""))).Header.Get("Location"); got != testOrigin+"/settings/connections?error=state%20expired" {
		t.Fatalf("%q", got)
	}
}

// The password form's body is capped (64 KiB) before it is parsed, in either
// encoding, so an oversized login — multipart especially, which would
// otherwise spill to temp files — is refused without reaching the backend.
func TestLoginBodyIsCapped(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	e.chat.respond = loginBackend(200, true, "epoch-1")
	pad := strings.Repeat("x", 200<<10)

	res := e.do(loginReq("email=bob%40x.com&password=hunter2&pad=" + pad))
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "/login?e=") {
		t.Fatalf("oversized urlencoded login: %d %q", res.StatusCode, loc)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("email", "bob@x.com")
	_ = mw.WriteField("password", "hunter2")
	fw, _ := mw.CreateFormFile("blob", "big.bin")
	_, _ = fw.Write([]byte(pad))
	_ = mw.Close()
	res = e.do(req("POST", "/api/auth/login", body.String(), "Origin", "https://fleet.example.com", "Content-Type", mw.FormDataContentType()))
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "/login?e=") {
		t.Fatalf("oversized multipart login: %d %q", res.StatusCode, loc)
	}
	for _, c := range e.chat.calls {
		if c.Path == "/auth/verify" {
			t.Fatalf("an oversized login reached /auth/verify")
		}
	}

	// A normal multipart login still works.
	body.Reset()
	mw = multipart.NewWriter(&body)
	_ = mw.WriteField("email", "bob@x.com")
	_ = mw.WriteField("password", "hunter2")
	_ = mw.Close()
	res = e.do(req("POST", "/api/auth/login", body.String(), "Origin", "https://fleet.example.com", "Content-Type", mw.FormDataContentType()))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/" {
		t.Fatalf("multipart login: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}
