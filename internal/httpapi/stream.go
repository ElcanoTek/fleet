// SSE stream reattach + inflight probe for a conversation's running (or
// recently finished) turn: GET /conversations/{id}/stream and
// /conversations/{id}/inflight, with the DB-fallback replay used when the
// in-memory buffer is gone. Split out of server.go (#1127); the buffer itself
// lives in turn_buffer.go.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/store"
)

// handleStream reattaches an SSE client to an in-flight (or recently
// finished) turn's event buffer. Query param turn_id, if present, must
// match the buffer's turn to guard against stale client retries racing
// a superseding turn. Last-Event-ID tells us how much the client has
// already processed.
//
// Fallback order when the in-memory buffer is gone (evicted after TTL
// or wiped by a restart):
//  1. If the query carries an explicit turn_id we know about in the
//     `turns` table, replay from turn_events.
//  2. Otherwise 204 — client should reload the DB-backed history.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, convID string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	lastEventID := parseLastEventID(r)
	requestedTurnID := r.URL.Query().Get("turn_id")
	// Client's declared SSE capabilities (#194); nil = no filter (full stream).
	// Applied to both the live-buffer reattach and the DB-fallback replay below.
	caps := parseClientCapabilities(r.Header.Get(clientCapabilitiesHeaderName))

	entry, ok := s.getInflight(convID)
	// If the client is asking about a different (older) turn than the one
	// we currently have buffered, fall through to the DB lookup below.
	if ok && entry.buf != nil && (requestedTurnID == "" || requestedTurnID == entry.turnID) {
		s.sseReconnects.inc("within_buffer")
		if err := entry.buf.Attach(r.Context(), lastEventID, w, caps); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("stream Attach (user=%q conv=%q): %v", user, convID, err) //nolint:gosec // identifiers are %q-quoted
		}
		return
	}

	// DB fallback. Needs an explicit turn_id so we know which row to
	// look up — without one, the client should just reload history.
	if requestedTurnID == "" {
		s.sseReconnects.inc("no_content")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Conversation scope is in the query (#1112). The equality check
	// below stays as defense in depth.
	turn, err := s.store.LookupTurnInConversation(r.Context(), requestedTurnID, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if turn == nil || turn.ConversationID != convID {
		s.sseReconnects.inc("no_content")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	events, err := s.store.LoadTurnEvents(r.Context(), requestedTurnID, lastEventID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The in-memory buffer is gone (evicted after TTL, or wiped by a restart). If
	// the turn already finished, the DB has the full event log including the
	// terminal frame; tag this as buffer_expired and send a synthetic notice so
	// the UI shows an inline "turn completed, refresh" rather than a blank stream.
	// If it's still running, this is a best-effort frozen replay (no live channel).
	outcome := "db_fallback"
	if turn.FinishedAt.Valid {
		outcome = "buffer_expired"
	}
	s.sseReconnects.inc(outcome)
	if err := replayEventsFromDB(w, events, caps); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("DB replay (user=%q conv=%q turn=%q): %v", user, convID, requestedTurnID, err) //nolint:gosec // identifiers are %q-quoted
		return
	}
	if outcome == "buffer_expired" {
		writeBufferExpiredFrame(w, requestedTurnID)
	}
}

// writeBufferExpiredFrame emits a synthetic terminal SSE frame telling the
// client the live buffer is gone but the turn finished — the UI can link to the
// transcript via turn_id rather than showing a silently-blank turn. Best-effort:
// flush errors (client already gone) are ignored.
func writeBufferExpiredFrame(w http.ResponseWriter, turnID string) {
	payload, _ := json.Marshal(map[string]any{
		"type":    "buffer_expired",
		"message": "Turn completed. Refresh to see the final result.",
		"turn_id": turnID,
	})
	_, _ = fmt.Fprintf(w, "event: buffer_expired\ndata: %s\n\n", payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// replayEventsFromDB writes a slice of persisted events as SSE frames
// using the same framing the live buffer uses. Sets the SSE headers
// + flushes per event. caps filters the replay by the client's declared
// SSE capabilities (#194), matching the live Attach path; nil = no filter.
func replayEventsFromDB(w http.ResponseWriter, events []store.TurnEvent, caps map[SSECapability]bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	setSupportedCapabilitiesHeader(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	_ = writeCapabilitiesFrame(w, flusher)
	for _, e := range events {
		if !shouldEmit(caps, e.Name) {
			continue
		}
		if err := writeSSEFrame(w, flusher, bufferedEvent{
			ID:   e.EventID,
			Name: e.Name,
			Data: e.Data,
		}); err != nil {
			return err
		}
	}
	return nil
}

// handleInflight is a cheap JSON probe the client calls on mount /
// visibilitychange / online to decide whether to open a reattach
// stream. Returns {inflight, turn_id?, last_event_id?}.
func (s *Server) handleInflight(w http.ResponseWriter, r *http.Request, convID string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	entry, ok := s.getInflight(convID)
	if !ok || entry.buf == nil {
		writeJSON(w, map[string]any{"inflight": false})
		return
	}

	writeJSON(w, map[string]any{
		"inflight":      entry.IsRunning(),
		"turn_id":       entry.turnID,
		"last_event_id": entry.buf.HighestID(),
	})
}

// parseLastEventID extracts the `Last-Event-ID` header, falling back
// to the `last_event_id` query param. Returns 0 for missing/invalid
// values — the caller will replay from the beginning.
func parseLastEventID(r *http.Request) uint64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	if raw == "" {
		return 0
	}
	var id uint64
	if _, err := fmt.Sscanf(raw, "%d", &id); err != nil {
		return 0
	}
	return id
}
