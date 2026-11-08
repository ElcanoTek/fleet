// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"testing"
)

// A browser-supplied identity header with no session is not an identity: the
// gate refuses the request and no backend is called.
func TestIdentityHeadersWithoutASessionAreIgnored(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/api/personas", "/api/orchestrator/me"} {
		res := e.do(req("GET", path, "",
			"X-User-Email", "victim@x.com",
			"X-User-Session-Epoch", "e",
			"X-Chat-Server-Token", "guess",
			"X-Orchestrator-Server-Token", "guess",
		))
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", path, res.StatusCode)
		}
	}
	if e.chat.count() != 0 || e.orch.count() != 0 {
		t.Fatalf("a backend was called: chat %d orch %d", e.chat.count(), e.orch.count())
	}
}

// With a real session the same headers change nothing: the backend gets the
// session's principal and none of the headers.
func TestIdentityHeadersWithASessionYieldTheSessionPrincipal(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(signedIn(t, req("GET", "/api/personas", "", "X-User-Email", "victim@x.com", "X-User-Session-Epoch", "forged")))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	got := e.chat.last(t)
	if !got.isSession("alice@example.com") || got.Who.Session.Epoch != "epoch-1" || !got.noIdentityHeaders() {
		t.Fatalf("principal %+v headers %v", got.Who, got.Header)
	}
}
