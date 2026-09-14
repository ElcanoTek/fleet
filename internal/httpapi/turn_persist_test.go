package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// gatedPersister wraps the real *store.Store and parks every InsertTurnEvents
// until the test opens the gate, forcing the bounded persistCh to saturate so
// Emit drops events on the live path — the exact slow-Postgres condition issue
// #32 guards.
//
// The gate is a barrier, NOT a sleep. An earlier version stalled each insert
// for a fixed 40ms and hoped the producer outran it; that made the test a race
// between two clocks, and it lost on a loaded CI runner (the backfill's own 5s
// per-chunk budget expired, the turn came back lossy, and the -race lane went
// red on a diff that touched no Go code). Blocking until released makes the
// drops a structural certainty at any speed: while a flush is parked the
// persister can absorb at most flushBatchSize + persistChanDepth events, so
// emitting more than that sum always overflows.
//
// failInserts, when set, also makes every InsertTurnEvents FAIL, so even the
// Finish backfill cannot heal the gap (the genuinely-lossy case).
type gatedPersister struct {
	*store.Store
	// gate blocks every insert until closed. Nil means "never block".
	gate        chan struct{}
	failInserts bool
}

func (p *gatedPersister) InsertTurnEvents(ctx context.Context, events []store.TurnEvent) error {
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.failInserts {
		return errors.New("simulated turn_events insert failure")
	}
	return p.Store.InsertTurnEvents(ctx, events)
}

// TestPersister_BackfillHealsDropsUnderBackpressure: when a stalled persister
// saturates the persist channel and the live path drops events, Finish re-sends
// the full in-memory snapshot so the persisted ledger ends up gapless (no
// permanently-dropped events) and the turn is NOT flagged lossy — the heal
// succeeded. Core acceptance for issue #32.
func TestPersister_BackfillHealsDropsUnderBackpressure(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf, turnID, tok, _ := s.registerTurn(conv.ID, cancel)

	gate := make(chan struct{})
	stalled := &gatedPersister{Store: s.concreteStore(t), gate: gate}
	// t.Context(): attachPersister uses ctx only for the CreateTurn round-trip
	// and does not retain it, so the test's own lifetime is the honest bound —
	// no invented timeout to outgrow.
	if err := buf.attachPersister(t.Context(), stalled); err != nil {
		t.Fatalf("attachPersister: %v", err)
	}

	// With the persister parked on the gate it can hold at most flushBatchSize
	// events in its pending batch plus persistChanDepth queued behind them, so
	// emitting more than that sum drops the remainder on the live path no matter
	// how the two goroutines interleave. n also spans several backfill chunks.
	const n = 2000
	if n <= persistChanDepth+flushBatchSize {
		t.Fatalf("n = %d must exceed persistChanDepth+flushBatchSize (%d) to guarantee drops",
			n, persistChanDepth+flushBatchSize)
	}
	for i := 1; i < n; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Emit("turn.completed", map[string]any{}) // event #n; terminal marker

	// Pin the precondition that makes the heal assertions meaningful: the live
	// path really did drop. Without this the test would quietly decay into
	// "persist 2000 events happily" if the buffering ever grew enough to absorb
	// them, and would still pass while testing nothing.
	buf.mu.Lock()
	dropped := buf.needsBackfill
	buf.mu.Unlock()
	if !dropped {
		t.Fatal("no event was dropped on the live path: backpressure never happened, so the backfill below has nothing to heal")
	}

	// Every drop is now recorded (Emit flags needsBackfill synchronously), so the
	// gate can open: Finish waits on the persister goroutine, which is parked on
	// it, and the backfill then runs at full speed against an unencumbered DB.
	close(gate)

	s.finishTurn(conv.ID, tok)

	// The persisted ledger must be the COMPLETE, gapless 1..n — backfill healed
	// every drop that the assertion above proved happened.
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
	buf, turnID, tok, _ := s.registerTurn(conv.ID, cancel)

	// Every InsertTurnEvents fails: the live flush fails (→ needsBackfill) AND the
	// Finish backfill fails (→ lossy). FinishTurn itself is a different statement
	// (delegated to the real store), so the turn still seals. No gate here — this
	// case needs failures, not backpressure, so nothing blocks.
	failing := &gatedPersister{Store: s.concreteStore(t), failInserts: true}
	if err := buf.attachPersister(t.Context(), failing); err != nil {
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
	buf, turnID, tok, _ := s.registerTurn(conv.ID, cancel)

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
	buf, turnID, tok, _ := s.registerTurn(conv.ID, cancel)
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
	// The reconnect is classified as buffer_expired (not db_fallback): the turn
	// had already finished, so the DB holds the complete event log including the
	// terminal frame. The distinction drives the UI's inline "turn completed"
	// notice, so pin it rather than just the replayed bytes.
	if counts := s.SSEReconnectCounts(); counts["buffer_expired"] != 1 {
		t.Errorf("reconnect outcomes = %v, want buffer_expired=1", counts)
	}
}

// A reconnect that names no turn_id with nothing buffered has nothing to serve:
// the handler 204s and tallies the outcome as no_content. Together with the
// buffer_expired assertion above this pins the classification the reconnect
// counter reports — it is written on every /stream reattach, so a mislabelled
// outcome would otherwise go unnoticed.
func TestStreamEndpoint_NoContentReconnectIsTallied(t *testing.T) {
	s := serverFixture(t)
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/conversations/"+conv.ID+"/stream", bytes.NewReader(nil))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "alice@x.com")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("stream: status %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if counts := s.SSEReconnectCounts(); counts["no_content"] != 1 {
		t.Errorf("reconnect outcomes = %v, want no_content=1", counts)
	}
}

// The post-turn retention sweep prunes the durable turn ledger: a turn
// terminal for longer than FLEET_TURN_EVENT_RETENTION_DAYS loses its turns
// row and, via cascade, its turn_events — while a fresh turn survives. This
// exercises the same sweepRetention call both turn paths run, so the ledger
// can never silently return to unbounded growth.
func TestSweepRetention_PrunesAgedTurnLedger(t *testing.T) {
	s := serverFixture(t)
	s.cfg.TurnEventRetentionDays = 14
	conv, err := s.store.CreateConversation(t.Context(), "alice@x.com", "hi", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	mkTurn := func(turnID string, finishedAt int64) {
		t.Helper()
		if err := s.store.CreateTurn(t.Context(), turnID, conv.ID, finishedAt-1); err != nil {
			t.Fatalf("CreateTurn: %v", err)
		}
		if err := s.store.InsertTurnEvents(t.Context(), []store.TurnEvent{
			{TurnID: turnID, EventID: 1, Name: "turn.started", Data: []byte(`{}`), CreatedAt: finishedAt - 1},
		}); err != nil {
			t.Fatalf("InsertTurnEvents: %v", err)
		}
		if err := s.store.FinishTurn(t.Context(), turnID, store.TurnStatusCompleted, finishedAt, false); err != nil {
			t.Fatalf("FinishTurn: %v", err)
		}
	}
	mkTurn("aged-turn", time.Now().AddDate(0, 0, -15).Unix())
	mkTurn("fresh-turn", time.Now().Unix())

	s.sweepRetention(t.Context())

	if rec, _ := s.store.LookupTurn(t.Context(), "aged-turn"); rec != nil {
		t.Errorf("aged turn survived the retention sweep: %+v", rec)
	}
	if events, _ := s.store.LoadTurnEvents(t.Context(), "aged-turn", 0); len(events) != 0 {
		t.Errorf("aged turn's events survived the retention sweep: %d", len(events))
	}
	if rec, _ := s.store.LookupTurn(t.Context(), "fresh-turn"); rec == nil {
		t.Error("fresh turn was swept; retention must only reclaim aged-out turns")
	}
}
