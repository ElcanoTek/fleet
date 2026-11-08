// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/principal"
)

// TestOrchestratorMountBoundaryOrder pins the browser boundary in front of the
// orchestrator router, in order: the gate (no credential → 401, before
// anything else), the route (unregistered → JSON 404/405, before CSRF), CSRF
// on a mutating verb, then the call.
func TestOrchestratorMountBoundaryOrder(t *testing.T) {
	e := newTestEnv(t)
	evil := []string{"Origin", "https://evil.example"}
	good := []string{"Origin", testOrigin, "Content-Type", "application/json"}

	if res := e.do(req("POST", "/api/orchestrator/no-such-route", "", evil...)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential: %d, want 401 from the gate", res.StatusCode)
	}
	res := e.do(signedIn(t, req("POST", "/api/orchestrator/no-such-route", "", evil...)))
	if body := readBody(t, res); res.StatusCode != http.StatusNotFound || !strings.Contains(body, `"not found"`) {
		t.Errorf("unregistered: %d %q, want the JSON 404", res.StatusCode, body)
	}
	res = e.do(signedIn(t, req("PATCH", "/api/orchestrator/tasks/t1", "", evil...)))
	if res.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(res.Header.Get("Allow"), "GET") {
		t.Errorf("wrong method: %d Allow=%q, want 405 with Allow", res.StatusCode, res.Header.Get("Allow"))
	}
	if res := e.do(signedIn(t, req("POST", "/api/orchestrator/tasks", "{}", evil...))); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d, want 403", res.StatusCode)
	}
	if n := e.orch.count(); n != 0 {
		t.Fatalf("%d requests reached the orchestrator past a refusal", n)
	}
	if res := e.do(signedIn(t, req("POST", "/api/orchestrator/tasks", "{}", good...))); res.StatusCode != http.StatusOK {
		t.Errorf("same-origin POST: %d", res.StatusCode)
	}
	got := e.orch.last(t)
	if got.Path != "/tasks" || got.Body != "{}" || !got.HasWho || got.Who.AuthMethod != principal.MethodSession || got.RemoteAddr != backendRemoteAddr {
		t.Errorf("call: %+v", got)
	}
}

// TestOrchestratorMountServesTheWholeRouter: a route the browser never had a
// row for answers under the prefix, with its query forwarded verbatim as on
// /v1; a bearer is forwarded and no principal is attached; an encoded
// segment reaches the router still encoded.
func TestOrchestratorMountServesTheWholeRouter(t *testing.T) {
	e := newTestEnv(t)
	fake := e.g.orch.(*fakeOrchestrator)
	fake.routes.Method("GET", "/keys", fake.fakeBackend)

	e.do(req("GET", "/api/orchestrator/keys?limit=2&x=y", "", "Authorization", "Bearer k1", "Cookie", "a=b"))
	got := e.orch.last(t)
	if got.URI != "/keys?limit=2&x=y" || got.Header.Get("Authorization") != "Bearer k1" || got.Header.Get("Cookie") != "" || got.HasWho {
		t.Errorf("bearer call: %+v", got)
	}

	e.do(signedIn(t, req("GET", "/api/orchestrator/tasks/a%2Fb", "")))
	if got := e.orch.last(t); got.Path != "/tasks/a/b" || got.RawPath != "/tasks/a%2Fb" {
		t.Errorf("encoded segment: path %q raw %q", got.Path, got.RawPath)
	}

	// The renamed rows answer at both their browser path and their router path.
	for _, p := range []string{"/api/orchestrator/config", "/api/orchestrator/api/config"} {
		e.do(signedIn(t, req("GET", p, "")))
		if got := e.orch.last(t); got.URI != "/api/config" {
			t.Errorf("%s → %q", p, got.URI)
		}
	}
}
