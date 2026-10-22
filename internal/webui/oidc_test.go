// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// Ported from web/src/app/lib/oidc.test.ts and the oidc/{start,callback}
// route tests.

func TestOIDCConfigFromEnv(t *testing.T) {
	t.Setenv("FLEET_OIDC_ISSUER", "https://idp.example.com//")
	t.Setenv("FLEET_OIDC_CLIENT_ID", "client-123")
	t.Setenv("FLEET_OIDC_CLIENT_SECRET", "secret-xyz")
	t.Setenv("FLEET_OIDC_ALLOWED_DOMAINS", " @Example.com, , other.org ")
	o := ConfigFromEnv().OIDC
	if o == nil || o.Issuer != "https://idp.example.com" || o.Scopes != "openid email" || o.ButtonLabel != "Sign in with SSO" ||
		strings.Join(o.AllowedDomains, ",") != "example.com,other.org" || o.AutoStart {
		t.Fatalf("%+v", o)
	}
	for _, v := range []string{"1", "true", "YES", "on"} {
		t.Setenv("FLEET_OIDC_AUTO_START", v)
		if !ConfigFromEnv().OIDC.AutoStart {
			t.Errorf("%q did not enable auto-start", v)
		}
	}
	for _, k := range []string{"FLEET_OIDC_ISSUER", "FLEET_OIDC_CLIENT_ID", "FLEET_OIDC_CLIENT_SECRET"} {
		old := ConfigFromEnv()
		_ = old
		t.Setenv(k, "")
		if ConfigFromEnv().OIDC != nil {
			t.Errorf("missing %s still enabled OIDC", k)
		}
		t.Setenv(k, "x")
	}
}

func TestShouldAutoStartLogin(t *testing.T) {
	on := &OIDCConfig{AutoStart: true}
	if shouldAutoStartLogin(url.Values{}, nil) || shouldAutoStartLogin(url.Values{}, &OIDCConfig{}) {
		t.Fatal("auto-started with the flag off")
	}
	if !shouldAutoStartLogin(url.Values{}, on) {
		t.Fatal("plain anonymous visit did not auto-start")
	}
	for _, q := range []string{"e=invalid", "manual=1", "sso=none"} {
		v, _ := url.ParseQuery(q)
		if shouldAutoStartLogin(v, on) {
			t.Errorf("%s auto-started", q)
		}
	}
}

func TestEmailDomainAllowed(t *testing.T) {
	if !emailDomainAllowed("a@any.com", nil) {
		t.Fatal("empty allowlist rejected")
	}
	if !emailDomainAllowed("a@Example.COM", []string{"example.com"}) || emailDomainAllowed("a@evil.com", []string{"example.com"}) || emailDomainAllowed("no-at", []string{"example.com"}) {
		t.Fatal("allowlist")
	}
}

func jwt(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestValidateIDToken(t *testing.T) {
	cfg := &OIDCConfig{ClientID: "client-123"}
	doc := &discoveryDoc{Issuer: "https://idp.example.com"}
	now := time.Unix(1_800_000_000, 0)
	base := func() map[string]any {
		return map[string]any{"iss": "https://idp.example.com", "aud": "client-123", "exp": float64(now.Unix() + 60), "nonce": "n1", "email": " Alice@Example.com ", "sub": "s1"}
	}
	email, sub, reason := validateIDToken(base(), cfg, doc, "n1", now)
	if reason != "" || email != "alice@example.com" || sub != "s1" {
		t.Fatalf("%q %q %q", email, sub, reason)
	}
	arr := base()
	arr["aud"] = []any{"other", "client-123"}
	if _, _, r := validateIDToken(arr, cfg, doc, "n1", now); r != "" {
		t.Fatalf("array audience: %q", r)
	}
	for name, mut := range map[string]func(map[string]any){
		"issuer mismatch":    func(c map[string]any) { c["iss"] = "https://evil" },
		"audience mismatch":  func(c map[string]any) { c["aud"] = "other" },
		"azp mismatch":       func(c map[string]any) { c["azp"] = "other" },
		"expired":            func(c map[string]any) { c["exp"] = float64(now.Unix()) },
		"nonce mismatch":     func(c map[string]any) { c["nonce"] = "n2" },
		"no email claim":     func(c map[string]any) { delete(c, "email") },
		"no subject claim":   func(c map[string]any) { c["sub"] = strings.Repeat("s", 256) },
		"email not verified": func(c map[string]any) { c["email_verified"] = false },
	} {
		c := base()
		mut(c)
		if _, _, r := validateIDToken(c, cfg, doc, "n1", now); r != name {
			t.Errorf("%s: got %q", name, r)
		}
	}
	if _, _, r := validateIDToken(decodeJWTClaims("not.a"), cfg, doc, "n1", now); r != "unparseable id_token" {
		t.Fatalf("%q", r)
	}
	if decodeJWTClaims(jwt(map[string]any{"a": "b"}))["a"] != "b" {
		t.Fatal("decode")
	}
}

func TestPKCE(t *testing.T) {
	// RFC 7636 appendix B.
	if got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("%q", got)
	}
	if a, b := randomURLSafe(32), randomURLSafe(32); a == b {
		t.Fatal("random repeated")
	}
}

// fakeIDP is a minimal OIDC provider.
type fakeIDP struct {
	srv         *httptest.Server
	discoveries atomic.Int32
	issuer      string
	authMethods []string
	tokenForm   url.Values
	tokenAuth   string
	idClaims    map[string]any
	tokenStatus int
}

func newFakeIDP(t *testing.T) *fakeIDP {
	f := &fakeIDP{tokenStatus: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			f.discoveries.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": firstNonEmpty(f.issuer, f.srv.URL), "authorization_endpoint": f.srv.URL + "/authorize",
				"token_endpoint": f.srv.URL + "/token", "token_endpoint_auth_methods_supported": f.authMethods,
			})
		case "/token":
			_ = r.ParseForm()
			f.tokenForm = r.PostForm
			f.tokenAuth = r.Header.Get("Authorization")
			if f.tokenStatus != 200 {
				w.WriteHeader(f.tokenStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": jwt(f.idClaims)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func oidcEnv(t *testing.T, idp *fakeIDP) *testEnv {
	return newTestEnv(t, withOrigin(testOrigin), withOIDC(&OIDCConfig{Issuer: idp.srv.URL, ClientID: "client-123", ClientSecret: "secret-xyz", Scopes: "openid email", ButtonLabel: "SSO"}))
}

func TestOIDCStart(t *testing.T) {
	idp := newFakeIDP(t)
	e := oidcEnv(t, idp)
	res := e.do(req("GET", "/api/auth/oidc/start?silent=1", ""))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	q := loc.Query()
	if loc.Path != "/authorize" || q.Get("client_id") != "client-123" || q.Get("redirect_uri") != testOrigin+"/api/auth/oidc/callback" ||
		q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("prompt") != "none" || q.Get("scope") != "openid email" {
		t.Fatalf("%s", loc)
	}
	cookies := map[string]string{}
	for _, c := range res.Cookies() {
		cookies[c.Name] = c.Value
		if !c.HttpOnly || c.MaxAge != 600 || !c.Secure {
			t.Errorf("%s attributes %+v", c.Name, c)
		}
	}
	if cookies[oidcStateCookie] != q.Get("state") || cookies[oidcNonceCookie] != q.Get("nonce") || pkceChallenge(cookies[oidcVerifierCookie]) != q.Get("code_challenge") {
		t.Fatal("state/nonce/verifier do not match the authorization request")
	}
	// Discovery is cached per issuer.
	e.do(req("GET", "/api/auth/oidc/start", ""))
	if idp.discoveries.Load() != 1 {
		t.Fatalf("discoveries %d", idp.discoveries.Load())
	}
	// Not configured / broken discovery bounce to the login page.
	e2 := newTestEnv(t, withOrigin(testOrigin))
	if got := e2.do(req("GET", "/api/auth/oidc/start", "")).Header.Get("Location"); got != testOrigin+"/login?e=oidc_unavailable" {
		t.Fatalf("%q", got)
	}
	idp2 := newFakeIDP(t)
	idp2.issuer = "https://someone-else.example"
	e3 := oidcEnv(t, idp2)
	if got := e3.do(req("GET", "/api/auth/oidc/start", "")).Header.Get("Location"); got != testOrigin+"/login?e=oidc_error" {
		t.Fatalf("issuer mismatch: %q", got)
	}
	// A failed discovery is not cached.
	e3.do(req("GET", "/api/auth/oidc/start", ""))
	if idp2.discoveries.Load() != 2 {
		t.Fatalf("failure cached: %d", idp2.discoveries.Load())
	}
}

func callbackReq(q string, state, nonce, verifier string) *http.Request {
	r := req("GET", "/api/auth/oidc/callback?"+q, "")
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	r.AddCookie(&http.Cookie{Name: oidcNonceCookie, Value: nonce})
	r.AddCookie(&http.Cookie{Name: oidcVerifierCookie, Value: verifier})
	return r
}

func TestOIDCCallbackMintsACentralSession(t *testing.T) {
	idp := newFakeIDP(t)
	e := oidcEnv(t, idp)
	idp.idClaims = map[string]any{"iss": idp.srv.URL, "aud": "client-123", "exp": float64(time.Now().Add(time.Hour).Unix()), "nonce": "n1", "email": "Dave@X.com", "sub": "acct-1"}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"session_epoch": "ext-1"})
	}
	res := e.do(callbackReq("code=c1&state=s1", "s1", "n1", "v1"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != testOrigin+"/" {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if idp.tokenForm.Get("code") != "c1" || idp.tokenForm.Get("code_verifier") != "v1" || idp.tokenForm.Get("client_secret") != "secret-xyz" || idp.tokenAuth != "" {
		t.Fatalf("token request %v %q", idp.tokenForm, idp.tokenAuth)
	}
	epochCall := e.chat.last(t)
	if epochCall.URI != "/auth/external-session-epoch" || epochCall.Body != `{"issuer":"`+idp.srv.URL+`","subject":"acct-1"}` {
		t.Fatalf("%q %q", epochCall.URI, epochCall.Body)
	}
	var minted string
	for _, c := range res.Cookies() {
		if c.Name == websession.CookieName {
			minted = c.Value
		}
		if c.Name == oidcStateCookie && c.MaxAge != -1 && c.Value != "" {
			t.Errorf("temp cookie not cleared: %+v", c)
		}
	}
	s := websession.NewHMAC(testSecret).Verify(minted)
	if s == nil || s.Source != websession.SourceOIDC || s.Email != "dave@x.com" || s.Subject != "acct-1" || s.Epoch != "ext-1" {
		t.Fatalf("%+v", s)
	}
}

func TestOIDCCallbackUsesBasicAuthWhenAdvertised(t *testing.T) {
	idp := newFakeIDP(t)
	idp.authMethods = []string{"client_secret_basic"}
	e := oidcEnv(t, idp)
	idp.idClaims = map[string]any{"iss": idp.srv.URL, "aud": "client-123", "exp": float64(time.Now().Add(time.Hour).Unix()), "nonce": "n1", "email": "d@x.com", "sub": "s"}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"session_epoch":"e"}`) }
	e.do(callbackReq("code=c1&state=s1", "s1", "n1", "v1"))
	if idp.tokenAuth != "Basic "+base64.StdEncoding.EncodeToString([]byte("client-123:secret-xyz")) || idp.tokenForm.Has("client_secret") {
		t.Fatalf("%q %v", idp.tokenAuth, idp.tokenForm)
	}
}

func TestOIDCCallbackFailures(t *testing.T) {
	idp := newFakeIDP(t)
	e := oidcEnv(t, idp)
	good := map[string]any{"iss": idp.srv.URL, "aud": "client-123", "exp": float64(time.Now().Add(time.Hour).Unix()), "nonce": "n1", "email": "d@x.com", "sub": "s"}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"session_epoch":"e"}`) }
	cases := []struct {
		name  string
		r     *http.Request
		setup func()
		want  string
	}{
		{"silent: no central session", callbackReq("error=login_required", "", "", ""), nil, "/login?sso=none"},
		{"denied", callbackReq("error=access_denied", "s1", "n1", "v1"), nil, "/login?e=oidc_denied"},
		{"state mismatch", callbackReq("code=c&state=s2", "s1", "n1", "v1"), nil, "/login?e=oidc_error"},
		{"missing cookies", callbackReq("code=c&state=s1", "", "", ""), nil, "/login?e=oidc_error"},
		{"token endpoint error", callbackReq("code=c&state=s1", "s1", "n1", "v1"), func() { idp.tokenStatus = 400 }, "/login?e=oidc_error"},
		{"bad nonce", callbackReq("code=c&state=s1", "s1", "n2", "v1"), func() { idp.tokenStatus = 200; idp.idClaims = good }, "/login?e=oidc_error"},
		{"domain", callbackReq("code=c&state=s1", "s1", "n1", "v1"), func() { e.g.cfg.OIDC.AllowedDomains = []string{"corp.com"} }, "/login?e=oidc_domain"},
	}
	for _, c := range cases {
		if c.setup != nil {
			c.setup()
		}
		res := e.do(c.r)
		if res.Header.Get("Location") != testOrigin+c.want || strings.Contains(setCookies(res), websession.CookieName+"=e") {
			t.Errorf("%s: %q", c.name, res.Header.Get("Location"))
		}
		if !strings.Contains(setCookies(res), oidcStateCookie+"=; Path=/; Max-Age=0") {
			t.Errorf("%s: temp cookies not cleared", c.name)
		}
	}
	// No epoch from chat-server: refuse rather than mint an epoch-less cookie.
	e.g.cfg.OIDC.AllowedDomains = nil
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	if got := e.do(callbackReq("code=c&state=s1", "s1", "n1", "v1")).Header.Get("Location"); got != testOrigin+"/login?e=oidc_error" {
		t.Fatalf("no epoch: %q", got)
	}
}

func TestRedirectURIRules(t *testing.T) {
	idp := newFakeIDP(t)
	// Pinned wins.
	e := newTestEnv(t, withOrigin(testOrigin), withOIDC(&OIDCConfig{Issuer: idp.srv.URL, ClientID: "c", ClientSecret: "s", RedirectURI: "https://pinned.example/cb"}))
	if got := e.g.redirectURI(req("GET", "/", "")); got != "https://pinned.example/cb" {
		t.Fatalf("%q", got)
	}
	// Canonical origin ignores spoofed forwarded headers.
	e = newTestEnv(t, withOrigin(testOrigin), withOIDC(&OIDCConfig{Issuer: idp.srv.URL, ClientID: "c", ClientSecret: "s"}))
	if got := e.g.redirectURI(req("GET", "/", "", "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "http")); got != testOrigin+"/api/auth/oidc/callback" {
		t.Fatalf("%q", got)
	}
	// Local dev: the forwarded host.
	e = newTestEnv(t, withOIDC(&OIDCConfig{Issuer: idp.srv.URL, ClientID: "c", ClientSecret: "s"}))
	if got := e.g.redirectURI(req("GET", "/", "", "X-Forwarded-Host", "dev.example", "X-Forwarded-Proto", "https")); got != "https://dev.example/api/auth/oidc/callback" {
		t.Fatalf("%q", got)
	}
}
