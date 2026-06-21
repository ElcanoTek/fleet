package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// Events emitted through a buffer with a persister attached should end
// up in the turn_events table once the goroutine flushes, and the
// `turns` row should be marked completed after Finish.
func TestPersister_EventsPersistThenFinish(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok := s.registerTurn(conv.ID, cancel)

	ctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cc()
	if err := buf.attachPersister(ctx, s.store); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}

	buf.Emit("a", map[string]any{"n": 1})
	buf.Emit("b", map[string]any{"n": 2})
	buf.Emit("turn.completed", map[string]any{})

	s.finishTurn(conv.ID, tok)

	// After Finish returns, the persister goroutine has drained and
	// FinishTurn has committed. No sleep needed.
	events, err := s.store.LoadTurnEvents(t.Context(), turnID, 0)
	if err != nil {
		t.Fatalf("LoadTurnEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("persisted %d events, want 3", len(events))
	}
	for i, want := range []string{"a", "b", "turn.completed"} {
		if events[i].Name != want {
			t.Errorf("event[%d].Name = %q, want %q", i, events[i].Name, want)
		}
		if events[i].EventID != uint64(i+1) {
			t.Errorf("event[%d].EventID = %d, want %d", i, events[i].EventID, i+1)
		}
	}

	rec, err := s.store.LookupTurn(t.Context(), turnID)
	if err != nil {
		t.Fatalf("LookupTurn: %v", err)
	}
	if rec == nil || rec.Status != "completed" {
		t.Errorf("status = %+v, want completed", rec)
	}

	// Evict the retained buffer so the TTL timer doesn't leak.
	s.inflightMu.Lock()
	delete(s.inflight, conv.ID)
	s.inflightMu.Unlock()
}

// When the in-memory buffer has been evicted, /stream falls back to
// reading turn_events from Postgres. The client supplies turn_id so
// the handler knows which row to read.
func TestStreamEndpoint_DBFallback(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Register, persist, and finish a turn. Then evict it from the
	// inflight map so /stream has to go to the DB.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok := s.registerTurn(conv.ID, cancel)
	ctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cc()
	if err := buf.attachPersister(ctx, s.store); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}
	for i := 1; i <= 3; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Emit("turn.completed", map[string]any{})
	s.finishTurn(conv.ID, tok)

	// Force the fallback path: wipe the retained entry.
	s.inflightMu.Lock()
	delete(s.inflight, conv.ID)
	s.inflightMu.Unlock()

	// Replay from the DB. Last-Event-ID:1 → expect events 2+ only.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/conversations/"+conv.ID+"/stream?turn_id="+turnID, bytes.NewReader(nil))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "alice@x.com")
	req.Header.Set("Last-Event-ID", "1")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stream: status %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "id: 1\n") {
		t.Errorf("replay included acknowledged event: %s", body)
	}
	for _, want := range []string{"id: 2\n", "id: 3\n", "id: 4\n", "event: turn.completed\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("DB replay missing %q:\n%s", want, body)
		}
	}
}

// A turn that's still 'running' on startup gets marked errored and
// given a synthetic terminal event. Simulates crash recovery.
func TestCrashRecovery_MarksRunningTurnsErrored(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Create a turn and a partial event log — as if the previous
	// process died mid-flight.
	turnID := "stranded-turn-1"
	if err := s.store.CreateTurn(t.Context(), turnID, conv.ID, time.Now().Unix()); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if err := s.store.InsertTurnEvents(t.Context(), []store.TurnEvent{
		{TurnID: turnID, EventID: 1, Name: "turn.started", Data: []byte(`{}`), CreatedAt: time.Now().Unix()},
		{TurnID: turnID, EventID: 2, Name: "text.delta", Data: []byte(`{"text":"partial"}`), CreatedAt: time.Now().Unix()},
	}); err != nil {
		t.Fatalf("InsertTurnEvents: %v", err)
	}

	stranded, err := s.store.MarkRunningTurnsErrored(t.Context())
	if err != nil {
		t.Fatalf("MarkRunningTurnsErrored: %v", err)
	}
	if len(stranded) != 1 || stranded[0] != turnID {
		t.Errorf("stranded = %v, want [%s]", stranded, turnID)
	}

	rec, _ := s.store.LookupTurn(t.Context(), turnID)
	if rec == nil || rec.Status != "error" {
		t.Errorf("status = %+v, want error", rec)
	}

	events, _ := s.store.LoadTurnEvents(t.Context(), turnID, 0)
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3 (2 original + 1 synthetic)", len(events))
	}
	if events[2].Name != "turn.error" {
		t.Errorf("synthetic event name = %q, want turn.error", events[2].Name)
	}
}
