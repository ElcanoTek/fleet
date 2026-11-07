package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
)

// eventSinkPersister is the subset of *store.Store the buffer needs
// for incremental event persistence. Allows tests to stub it out.
type eventSinkPersister interface {
	CreateTurn(ctx context.Context, turnID, convID string, startedAt int64) error
	InsertTurnEvents(ctx context.Context, events []store.TurnEvent) error
	FinishTurn(ctx context.Context, turnID string, status store.TurnStatus, finishedAt int64, lossy bool) error
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
	convID    string
	turnID    string
	inputKind string

	// userCommitStore, when set before the turn starts, is the store the turn
	// commits its history through instead of the server's (a webhook turn's
	// webhookCommitStore). Read by runTurnAsync via Server.turnStore.
	userCommitStore chatStore

	// stoppedByKey records that a Stop naming this turn's input key reached
	// it, confirmed or not: the turn's settlement then cancels its drained
	// row, unless its user entry committed, instead of returning it to the
	// queue — a late terminal frame must not let a drain re-run the input
	// the Stop was for.
	stoppedByKey atomic.Bool
	// stoppedSteers lists the injected steer rows a Stop by key named while
	// this turn carried them (guarded by mu): the settlement cancels them
	// rather than return an uncommitted one to the queue.
	stoppedSteers []string

	mu          sync.Mutex
	events      []bufferedEvent
	subscribers map[uint64]chan bufferedEvent
	// evictedSubs marks subscriptions closed for falling behind (Emit /
	// broadcastControl found their channel full), as opposed to closed by
	// Finish. The reader consumes the mark to end the stream with an explicit
	// "evicted" frame — without it a congested client saw the same clean EOF
	// a finished turn produces and rendered a still-generating turn as done.
	evictedSubs map[uint64]bool
	revokedSubs map[uint64]bool
	subViewers  map[uint64]string
	nextSubID   uint64
	closed      bool
	finishedAt  time.Time

	// maxBytes caps the cumulative size of buffered event Data (0 = unlimited).
	// When exceeded, the oldest events are evicted from the front (sliding
	// window); totalBytes tracks the running sum so eviction is O(evicted).
	maxBytes   int
	totalBytes int

	// Persister plumbing. persistCh is nil when no persister is
	// configured (tests with no store). The goroutine drains this
	// channel + a periodic tick; done signals it to flush and exit
	// on buffer Finish.
	persister eventSinkPersister
	persistCh chan bufferedEvent
	persistWG sync.WaitGroup
	// needsBackfill is set (under mu) whenever an event could not be
	// confirmed into turn_events on the live path — either persistCh was
	// full when Emit tried to enqueue it, or a runPersister batch insert
	// failed. Finish reads it to decide whether to re-send the full event
	// snapshot before sealing the turn, so a saturation/latency blip never
	// leaves a permanent gap in the persisted ledger.
	needsBackfill bool
	// droppedOnFull counts the events Emit could not hand to the persister
	// because persistCh was full. needsBackfill deliberately does NOT
	// distinguish its two causes — it only has to answer "re-send the
	// snapshot?" — so this isolates genuine backpressure from a failed batch
	// insert. Written under mu on the same line that sets needsBackfill.
	droppedOnFull int

	// toolDurations records each tool.result's duration_ms by call id (guarded
	// by mu). Kept apart from events because the sliding window can evict
	// early frames, and the tool-call audit ledger needs every call's timing.
	toolDurations map[string]int64

	// compactions counts the turn's fleet.context_compacted frames (guarded by
	// mu), an end-of-turn struggle signal (docs/CHAT-ESCALATION.md). Counted
	// on Emit for the same reason as toolDurations: the window can evict them.
	compactions int
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
		evictedSubs: make(map[uint64]bool),
		revokedSubs: make(map[uint64]bool),
		subViewers:  make(map[uint64]string),
		maxBytes:    sseMaxBytesPerTurn,
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
	if err := p.CreateTurn(ctx, b.turnID, b.convID, time.Now().Unix()); err != nil {
		return fmt.Errorf("CreateTurn: %w", err)
	}
	// Wire the persister only once the turn row exists. Assigning it before
	// CreateTurn succeeded left a failed attach with a live persister, and
	// Finish then called FinishTurn (and the backfill) against a turn row that
	// was never inserted — a spurious error for a turn the caller had already
	// abandoned at the CreateTurn failure.
	//
	// Under b.mu: Emit reads persistCh under the lock, and Emit CAN run before
	// attach finishes — the buffer is registered in Server.inflight before
	// startTurn attaches the persister, and the previous turn's completion
	// goroutine publishes queue.updated into whatever buffer is live for the
	// conversation (emitQueueUpdate). The race detector caught exactly that
	// interleaving in TestChatSecondTurnReplaysHistory.
	//
	// Buffered channel so Emit never blocks on DB latency. If the
	// goroutine falls far enough behind that Emit drops events here,
	// the in-memory buffer still has them and Finish backfills the full
	// snapshot before sealing the turn (see needsBackfill / Finish), so
	// the persisted ledger is healed rather than left with a permanent gap.
	b.mu.Lock()
	b.persister = p
	b.persistCh = make(chan bufferedEvent, persistChanDepth)
	b.mu.Unlock()
	b.persistWG.Add(1)
	go b.runPersister()
	return nil
}

// Persister sizing. These are named (rather than literals at their use sites)
// because together they bound how much the persister can absorb while a flush
// is in flight: at most flushBatchSize events held in the pending batch plus
// persistChanDepth queued behind it. Emitting more than that sum while the
// persister is stalled GUARANTEES the live path drops events, which is what
// makes the backfill test deterministic instead of a race against a sleep.
const (
	persistChanDepth = 512
	flushInterval    = 50 * time.Millisecond
	flushBatchSize   = 64
)

// runPersister drains persistCh, batching writes every flushInterval
// or flushBatchSize events (whichever comes first). Exits when
// persistCh is closed.
func (b *turnBuffer) runPersister() {
	defer b.persistWG.Done()
	defer safe.Recover("httpapi.turn_buffer.persister", nil)

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
			logging.From(ctx).Error("persist turn_events", "turn_id", b.turnID, "n", len(toStore), "err", err)
			// These rows are unconfirmed; flag the turn for a full backfill on
			// Finish rather than leaving a permanent gap.
			b.markNeedsBackfill()
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
// subscriberCount returns the number of live subscribers. Test-only barrier
// helper: tests poll it (instead of a fixed sleep) to wait until Attach
// goroutines have registered before Emit.
func (b *turnBuffer) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

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
	// IDs are monotonic across the turn even after eviction: derive the next id
	// from the last event (not len(events), which shrinks when the window slides).
	var id uint64 = 1
	if n := len(b.events); n > 0 {
		id = b.events[n-1].ID + 1
	}
	ev := bufferedEvent{ID: id, Name: event, Data: data}
	b.events = append(b.events, ev)
	b.noteToolDuration(event, payload)
	if event == "fleet.context_compacted" {
		b.compactions++
	}
	b.totalBytes += len(data)

	// Sliding-window eviction: drop the oldest events once over the byte cap,
	// always keeping at least the newest so a reconnecting client still gets
	// something (plus a `reconnect` event describing the gap — see Attach).
	for b.maxBytes > 0 && b.totalBytes > b.maxBytes && len(b.events) > 1 {
		b.totalBytes -= len(b.events[0].Data)
		b.events = b.events[1:]
	}

	for subID, ch := range b.subscribers {
		select {
		case ch <- ev:
		default:
			// Slow subscriber — evict. They can reattach with
			// Last-Event-ID and pick up where they dropped.
			b.evictedSubs[subID] = true
			close(ch)
			delete(b.subscribers, subID)
		}
	}

	// Persistence fan-out. Non-blocking so DB slowness never stalls live
	// streaming (this runs under b.mu, which Attach/Finish also take, and
	// fantasy's streaming callbacks hold other locks behind us — so it must
	// not block). If the persister channel is full we drop the event from the
	// live persist path but flag the turn: Finish re-sends the full in-memory
	// snapshot before sealing it, so the row is recovered rather than lost.
	if b.persistCh != nil {
		select {
		case b.persistCh <- ev:
		default:
			b.needsBackfill = true // already under b.mu
			b.droppedOnFull++
			slog.Info("persister channel full; event deferred to Finish backfill", "turn_id", b.turnID, "id", ev.ID)
		}
	}
}

// noteToolDuration captures a tool.result frame's duration_ms. Caller holds mu.
func (b *turnBuffer) noteToolDuration(event string, payload any) {
	if event != "tool.result" {
		return
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return
	}
	id, _ := m["id"].(string)
	ms, ok := m["duration_ms"].(int64)
	if id == "" || !ok {
		return
	}
	if b.toolDurations == nil {
		b.toolDurations = make(map[string]int64)
	}
	b.toolDurations[id] = ms
}

// toolDurationsSnapshot returns a copy of the per-call tool durations seen so
// far, keyed by tool-call id.
func (b *turnBuffer) toolDurationsSnapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int64, len(b.toolDurations))
	for id, ms := range b.toolDurations {
		out[id] = ms
	}
	return out
}

// broadcastControl fans out a one-shot control frame (e.g. "shutdown") to all
// live subscribers WITHOUT appending it to the replay buffer or the persistence
// ledger — it is a transient hint, not part of the turn's event history. It
// reuses the current highest event id so it never advances a client's
// Last-Event-ID into a gap: a reconnect resumes exactly where it left off. A
// sealed buffer has no subscribers, so this is a no-op there.
func (b *turnBuffer) broadcastControl(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	var id uint64
	if n := len(b.events); n > 0 {
		id = b.events[n-1].ID
	}
	ev := bufferedEvent{ID: id, Name: name, Data: []byte("{}")}
	for subID, ch := range b.subscribers {
		select {
		case ch <- ev:
		default:
			// Slow subscriber — evict (mirrors Emit). They reattach and learn of
			// the shutdown from /healthz 503 / the sealed buffer instead.
			b.evictedSubs[subID] = true
			close(ch)
			delete(b.subscribers, subID)
		}
	}
}

// markNeedsBackfill flags the turn for a full snapshot backfill on Finish. Used
// by runPersister (which holds no lock); Emit sets the flag inline since it
// already holds b.mu.
func (b *turnBuffer) markNeedsBackfill() {
	b.mu.Lock()
	b.needsBackfill = true
	b.mu.Unlock()
}

// Finish seals the buffer. Emit becomes a no-op, all subscriber
// channels are closed so their Attach goroutines see EOF, and the
// persister goroutine is told to flush + exit. Safe to call twice;
// subsequent calls are no-ops.
//
// The terminal status (`completed` / `cancelled` / `error`) is
// inferred from the last terminal event in the log so the caller
// doesn't have to pass it in — it's already there in the stream.
// Sealed reports whether Finish has run. The inflight entry's finishedAt is
// stamped shortly AFTER the seal (finishTurn seals first, then flips the
// entry), so "sealed" is the earliest reliable this-turn-is-over signal — the
// #785 busy check uses it to avoid queueing a submission that raced the last
// microseconds of the previous turn's bookkeeping.
// terminalOutcome returns the turn's terminal frame (turn.completed,
// turn.cancelled, turn.error or turn.model_required), "" before it has
// emitted one. It reads the frame itself, sealed or not: a turn does
// post-turn work before sealing.
func (b *turnBuffer) terminalOutcome() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.events) - 1; i >= 0; i-- {
		switch name := b.events[i].Name; name {
		case "turn.completed", "turn.cancelled", "turn.error", "turn.model_required":
			return name
		}
	}
	return ""
}

func (b *turnBuffer) Sealed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

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
		// If any event went unconfirmed on the live path (persistCh saturation or a
		// failed batch insert), re-send the FULL in-memory snapshot now, BEFORE
		// sealing the turn. InsertTurnEvents is idempotent (ON CONFLICT DO NOTHING),
		// so this heals the gap without duplicating the rows that did land. We snapshot
		// under the lock (the publisher is done, but b.events is shared state) and
		// write outside it. lossy stays false on the happy path (no backfill) and when
		// the backfill fully succeeds; it is set only when the persisted ledger is
		// genuinely still incomplete after the heal attempt.
		lossy := false
		b.mu.Lock()
		needsBackfill := b.needsBackfill
		var snapshot []bufferedEvent
		if needsBackfill {
			snapshot = append([]bufferedEvent(nil), b.events...)
		}
		b.mu.Unlock()
		if needsBackfill {
			if err := b.backfill(snapshot); err != nil {
				slog.Error("persist backfill", "turn_id", b.turnID, "n", len(snapshot), "err", err)
				lossy = true
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := b.persister.FinishTurn(ctx, b.turnID, status, b.finishedAt.Unix(), lossy); err != nil {
			logging.From(ctx).Error("persist FinishTurn", "turn_id", b.turnID, "err", err)
		}
		cancel()
	}
}

// backfill re-sends the full event snapshot to the persister in bounded chunks,
// healing any rows dropped on the live path. ON CONFLICT DO NOTHING makes the
// re-send idempotent against rows that already landed. Chunked so a very chatty
// turn stays well under Postgres' parameter cap (InsertTurnEvents uses 5 params
// per row). Returns the first error so Finish can flag the turn lossy.
func (b *turnBuffer) backfill(events []bufferedEvent) error {
	const chunk = 500
	now := time.Now().Unix()
	for start := 0; start < len(events); start += chunk {
		end := start + chunk
		if end > len(events) {
			end = len(events)
		}
		batch := make([]store.TurnEvent, 0, end-start)
		for _, ev := range events[start:end] {
			batch = append(batch, store.TurnEvent{
				TurnID:    b.turnID,
				EventID:   ev.ID,
				Name:      ev.Name,
				Data:      ev.Data,
				CreatedAt: now,
			})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := b.persister.InsertTurnEvents(ctx, batch)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
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
		case "turn.model_required":
			// The engine deliberately emits turn.model_required INSTEAD of a
			// generic turn.error (the user can fix it by picking another
			// model). It is still a failed turn: sealing it `completed` with
			// history_committed_at NULL would break the journal's "gates
			// terminal success" contract and make RecoverStrandedTurns skip it.
			return store.TurnStatusError
		}
	}
	return store.TurnStatusCompleted
}

// sanitizeTurnEventForViewer filters or redacts sender-private fields from a
// turn event before it is delivered to viewerEmail:
//   - user.message: strips injected_context when viewerEmail is not the message
//     sender (defaulting to convOwnerEmail when sender_email is absent).
//   - queue.updated: skips the event when user_email is set to another user so
//     one multiplayer participant's queue updates never overwrite another
//     participant's visible queue, and strips the internal user_email tag when
//     delivering to the matching user.
func sanitizeTurnEventForViewer(name string, data []byte, viewerEmail, convOwnerEmail string) ([]byte, bool) {
	switch name {
	case "conversation":
		return sanitizeConversationEvent(data, viewerEmail, convOwnerEmail)
	case "user.message":
		if viewerEmail == "" || len(data) == 0 {
			return data, true
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return data, true
		}
		if _, hasInjected := m["injected_context"]; !hasInjected {
			return data, true
		}
		sender, _ := m["sender_email"].(string)
		sender = strings.TrimSpace(sender)
		if sender == "" {
			sender = strings.TrimSpace(convOwnerEmail)
		}
		if sender != "" && !strings.EqualFold(viewerEmail, sender) {
			delete(m, "injected_context")
			if b, err := json.Marshal(m); err == nil {
				return b, true
			}
		}
		return data, true
	case "mcp.servers_updated":
		return sanitizeMCPServersUpdatedEvent(data, viewerEmail, convOwnerEmail)
	case "queue.updated":
		return sanitizeQueueUpdatedEvent(data, viewerEmail)
	case "turn.completed", "turn.cancelled", "turn.error", "turn.model_required":
		return sanitizeTerminalTurnEvent(name, data, viewerEmail, convOwnerEmail)
	case "tool.approval_required", "tool.approval_card_fallback", "tool.approval_superseded", "tool.action_recorded", "tool.action_record_card", "tool.auto_resolved", "tool.call", "tool.result", "subagent.progress", "reasoning.start", "reasoning.delta", "reasoning.end", "memory.proposed", "patrol.updated":
		if len(data) == 0 {
			return data, true
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return data, true
		}
		uEmail, _ := m["user_email"].(string)
		uEmail = strings.TrimSpace(uEmail)
		if uEmail == "" {
			return data, true
		}
		if viewerEmail != "" && !strings.EqualFold(viewerEmail, uEmail) {
			return nil, false
		}
		delete(m, "user_email")
		if b, err := json.Marshal(m); err == nil {
			return b, true
		}
		return data, true
	default:
		return data, true
	}
}

func sanitizeConversationEvent(data []byte, viewerEmail, convOwnerEmail string) ([]byte, bool) {
	if len(data) == 0 || viewerEmail == "" || convOwnerEmail == "" || strings.EqualFold(viewerEmail, convOwnerEmail) {
		return data, true
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data, true
	}
	if _, ok := m["auto_approve"]; !ok {
		return data, true
	}
	delete(m, "auto_approve")
	if b, err := json.Marshal(m); err == nil {
		return b, true
	}
	return data, true
}

func sanitizeTerminalTurnEvent(name string, data []byte, viewerEmail, convOwnerEmail string) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data, true
	}
	uEmail, _ := m["user_email"].(string)
	uEmail = strings.TrimSpace(uEmail)
	delete(m, "user_email")
	sender := uEmail
	if sender == "" {
		sender = strings.TrimSpace(convOwnerEmail)
	}
	if viewerEmail != "" && sender != "" && !strings.EqualFold(viewerEmail, sender) {
		switch name {
		case "turn.completed":
			return []byte("{}"), true
		case "turn.cancelled":
			return []byte(`{"reason":"cancelled"}`), true
		case "turn.error":
			return []byte(`{"message":"Turn failed."}`), true
		case "turn.model_required":
			out := map[string]any{"message": "Turn failed."}
			if reason, ok := m["reason"].(string); ok && reason != "" {
				out["reason"] = reason
			}
			if b, err := json.Marshal(out); err == nil {
				return b, true
			}
			return []byte(`{"message":"Turn failed."}`), true
		}
	}
	if b, err := json.Marshal(m); err == nil {
		return b, true
	}
	return data, true
}

func sanitizeMCPServersUpdatedEvent(data []byte, viewerEmail, convOwnerEmail string) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data, true
	}
	partRaw, hasPart := m["participant_enabled_optional"]
	ownerRaw, hasOwner := m["owner_enabled_optional"]
	viewerMap, _ := m["viewer_enabled_optional"].(map[string]any)
	uEmail, _ := m["user_email"].(string)
	uEmail = strings.TrimSpace(uEmail)
	delete(m, "participant_enabled_optional")
	delete(m, "owner_enabled_optional")
	delete(m, "viewer_enabled_optional")
	delete(m, "user_email")
	if !hasPart {
		return data, true
	}
	isSender := viewerEmail != "" && uEmail != "" && strings.EqualFold(viewerEmail, uEmail)
	if viewerEmail != "" && !isSender {
		targetRaw := partRaw
		viewerKey := strings.ToLower(strings.TrimSpace(viewerEmail))
		if vSpecific, ok := viewerMap[viewerKey]; ok {
			targetRaw = vSpecific
		} else if convOwnerEmail != "" && strings.EqualFold(viewerEmail, convOwnerEmail) {
			if hasOwner {
				targetRaw = ownerRaw
			} else if uEmail == "" {
				if b, err := json.Marshal(m); err == nil {
					return b, true
				}
				return data, true
			}
		}
		m["enabled_optional"] = targetRaw
		allowed := map[string]bool{}
		if arr, ok := targetRaw.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					allowed[strings.ToLower(strings.TrimSpace(s))] = true
				}
			}
		}
		if addedArr, ok := m["added"].([]any); ok && len(addedArr) > 0 {
			var filteredAdded []string
			for _, v := range addedArr {
				if s, ok := v.(string); ok && allowed[strings.ToLower(strings.TrimSpace(s))] {
					filteredAdded = append(filteredAdded, s)
				}
			}
			if len(filteredAdded) == 0 {
				return nil, false
			}
			m["added"] = filteredAdded
		}
	}
	if b, err := json.Marshal(m); err == nil {
		return b, true
	}
	return data, true
}

func sanitizeQueueUpdatedEvent(data []byte, viewerEmail string) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data, true
	}
	uEmail, _ := m["user_email"].(string)
	uEmail = strings.TrimSpace(uEmail)
	if uEmail != "" && viewerEmail != "" && !strings.EqualFold(viewerEmail, uEmail) {
		return nil, false
	}
	delete(m, "user_email")
	if rawItems, ok := m["items"].([]any); ok {
		filtered := make([]any, 0, len(rawItems))
		for _, rawItem := range rawItems {
			itemMap, ok := rawItem.(map[string]any)
			if !ok {
				filtered = append(filtered, rawItem)
				continue
			}
			itemUser, _ := itemMap["user_email"].(string)
			itemUser = strings.TrimSpace(itemUser)
			delete(itemMap, "user_email")
			if uEmail == "" && viewerEmail != "" && itemUser != "" && !strings.EqualFold(viewerEmail, itemUser) {
				continue
			}
			filtered = append(filtered, itemMap)
		}
		m["items"] = filtered
	}
	if b, err := json.Marshal(m); err == nil {
		return b, true
	}
	return data, true
}

// Attach writes every event with ID > lastEventID to w in SSE framing,
// then streams any subsequent events as they arrive. Returns when the
// buffer is sealed and fully drained, or when ctx is cancelled (client
// disconnect). Must be called BEFORE any other write to w — it sets
// the SSE headers + status line itself.
//
// caps is the client's declared SSE capability set (#194): a nil set means "no
// filter, emit everything" (the default for a client that doesn't negotiate),
// otherwise governed events the client didn't declare are suppressed. Lifecycle
// and control frames always flow — see shouldEmit.
func (b *turnBuffer) Attach(ctx context.Context, lastEventID uint64, w http.ResponseWriter, caps map[SSECapability]bool) error {
	return b.AttachForViewer(ctx, lastEventID, w, caps, "", "")
}

// AttachForViewer is Attach with per-viewer sanitization of sender-only context
// (`user.message.injected_context`) and user-scoped `queue.updated` frames in
// multiplayer conversations.
func (b *turnBuffer) AttachForViewer(ctx context.Context, lastEventID uint64, w http.ResponseWriter, caps map[SSECapability]bool, viewerEmail, convOwnerEmail string) error {
	return b.AttachForViewerChecked(ctx, lastEventID, w, caps, viewerEmail, convOwnerEmail, nil)
}

// AttachForViewerChecked is AttachForViewer with an optional allowCheck hook
// evaluated after subscriber registration and during replay so a concurrent
// multiplayer access revocation cannot slip between the route's initial lookup
// and subscriber registration.
func (b *turnBuffer) AttachForViewerChecked(ctx context.Context, lastEventID uint64, w http.ResponseWriter, caps map[SSECapability]bool, viewerEmail, convOwnerEmail string, allowCheck func() bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}

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
		b.subViewers[subID] = strings.ToLower(strings.TrimSpace(viewerEmail))
	}
	b.mu.Unlock()

	if (allowCheck != nil && !allowCheck()) || b.isSubRevoked(subID) {
		b.unsubscribe(subID)
		http.Error(w, "conversation not found", http.StatusNotFound)
		return nil
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	setSupportedCapabilitiesHeader(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Advertise supported capabilities as the very first frame so a client that
	// can't read response headers (browser EventSource) still discovers them.
	// Best-effort: a write error here is surfaced by the replay/live writes below.
	_ = writeCapabilitiesFrame(w, flusher)

	// If the sliding window evicted events the client hadn't seen yet (the oldest
	// surviving event is newer than its next-expected id), tell it up front so the
	// UI can surface a non-intrusive "some events may be missing" notice. Omitted
	// on a clean reconnect within the full buffer.
	if lastEventID > 0 && len(replay) > 0 && replay[0].ID > lastEventID+1 {
		missed := replay[0].ID - (lastEventID + 1)
		if err := writeReconnectFrame(w, flusher, missed, replay[0].ID); err != nil {
			b.unsubscribe(subID)
			return err
		}
	}

	// Pump replay (filtered by the client's declared capabilities and viewer scope).
	for _, e := range replay {
		if b.isSubRevoked(subID) || (allowCheck != nil && !allowCheck()) {
			b.unsubscribe(subID)
			return nil
		}
		if !shouldEmit(caps, e.Name) {
			continue
		}
		sanitizedData, keep := sanitizeTurnEventForViewer(e.Name, e.Data, viewerEmail, convOwnerEmail)
		if !keep {
			continue
		}
		e.Data = sanitizedData
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

	// Idle keepalive: ping when no real event has been written for the heartbeat
	// interval, resetting the timer on every real event so we only ping during
	// genuine quiet. A nil channel (interval 0) never fires.
	var hb *time.Ticker
	var heartbeatC <-chan time.Time
	if sseHeartbeatInterval > 0 {
		hb = time.NewTicker(sseHeartbeatInterval)
		defer hb.Stop()
		heartbeatC = hb.C
	}

	for {
		select {
		case <-ctx.Done():
			b.unsubscribe(subID)
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				if b.takeEvicted(subID) {
					// Closed for falling behind, NOT because the turn
					// finished. End with an explicit evicted frame so the
					// client reattaches with Last-Event-ID instead of
					// reading this clean EOF as turn-complete.
					return writeEvictedFrame(w, flusher)
				}
				// Buffer sealed — our subscription was closed by Finish.
				return nil
			}
			if !shouldEmit(caps, ev.Name) {
				continue
			}
			sanitizedData, keep := sanitizeTurnEventForViewer(ev.Name, ev.Data, viewerEmail, convOwnerEmail)
			if !keep {
				continue
			}
			ev.Data = sanitizedData
			if err := writeSSEFrame(w, flusher, ev); err != nil {
				b.unsubscribe(subID)
				return err
			}
			if hb != nil {
				hb.Reset(sseHeartbeatInterval)
			}
		case <-heartbeatC:
			if err := writeHeartbeat(w, flusher); err != nil {
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
	delete(b.subViewers, id)
}

// subscriberEmails returns the deduplicated viewer emails of all currently
// attached live SSE subscribers on this buffer.
func (b *turnBuffer) subscriberEmails() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := make(map[string]bool, len(b.subViewers))
	var out []string
	for subID := range b.subscribers {
		v := strings.ToLower(strings.TrimSpace(b.subViewers[subID]))
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// detachUnauthorizedViewers closes and removes any live subscriber whose
// viewerEmail is neither the conversation owner nor in allowedViewers.
func (b *turnBuffer) detachUnauthorizedViewers(ownerEmail string, allowedViewers map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for subID, ch := range b.subscribers {
		v := b.subViewers[subID]
		if v == "" || strings.EqualFold(v, ownerEmail) || allowedViewers[v] {
			continue
		}
		if b.revokedSubs == nil {
			b.revokedSubs = make(map[uint64]bool)
		}
		b.revokedSubs[subID] = true
		close(ch)
		delete(b.subscribers, subID)
		delete(b.subViewers, subID)
	}
}

func (b *turnBuffer) isSubRevoked(subID uint64) bool {
	if subID == 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.revokedSubs[subID]
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

// takeEvicted reports (and clears) whether subID's channel was closed by
// eviction rather than by Finish.
func (b *turnBuffer) takeEvicted(subID uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.evictedSubs[subID] {
		delete(b.evictedSubs, subID)
		return true
	}
	return false
}

// writeEvictedFrame emits a synthetic `reconnect` event telling the client its
// subscription was dropped for falling behind while the turn kept running —
// the signal to reattach with Last-Event-ID rather than treat the stream's
// end as turn-complete. No id line, like writeReconnectFrame.
func writeEvictedFrame(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, "event: reconnect\ndata: {\"type\":\"evicted\"}\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeReconnectFrame emits a synthetic `reconnect` event (no id line, so it
// doesn't advance the client's Last-Event-ID) describing how many events the
// sliding window dropped before the replay the client is about to receive.
func writeReconnectFrame(w http.ResponseWriter, flusher http.Flusher, missed, resumedFromID uint64) error {
	payload, _ := json.Marshal(map[string]any{
		"type":            "reconnect",
		"missed_events":   missed,
		"resumed_from_id": resumedFromID,
	})
	if _, err := fmt.Fprintf(w, "event: reconnect\ndata: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeHeartbeat emits an SSE comment frame. Comments keep the socket alive
// without dispatching a client-visible message event.
func writeHeartbeat(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeCapabilitiesFrame emits the synthetic fleet.capabilities event (#194)
// advertising the server's supported SSE capability set and its keepalive
// cadence (`heartbeat_ms`, 0 = disabled — see heartbeatIntervalMs for why a
// client needs it). Like writeReconnectFrame it omits the id line, so it never
// advances the client's Last-Event-ID (real event ids start at 1) and is not
// persisted to turn_events.
func writeCapabilitiesFrame(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: {\"type\":%q,\"supported\":%s,\"heartbeat_ms\":%d}\n\n",
		capabilitiesEventName, capabilitiesEventName, supportedCapabilitiesJSON(),
		heartbeatIntervalMs()); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// historyPersistedEntries pairs just-persisted history entries with their DB
// ids for the history.persisted stream event: the minimal shape ({id, role})
// the client needs to backfill Message.dbId (the Branch button gates on it).
// ids is order-aligned with entries (AppendHistory's RETURNING order); a
// length mismatch yields the common prefix rather than a panic.
func historyPersistedEntries(entries []agent.HistoryEntry, ids []int64) []map[string]any {
	n := len(entries)
	if len(ids) < n {
		n = len(ids)
	}
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{"id": ids[i], "role": entries[i].Role})
	}
	return out
}

// markStoppedByKey records a Stop by key of this turn's input, and
// addStoppedSteer one of the injected steer rowID it carries — only while
// the buffer is open, checked under the same lock Finish seals it with. The
// settlement reads both after the turn's Finish, so a record that lands is
// always seen; one that reports false came after the seal, and the Stop
// must treat the turn as ended.
func (b *turnBuffer) markStoppedByKey() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.stoppedByKey.Store(true)
	return true
}

func (b *turnBuffer) addStoppedSteer(rowID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.stoppedSteers = append(b.stoppedSteers, rowID)
	return true
}

// stoppedSteerIDs returns the steer rows addStoppedSteer recorded.
func (b *turnBuffer) stoppedSteerIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.stoppedSteers...)
}

// compactionCount returns how many times this turn's context was compacted.
func (b *turnBuffer) compactionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.compactions
}
