// Cursor-paginated turn-event read path (#189):
// GET /conversations/{id}/events. Split out of server.go (#1127).

package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ElcanoTek/fleet/internal/store"
)

// turnEventJSON is one event in a paginated page response. Mirrors the on-disk
// row plus the denormalised turn_index, with `data` re-exposed as raw JSON so
// the client gets the original payload, not a double-encoded string.
type turnEventJSON struct {
	Sequence  int64           `json:"sequence"`
	TurnID    string          `json:"turn_id"`
	TurnIndex int             `json:"turn_index"`
	EventID   uint64          `json:"event_id"`
	Name      string          `json:"event_name"`
	Data      json.RawMessage `json:"data"`
	CreatedAt int64           `json:"created_at"`
}

// turnEventPageResponse is the JSON body of GET /conversations/{id}/events.
// Cursors are strings so a future opaque encoding doesn't change the wire
// contract; "" means "no further page in that direction".
type turnEventPageResponse struct {
	Events     []turnEventJSON `json:"events"`
	NextCursor string          `json:"next_cursor"`
	PrevCursor string          `json:"prev_cursor"`
	HasMore    bool            `json:"has_more"`
}

// handleTurnEventsPage serves the cursor-paginated turn-event read path (#189):
//
//	GET /conversations/{id}/events?cursor={seq}&limit={n}&direction={asc|desc}
//
// It is purely additive — the existing GET /conversations/{id} (full history)
// and the /stream reattach path are untouched, so omitting this endpoint is the
// current behaviour. Auth is the standard conversation ownership check (the
// caller must own the conversation), matching every other /conversations route.
//
//   - cursor: opaque int64 sequence; 0 (or absent) is the sentinel for
//     head/tail depending on direction. Clients treat it as opaque.
//   - limit: default store.DefaultTurnEventPageLimit, capped at
//     store.MaxTurnEventPageLimit by the store.
//   - direction: "desc" (default — newest first, for initial load) or "asc"
//     (forward from cursor, for reconnect/catch-up).
func (s *Server) handleTurnEventsPage(w http.ResponseWriter, r *http.Request, convID string) {
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

	q := r.URL.Query()

	// direction → asc flag. Default desc (newest first) per the issue.
	asc := false
	switch q.Get("direction") {
	case "", "desc":
		asc = false
	case "asc":
		asc = true
	default:
		http.Error(w, "direction must be 'asc' or 'desc'", http.StatusBadRequest)
		return
	}

	// cursor: optional non-negative int64. Absent/"" = 0 sentinel.
	var cursor int64
	if raw := q.Get("cursor"); raw != "" {
		cursor, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			http.Error(w, "cursor must be a non-negative integer", http.StatusBadRequest)
			return
		}
	}

	// limit: optional. Reject obviously-bad input; the store clamps the upper
	// bound to MaxTurnEventPageLimit, but a non-numeric or negative value is a
	// client bug worth a 400.
	limit := store.DefaultTurnEventPageLimit
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
	}

	events, nextCursor, err := s.store.GetTurnEventPage(r.Context(), convID, cursor, limit, asc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := turnEventPageResponse{
		Events: make([]turnEventJSON, 0, len(events)),
	}
	for _, e := range events {
		data := json.RawMessage(e.Data)
		if len(data) == 0 {
			data = json.RawMessage("null")
		}
		resp.Events = append(resp.Events, turnEventJSON{
			Sequence:  e.Sequence,
			TurnID:    e.TurnID,
			TurnIndex: e.TurnIndex,
			EventID:   e.EventID,
			Name:      e.Name,
			Data:      data,
			CreatedAt: e.CreatedAt,
		})
	}

	// The store always returns the page ascending. nextCursor advances in the
	// REQUESTED direction: for asc it's the page's high edge (forward), for desc
	// it's the page's low edge (further back / scroll-up). Surface it under the
	// matching field name so the client doesn't have to know the direction math.
	resp.HasMore = nextCursor != 0
	if asc {
		if nextCursor != 0 {
			resp.NextCursor = strconv.FormatInt(nextCursor, 10)
		}
		// For an ascending page, "previous" is the low edge of what we returned.
		if len(events) > 0 {
			resp.PrevCursor = strconv.FormatInt(events[0].Sequence, 10)
		}
	} else {
		if nextCursor != 0 {
			resp.PrevCursor = strconv.FormatInt(nextCursor, 10)
		}
		// For a descending page, "next" (toward newer) is the high edge.
		if len(events) > 0 {
			resp.NextCursor = strconv.FormatInt(events[len(events)-1].Sequence, 10)
		}
	}

	writeJSON(w, resp)
}
