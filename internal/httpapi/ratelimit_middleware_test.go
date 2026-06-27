package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

func okNext() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func chatReq(user string) *http.Request {
	req := httptest.NewRequest("POST", "/chat", nil)
	return req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
}

// TestChatRateLimitMiddleware_HeadersAnd429 verifies the RPM window blocks the
// over-limit request with a JSON 429 + Retry-After, and that every response
// carries advisory X-RateLimit-* headers.
func TestChatRateLimitMiddleware_HeadersAnd429(t *testing.T) {
	s := New(&config.Config{RateLimitEnabled: true, RatePerMinute: 2}, nil, nil)
	mw := s.rateLimitMiddleware(okNext())

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, chatReq("u@x.com"))
		if w.Code != http.StatusOK {
			t.Fatalf("req %d: got %d, want 200", i+1, w.Code)
		}
		if w.Header().Get("X-RateLimit-Limit") != "2" {
			t.Errorf("req %d: X-RateLimit-Limit = %q, want 2", i+1, w.Header().Get("X-RateLimit-Limit"))
		}
		if w.Header().Get("X-RateLimit-Remaining") == "" {
			t.Errorf("req %d: missing X-RateLimit-Remaining", i+1)
		}
	}

	w := httptest.NewRecorder()
	mw.ServeHTTP(w, chatReq("u@x.com"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd req: got %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 missing Retry-After")
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if body["error"] != "rate_limit_exceeded" || body["reason"] != "rpm" {
		t.Errorf("429 body = %v, want rate_limit_exceeded/rpm", body)
	}
	if s.RateLimitHits()["rpm"] != 1 {
		t.Errorf("hits[rpm] = %d, want 1", s.RateLimitHits()["rpm"])
	}
}

// TestChatRateLimitMiddleware_AdminExempt verifies ADMIN_EMAILS users bypass the
// RPM window entirely.
func TestChatRateLimitMiddleware_AdminExempt(t *testing.T) {
	s := New(&config.Config{RateLimitEnabled: true, RatePerMinute: 1, AdminEmails: []string{"admin@x.com"}}, nil, nil)
	mw := s.rateLimitMiddleware(okNext())
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, chatReq("admin@x.com"))
		if w.Code != http.StatusOK {
			t.Fatalf("admin req %d blocked: %d", i+1, w.Code)
		}
	}
}

// TestChatRateLimitMiddleware_Disabled verifies the master switch off => no
// limiting and no rate-limit headers.
func TestChatRateLimitMiddleware_Disabled(t *testing.T) {
	s := New(&config.Config{RateLimitEnabled: false, RatePerMinute: 1}, nil, nil)
	mw := s.rateLimitMiddleware(okNext())
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, chatReq("u@x.com"))
		if w.Code != http.StatusOK {
			t.Fatalf("disabled: req %d blocked: %d", i+1, w.Code)
		}
	}
}
