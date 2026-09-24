package chattui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A targeted Stop the server answers 409 (the named turn had already ended)
// is ErrTurnNotRunning, so the caller knows nothing was stopped; other
// failures stay ordinary errors.
func TestCancelReportsATurnThatAlreadyEnded(t *testing.T) {
	for status, want := range map[int]error{http.StatusConflict: ErrTurnNotRunning, http.StatusNoContent: nil} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		c := NewClient(Config{ServerURL: srv.URL, Email: "u@example.com", Token: "t"})
		if err := c.Cancel("conv", "turn"); !errors.Is(err, want) || (want == nil && err != nil) {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	if err := NewClient(Config{ServerURL: srv.URL, Email: "u@example.com", Token: "t"}).Cancel("conv", "turn"); err == nil || errors.Is(err, ErrTurnNotRunning) {
		t.Errorf("500: err = %v, want an ordinary failure", err)
	}
}

// A Stop by key the server answers 409 (the input had already finished) is
// ErrTurnNotRunning too: nothing was stopped.
func TestCancelInputReportsAFinishedInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) }))
	defer srv.Close()
	if err := NewClient(Config{ServerURL: srv.URL, Email: "u@example.com", Token: "t"}).CancelInput("conv", "key"); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("err = %v, want ErrTurnNotRunning", err)
	}
}
