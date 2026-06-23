package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// slowPersister wraps the real *store.Store but stalls every InsertTurnEvents,
// forcing the bounded persistCh to saturate so Emit drops events on the live
// path — the exact slow-Postgres condition issue #32 guards. failInserts, when
// set, also makes every InsertTurnEvents FAIL, so even the Finish backfill cannot
// heal the gap (the genuinely-lossy case).
type slowPersister struct {
	*store.Store
	delay       time.Duration
	failInserts bool

	mu          sync.Mutex
	insertCalls int
}

func (p *slowPersister) InsertTurnEvents(ctx context.Context, events []store.TurnEvent) error {
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	p.mu.Lock()
	p.insertCalls++
	p.mu.Unlock()
	if p.failInserts {
		return errors.New("simulated turn_events insert failure")
	}
	return p.Store.InsertTurnEvents(ctx, events)
}

// TestPersister_BackfillHealsDropsUnderLatency: under DB latency that saturates
// the persist channel, Finish re-sends the full in-memory snapshot so the
// persisted ledger ends up gapless (no permanently-dropped events) and the turn
// is NOT flagged lossy — the heal succeeded. Core acceptance for issue #32.
func TestPersister_BackfillHealsDropsUnderLatency(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok := s.registerTurn(conv.ID, cancel)

	ctx, cc := context.WithTimeout(context.Background(), 30*time.Second)
	defer cc()
	slow := &slowPersister{Store: s.concreteStore(t), delay: 40 * time.Millisecond}
	if err := buf.attachPersister(ctx, slow); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}

	// Emit far more than the 512-deep persistCh in a tight loop while the persister
	// is stalled, so the channel saturates and the live path drops events.
	const n = 2000
	for i := 1; i < n; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Emit("turn.completed", map[string]any{}) // event #n; terminal marker

	s.finishTurn(conv.ID, tok)

	// The persisted ledger must be the COMPLETE, gapless 1..n — backfill healed
	// every drop. (If the live path never dropped, this still holds; the assertion
	// is on completeness, and slowPersister guarantees drops happened.)
	events, err := s.store.LoadTurnEvents(t.Context(), turnID, 0)
	if err != nil {
		t.Fatalf("LoadTurnEvents: %v", err)
	}
	if len(events) != n {
		t.Fatalf("persisted %d events, want %d (a gap means a turn_event was permanently lost)", len(events), n)
	}
	for i, e := range events {
		if e.EventID != uint64(i+1) {
			t.Fatalf("event[%d].EventID = %d, want %d (non-contiguous → lost event)", i, e.EventID, i+1)
		}
	}
	rec, err := s.store.LookupTurn(t.Context(), turnID)
	if err != nil {
		t.Fatalf("LookupTurn: %v", err)
	}
	if rec == nil || rec.Status != "completed" {
		t.Fatalf("status = %+v, want completed", rec)
	}
	if rec.Lossy {
		t.Errorf("turn flagged lossy=true, but the backfill healed every drop")
	}

	s.inflightMu.Lock()
	delete(s.inflight, conv.ID)
	s.inflightMu.Unlock()
}

// TestPersister_LossyWhenBackfillFails: when events are dropped AND the Finish
// backfill itself cannot persist them, the turn still reaches a terminal status
// but is flagged lossy=true — an honest "the persisted history is incomplete"
// signal instead of a silent gap. Issue #32 acceptance (the unhealable case).
func TestPersister_LossyWhenBackfillFails(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok := s.registerTurn(conv.ID, cancel)

	ctx, cc := context.WithTimeout(context.Background(), 30*time.Second)
	defer cc()
	// Every InsertTurnEvents fails: the live flush fails (→ needsBackfill) AND the
	// Finish backfill fails (→ lossy). FinishTurn itself is a different statement
	// (delegated to the real store), so the turn still seals.
	failing := &slowPersister{Store: s.concreteStore(t), failInserts: true}
	if err := buf.attachPersister(ctx, failing); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}

	for i := 1; i <= 5; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Emit("turn.completed", map[string]any{})

	s.finishTurn(conv.ID, tok)

	rec, err := s.store.LookupTurn(t.Context(), turnID)
	if err != nil {
		t.Fatalf("LookupTurn: %v", err)
	}
	if rec == nil {
		t.Fatal("turn row missing")
	}
	if rec.Status != "completed" {
		t.Errorf("status = %q, want completed (a lossy turn still seals)", rec.Status)
	}
	if !rec.Lossy {
		t.Error("turn must be flagged lossy=true when its events could not be persisted")
	}

	s.inflightMu.Lock()
	delete(s.inflight, conv.ID)
	s.inflightMu.Unlock()
}

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
	// A clean turn (no drops) is not flagged lossy and needs no backfill.
	if rec != nil && rec.Lossy {
		t.Errorf("clean turn marked lossy=true; want false")
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

	stranded, err := s.concreteStore(t).MarkRunningTurnsErrored(t.Context())
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
