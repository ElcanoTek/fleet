package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRecoverMiddleware_PanicReturns500 asserts a panic in a synchronous chat
// handler is converted into a 500 rather than crashing the single-host process.
func TestRecoverMiddleware_PanicReturns500(t *testing.T) {
	h := recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// TestRecoverMiddleware_PassesThroughOK confirms the middleware is transparent
// for a normal handler.
func TestRecoverMiddleware_PassesThroughOK(t *testing.T) {
	h := recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}
