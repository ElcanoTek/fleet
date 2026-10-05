package httpapi

// Fail-closed regression tests for the #785 idempotency/depth-cap store reads:
// a LookupInput or CountPendingInputs error must fail the submission (500, safe
// to retry with the same input_id) — not silently skip the idempotent-replay
// check (risking a duplicate billed turn) or waive the unattended-spend cap.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
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

// aheadFailStore fails only the place-in-line count behind the queue ack.
type aheadFailStore struct{ *fakeChatStore }

func (s *aheadFailStore) InputsAhead(context.Context, string) (int, bool, error) {
	return 0, false, errors.New("db down")
}

// The place-in-line count is not a gate: the input is already durably
// queued when it runs, so a failed count still answers 202 — without
// "ahead", so a client names no place rather than a wrong one.
func TestQueueSubmit_AheadCountErrorStillAcknowledges(t *testing.T) {
	eng := &gatedEngine{started: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	st := &aheadFailStore{fakeChatStore: newFakeChatStore()}
	srv := newDefaultChatServer(t, eng, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}

	go postChatJSON(t, srv, "u@x.com", map[string]any{"message": "long turn", "conversation_id": conv.ID})
	<-eng.started
	defer func() { eng.release <- struct{}{}; eng.release <- struct{}{} }() // the turn, then the drained input

	w := postChatJSON(t, srv, "u@x.com", map[string]any{
		"message": "queued", "conversation_id": conv.ID, "input_id": "q-1",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d, want 202: %s", w.Code, w.Body.String())
	}
	in := decodeQueueAck(t, w.Body.Bytes())
	if _, ok := in["ahead"]; ok {
		t.Fatalf("ack = %s, want no ahead when the count failed", w.Body.String())
	}
	if in["state"] != store.InputStateQueued {
		t.Fatalf("ack = %s, want the queued row", w.Body.String())
	}
}

// decodeQueueAck returns a queue ack's input object, as the wire carries it.
func decodeQueueAck(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var ack struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatalf("ack %s: %v", body, err)
	}
	return ack.Input
}

// capReplayStore reports the queue full and misses the first LookupInput:
// the resend's row commits between the early replay check and the busy
// path's cap check (a concurrent resend), so the cap path's own lookup is the
// one that answers it.
type capReplayStore struct {
	*fakeChatStore
	lookups atomic.Int32
}

func (s *capReplayStore) LookupInput(ctx context.Context, convID, clientID string) (*store.InputQueueRow, error) {
	if s.lookups.Add(1) == 1 {
		return nil, nil
	}
	return s.fakeChatStore.LookupInput(ctx, convID, clientID)
}

func (s *capReplayStore) CountPendingInputs(context.Context, string) (int, error) {
	return maxPendingInputs, nil
}

// The queue-cap replay (a full queue answering a resend of an input it
// already holds) carries the input's place in line like every other ack.
func TestQueueSubmit_CapReplayAckCarriesPlaceInLine(t *testing.T) {
	eng := &gatedEngine{started: make(chan struct{}, 4), release: make(chan struct{}, 4)}
	st := &capReplayStore{fakeChatStore: newFakeChatStore()}
	srv := newDefaultChatServer(t, eng, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, _, err := st.EnqueueInput(t.Context(), store.InputQueueRow{
			ID: fmt.Sprintf("row-%d", i), ConversationID: conv.ID, UserEmail: "u@x.com",
			ClientInputID: fmt.Sprintf("k-%d", i), Mode: store.InputModeQueued,
		}); err != nil {
			t.Fatal(err)
		}
	}

	go postChatJSON(t, srv, "u@x.com", map[string]any{"message": "long turn", "conversation_id": conv.ID})
	<-eng.started
	defer func() { // the turn, then the three queued inputs it drains
		for range 4 {
			eng.release <- struct{}{}
		}
	}()

	w := postChatJSON(t, srv, "u@x.com", map[string]any{
		"message": "resend", "conversation_id": conv.ID, "input_id": "k-2",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want the 200 cap replay: %s", w.Code, w.Body.String())
	}
	if n := st.lookups.Load(); n != 2 {
		t.Fatalf("LookupInput calls = %d, want 2 (the cap path answered the resend)", n)
	}
	if in := decodeQueueAck(t, w.Body.Bytes()); in["id"] != "row-2" || in["ahead"] != float64(2) {
		t.Fatalf("ack input = %v, want row-2 with 2 ahead", in)
	}
}

// A replayed input a Stop by key stamped is still 'queued' in its row, but
// the next claim cancels it: its ack names no place in line, and that
// expected "not in line" is not an error.
func TestQueueSubmit_StampedReplayAckNamesNoPlace(t *testing.T) {
	eng := &gatedEngine{started: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, eng, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}

	go postChatJSON(t, srv, "u@x.com", map[string]any{"message": "long turn", "conversation_id": conv.ID})
	<-eng.started
	defer func() { eng.release <- struct{}{}; eng.release <- struct{}{} }()

	body := map[string]any{"message": "queued", "conversation_id": conv.ID, "input_id": "q-1"}
	w := postChatJSON(t, srv, "u@x.com", body)
	if in := decodeQueueAck(t, w.Body.Bytes()); w.Code != http.StatusAccepted || in["ahead"] != float64(0) {
		t.Fatalf("first ack %d %s, want 202 next in line", w.Code, w.Body.String())
	}
	// A Stop by key stamped the row, and its withdrawal did not happen.
	if err := st.MarkInputStopRequested(t.Context(), conv.ID, "q-1"); err != nil {
		t.Fatal(err)
	}
	w = postChatJSON(t, srv, "u@x.com", body)
	in := decodeQueueAck(t, w.Body.Bytes())
	if w.Code != http.StatusOK || in["state"] != store.InputStateQueued {
		t.Fatalf("replay ack %d %s, want 200 for the still-queued row", w.Code, w.Body.String())
	}
	if _, ok := in["ahead"]; ok {
		t.Fatalf("replay ack = %s, want no ahead for a stamped row", w.Body.String())
	}
}
