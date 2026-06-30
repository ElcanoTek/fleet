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
	mux := buildOrchestratorMux(h, notes, reloadConfigHandler(nil))

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
	}
}
