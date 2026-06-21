package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// eventSinkPersister is the subset of *store.Store the buffer needs
// for incremental event persistence. Allows tests to stub it out.
type eventSinkPersister interface {
	CreateTurn(ctx context.Context, turnID, convID string, startedAt int64) error
	InsertTurnEvents(ctx context.Context, events []store.TurnEvent) error
	FinishTurn(ctx context.Context, turnID string, status store.TurnStatus, finishedAt int64) error
}

// turnBuffer is the single source of truth for a turn's event stream.
//
// It implements agent.EventSink so the agent (or runMockTurn) writes
// through it, and exposes Attach so any number of HTTP responses can
// subscribe — the initial POST /chat connection plus any later
// GET /conversations/{id}/stream reattach. Events carry a monotonic
// per-turn id (starting at 1) which the client echoes back via
// Last-Event-ID when reconnecting.
//
// Lifecycle: Emit is callable until Finish seals the buffer; after
// that, Emit is a no-op and all live subscribers are closed with EOF.
// The buffer is retained in Server.inflight for bufferRetainTTL after
// Finish so a client that returns within that window still sees the
// complete replay. Events are also streamed out to a persister
// goroutine that batch-writes them to Postgres, so a crash mid-turn
// can still be recovered by reading turn_events.
type turnBuffer struct {
	convID string
	turnID string

	mu          sync.Mutex
	events      []bufferedEvent
	subscribers map[uint64]chan bufferedEvent
	nextSubID   uint64
	closed      bool
	finishedAt  time.Time

	// Persister plumbing. persistCh is nil when no persister is
	// configured (tests with no store). The goroutine drains this
	// channel + a periodic tick; done signals it to flush and exit
	// on buffer Finish.
	persister eventSinkPersister
	persistCh chan bufferedEvent
	persistWG sync.WaitGroup
}

// bufferedEvent is one already-serialized SSE frame. Data is the
// marshalled JSON payload, not the raw value, so fan-out doesn't
// reserialize for every subscriber.
type bufferedEvent struct {
	ID   uint64
	Name string
	Data []byte
}

func newTurnBuffer(convID, turnID string) *turnBuffer {
	return &turnBuffer{
		convID:      convID,
		turnID:      turnID,
		subscribers: make(map[uint64]chan bufferedEvent),
	}
}

// attachPersister wires a persister goroutine that batches + flushes
// events to Postgres. Must be called BEFORE the first Emit so the
// CreateTurn row exists before any turn_events row tries to FK into
// it. No-op if persister is nil (mock-mode tests).
func (b *turnBuffer) attachPersister(ctx context.Context, p eventSinkPersister) error {
	if p == nil {
		return nil
	}
	b.persister = p
	if err := p.CreateTurn(ctx, b.turnID, b.convID, time.Now().Unix()); err != nil {
		return fmt.Errorf("CreateTurn: %w", err)
	}
	// Buffered channel so Emit never blocks on DB latency. If the
	// goroutine falls far enough behind that we drop events here,
	// the in-memory buffer still has them — crash recovery becomes
	// incomplete, but live replay stays correct.
	b.persistCh = make(chan bufferedEvent, 512)
	b.persistWG.Add(1)
	go b.runPersister()
	return nil
}

// runPersister drains persistCh, batching writes every flushInterval
// or flushBatchSize events (whichever comes first). Exits when
// persistCh is closed.
func (b *turnBuffer) runPersister() {
	defer b.persistWG.Done()

	const (
		flushInterval  = 50 * time.Millisecond
		flushBatchSize = 64
	)

	pending := make([]bufferedEvent, 0, flushBatchSize)
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()

	flush := func() {
		if len(pending) == 0 {
			return
		}
		toStore := make([]store.TurnEvent, len(pending))
		now := time.Now().Unix()
		for i, ev := range pending {
			toStore[i] = store.TurnEvent{
				TurnID:    b.turnID,
				EventID:   ev.ID,
				Name:      ev.Name,
				Data:      ev.Data,
				CreatedAt: now,
			}
		}
		// 5s budget — Postgres + indexes; plenty. A timeout here
		// means something's wrong with the DB; we log and drop so
		// the turn can still finish.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := b.persister.InsertTurnEvents(ctx, toStore); err != nil {
			log.Printf("persist turn_events (turn=%s n=%d): %v", b.turnID, len(toStore), err)
		}
		cancel()
		pending = pending[:0]
	}

	for {
		select {
		case ev, ok := <-b.persistCh:
			if !ok {
				flush()
				return
			}
			pending = append(pending, ev)
			if len(pending) >= flushBatchSize {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// Emit implements agent.EventSink. Assigns a monotonic id, appends to
// the log, and non-blocking-sends to every live subscriber. Subscribers
// whose channel is full are evicted — they can reattach and replay
// from their Last-Event-ID. Must NOT block; fantasy's streaming
// callbacks hold other locks behind us.
func (b *turnBuffer) Emit(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"marshal_error":%q}`, err.Error()))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	id := uint64(len(b.events)) + 1
	ev := bufferedEvent{ID: id, Name: event, Data: data}
	b.events = append(b.events, ev)

	for subID, ch := range b.subscribers {
		select {
		case ch <- ev:
		default:
			// Slow subscriber — evict. They can reattach with
			// Last-Event-ID and pick up where they dropped.
			close(ch)
			delete(b.subscribers, subID)
		}
	}

	// Persistence fan-out. Non-blocking so DB slowness never stalls
	// live streaming. If the persister channel is full, drop the
	// event from the persistent log (in-memory still has it); log
	// once per buffer via the count so the operator notices.
	if b.persistCh != nil {
		select {
		case b.persistCh <- ev:
		default:
			log.Printf("persister channel full (turn=%s); dropping event id=%d", b.turnID, ev.ID)
		}
	}
}

// Finish seals the buffer. Emit becomes a no-op, all subscriber
// channels are closed so their Attach goroutines see EOF, and the
// persister goroutine is told to flush + exit. Safe to call twice;
// subsequent calls are no-ops.
//
// The terminal status (`completed` / `cancelled` / `error`) is
// inferred from the last terminal event in the log so the caller
// doesn't have to pass it in — it's already there in the stream.
func (b *turnBuffer) Finish() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.finishedAt = time.Now()
	status := inferTerminalStatus(b.events)
	for subID, ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, subID)
	}
	// Close (don't nil) persistCh so the persister goroutine sees a
	// closed-channel signal on its next receive and exits cleanly.
	// Nil-ing it would cause the goroutine's next iteration to block
	// on `<-nil` forever (only the tick case would fire, flushing
	// empty batches until the test timeout).
	persistCh := b.persistCh
	b.mu.Unlock()

	if persistCh != nil {
		close(persistCh)
		b.persistWG.Wait()
	}
	if b.persister != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := b.persister.FinishTurn(ctx, b.turnID, status, b.finishedAt.Unix()); err != nil {
			log.Printf("persist FinishTurn (turn=%s): %v", b.turnID, err)
		}
		cancel()
	}
}

// inferTerminalStatus scans the event log for a terminal marker and
// returns the matching status. Defaults to `completed` if the buffer
// ended cleanly without an explicit signal — that matches the old
// POST-returns-normally semantics.
func inferTerminalStatus(events []bufferedEvent) store.TurnStatus {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Name {
		case "turn.completed":
			return store.TurnStatusCompleted
		case "turn.cancelled":
			return store.TurnStatusCancelled
		case "turn.error":
			return store.TurnStatusError
		}
	}
	return store.TurnStatusCompleted
}

// Attach writes every event with ID > lastEventID to w in SSE framing,
// then streams any subsequent events as they arrive. Returns when the
// buffer is sealed and fully drained, or when ctx is cancelled (client
// disconnect). Must be called BEFORE any other write to w — it sets
// the SSE headers + status line itself.
func (b *turnBuffer) Attach(ctx context.Context, lastEventID uint64, w http.ResponseWriter) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Atomically grab the replay slice and register a live subscription.
	// Registering under the same lock the publisher uses guarantees no
	// event is both in the replay slice AND the live channel — nor
	// missed between them.
	b.mu.Lock()
	var replay []bufferedEvent
	for _, e := range b.events {
		if e.ID > lastEventID {
			replay = append(replay, e)
		}
	}
	var ch chan bufferedEvent
	var subID uint64
	if !b.closed {
		b.nextSubID++
		subID = b.nextSubID
		ch = make(chan bufferedEvent, 256)
		b.subscribers[subID] = ch
	}
	b.mu.Unlock()

	// Pump replay.
	for _, e := range replay {
		if err := writeSSEFrame(w, flusher, e); err != nil {
			b.unsubscribe(subID)
			return err
		}
	}

	// If buffer was already sealed when we attached, no live channel;
	// replay is all there is.
	if ch == nil {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			b.unsubscribe(subID)
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				// Buffer sealed — our subscription was closed by Finish.
				return nil
			}
			if err := writeSSEFrame(w, flusher, ev); err != nil {
				b.unsubscribe(subID)
				return err
			}
		}
	}
}

// HighestID returns the id of the most recently emitted event, or 0
// when the buffer is empty. Used by the /inflight probe to hint at a
// reasonable Last-Event-ID baseline for freshly-reconnecting clients.
func (b *turnBuffer) HighestID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) == 0 {
		return 0
	}
	return b.events[len(b.events)-1].ID
}

func (b *turnBuffer) unsubscribe(id uint64) {
	if id == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
}

// writeSSEFrame formats one event as an SSE frame and flushes. Any
// write error (client disconnect) propagates so Attach can unsubscribe.
func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, e bufferedEvent) error {
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Name, string(e.Data)); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
