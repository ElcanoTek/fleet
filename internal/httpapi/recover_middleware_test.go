package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// TestRecoverMiddleware_PanicReturnsJSON500 verifies a panicking chat handler is
// contained as a JSON 500 (not a process crash) and emitted via safe.EmitPanic (#241).
func TestRecoverMiddleware_PanicReturnsJSON500(t *testing.T) {
	const loc = "httpapi.handler GET /boom"
	prev := safe.PanicEventHook
	var emitted int
	safe.PanicEventHook = func(event safe.PanicEvent, _ any) {
		if event.Location == loc {
			emitted++
		}
	}
	t.Cleanup(func() { safe.PanicEventHook = prev })

	h := recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler boom")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/boom", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got %q (%v)", rr.Body.String(), err)
	}
	if body["error"] != "internal server error" {
		t.Errorf("body = %v, want internal server error", body)
	}
	if emitted != 1 {
		t.Errorf("panic events for %q = %d, want 1", loc, emitted)
	}
}

// TestRecoverMiddleware_AbortHandlerPropagates verifies a deliberate
// http.ErrAbortHandler is re-panicked (the server's to handle), not swallowed.
func TestRecoverMiddleware_AbortHandlerPropagates(t *testing.T) {
	h := recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		rec := recover()
		if err, ok := rec.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("ErrAbortHandler should propagate, got %v", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
}
