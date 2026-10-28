// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Ported from web/src/app/lib/backchannelLogout.test.ts and the
// backchannel-logout route test.

const bcAudience = "fleet-client"

type signingKey struct {
	priv ed25519.PrivateKey
	b64  string
	kid  string
}

func newSigningKey(t *testing.T) signingKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pub)
	return signingKey{priv, base64.StdEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(sum[:16])}
}

func (k signingKey) sign(t *testing.T, typ string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]any{"typ": typ, "alg": "EdDSA", "kid": k.kid})
	c, _ := json.Marshal(claims)
	head := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	return head + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.priv, []byte(head)))
}

func logoutClaims(issuer string, now time.Time) map[string]any {
	return map[string]any{
		"iss": issuer, "aud": bcAudience, "sub": "acct-1", "email": "Dave@X.com", "jti": "evt-1",
		"iat": float64(now.Unix()), "exp": float64(now.Add(2 * time.Minute).Unix()),
		"events": map[string]any{logoutEvent: map[string]any{}},
	}
}

// jwksServer publishes keys at /jwks.json and counts requests.
type jwksServer struct {
	srv  *httptest.Server
	hits atomic.Int32
	keys []signingKey
	down atomic.Bool
}

func newJWKSServer(t *testing.T, keys ...signingKey) *jwksServer {
	j := &jwksServer{keys: keys}
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.hits.Add(1)
		if j.down.Load() || r.URL.Path != "/jwks.json" {
			w.WriteHeader(503)
			return
		}
		var out []map[string]any
		out = append(out, map[string]any{"kty": "RSA", "n": "x"}, map[string]any{"kty": "OKP", "crv": "Ed25519", "x": 7}, map[string]any{"kty": "OKP", "crv": "Ed25519", "x": "AAAA"})
		for _, k := range j.keys {
			pub, _ := base64.StdEncoding.DecodeString(k.b64)
			out = append(out, map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub), "kid": k.kid})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func bcEnv(t *testing.T, issuer string, staticKey string) *testEnv {
	e := newTestEnv(t, withOIDC(&OIDCConfig{Issuer: issuer, ClientID: bcAudience, ClientSecret: "s"}))
	e.g.cfg.ElcanoPubKeyB64 = staticKey
	return e
}

func TestBackchannelVerifiesAJWKSOnlyKey(t *testing.T) {
	k := newSigningKey(t)
	j := newJWKSServer(t, k)
	e := bcEnv(t, j.srv.URL, "")
	now := time.Now()
	ev, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", logoutClaims(j.srv.URL, now)), j.srv.URL, bcAudience, now)
	if err != nil || ev.email != "dave@x.com" || ev.subject != "acct-1" || ev.eventID != "evt-1" {
		t.Fatalf("%v %+v", err, ev)
	}
	// Malformed JWKS entries were ignored, the good one used.
	if j.hits.Load() != 1 {
		t.Fatalf("hits %d", j.hits.Load())
	}
}

func TestBackchannelStaticKeyNeedsNoNetwork(t *testing.T) {
	k := newSigningKey(t)
	j := newJWKSServer(t)
	e := bcEnv(t, j.srv.URL, k.b64)
	now := time.Now()
	if _, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", logoutClaims(j.srv.URL, now)), j.srv.URL, bcAudience, now); err != nil {
		t.Fatal(err)
	}
	if j.hits.Load() != 0 {
		t.Fatalf("a static key fetched the JWKS (%d)", j.hits.Load())
	}
	// Previous keys are accepted too.
	old := newSigningKey(t)
	e.g.cfg.PreviousPubKeys = []string{old.b64}
	if _, err := e.g.verifyLogoutToken(old.sign(t, "logout+jwt", logoutClaims(j.srv.URL, now)), j.srv.URL, bcAudience, now); err != nil {
		t.Fatal(err)
	}
}

func TestBackchannelJWKSRefreshIsRateLimitedAndStaleKeysReread(t *testing.T) {
	k := newSigningKey(t)
	j := newJWKSServer(t, k)
	e := bcEnv(t, j.srv.URL, "")
	now := time.Now()
	unknown := newSigningKey(t)
	// Unknown kids: at most one refresh a minute.
	for i := 0; i < 3; i++ {
		if _, err := e.g.verifyLogoutToken(unknown.sign(t, "logout+jwt", logoutClaims(j.srv.URL, now)), j.srv.URL, bcAudience, now); err == nil {
			t.Fatal("unknown key verified")
		}
	}
	if j.hits.Load() != 1 {
		t.Fatalf("unknown kids amplified requests: %d", j.hits.Load())
	}
	// A known key in a fresh cache: no request.
	if _, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", logoutClaims(j.srv.URL, now)), j.srv.URL, bcAudience, now); err != nil {
		t.Fatal(err)
	}
	if j.hits.Load() != 1 {
		t.Fatalf("fresh cache refetched: %d", j.hits.Load())
	}
	// Stale (>10m): re-read first; a key Auth retired stops being trusted.
	later := now.Add(11 * time.Minute)
	j.keys = nil
	if _, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", logoutClaims(j.srv.URL, later)), j.srv.URL, bcAudience, later); err == nil {
		t.Fatal("a retired key was still trusted after the cache window")
	}
	// Auth unreachable keeps the last cached keys.
	k2 := newSigningKey(t)
	j.keys = []signingKey{k2}
	evenLater := later.Add(2 * time.Minute)
	if _, err := e.g.verifyLogoutToken(k2.sign(t, "logout+jwt", logoutClaims(j.srv.URL, evenLater)), j.srv.URL, bcAudience, evenLater); err != nil {
		t.Fatal(err)
	}
	j.down.Store(true)
	muchLater := evenLater.Add(20 * time.Minute)
	if _, err := e.g.verifyLogoutToken(k2.sign(t, "logout+jwt", logoutClaims(j.srv.URL, muchLater)), j.srv.URL, bcAudience, muchLater); err != nil {
		t.Fatalf("stale cached key dropped while Auth is down: %v", err)
	}
}

func TestBackchannelClaimChecks(t *testing.T) {
	k := newSigningKey(t)
	issuer := "https://auth.example.com"
	e := bcEnv(t, issuer, k.b64)
	now := time.Now()
	for name, mut := range map[string]func(map[string]any){
		"other audience": func(c map[string]any) { c["aud"] = "other" },
		"other issuer":   func(c map[string]any) { c["iss"] = "https://evil" },
		"bad email":      func(c map[string]any) { c["email"] = "no-at-sign" },
		"trailing at":    func(c map[string]any) { c["email"] = "x@" },
		"expired":        func(c map[string]any) { c["exp"] = float64(now.Add(-2 * time.Minute).Unix()) },
		"no expiry":      func(c map[string]any) { delete(c, "exp") },
		"future iat":     func(c map[string]any) { c["iat"] = float64(now.Add(5 * time.Minute).Unix()) },
		"fractional iat": func(c map[string]any) { c["iat"] = float64(now.Unix()) + 0.5 },
		"nonce":          func(c map[string]any) { c["nonce"] = "x" },
		"no event":       func(c map[string]any) { c["events"] = map[string]any{} },
		"null event":     func(c map[string]any) { c["events"] = map[string]any{logoutEvent: nil} },
		"no jti":         func(c map[string]any) { delete(c, "jti") },
		"long subject":   func(c map[string]any) { c["sub"] = strings.Repeat("s", 256) },
	} {
		c := logoutClaims(issuer, now)
		mut(c)
		if _, err := e.g.verifyLogoutToken(k.sign(t, "logout+jwt", c), issuer, bcAudience, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Wrong typ, alg, a tampered signature, and garbage.
	good := k.sign(t, "logout+jwt", logoutClaims(issuer, now))
	for name, tok := range map[string]string{
		"access typ as logout": k.sign(t, "access+jwt", logoutClaims(issuer, now)),
		"tampered":             good[:len(good)-4] + "AAAA",
		"garbage":              "a.b",
		"oversized":            strings.Repeat("a", maxBackchannelToken+1),
	} {
		if _, err := e.g.verifyLogoutToken(tok, issuer, bcAudience, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The trailing slash of the configured issuer is normalized.
	if ev, err := e.g.verifyLogoutToken(good, issuer+"/", bcAudience, now); err != nil || ev.issuer != issuer {
		t.Fatalf("%v %+v", err, ev)
	}
}

func accessClaims(issuer string, now time.Time, access map[string]any) map[string]any {
	c := logoutClaims(issuer, now)
	c["events"] = map[string]any{applicationAccessEvent: access}
	return c
}

func TestBackchannelApplicationAccess(t *testing.T) {
	k := newSigningKey(t)
	issuer := "https://auth.example.com"
	e := bcEnv(t, issuer, k.b64)
	now := time.Now()
	a, err := e.g.verifyAccessToken(k.sign(t, "access+jwt", accessClaims(issuer, now, map[string]any{"action": "grant", "version": float64(3), "settings": map[string]any{"role": "admin"}})), issuer, bcAudience, now)
	if err != nil || a.action != "grant" || a.version != 3 || a.settings["role"] != "admin" || a.issuedAt != now.Unix() {
		t.Fatalf("%v %+v", err, a)
	}
	for name, access := range map[string]map[string]any{
		"unknown action":  {"action": "promote", "version": float64(1)},
		"zero version":    {"action": "grant", "version": float64(0)},
		"float version":   {"action": "grant", "version": 1.5},
		"array settings":  {"action": "grant", "version": float64(1), "settings": []any{"x"}},
		"number setting":  {"action": "grant", "version": float64(1), "settings": map[string]any{"a": float64(1)}},
		"long setting":    {"action": "grant", "version": float64(1), "settings": map[string]any{"a": strings.Repeat("x", 65)}},
		"missing version": {"action": "revoke"},
	} {
		if _, err := e.g.verifyAccessToken(k.sign(t, "access+jwt", accessClaims(issuer, now, access)), issuer, bcAudience, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func bcPost(form string, contentLength int64) *http.Request {
	r := req("POST", "/api/auth/backchannel-logout", form, "Content-Type", "application/x-www-form-urlencoded")
	r.ContentLength = contentLength
	return r
}

func TestBackchannelRoute(t *testing.T) {
	k := newSigningKey(t)
	issuer := "https://auth.example.com"
	e := bcEnv(t, issuer, k.b64)
	now := time.Now()
	logout := "logout_token=" + url.QueryEscape(k.sign(t, "logout+jwt", logoutClaims(issuer, now)))

	res := e.do(bcPost(logout, int64(len(logout))))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	got := e.chat.last(t)
	if got.URI != "/auth/external-session-revoke" || got.Header.Get("X-User-Email") != "dave@x.com" || got.Body != `{"event_id":"evt-1","issuer":"https://auth.example.com","subject":"acct-1"}` {
		t.Fatalf("%q %q", got.URI, got.Body)
	}
	// chat-server could not record it → 503 (Auth retries).
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	if res := e.do(bcPost(logout, int64(len(logout)))); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("503: %d", res.StatusCode)
	}
	e.chat.respond = nil
	access := "access_token=" + url.QueryEscape(k.sign(t, "access+jwt", accessClaims(issuer, now, map[string]any{"action": "revoke", "version": float64(2)})))
	if res := e.do(bcPost(access, int64(len(access)))); res.StatusCode != http.StatusNoContent {
		t.Fatalf("access: %d", res.StatusCode)
	}
	if got := e.chat.last(t); got.URI != "/auth/external-access" || !strings.Contains(got.Body, `"action":"revoke","version":2,"issued_at":`) || strings.Contains(got.Body, "settings") {
		t.Fatalf("%q %q", got.URI, got.Body)
	}
	for name, c := range map[string]struct {
		r    *http.Request
		want int
	}{
		"both tokens":   {bcPost(logout+"&"+access, int64(len(logout+"&"+access))), 400},
		"neither":       {bcPost("x=1", 3), 400},
		"no length":     {bcPost(logout, -1), 411},
		"too large":     {bcPost(logout, maxBackchannelBody+1), 413},
		"invalid token": {bcPost("logout_token=abc", 16), 400},
	} {
		if res := e.do(c.r); res.StatusCode != c.want {
			t.Errorf("%s: %d want %d", name, res.StatusCode, c.want)
		}
	}
	// No CSRF Origin check (server-to-server), no session needed — and 404
	// when SSO is not configured.
	if res := newTestEnv(t).do(bcPost(logout, int64(len(logout)))); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured: %d", res.StatusCode)
	}
}

func teamRenameClaims(issuer string, now time.Time, rename map[string]any) map[string]any {
	return map[string]any{
		"iss": issuer, "aud": bcAudience, "jti": "evt-r1",
		"iat": float64(now.Unix()), "exp": float64(now.Add(2 * time.Minute).Unix()),
		"events": map[string]any{teamRenameEvent: rename},
	}
}

// A team rename is app-wide: a team+jwt naming no account, carrying exactly
// {from, to, version}. It is forwarded to chat-server with no user identity;
// an account claim, another event, an extra key or the wrong typ is refused,
// and an access token can never be read as one (or the other way round).
func TestBackchannelTeamRename(t *testing.T) {
	k := newSigningKey(t)
	issuer := "https://auth.example.com"
	e := bcEnv(t, issuer, k.b64)
	now := time.Now()
	good := map[string]any{"from": "Trading", "to": "Research", "version": float64(4)}
	tr, err := e.g.verifyTeamRenameToken(k.sign(t, "team+jwt", teamRenameClaims(issuer, now, good)), issuer, bcAudience, now)
	if err != nil || tr.from != "Trading" || tr.to != "Research" || tr.version != 4 || tr.eventID != "evt-r1" || tr.issuer != issuer {
		t.Fatalf("%v %+v", err, tr)
	}
	withSub := teamRenameClaims(issuer, now, good)
	withSub["sub"] = "acct-1"
	withEmail := teamRenameClaims(issuer, now, good)
	withEmail["email"] = "dave@x.com"
	twoEvents := teamRenameClaims(issuer, now, good)
	twoEvents["events"].(map[string]any)[applicationAccessEvent] = map[string]any{"action": "grant", "version": float64(1)}
	for name, tok := range map[string]string{
		"access typ":     k.sign(t, "access+jwt", teamRenameClaims(issuer, now, good)),
		"names account":  k.sign(t, "team+jwt", withSub),
		"names email":    k.sign(t, "team+jwt", withEmail),
		"two events":     k.sign(t, "team+jwt", twoEvents),
		"extra key":      k.sign(t, "team+jwt", teamRenameClaims(issuer, now, map[string]any{"from": "a", "to": "b", "version": float64(1), "x": "y"})),
		"no from":        k.sign(t, "team+jwt", teamRenameClaims(issuer, now, map[string]any{"from": "", "to": "b", "version": float64(1)})),
		"zero version":   k.sign(t, "team+jwt", teamRenameClaims(issuer, now, map[string]any{"from": "a", "to": "b", "version": float64(0)})),
		"long name":      k.sign(t, "team+jwt", teamRenameClaims(issuer, now, map[string]any{"from": "a", "to": strings.Repeat("x", 65), "version": float64(1)})),
		"other audience": k.sign(t, "team+jwt", func() map[string]any { c := teamRenameClaims(issuer, now, good); c["aud"] = "other"; return c }()),
		"expired":        k.sign(t, "team+jwt", teamRenameClaims(issuer, now.Add(-time.Hour), good)),
	} {
		if _, err := e.g.verifyTeamRenameToken(tok, issuer, bcAudience, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An access token is not readable as a team rename and vice versa.
	if _, err := e.g.verifyAccessToken(k.sign(t, "team+jwt", teamRenameClaims(issuer, now, good)), issuer, bcAudience, now); err == nil {
		t.Error("a team+jwt read as an access token")
	}

	// The route forwards it with no user identity.
	form := "team_token=" + url.QueryEscape(k.sign(t, "team+jwt", teamRenameClaims(issuer, now, good)))
	if res := e.do(bcPost(form, int64(len(form)))); res.StatusCode != http.StatusNoContent {
		t.Fatalf("team rename: %d", res.StatusCode)
	}
	got := e.chat.last(t)
	if got.URI != "/auth/external-team-rename" || got.Header.Get("X-User-Email") != "" || got.Header.Get("X-Chat-Server-Token") != testChatToken ||
		got.Body != `{"event_id":"evt-r1","issuer":"https://auth.example.com","version":4,"from":"Trading","to":"Research"}` {
		t.Fatalf("%q %v %q", got.URI, got.Header, got.Body)
	}
	access := "access_token=" + url.QueryEscape(k.sign(t, "access+jwt", accessClaims(issuer, now, map[string]any{"action": "revoke", "version": float64(2)})))
	if res := e.do(bcPost(form+"&"+access, int64(len(form+"&"+access)))); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("team + access tokens: %d", res.StatusCode)
	}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	if res := e.do(bcPost(form, int64(len(form)))); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("chat failure: %d (Auth must retry)", res.StatusCode)
	}
}
