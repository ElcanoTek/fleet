package chattui

import (
	"context"
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

// A 200 acknowledgement is a replay of an input accepted earlier, even when
// that input is still queued; a 202 is one queued just now.
func TestQueuedReplayIsTheServersStatus(t *testing.T) {
	for status, want := range map[int]bool{http.StatusOK: true, http.StatusAccepted: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"queued":true,"input":{"id":"r","mode":"queued","state":"queued","position":2},"conversation_id":"c"}`))
		}))
		c := NewClient(Config{ServerURL: srv.URL, Email: "u@example.com", Token: "t"})
		_, err := c.StreamInput(context.Background(), "hi", "c", "k", func(Event) {})
		var q *QueuedError
		if !errors.As(err, &q) || q.Replayed() != want {
			t.Errorf("status %d: err %v, replayed %v; want %v", status, err, q != nil && q.Replayed(), want)
		}
		srv.Close()
	}
}

// A 202 from a targeted Stop is ErrStopUnconfirmed: sent, not yet confirmed.
func TestCancelReportsAnUnconfirmedStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@example.com", Token: "t"})
	if err := c.Cancel("conv", "turn"); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("Cancel: %v, want ErrStopUnconfirmed", err)
	}
	if err := c.CancelInput("conv", "key"); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("CancelInput: %v, want ErrStopUnconfirmed", err)
	}
}
