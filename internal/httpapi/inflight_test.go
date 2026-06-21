package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInflightRegistry covers the cancel-on-replace + token-scoped
// finish semantics of registerTurn / finishTurn. Each call returns a
// fresh buffer plus a monotonic token the finish call must present.
func TestInflightRegistry_CancelOnReplace(t *testing.T) {
	s := serverFixture(t)

	// Register a turn for conv "A".
	ctx1, cancel1 := context.WithCancel(context.Background())
	buf1, _, tok1 := s.registerTurn("A", cancel1)

	// Replace it with a second turn for the same conv. The first one
	// should be cancelled by the act of replacement, and its buffer
	// sealed so any subscriber sees EOF.
	ctx2, cancel2 := context.WithCancel(context.Background())
	buf2, _, tok2 := s.registerTurn("A", cancel2)
	defer cancel2()

	if ctx1.Err() == nil {
		t.Error("first turn was not cancelled when replaced")
	}
	if ctx2.Err() != nil {
		t.Error("replacement turn was cancelled prematurely")
	}
	if tok1 == tok2 {
		t.Error("replacement should have a fresh token, got equal tokens")
	}
	if buf1 == buf2 {
		t.Error("replacement should have a fresh buffer")
	}
	// Emit on the old buffer must be a no-op.
	before := buf1.HighestID()
	buf1.Emit("should-drop", nil)
	if buf1.HighestID() != before {
		t.Errorf("old buffer accepted post-finish emit")
	}
}

func TestInflightRegistry_FinishScopedByToken(t *testing.T) {
	s := serverFixture(t)

	_, cancel1 := context.WithCancel(context.Background())
	_, _, tok1 := s.registerTurn("A", cancel1)

	// Replace before the original handler finished.
	_, cancel2 := context.WithCancel(context.Background())
	_, _, _ = s.registerTurn("A", cancel2)

	// Now the original turn's deferred finishTurn runs with the stale
	// token. It MUST NOT mutate the replacement entry.
	s.finishTurn("A", tok1)

	s.inflightMu.Lock()
	entry, present := s.inflight["A"]
	s.inflightMu.Unlock()
	if !present {
		t.Fatal("replacement entry was clobbered by stale finishTurn")
	}
	if !entry.IsRunning() {
		t.Error("replacement entry was marked finished by stale finishTurn")
	}

	cancel1()
	cancel2()
}

func TestInflightRegistry_CancelInflight(t *testing.T) {
	s := serverFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	_, _, tok := s.registerTurn("A", cancel)
	defer s.finishTurn("A", tok)

	if !s.cancelInflight("A") {
		t.Fatal("cancelInflight returned false for live entry")
	}
	if ctx.Err() == nil {
		t.Error("turn context was not cancelled")
	}
	if s.cancelInflight("nonexistent") {
		t.Error("cancelInflight returned true for missing entry")
	}
}

// A finished buffer is retained in the map but cancelInflight should
// report it as no-op (nothing to stop).
func TestInflightRegistry_CancelAfterFinishIsNoOp(t *testing.T) {
	s := serverFixture(t)

	_, cancel := context.WithCancel(context.Background())
	_, _, tok := s.registerTurn("A", cancel)
	s.finishTurn("A", tok)

	if s.cancelInflight("A") {
		t.Error("cancelInflight returned true for already-finished turn")
	}
	// Retained entry should still be in the map for replay.
	entry, ok := s.getInflight("A")
	if !ok {
		t.Fatal("finished entry was evicted immediately; expected retention")
	}
	if entry.IsRunning() {
		t.Error("retained entry reports still-running")
	}

	// Manually evict so the TTL timer doesn't leak across tests.
	s.inflightMu.Lock()
	delete(s.inflight, "A")
	s.inflightMu.Unlock()
}

// TestCancelEndpoint_OwnerScoped confirms POST /conversations/{id}/cancel
// only cancels turns owned by the calling user, and returns 404 for a
// conversation owned by someone else (so a token leak can't be used to
// kill another tenant's work).
func TestCancelEndpoint_OwnerScoped(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Register an in-flight turn under that conv.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, tok := s.registerTurn(conv.ID, cancel)
	defer s.finishTurn(conv.ID, tok)

	h := s.Routes()

	// Bob (not the owner) cannot cancel — handler must return 404.
	rr := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/cancel", nil, "bob@x.com")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-owner cancel: status %d, want 404", rr.Code)
	}
	if ctx.Err() != nil {
		t.Error("non-owner cancel still cancelled the turn")
	}

	// Alice (owner) succeeds with 204 and the turn is cancelled.
	rr = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/cancel", nil, "alice@x.com")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("owner cancel: status %d body=%q, want 204", rr.Code, rr.Body.String())
	}
	if ctx.Err() == nil {
		t.Error("owner cancel did not cancel the turn")
	}
}

func TestCancelEndpoint_NoInflightStillReturns204(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// No registerTurn call — endpoint should still 204 (idempotent).
	h := s.Routes()
	rr := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/cancel", nil, "alice@x.com")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rr.Code)
	}
}

// /inflight returns a JSON probe the client uses to decide whether to
// open a reattach stream.
func TestInflightEndpoint_ReportsStatus(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	h := s.Routes()

	// No in-flight turn yet.
	rr := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/inflight", nil, "alice@x.com")
	if rr.Code != http.StatusOK {
		t.Fatalf("no-turn inflight: status %d body=%s", rr.Code, rr.Body.String())
	}
	var probe map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if probe["inflight"] != false {
		t.Errorf("expected inflight=false, got %v", probe)
	}

	// Register a turn and prime the buffer like postChat does.
	_, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	buf, turnID, tok := s.registerTurn(conv.ID, turnCancel)
	defer s.finishTurn(conv.ID, tok)
	buf.Emit("conversation", map[string]any{"id": conv.ID})
	buf.Emit("turn.started", map[string]any{"turn_id": turnID})

	rr = do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/inflight", nil, "alice@x.com")
	if rr.Code != http.StatusOK {
		t.Fatalf("running inflight: status %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if probe["inflight"] != true {
		t.Errorf("expected inflight=true, got %v", probe)
	}
	if probe["turn_id"] != turnID {
		t.Errorf("expected turn_id=%q, got %v", turnID, probe["turn_id"])
	}
	// JSON numbers decode as float64.
	if lid, _ := probe["last_event_id"].(float64); lid != 2 {
		t.Errorf("expected last_event_id=2 (2 primed events), got %v", probe["last_event_id"])
	}
}

// Cross-user probes are 404. Prevents a token leak probing other tenants.
func TestInflightEndpoint_OwnerScoped(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	h := s.Routes()
	rr := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/inflight", nil, "eve@x.com")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user inflight: status %d, want 404", rr.Code)
	}
}

// Stream reattach with Last-Event-ID should only replay events AFTER
// the given id.
func TestStreamEndpoint_ReplaysFromLastEventID(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Register a turn, prime some events, then finish it so Attach
	// returns promptly without a live channel.
	_, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	buf, turnID, tok := s.registerTurn(conv.ID, turnCancel)
	for i := 1; i <= 4; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	s.finishTurn(conv.ID, tok)

	// Simulate a reconnect that already saw events 1-2.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/conversations/"+conv.ID+"/stream?turn_id="+turnID, bytes.NewReader(nil))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "alice@x.com")
	req.Header.Set("Last-Event-ID", "2")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stream: status %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") {
		t.Errorf("replay included acknowledged events: %s", body)
	}
	if !strings.Contains(body, "id: 3\n") || !strings.Contains(body, "id: 4\n") {
		t.Errorf("replay missing unacknowledged events: %s", body)
	}

	// Cleanup retained entry so the TTL timer doesn't leak.
	s.inflightMu.Lock()
	delete(s.inflight, conv.ID)
	s.inflightMu.Unlock()
}
