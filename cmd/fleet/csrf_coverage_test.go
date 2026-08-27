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
	h := handlers.New(handlers.Config{}, nil, nil)
	notes := handlers.NewNotesHandlers(nil, h)
	mux := buildOrchestratorMux(h, notes, reloadConfigHandler(nil), mcpReloadHandler(nil))

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

		// The Next-proxy shared-token exemption composes with fail-closed auth:
		// a present-but-WRONG X-Orchestrator-Server-Token skips the CSRF gate
		// (a custom header cannot be attached by a cross-site browser request,
		// so it is not CSRF-forgeable) and is then rejected by the handler's
		// own auth — 403 Forbidden, never the CSRF message, never a success.
		req := httptest.NewRequest("POST", path, nil)
		req.Host = host
		req.Header.Set("X-Orchestrator-Server-Token", "wrong-token")
		req.Header.Set("X-User-Email", "attacker@example.com")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden || strings.Contains(rr.Body.String(), csrfMsg) {
			t.Errorf("POST %s wrong shared token: code=%d body=%q, want 403 from fail-closed auth (not CSRF, not success)", path, rr.Code, rr.Body.String())
		}
	}

	// /a2a is DELIBERATELY exempt from the cookie-CSRF origin check (#1279):
	// its dispatcher authenticates with X-API-Key only — no cookie path — and
	// the normal A2A client is a non-browser caller that sends no Origin at
	// all. This locks in that an origin-less POST reaches the handler (which,
	// in this unwired mux, answers 501 a2a_disabled) instead of being CSRF-
	// blocked; if the /a2a auth model ever grows a cookie path, this exemption
	// and test must be revisited together.
	if rr := do("/a2a", ""); strings.Contains(rr.Body.String(), csrfMsg) || rr.Code != http.StatusNotImplemented {
		t.Errorf("POST /a2a no-origin: code=%d body=%q, want the 501 a2a_disabled handler response, never the CSRF block", rr.Code, rr.Body.String())
	}
}
