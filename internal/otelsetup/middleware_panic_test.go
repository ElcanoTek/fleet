package otelsetup

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// A handler panic (recovered further out by the server's recover middleware)
// ends the request's server span as a "panic" failure (#2052).
func TestMiddlewareEndsServerSpanOnHandlerPanic(t *testing.T) {
	sr := spantest.Install(t)
	h := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the handler panic did not propagate to the outer recover")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/conversations", nil))
	}()
	sr.AssertAllEnded(t)
	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans ended, want the one server span", len(ended))
	}
	spantest.AssertStatus(t, ended[0], codes.Error, "panic")
}
