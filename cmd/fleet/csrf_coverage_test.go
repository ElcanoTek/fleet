package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/handlers"
)

// TestOrchestratorCSRFCoverage proves the global CSRFMiddleware actually wraps
// the cookie-authenticated mutating routes (POST /tasks, /upload) in the REAL
// buildOrchestratorMux — these routes are registered outside any explicit auth
// group, so their CSRF coverage depends on the global middleware ordering. This
// test locks that in structurally (#304): a cross-origin / origin-less cookie
// request is blocked by CSRF before reaching the handler, while a same-origin
// request clears the CSRF gate (and only then meets the handler's own auth).
func TestOrchestratorCSRFCoverage(t *testing.T) {
	t.Parallel()

	h := handlers.New(handlers.Config{}, nil, nil)
	mux := buildOrchestratorMux(h, reloadConfigHandler(nil), mcpReloadHandler(nil), nil)

	const host = "fleet.example.com"
	do := func(path, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	const csrfMsg = "Cross-origin request blocked"
	for _, path := range []string{"/tasks", "/upload"} {
		// Cross-origin cookie request → blocked by CSRF before auth.
		if rr := do(path, "https://evil.example.com"); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), csrfMsg) {
			t.Errorf("POST %s cross-origin: code=%d body=%q, want 403 + CSRF block (route must be CSRF-covered)", path, rr.Code, rr.Body.String())
		}
		// Missing Origin → also blocked (real browsers always send it on mutating requests).
		if rr := do(path, ""); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), csrfMsg) {
			t.Errorf("POST %s no-origin: code=%d, want 403 + CSRF block", path, rr.Code)
		}
		// Same-origin clears the CSRF gate; whatever the handler returns, it must
		// NOT be the CSRF rejection.
		if rr := do(path, "https://"+host); strings.Contains(rr.Body.String(), csrfMsg) {
			t.Errorf("POST %s same-origin: still CSRF-blocked (%q); the Origin check should have passed", path, rr.Body.String())
		}

		// The retired service token (ADR-0108) is refused ahead of CSRF with
		// a 401 that names the replacement — never the CSRF message, never a
		// success, whatever identity header rides along.
		req := httptest.NewRequest("POST", path, nil)
		req.Host = host
		req.Header.Set("X-Orchestrator-Server-Token", "any-token")
		req.Header.Set("X-User-Email", "attacker@example.com")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "service_token_retired") {
			t.Errorf("POST %s retired service token: code=%d body=%q, want 401 service_token_retired", path, rr.Code, rr.Body.String())
		}
	}

	// /a2a is DELIBERATELY exempt from the cookie-CSRF origin check (#1279):
	// its dispatcher authenticates with X-API-Key only — no cookie path — and
	// the normal A2A client is a non-browser caller that sends no Origin at
	// all. This locks in that an origin-less POST reaches the handler (which,
	// in this unwired mux, answers 501 a2a_disabled) instead of being CSRF-
	// blocked; if the /a2a auth model ever grows a cookie path, this exemption
	// and test must be revisited together.
	// Both spellings: httpx-based A2A clients (the official TCK included)
	// POST to the trailing-slash form, so it carries the same exemption.
	for _, path := range []string{"/a2a", "/a2a/"} {
		if rr := do(path, ""); strings.Contains(rr.Body.String(), csrfMsg) || rr.Code != http.StatusNotImplemented {
			t.Errorf("POST %s no-origin: code=%d body=%q, want the 501 a2a_disabled handler response, never the CSRF block", path, rr.Code, rr.Body.String())
		}
	}
}
