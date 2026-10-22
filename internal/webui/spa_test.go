// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// The SPA serving contract (ADR-0091): hashed assets immutable, other public
// files revalidated, index.html never cached and rendered per request with
// the page's metadata and bootstrap; client routes fall back to index.html;
// an unknown route is the SPA's not-found page with a 404 status; the
// server-side work the Next server components did happens before the page.

func bootstrapOf(t *testing.T, body string) Bootstrap {
	t.Helper()
	m := regexp.MustCompile(`<script type="application/json" id="fleet-bootstrap">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no bootstrap in %q", body)
	}
	var b Bootstrap
	if err := json.Unmarshal([]byte(m[1]), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStaticAssetCaching(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(req("GET", "/assets/index-abc123.js", ""))
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || readBody(t, res) != testAssetJS {
		t.Fatalf("%d %v", res.StatusCode, res.Header)
	}
	if !strings.Contains(res.Header.Get("Content-Type"), "javascript") || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%v", res.Header)
	}
	// Non-hashed public files revalidate by ETag (a deploy must be picked up),
	// and need no session.
	res = e.do(req("GET", "/sw.js", ""))
	etag := res.Header.Get("ETag")
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-cache" || etag == "" {
		t.Fatalf("sw.js: %d %v", res.StatusCode, res.Header)
	}
	if res := e.do(req("GET", "/sw.js", "", "If-None-Match", etag)); res.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation: %d", res.StatusCode)
	}
	// A missing hashed asset is a plain 404, never index.html (a stale tab
	// would try to run it as a module).
	res = e.do(req("GET", "/assets/gone-123.js", ""))
	if body := readBody(t, res); res.StatusCode != 404 || strings.Contains(body, "<html") {
		t.Fatalf("missing asset: %d %q", res.StatusCode, body)
	}
	// index.html and build-id.txt are not static files.
	for _, p := range []string{"/index.html", "/build-id.txt"} {
		if res := e.do(req("GET", p, "")); res.StatusCode == 200 && res.Header.Get("Cache-Control") == "no-cache" {
			t.Errorf("%s served as a static file", p)
		}
	}
	// Mutating a static path is refused.
	if res := e.do(req("POST", "/sw.js", "")); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST static: %d", res.StatusCode)
	}
}

func TestIndexRenderedPerRequest(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	e.chat.respond = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/brand/meta" {
			if r.Header.Get("X-Chat-Server-Token") != testChatToken || r.Header.Get("X-User-Email") != "" {
				t.Errorf("/brand/meta must be token-only: %v", r.Header)
			}
			_, _ = io.WriteString(w, `{"app_name":"Reklaim","login_title":"Reklaim it.","share_title":"","share_description":"<desc>","background_dark":"#0A0908","share_image_url":"https://evil.example/x.png"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}
	res := e.do(signedIn(t, req("GET", "/settings/admin", "")))
	body := readBody(t, res)
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store, must-revalidate" || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("%d %v", res.StatusCode, res.Header)
	}
	for _, want := range []string{
		"<title>Reklaim</title>",
		`<meta name="description" content="&lt;desc&gt;">`,
		`<meta property="og:title" content="Reklaim">`,
		`<meta property="og:image" content="` + testOrigin + `/share.png">`,
		`<meta property="og:image:width" content="1280">`,
		`<meta name="theme-color" media="(prefers-color-scheme: dark)" content="#0A0908">`,
		`<meta name="theme-color" media="(prefers-color-scheme: light)" content="#f4f6fb">`,
		`<meta name="robots" content="noindex, nofollow">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "evil.example") {
		t.Error("an absolute share_image_url reached the page")
	}
	b := bootstrapOf(t, body)
	if b.BuildID != testBuildID || b.Branding.AppName != "Reklaim" || b.Branding.LoginTitle != "Reklaim it." || b.Session == nil || b.Session.Email != "alice@example.com" {
		t.Fatalf("%+v", b)
	}
	// The bootstrap is HTML-safe JSON: a branding value cannot close the script.
	e.chat.respond = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"app_name":"</script><script>alert(1)</script>"}`)
	}
	e.g.branding = newBrandingCache(e.g)
	body = readBody(t, e.do(signedIn(t, req("GET", "/chat", ""))))
	if strings.Contains(body, "</script><script>alert(1)") {
		t.Fatal("bootstrap/title not escaped")
	}
}

func TestClientRoutesAndNotFound(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/chat", "/orchestrator", "/settings", "/settings/connections", "/settings/admin/users", "/help", "/help/chat", "/no-access"} {
		res := e.do(signedIn(t, req("GET", p, "")))
		body := readBody(t, res)
		if res.StatusCode != 200 || !strings.Contains(body, `id="fleet-bootstrap"`) || bootstrapOf(t, body).NotFound {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
	for _, p := range []string{"/nope", "/help/not-a-guide", "/settings/nope"} {
		res := e.do(signedIn(t, req("GET", p, "")))
		if res.StatusCode != 404 || !bootstrapOf(t, readBody(t, res)).NotFound {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
	// Help pages carry their own titles.
	body := readBody(t, e.do(signedIn(t, req("GET", "/help/operations-center", ""))))
	if !strings.Contains(body, "<title>Operations Center guide — Fleet</title>") {
		t.Fatal("guide title")
	}
	if !strings.Contains(readBody(t, e.do(signedIn(t, req("GET", "/help", "")))), "<title>Guides — Fleet</title>") {
		t.Fatal("guides title")
	}
}

func TestServerSideRedirects(t *testing.T) {
	e := newTestEnv(t, withOrigin(testOrigin))
	cases := []struct {
		path   string
		status int
		loc    string
	}{
		{"/", 307, "/chat"},
		{"/admin", 308, "/settings/admin"},
		{"/orchestrator/tasks/abc%20def", 307, "/orchestrator?task=abc%20def"},
		{"/orchestrator/tasks/99999999-9999-9999-9999-999999999999", 307, "/orchestrator?task=99999999-9999-9999-9999-999999999999"},
	}
	for _, c := range cases {
		res := e.do(signedIn(t, req("GET", c.path, "")))
		if res.StatusCode != c.status || res.Header.Get("Location") != testOrigin+c.loc {
			t.Errorf("%s: %d %q", c.path, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

// Ported from web/src/app/chat/page.test.tsx: the membership entry gate.
func TestChatMembershipGate(t *testing.T) {
	membership := func(status int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/membership" {
				w.WriteHeader(status)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		}
	}
	oidcTok, _ := websession.NewHMAC(testSecret).MintOIDC("x@omc.com", "e", "https://auth", "s")
	for _, source := range []string{"oidc", "elcano"} {
		e := newTestEnv(t, withOrigin(testOrigin))
		mk := func() *http.Request {
			if source == "oidc" {
				return withCookie(req("GET", "/chat", ""), websession.CookieName, oidcTok)
			}
			return withCookie(req("GET", "/chat", ""), "elcano_auth", e.mintElcano(t, "x@omc.com"))
		}
		e.chat.respond = membership(403)
		res := e.do(mk())
		if res.StatusCode != 307 || res.Header.Get("Location") != testOrigin+"/no-access" {
			t.Errorf("%s non-member: %d %q", source, res.StatusCode, res.Header.Get("Location"))
		}
		var probe recordedCall
		for _, c := range e.chat.calls {
			if c.Path == "/auth/membership" {
				probe = c
			}
		}
		if probe.Header.Get("X-User-Email") != "x@omc.com" {
			t.Errorf("%s: membership probe %v", source, probe.Header)
		}
		e.chat.respond = membership(200)
		if res := e.do(mk()); res.StatusCode != 200 {
			t.Errorf("%s member: %d", source, res.StatusCode)
		}
		// A failing backend must not trap the user on no-access.
		e.chat.respond = membership(500)
		if res := e.do(mk()); res.StatusCode != 200 {
			t.Errorf("%s backend error: %d", source, res.StatusCode)
		}
	}
	// Password sessions are members by construction: no probe.
	e := newTestEnv(t)
	e.do(signedIn(t, req("GET", "/chat", "")))
	for _, c := range e.chat.calls {
		if c.Path == "/auth/membership" {
			t.Fatal("password session probed membership")
		}
	}
}

// Ported from the server half of web/src/app/login/login-card.test.tsx.
func TestLoginPageServerWiring(t *testing.T) {
	idp := &OIDCConfig{Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s", ButtonLabel: "Sign in with Okta", AutoStart: true}
	e := newTestEnv(t, withOrigin(testOrigin), withOIDC(idp))
	res := e.do(req("GET", "/login", ""))
	if res.StatusCode != 307 || res.Header.Get("Location") != testOrigin+"/api/auth/oidc/start?silent=1" {
		t.Fatalf("auto-start: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, q := range []string{"?sso=none", "?manual=1", "?e=invalid"} {
		res := e.do(req("GET", "/login"+q, ""))
		b := bootstrapOf(t, readBody(t, res))
		if res.StatusCode != 200 || !b.Login.OIDCEnabled || b.Login.OIDCLabel != "Sign in with Okta" || !b.Login.MagicLinkLoginEnabled {
			t.Errorf("%s: %d %+v", q, res.StatusCode, b.Login)
		}
	}
	idp.AutoStart = false
	if res := e.do(req("GET", "/login", "")); res.StatusCode != 200 {
		t.Fatalf("auto-start off: %d", res.StatusCode)
	}
	e.g.cfg.ElcanoPubKeyB64 = ""
	if b := bootstrapOf(t, readBody(t, e.do(req("GET", "/login", "")))); b.Login.MagicLinkLoginEnabled {
		t.Fatal("magic link enabled without AUTH_SIGNING_PUBKEY")
	}
	// Team link: the query or a parked cookie.
	if b := bootstrapOf(t, readBody(t, e.do(req("GET", "/login?team_link=1", "")))); !b.Login.TeamLink {
		t.Fatal("team_link query")
	}
	if b := bootstrapOf(t, readBody(t, e.do(withCookie(req("GET", "/login?e=invalid", ""), "fleet_team_link", testTeamLinkID)))); !b.Login.TeamLink {
		t.Fatal("parked cookie")
	}
	if b := bootstrapOf(t, readBody(t, e.do(req("GET", "/login", "")))); b.Login.TeamLink || b.Session != nil {
		t.Fatalf("%+v", b)
	}
	// The login page still renders with no backend at all.
	g, _ := New(Config{}, Options{Bundle: testBundle()})
	rec := newRecorder()
	g.ServeHTTP(rec, req("GET", "/login", ""))
	if rec.Code != 200 || bootstrapOf(t, rec.Body.String()).Branding.LoginTitle != "Welcome aboard." {
		t.Fatalf("no backend: %d", rec.Code)
	}
}

func TestSharedSnapshot(t *testing.T) {
	e := newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/shared/") {
			if r.Header.Get("X-User-Email") != "" || r.Header.Get("X-Chat-Server-Token") != testChatToken {
				t.Errorf("shared fetch must be token-only: %v", r.Header)
			}
			if r.RequestURI != "/shared/tok%201" {
				w.WriteHeader(404)
				return
			}
			_, _ = io.WriteString(w, `{"title":"T","messages":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}
	res := e.do(req("GET", "/shared/tok%201", ""))
	b := bootstrapOf(t, readBody(t, res))
	if res.StatusCode != 200 || !strings.Contains(string(b.Shared), `"title":"T"`) {
		t.Fatalf("%d %s", res.StatusCode, b.Shared)
	}
	res = e.do(req("GET", "/shared/revoked", ""))
	if b := bootstrapOf(t, readBody(t, res)); res.StatusCode != 404 || !b.NotFound || b.Shared != nil {
		t.Fatalf("revoked: %d", res.StatusCode)
	}
}

func TestNoBundleAnswers503ForPagesOnly(t *testing.T) {
	e := newTestEnv(t, withBundle(fstest.MapFS{}))
	if e.g.BundlePresent() || e.g.BuildID() != "dev" {
		t.Fatal("empty bundle reported present")
	}
	res := e.do(signedIn(t, req("GET", "/chat", "")))
	if res.StatusCode != 503 || !strings.Contains(readBody(t, res), "make build") {
		t.Fatalf("%d", res.StatusCode)
	}
	if res := e.do(signedIn(t, req("GET", "/api/personas", ""))); res.StatusCode != 200 {
		t.Fatalf("API without a bundle: %d", res.StatusCode)
	}
}

func TestIndexWithoutMarkersIsRejected(t *testing.T) {
	_, err := New(Config{}, Options{Bundle: fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}})
	if err == nil {
		t.Fatal("an index.html without the markers was accepted")
	}
}

func TestBuildIDFallsBackToIndexHash(t *testing.T) {
	b := testBundle()
	delete(b, "build-id.txt")
	e := newTestEnv(t, withBundle(b))
	if id := e.g.BuildID(); len(id) != 12 || id == testBuildID {
		t.Fatalf("%q", id)
	}
}

// The release check: the real embedded bundle (built by `make web` before
// `make bins`) is what EmbeddedBuildID reports. Without one it is "" — the
// release workflow fails on that.
func TestEmbeddedBuildIDMatchesPresence(t *testing.T) {
	id := EmbeddedBuildID()
	g, err := New(Config{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if g.BundlePresent() != (id != "") {
		t.Fatalf("present=%v id=%q", g.BundlePresent(), id)
	}
}
