// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ported from web/src/app/lib/csrf.test.ts.

func csrfReq(host, origin, xfh string) *http.Request {
	r := httptest.NewRequest("POST", "http://"+host+"/api/foo", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if xfh != "" {
		r.Header.Set("X-Forwarded-Host", xfh)
	}
	return r
}

func TestVerifyOriginWithoutCanonicalOrigin(t *testing.T) {
	g := newTestEnv(t).g
	cases := []struct {
		name              string
		host, origin, xfh string
		ok                bool
	}{
		{"same origin", "chat.example.com", "http://chat.example.com", "", true},
		{"missing Origin", "chat.example.com", "", "", false},
		{"cross origin", "chat.example.com", "http://evil.example.net", "", false},
		{"malformed Origin", "chat.example.com", "not-a-url", "", false},
		{"x-forwarded-host ahead of host", "127.0.0.1:3000", "https://chat.example.com", "chat.example.com", true},
		{"mismatching origin, matching host", "chat.example.com", "https://attacker.example", "", false},
		{"port-specific localhost dev", "localhost:3000", "http://localhost:3000", "", true},
	}
	for _, c := range cases {
		if got := g.originRejection(csrfReq(c.host, c.origin, c.xfh)) == ""; got != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, got, c.ok)
		}
	}
}

func TestVerifyOriginWithCanonicalOrigin(t *testing.T) {
	g := newTestEnv(t, withOrigin("https://fleet.example.com")).g
	cases := []struct {
		name              string
		host, origin, xfh string
		ok                bool
	}{
		{"origin matching only a spoofed x-forwarded-host", "fleet.example.com", "https://evil.example.net", "evil.example.net", false},
		{"canonical origin despite spoofed x-forwarded-host", "fleet.example.com", "https://fleet.example.com", "evil.example.net", true},
		{"loopback tunnel localhost", "localhost:3000", "http://localhost:3000", "", true},
		{"loopback tunnel 127.0.0.1", "127.0.0.1:3000", "http://127.0.0.1:3000", "", true},
		{"loopback tunnel [::1]", "[::1]:3000", "http://[::1]:3000", "", true},
		{"loopback origin, other port", "localhost:3000", "http://localhost:8080", "", false},
		{"loopback origin, real deployment host", "fleet.example.com", "http://localhost:3000", "", false},
		{"x-forwarded-host ignored for the loopback exception", "fleet.example.com", "http://localhost:3000", "localhost:3000", false},
		{"lookalike loopback subdomain", "localhost.evil.example:3000", "http://localhost.evil.example:3000", "", false},
		{"non-loopback cross origin", "fleet.example.com", "https://evil.example.net", "", false},
	}
	for _, c := range cases {
		if got := g.originRejection(csrfReq(c.host, c.origin, c.xfh)) == ""; got != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, got, c.ok)
		}
	}
}

func TestCSRFRejectionShapeAndOrder(t *testing.T) {
	e := newTestEnv(t)
	// The gate's 401 comes first for an unauthenticated mutating request.
	if res := e.do(req("POST", "/api/conversations", "{}", "Origin", "https://evil.example")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", res.StatusCode)
	}
	// Signed in, a cross-site POST is a 403 {"error":"forbidden"} that never
	// reaches the backend; the reason is not leaked.
	res := e.do(signedIn(t, req("POST", "/api/conversations", "{}", "Origin", "https://evil.example")))
	body := readBody(t, res)
	if res.StatusCode != http.StatusForbidden || strings.TrimSpace(body) != `{"error":"forbidden"}` {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	if e.chat.count() != 0 {
		t.Fatal("a CSRF-rejected request reached the backend")
	}
	// GETs are not origin-checked.
	if res := e.do(signedIn(t, req("GET", "/api/conversations", ""))); res.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d", res.StatusCode)
	}
	// Every mutating verb on every registered mutating route is checked:
	// a cross-site request reaches no backend.
	for _, rt := range readNextRoutes(t) {
		if !isMutating(rt.method) || rt.path == "/api/auth/backchannel-logout" {
			continue
		}
		before := e.chat.count() + e.orch.count()
		res := e.do(signedIn(t, req(rt.method, samplePath(rt.path), "{}", "Origin", "https://evil.example")))
		if res.StatusCode != http.StatusForbidden || e.chat.count()+e.orch.count() != before {
			t.Errorf("%s %s: %d without a CSRF rejection", rt.method, rt.path, res.StatusCode)
		}
	}
}
