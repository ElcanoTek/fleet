// Copyright (c) 2025 ElcanoTek

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestSchedRateLimitMiddleware_PerIdentityLimit verifies a non-admin caller is
// throttled by the per-identity (here, per-IP) window, and that the 429 carries
// the full header set and JSON body.
func TestSchedRateLimitMiddleware_PerIdentityLimit(t *testing.T) {
	h := New(Config{SchedRateLimitPerMinute: 2, SchedGlobalRateLimitPerMinute: 1000}, nil, nil)
	mw := h.SchedRateLimitMiddleware(okHandler())
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/tasks", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := do(); w.Code != http.StatusOK {
			t.Fatalf("req %d: got %d, want 200", i+1, w.Code)
		}
	}
	w := do()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd req: got %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "2" {
		t.Errorf("X-RateLimit-Limit = %q, want 2", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if w.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("missing X-RateLimit-Reset header")
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "rate_limit_exceeded" {
		t.Errorf("body error = %v, want rate_limit_exceeded", body["error"])
	}
	if counts := h.RateLimitExceededCounts(); counts["minute"] != 1 {
		t.Errorf("exceeded[minute] = %d, want 1", counts["minute"])
	}
}

// TestSchedRateLimitMiddleware_AdminBypass verifies the admin key is never
// throttled, even past a tiny limit.
func TestSchedRateLimitMiddleware_AdminBypass(t *testing.T) {
	h := New(Config{AdminAPIKey: "admin-secret", SchedRateLimitPerMinute: 1, SchedGlobalRateLimitPerMinute: 1}, nil, nil)
	mw := h.SchedRateLimitMiddleware(okHandler())
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/tasks", nil)
		req.Header.Set("X-API-Key", "admin-secret")
		req.RemoteAddr = "203.0.113.6:1"
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("admin req %d blocked: %d", i+1, w.Code)
		}
	}
}

// TestSchedRateLimitMiddleware_GlobalCap verifies the process-wide cap blocks a
// second caller even from a different identity.
func TestSchedRateLimitMiddleware_GlobalCap(t *testing.T) {
	h := New(Config{SchedRateLimitPerMinute: 1000, SchedGlobalRateLimitPerMinute: 1}, nil, nil)
	mw := h.SchedRateLimitMiddleware(okHandler())

	req1 := httptest.NewRequest("POST", "/tasks", nil)
	req1.RemoteAddr = "203.0.113.7:1"
	w1 := httptest.NewRecorder()
	mw.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("1st request: got %d, want 200", w1.Code)
	}

	req2 := httptest.NewRequest("POST", "/tasks", nil)
	req2.RemoteAddr = "203.0.113.8:1" // different IP — global cap still applies
	w2 := httptest.NewRecorder()
	mw.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd request (different IP) should hit global cap: got %d", w2.Code)
	}
	if counts := h.RateLimitExceededCounts(); counts["global"] != 1 {
		t.Errorf("exceeded[global] = %d, want 1", counts["global"])
	}
}

// TestSchedRateLimitMiddleware_PerKeyOverride verifies an API key's own
// RateLimit overrides the global per-minute default for that key.
func TestSchedRateLimitMiddleware_PerKeyOverride(t *testing.T) {
	dir := t.TempDir()
	mgr, err := apikeys.NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// RateLimit=1 → this key may submit one task/minute, overriding the 100 default.
	_, rawKey, err := mgr.CreateKey("ci", nil, nil, nil, 1, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	h := New(Config{SchedRateLimitPerMinute: 100, SchedGlobalRateLimitPerMinute: 1000}, nil, mgr)
	mw := h.SchedRateLimitMiddleware(okHandler())
	do := func() int {
		req := httptest.NewRequest("POST", "/tasks", nil)
		req.Header.Set("X-API-Key", rawKey)
		req.RemoteAddr = "203.0.113.9:1"
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)
		return w.Code
	}
	if c := do(); c != http.StatusOK {
		t.Fatalf("1st request: got %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("2nd request should hit the key's override cap (1/min): got %d", c)
	}
}
