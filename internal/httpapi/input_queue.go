package httpapi

// Conversation-owned input queue + mid-turn steering (#785). A submission
// that arrives while a turn is running becomes a durable queue row BEFORE the
// API acknowledges it — never an implicit cancel of the active turn (explicit
// /cancel stays the only Stop). Queued rows drain as ordinary separate turns;
// steer rows are additionally offered to the running turn's PrepareStep
// boundary via the steerMailbox and fall back to a queued turn if the turn
// ends first. There is no long-lived per-conversation goroutine: the drain
// loop is carried by enqueue kicks and turn-completion tail calls, and the
// single-writer guarantee stays where it always was — registerTurn/inflightMu.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/truncate"
)

// steerMailbox is the one-slot handoff between the HTTP layer and the running
// turn's PrepareStep boundary. offer is non-blocking: the DB row stays
// 'queued' until the agentcore boundary Acknowledges (the durable
// queued->injected flip), so a missed or unconsumed offer automatically
// degrades to the durable fallback — the row drains as the next turn.
type steerMailbox struct {
	store  chatStore
	turnID string
	buf    *turnBuffer
	user   string
	convID string
	slot   chan agentcore.SteerMessage
}

func newSteerMailbox(st chatStore, user, convID, turnID string, buf *turnBuffer) *steerMailbox {
	return &steerMailbox{store: st, turnID: turnID, buf: buf, user: user, convID: convID,
		slot: make(chan agentcore.SteerMessage, 1)}
}

// offer hands a queued steer row to the running turn. A full slot (one
// pending steer at a time) silently leaves the row queued — the durable
// fallback runs it as the next turn.
func (m *steerMailbox) offer(id, text string) {
	select {
	case m.slot <- agentcore.SteerMessage{ID: id, Text: text}:
	default:
	}
}

// Poll implements agentcore.SteerSource.
func (m *steerMailbox) Poll() (agentcore.SteerMessage, bool) {
	select {
	case msg := <-m.slot:
		return msg, true
	default:
		return agentcore.SteerMessage{}, false
	}
}

// Acknowledge implements agentcore.SteerSource: the durable queued->injected
// flip, on a fresh context (the turn ctx may be near its deadline). A lost
// race with remove/cancel refuses injection — the message is gone, not
// silently injected after the user deleted it.
func (m *steerMailbox) Acknowledge(_ context.Context, id string) error {
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := m.store.MarkInputInjected(actx, id, m.turnID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("input %s is no longer queued", id)
	}
	m.emitQueueSnapshot()
	return nil
}

func (m *steerMailbox) emitQueueSnapshot() {
	if m.buf == nil {
		return
	}
	qctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	items, err := m.store.ListQueuedInputs(qctx, m.user, m.convID)
	if err != nil {
		return
	}
	m.buf.Emit("queue.updated", map[string]any{"items": queueItemsPayload(items)})
}

// queueItemsPayload is the wire shape of one queue snapshot (full snapshot on
// every mutation — reattach needs no event sourcing).
func queueItemsPayload(items []store.InputQueueRow) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		preview := truncate.Clamp(it.Message, 160, "…")
		out = append(out, map[string]any{
			"id":              it.ID,
			"client_input_id": it.ClientInputID,
			"mode":            it.Mode,
			"state":           it.State,
			"position":        it.Position,
			"message_preview": preview,
			"has_attachments": it.Attachments != "" && it.Attachments != "[]",
			"created_at":      it.CreatedAt,
			"turn_id":         it.TurnID,
		})
	}
	return out
}

// emitQueueUpdate publishes the queue snapshot into the conversation's live
// buffer, when one is running (nil-safe otherwise — the GET endpoint is the
// authoritative snapshot for idle conversations).
func (s *Server) emitQueueUpdate(ctx context.Context, user, convID string) {
	entry, ok := s.getInflight(convID)
	if !ok || entry.buf == nil {
		return
	}
	items, err := s.store.ListQueuedInputs(ctx, user, convID)
	if err != nil {
		return
	}
	entry.buf.Emit("queue.updated", map[string]any{"items": queueItemsPayload(items)})
}

// maxPendingInputs caps how many still-queued rows one conversation may hold.
// Each row later drains as a full governed turn, so the cap bounds unattended
// LLM spend the same way boot recovery's no-auto-drain rule does.
const maxPendingInputs = 20

// writeQueueAck writes the JSON acknowledgement for an accepted (202) or
// replayed (200) queue row.
func writeQueueAck(w http.ResponseWriter, status int, convID string, row store.InputQueueRow) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"queued": true,
		"input": map[string]any{
			"id": row.ID, "client_input_id": row.ClientInputID,
			"mode": row.Mode, "state": row.State, "position": row.Position,
		},
		"conversation_id": convID,
	})
}

// handleBusySubmit is postChat's queue branch: the conversation has a running
// turn, so the submission becomes a durable queue row and the client gets a
// 202 JSON acknowledgement instead of an SSE stream (200 on an idempotent
// replay of an already-accepted input).
func (s *Server) handleBusySubmit(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, req chatRequest) {
	attachments := "[]"
	if len(req.Attachments) > 0 {
		if raw, err := json.Marshal(req.Attachments); err == nil {
			attachments = string(raw)
		}
	}
	mode := store.InputModeQueued
	// Steering carries text only: attachments need the full turn-start
	// multimodal path, so a steer submission with attachments downgrades to a
	// queued follow-up (stated in the response).
	if strings.EqualFold(req.Mode, "steer") && len(req.Attachments) == 0 {
		mode = store.InputModeSteer
	}
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" {
		clientID = uuid.NewString()
	}
	// Depth cap: every queued row later runs as a full governed turn, so an
	// unbounded queue is unbounded unattended LLM spend (a retrying client
	// minting fresh input_ids can enqueue hundreds during one long turn).
	// Idempotent replays of an already-accepted input still pass through —
	// EnqueueInput's conflict path returns the existing row without growing
	// the queue.
	if pending, cerr := s.store.CountPendingInputs(r.Context(), conv.ID); cerr == nil && pending >= maxPendingInputs {
		if existing, lerr := s.store.LookupInput(r.Context(), conv.ID, clientID); lerr == nil && existing != nil {
			writeQueueAck(w, http.StatusOK, conv.ID, *existing)
			return
		}
		http.Error(w, fmt.Sprintf("input queue is full (%d pending); wait for the queue to drain or remove queued inputs", maxPendingInputs), http.StatusTooManyRequests)
		return
	}
	row, created, err := s.store.EnqueueInput(r.Context(), store.InputQueueRow{
		ID: uuid.NewString(), ConversationID: conv.ID, UserEmail: user,
		ClientInputID: clientID, Message: req.Message, Attachments: attachments, Mode: mode,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if created && row.Mode == store.InputModeSteer {
		if entry, ok := s.getInflight(conv.ID); ok && entry.IsRunning() && entry.steer != nil {
			entry.steer.offer(row.ID, req.Message)
		}
	}
	s.emitQueueUpdate(r.Context(), user, conv.ID)
	// Close the submit-vs-completion race: if the running turn finished
	// between our busy check and the durable insert, this kick drains the row
	// immediately instead of leaving it stranded until the next submission.
	s.maybeDrainQueue(conv.ID)

	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeQueueAck(w, status, conv.ID, row)
}

// rekickDrainAfter schedules a bounded retry kick for rows that went back to
// 'queued' with no natural kicker (concurrency cap full, transient launch
// failure, race-loser un-claim). Without it those rows stall until the next
// submission; with it the queue self-heals a few seconds later.
func (s *Server) rekickDrainAfter(convID string, d time.Duration) {
	time.AfterFunc(d, func() { s.maybeDrainQueue(convID) })
}

// markStopAll records a Stop scope=all instant for convID. Rows accepted
// BEFORE it must not launch even if they were in claim-limbo (claimed by a
// drain but not yet registered) when CancelQueuedInputs swept the queued set.
func (s *Server) markStopAll(convID string) {
	now := time.Now().Unix()
	s.inflightMu.Lock()
	if s.stopEpochs == nil {
		s.stopEpochs = make(map[string]int64)
	}
	// An epoch only gates rows that were already accepted when Stop fired;
	// once the max turn lifetime has passed nothing can still be in
	// claim-limbo from that instant, so prune stale entries here instead of
	// letting the map grow for the process lifetime.
	horizon := now - int64(s.turnTimeout()/time.Second) - 60
	for k, v := range s.stopEpochs {
		if v < horizon {
			delete(s.stopEpochs, k)
		}
	}
	s.stopEpochs[convID] = now
	s.inflightMu.Unlock()
}

// stoppedSince reports whether a Stop scope=all was issued after the row's
// acceptance — the launch gate for claim-limbo rows. The comparison is STRICT:
// created_at has second granularity, so a row accepted in the same second as
// (but after) the Stop is a fresh post-Stop submission that must run, not be
// silently discarded after its 202 acknowledgement. A pre-Stop row in that
// same second is still swept by CancelQueuedInputs in the common (unclaimed)
// case — only the sub-second claim-limbo overlap trades the other way.
func (s *Server) stoppedSince(convID string, createdAt int64) bool {
	s.inflightMu.Lock()
	epoch, ok := s.stopEpochs[convID]
	s.inflightMu.Unlock()
	return ok && createdAt < epoch
}

// maybeDrainQueue claims and launches the conversation's next queued input
// when no turn is running. Re-entrant and race-safe: the claim is DB-atomic,
// registerTurn refuses while a turn runs (the loser un-claims), and each
// launched turn tail-calls back here on completion.
func (s *Server) maybeDrainQueue(convID string) {
	if s.shuttingDown.Load() {
		return // rows stay durable; boot recovery re-queues, never auto-runs
	}
	if entry, ok := s.getInflight(convID); ok && entry.IsRunning() {
		return // its completion tail-call re-kicks
	}
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claimTurnID := uuid.NewString()
	row, err := s.store.ClaimNextQueuedInput(dctx, convID, claimTurnID)
	if err != nil {
		log.Printf("input queue claim (conv=%s): %v", convID, err) //nolint:gosec // G706: server-generated UUIDs + internal error — no request-authored text is logged.
		return
	}
	if row == nil {
		return
	}
	if !s.launchQueuedTurn(convID, row) {
		// A direct submission won the registerTurn race; put the row back.
		// The winner's completion re-drains it, and the bounded re-kick
		// covers a winner whose tail already ran before our un-claim landed.
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.store.MarkInputTerminal(rctx, row.ID, store.InputStateQueued); err != nil {
			log.Printf("input queue unclaim (conv=%s input=%s): %v", convID, row.ID, err) //nolint:gosec // G706: server-generated UUIDs + internal error — no request-authored text is logged.
		}
		rcancel()
		s.rekickDrainAfter(convID, 2*time.Second)
	}
}

// launchQueuedTurn runs one claimed queue row as an ordinary turn — the same
// prep, buffer, and runTurnAsync path as a direct submission, so every
// governance and persistence property (#798 included) holds unchanged.
func (s *Server) launchQueuedTurn(convID string, row *store.InputQueueRow) bool {
	// Stop scope=all gate: a row accepted before the Stop instant must not
	// launch, even if it was claimed (invisible to CancelQueuedInputs) while
	// the sweep ran.
	if s.stoppedSince(convID, row.CreatedAt) {
		s.terminalizeQueueRow(row.ID, store.InputStateCancelled)
		return true
	}
	// The ROW's owner is authoritative — the drain kick may come from another
	// actor's request path (e.g. a different session's /cancel bookkeeping).
	user := row.UserEmail
	lctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conv, err := s.store.Get(lctx, user, convID)
	if err != nil {
		// TRANSIENT store failure: the 202-acknowledged input must survive.
		// Back to queued + a bounded re-kick; never cancelled for weather.
		log.Printf("input queue: conversation %s load failed: %v", convID, err) //nolint:gosec // G706: server-generated UUIDs + internal error — no request-authored text is logged.
		s.terminalizeQueueRow(row.ID, store.InputStateQueued)
		s.rekickDrainAfter(convID, 3*time.Second)
		return true
	}
	if conv == nil {
		// Definitively gone (deleted/expired): cancel is honest.
		s.terminalizeQueueRow(row.ID, store.InputStateCancelled)
		return true
	}
	var attachments []chatAttachment
	if row.Attachments != "" {
		_ = json.Unmarshal([]byte(row.Attachments), &attachments)
	}
	// Queue-drained turns bypass the HTTP admission middleware (admission
	// happened when the input was accepted); the per-user concurrency cap
	// still applies. A full limiter leaves the row queued for the next kick.
	if s.concurrent != nil && !s.concurrent.Acquire(user) {
		// Cap full (often because the just-completed turn's own slot releases
		// AFTER its tail drain). Re-queue with a bounded re-kick so the row
		// drains once a slot frees instead of stalling until the next submit.
		s.terminalizeQueueRow(row.ID, store.InputStateQueued)
		s.rekickDrainAfter(convID, 2*time.Second)
		return true
	}
	releaseSlot := func() {
		if s.concurrent != nil {
			s.concurrent.Release(user)
		}
	}

	req := chatRequest{
		ConversationID: convID,
		Message:        row.Message,
		Attachments:    attachments,
	}
	if !s.startTurn(nil, nil, user, conv, req, row.ID, releaseSlot) {
		releaseSlot()
		return false
	}
	return true
}

// terminalizeQueueRow best-effort flips a row's state on a fresh context.
func (s *Server) terminalizeQueueRow(id, state string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.MarkInputTerminal(ctx, id, state); err != nil {
		log.Printf("input queue state (%s -> %s): %v", id, state, err) //nolint:gosec // G706: server-generated UUIDs + internal error — no request-authored text is logged.
	}
}

// handleQueueRoutes serves the queue API under /conversations/{id}/queue:
// GET (snapshot), DELETE {inputID} (remove), POST {inputID}/send-now
// (promote; offered to the running turn when the row is steerable).
func (s *Server) handleQueueRoutes(w http.ResponseWriter, r *http.Request, user, convID, subArg string) {
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	switch {
	case subArg == "" && r.Method == http.MethodGet:
		items, err := s.store.ListQueuedInputs(r.Context(), user, convID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": queueItemsPayload(items)})
	case subArg != "" && !strings.Contains(subArg, "/") && r.Method == http.MethodDelete:
		ok, err := s.store.RemoveQueuedInput(r.Context(), user, convID, subArg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "input is no longer queued", http.StatusConflict)
			return
		}
		s.emitQueueUpdate(r.Context(), user, convID)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(subArg, "/send-now") && r.Method == http.MethodPost:
		inputID := strings.TrimSuffix(subArg, "/send-now")
		ok, err := s.store.PromoteQueuedInput(r.Context(), user, convID, inputID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "input is no longer queued", http.StatusConflict)
			return
		}
		// Running turn: offer the promoted row to the steer boundary too (the
		// queued->injected flip still gates on the boundary's Acknowledge).
		if entry, running := s.getInflight(convID); running && entry.IsRunning() && entry.steer != nil {
			items, lerr := s.store.ListQueuedInputs(r.Context(), user, convID)
			if lerr == nil {
				for _, it := range items {
					// Only text-only steer rows inject mid-turn: offering a
					// queued row with attachments would silently drop them
					// (steering is text-only; the row drains as a full turn).
					if it.ID == inputID && it.State == store.InputStateQueued &&
						it.Mode == store.InputModeSteer && (it.Attachments == "" || it.Attachments == "[]") {
						entry.steer.offer(it.ID, it.Message)
						break
					}
				}
			}
		}
		s.emitQueueUpdate(r.Context(), user, convID)
		s.maybeDrainQueue(convID)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// steerSourceOrNil avoids the typed-nil interface trap: a nil *steerMailbox
// stored in an agentcore.SteerSource interface would be non-nil to the seam's
// nil checks yet panic on use.
func steerSourceOrNil(m *steerMailbox) agentcore.SteerSource {
	if m == nil {
		return nil
	}
	return m
}
