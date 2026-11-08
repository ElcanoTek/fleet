package otelsetup

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// TestMiddlewareAttachesRequestIDToLogContext: the handler's context carries
// the same request_id the response returns as X-Request-Id, so every
// logging.From(r.Context()) line of the request is correlatable (#2052).
func TestMiddlewareAttachesRequestIDToLogContext(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = logging.Value(r.Context(), logging.KeyRequestID)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conversations", nil))

	want := rec.Header().Get("X-Request-Id")
	if want == "" {
		t.Fatal("no X-Request-Id header")
	}
	if seen != want {
		t.Fatalf("request_id on the handler context = %q, want the X-Request-Id %q", seen, want)
	}
}
