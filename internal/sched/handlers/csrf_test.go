// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCSRFMiddlewareOrigin exercises the stateless Origin-based CSRF defense
// that replaced the in-memory synchronizer-token store. On the cookie/session
// path, mutating requests must carry a same-origin Origin header; requests
// authenticated by a token that the browser does not auto-attach (API key,
// bearer, registration) and safe methods stay exempt. Mirrors chat's
// verifyOrigin contract.
func TestCSRFMiddlewareOrigin(t *testing.T) {
	h := &Handlers{}
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	handler := h.CSRFMiddleware(next)

	// do issues a request through the middleware. Host defaults to the
	// canonical host; setup can override headers/host per case.
	do := func(method, path string, setup func(*http.Request)) *httptest.ResponseRecorder {
		reached = false
		req := httptest.NewRequest(method, path, nil)
		req.Host = "moc.elcanotek.com"
		if setup != nil {
			setup(req)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	// --- cookie/session path: the Origin header is enforced ---

	t.Run("same-origin POST passes", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("Origin", "https://moc.elcanotek.com")
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("same-origin POST should pass: code=%d reached=%v", rr.Code, reached)
		}
	})

	t.Run("missing Origin rejected", func(t *testing.T) {
		rr := do("POST", "/tasks", nil)
		if rr.Code != http.StatusForbidden || reached {
			t.Fatalf("missing Origin must be 403: code=%d reached=%v", rr.Code, reached)
		}
	})

	t.Run("malformed Origin rejected", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("Origin", "%zz") // url.Parse error
		})
		if rr.Code != http.StatusForbidden || reached {
			t.Fatalf("malformed Origin must be 403: code=%d", rr.Code)
		}
	})

	t.Run("Origin: null rejected", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("Origin", "null") // parses but has empty host
		})
		if rr.Code != http.StatusForbidden || reached {
			t.Fatalf("Origin null must be 403: code=%d", rr.Code)
		}
	})

	t.Run("cross-origin host rejected", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example.com")
		})
		if rr.Code != http.StatusForbidden || reached {
			t.Fatalf("cross-origin POST must be 403: code=%d", rr.Code)
		}
	})

	t.Run("Origin matching X-Forwarded-Host passes (behind proxy)", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Host = "127.0.0.1:8000" // what the server sees behind Caddy
			r.Header.Set("X-Forwarded-Host", "moc.elcanotek.com")
			r.Header.Set("Origin", "https://moc.elcanotek.com")
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("Origin matching X-Forwarded-Host should pass: code=%d", rr.Code)
		}
	})

	t.Run("host comparison is case-insensitive", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("Origin", "https://MOC.Elcanotek.com")
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("case-insensitive host should pass: code=%d", rr.Code)
		}
	})

	// --- exemptions: safe methods and non-cookie auth skip the Origin check ---

	t.Run("safe GET bypasses the check even cross-origin", func(t *testing.T) {
		rr := do("GET", "/tasks", func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example.com")
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("GET should bypass CSRF: code=%d", rr.Code)
		}
	})

	t.Run("API key request is exempt", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("X-API-Key", "some-key") // no Origin
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("X-API-Key POST should bypass CSRF: code=%d", rr.Code)
		}
	})

	t.Run("bearer token request is exempt", func(t *testing.T) {
		rr := do("POST", "/tasks", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer some-token") // no Origin
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("Bearer POST should bypass CSRF: code=%d", rr.Code)
		}
	})

	t.Run("registration token request is exempt", func(t *testing.T) {
		rr := do("POST", "/register", func(r *http.Request) {
			r.Header.Set("X-Registration-Token", "reg-token") // no Origin
		})
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("X-Registration-Token POST should bypass CSRF: code=%d", rr.Code)
		}
	})

	t.Run("/auth/login is exempt (issues a session, no cookie to forge)", func(t *testing.T) {
		rr := do("POST", "/auth/login", nil) // no Origin, no token
		if rr.Code != http.StatusOK || !reached {
			t.Fatalf("/auth/login should bypass CSRF: code=%d", rr.Code)
		}
	})
}
