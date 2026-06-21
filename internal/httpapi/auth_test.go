package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The auth middleware is the trust boundary between Next.js and chat-server.
// It needs to (a) reject requests missing or forging the shared token,
// (b) reject requests without X-User-Email, and (c) make the authenticated
// user available on the request context for handlers.

func newAuthTestServer() *Server {
	return &Server{sharedToken: "topsecret"}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	s := newAuthTestServer()
	h := s.authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not be called")
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	req.Header.Set("X-User-Email", "u@x.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status: %d want 403", w.Code)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	s := newAuthTestServer()
	h := s.authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not be called")
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	req.Header.Set("X-Chat-Server-Token", "wrong")
	req.Header.Set("X-User-Email", "u@x.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status: %d want 403", w.Code)
	}
}

func TestAuth_RejectsMissingUserEmail(t *testing.T) {
	s := newAuthTestServer()
	h := s.authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not be called")
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: %d want 400", w.Code)
	}
}

func TestAuth_PassesAndInjectsUser(t *testing.T) {
	s := newAuthTestServer()
	var seenUser string
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUser = userFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	req.Header.Set("X-User-Email", "u@x.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: %d want 200", w.Code)
	}
	if seenUser != "u@x.com" {
		t.Errorf("user: got %q", seenUser)
	}
}

func TestUserFromCtx_Empty(t *testing.T) {
	if got := userFromCtx(context.Background()); got != "" {
		t.Errorf("empty ctx: got %q", got)
	}
}
