package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestHealthz_ShuttingDown pins that /healthz flips to 503 the moment graceful
// shutdown begins (#278), so a load balancer / readiness probe stops routing new
// traffic here while the box drains.
func TestHealthz_ShuttingDown(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)
	s.shuttingDown.Store(true)

	rr := httptest.NewRecorder()
	s.healthz(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "shutting_down" {
		t.Errorf("status = %v, want shutting_down", body["status"])
	}
}

// TestPostChat_RejectedWhileShuttingDown pins that new turns are refused with 503
// during drain — the shutdown check sits before any auth/store work.
func TestPostChat_RejectedWhileShuttingDown(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)
	s.shuttingDown.Store(true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"message":"hi"}`))
	s.postChat(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
}

// TestBeginShutdown sets the draining flag, broadcasts a one-shot shutdown frame
// to live subscribers of in-flight turns, and is idempotent.
func TestBeginShutdown(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)

	buf := newTurnBuffer("c1", "t1")
	ch := subscribe(buf)
	s.inflightMu.Lock()
	s.inflight["c1"] = inflightEntry{cancel: func() {}, token: 1, buf: buf, turnID: "t1"}
	s.inflightMu.Unlock()

	s.BeginShutdown()
	if !s.shuttingDown.Load() {
		t.Fatal("shuttingDown not set")
	}
	select {
	case ev := <-ch:
		if ev.Name != "shutdown" {
			t.Errorf("frame = %q, want shutdown", ev.Name)
		}
	default:
		t.Fatal("in-flight subscriber did not receive a shutdown frame")
	}

	// Idempotent: a second call must not panic or re-broadcast (channel stays empty).
	s.BeginShutdown()
	select {
	case ev := <-ch:
		t.Errorf("second BeginShutdown re-broadcast %q", ev.Name)
	default:
	}
}

// TestDrainTurns covers both outcomes: an immediate drain when nothing is in
// flight, and a successful drain once an active turn finishes.
func TestDrainTurns(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !s.DrainTurns(ctx) {
		t.Fatal("DrainTurns with no active turns should return true immediately")
	}

	s.activeTurns.Add(1)
	s.activeTurnCount.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.activeTurnCount.Add(-1)
		s.activeTurns.Done()
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if !s.DrainTurns(ctx2) {
		t.Fatal("DrainTurns should return true after the active turn finished")
	}
	if got := s.ActiveTurns(); got != 0 {
		t.Errorf("ActiveTurns = %d, want 0", got)
	}
}

// TestDrainTurns_GraceExpires pins that DrainTurns reports false when a stuck
// turn outlasts the grace deadline — the caller then force-cancels.
func TestDrainTurns_GraceExpires(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)
	s.activeTurns.Add(1)
	s.activeTurnCount.Add(1)
	// Release after the assertion so the helper goroutine inside DrainTurns exits.
	defer func() {
		s.activeTurnCount.Add(-1)
		s.activeTurns.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if s.DrainTurns(ctx) {
		t.Fatal("DrainTurns should time out (false) while a turn is stuck")
	}
	if got := s.ActiveTurns(); got != 1 {
		t.Errorf("ActiveTurns = %d, want 1", got)
	}
}

// TestCancelInflightTurns fires every running turn's cancel func and returns the
// count cancelled.
func TestCancelInflightTurns(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)

	cancelled := make(chan struct{})
	s.inflightMu.Lock()
	s.inflight["c1"] = inflightEntry{cancel: func() { close(cancelled) }, token: 1, buf: newTurnBuffer("c1", "t1"), turnID: "t1"}
	s.inflightMu.Unlock()

	if n := s.CancelInflightTurns(); n != 1 {
		t.Fatalf("CancelInflightTurns = %d, want 1", n)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel func was not fired")
	}
}

// TestBroadcastControl pins that a control frame fans out to live subscribers
// WITHOUT entering the replay buffer or advancing the Last-Event-ID — a
// reconnecting client must resume exactly where it left off.
func TestBroadcastControl(t *testing.T) {
	b := newTurnBuffer("c1", "t1")
	b.Emit("turn.started", map[string]any{"x": 1})
	highBefore := b.HighestID()
	ch := subscribe(b)

	b.broadcastControl("shutdown")

	select {
	case ev := <-ch:
		if ev.Name != "shutdown" {
			t.Errorf("frame = %q, want shutdown", ev.Name)
		}
		if ev.ID != highBefore {
			t.Errorf("control frame id = %d, want reuse of highest %d (no gap)", ev.ID, highBefore)
		}
	default:
		t.Fatal("subscriber did not receive the control frame")
	}
	if got := b.HighestID(); got != highBefore {
		t.Errorf("HighestID advanced to %d (want %d) — control frame must not enter replay", got, highBefore)
	}
}

// subscribe registers a buffered subscriber channel on b (white-box helper) and
// returns it. Mirrors what Attach does, minus the HTTP plumbing.
func subscribe(b *turnBuffer) chan bufferedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSubID++
	ch := make(chan bufferedEvent, 8)
	b.subscribers[b.nextSubID] = ch
	return ch
}
