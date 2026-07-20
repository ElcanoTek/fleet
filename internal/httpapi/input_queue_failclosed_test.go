package httpapi

// Fail-closed regression tests for the #785 idempotency/depth-cap store reads:
// a LookupInput or CountPendingInputs error must fail the submission (500, safe
// to retry with the same input_id) — not silently skip the idempotent-replay
// check (risking a duplicate billed turn) or waive the unattended-spend cap.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// failClosedStore wraps the in-memory fake with injectable read failures.
type failClosedStore struct {
	*fakeChatStore
	lookupErr       error
	countErr        error
	pendingOverride int // when > 0, CountPendingInputs reports this
}

func (s *failClosedStore) LookupInput(ctx context.Context, convID, clientID string) (*store.InputQueueRow, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	return s.fakeChatStore.LookupInput(ctx, convID, clientID)
}

func (s *failClosedStore) CountPendingInputs(ctx context.Context, convID string) (int, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	if s.pendingOverride > 0 {
		return s.pendingOverride, nil
	}
	return s.fakeChatStore.CountPendingInputs(ctx, convID)
}

// Idle path: an input_id replay check that errors must refuse the submission —
// falling through would start a fresh (billed) turn for an input that may
// already have been accepted.
func TestChatSubmit_LookupErrorFailsClosed_IdlePath(t *testing.T) {
	engine := &fakeEngine{}
	st := &failClosedStore{fakeChatStore: newFakeChatStore(), lookupErr: errors.New("db down")}
	srv := newDefaultChatServer(t, engine, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}

	w := postChatRequest(t, srv, map[string]any{
		"message": "hello", "conversation_id": conv.ID, "input_id": "idem-1",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "input lookup failed") {
		t.Fatalf("body = %q, want the lookup failure surfaced", w.Body.String())
	}
	if len(st.history[conv.ID]) != 0 {
		t.Fatal("a turn ran despite the failed idempotency check")
	}
}

// Busy path: a depth-cap count error must refuse the submission, not waive the
// unattended-spend cap.
func TestQueueSubmit_CountErrorFailsClosed(t *testing.T) {
	eng := &gatedEngine{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	st := &failClosedStore{fakeChatStore: newFakeChatStore(), countErr: errors.New("db down")}
	srv := newDefaultChatServer(t, eng, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}

	go postChatJSON(t, srv, "u@x.com", map[string]any{"message": "long turn", "conversation_id": conv.ID})
	<-eng.started
	defer func() { eng.release <- struct{}{} }()

	w := postChatJSON(t, srv, "u@x.com", map[string]any{
		"message": "queued", "conversation_id": conv.ID, "input_id": "q-1",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "input queue check failed") {
		t.Fatalf("body = %q, want the count failure surfaced", w.Body.String())
	}
	if n, _ := st.fakeChatStore.CountPendingInputs(context.Background(), conv.ID); n != 0 {
		t.Fatalf("a row was enqueued despite the failed cap check: %d", n)
	}
}

// Busy path at the cap: a replay-lookup error is a 500 (retryable, says nothing
// about the input), not a misleading 429 "queue full" refusal.
func TestQueueSubmit_LookupErrorAtCapIs500Not429(t *testing.T) {
	eng := &gatedEngine{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	st := &failClosedStore{
		fakeChatStore:   newFakeChatStore(),
		lookupErr:       errors.New("db down"),
		pendingOverride: maxPendingInputs,
	}
	srv := newDefaultChatServer(t, eng, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}

	go postChatJSON(t, srv, "u@x.com", map[string]any{"message": "long turn", "conversation_id": conv.ID})
	<-eng.started
	defer func() { eng.release <- struct{}{} }()

	w := postChatJSON(t, srv, "u@x.com", map[string]any{
		"message": "replay?", "conversation_id": conv.ID, "input_id": "q-1",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (not a false queue-full): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "input lookup failed") {
		t.Fatalf("body = %q, want the lookup failure surfaced", w.Body.String())
	}
}
