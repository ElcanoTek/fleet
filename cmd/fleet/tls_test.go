package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedirectToHTTPS(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/chat?x=1", nil)
	req.Host = "example.com:80"
	rr := httptest.NewRecorder()
	redirectToHTTPS(rr, req)
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("code = %d, want 301", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "https://example.com/chat?x=1" {
		t.Errorf("Location = %q, want https://example.com/chat?x=1 (port stripped)", loc)
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// HSTS present when TLS is active.
	rr := httptest.NewRecorder()
	securityHeadersMiddleware(next, true).ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS header missing when TLS active")
	}
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options missing")
	}
	if rr.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("X-Frame-Options missing")
	}

	// HSTS absent over plain HTTP (no-op per spec).
	rr2 := httptest.NewRecorder()
	securityHeadersMiddleware(next, false).ServeHTTP(rr2, httptest.NewRequest("GET", "/", nil))
	if rr2.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS header present over plain HTTP")
	}
	if rr2.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options should always be set")
	}
}

func TestStripPort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"example.com:80", "example.com"},
		{"example.com", "example.com"},
		{"127.0.0.1:8443", "127.0.0.1"},
	} {
		if got := stripPort(tc.in); got != tc.want {
			t.Errorf("stripPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
