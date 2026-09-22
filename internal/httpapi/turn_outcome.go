// Per-turn outcome probe: GET /conversations/{id}/turns/{turn_id} (#1593).
//
// /inflight answers "is anything running on this conversation" and the
// conversation history answers "is there an answer"; neither answers "what
// became of THIS turn", so the client had to infer it from the two together —
// and several shapes are genuinely undecidable that way. The worst is a turn
// that ended before producing a reply (turn.model_required, a pre-answer
// provider error): nothing is live, nothing is retained, and the transcript
// ends at the user's prompt, which is indistinguishable from a turn that
// simply produced nothing. The client then renders the question with no answer
// and no Retry — the wrong affordance for a failure.
//
// The server has recorded the answer all along (turns.status since migration
// 002, the terminal frame in turn_events, the user entry's turn_seq 1 row).
// This endpoint is a read over that record; it adds no state and no schema.

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/store"
)

// Turn states as the client sees them. Deliberately NOT store.TurnStatus
// verbatim: `error` is a status, `failed` is what the UI is deciding about,
// and a turn the server is still running has no row-level state of its own —
// it is the absence of a terminal one.
const (
	turnStateRunning   = "running"
	turnStateCompleted = "completed"
	turnStateFailed    = "failed"
	turnStateCancelled = "cancelled"
)

// Failure reasons. `model_required` is broken out because the engine emits
// turn.model_required INSTEAD of turn.error for a failure the user can fix by
// picking another model, and the client already has a banner for exactly that
// payload — telling it apart here is what lets the banner survive a dropped
// socket instead of degrading to a generic error.
const (
	turnReasonModelRequired = "model_required"
	turnReasonError         = "error"
	turnReasonCancelled     = "cancelled"
)

// turnOutcomeResponse is the JSON body. `detail` is the terminal SSE frame's
// payload verbatim, so a client that already parses turn.model_required /
// turn.error / turn.cancelled frames needs no second vocabulary for them.
type turnOutcomeResponse struct {
	TurnID string `json:"turn_id"`
	State  string `json:"state"`
	// Reason is set only on a non-completed state, and only when the frame
	// that sealed the turn survived to the ledger.
	Reason string `json:"reason,omitempty"`
	// Detail is raw so an unparseable payload cannot fail the whole probe:
	// the state is the load-bearing field and it must still get through.
	Detail json.RawMessage `json:"detail,omitempty"`
	// UserCommitted closes the registered-before-committed window: false on a
	// running turn means the transcript legitimately does not hold this
	// turn's prompt yet.
	UserCommitted bool  `json:"user_committed"`
	Lossy         bool  `json:"lossy,omitempty"`
	FinishedAt    int64 `json:"finished_at,omitempty"`
}

// turnStateFor maps the persisted status to the client-facing state. A status
// this build does not know is reported as running rather than guessed at: a
// client that keeps waiting and re-asks is always recoverable, one that stamps
// a wrong terminal verdict is not.
func turnStateFor(status store.TurnStatus) string {
	switch status {
	case store.TurnStatusCompleted:
		return turnStateCompleted
	case store.TurnStatusCancelled:
		return turnStateCancelled
	case store.TurnStatusError:
		return turnStateFailed
	default:
		return turnStateRunning
	}
}

// turnReasonFor names the terminal frame that sealed the turn. Empty when the
// turn is still running, or when it died before the frame reached the ledger —
// an honest "the record does not say", which the client renders as its generic
// failure copy rather than inventing a cause.
func turnReasonFor(eventName string) string {
	switch eventName {
	case "turn.model_required":
		return turnReasonModelRequired
	case "turn.error":
		return turnReasonError
	case "turn.cancelled":
		return turnReasonCancelled
	default:
		return ""
	}
}

// newTurnOutcomeResponse shapes the persisted record for the wire.
//
// liveRunning is the in-memory registry's view of the SAME turn. It overrides
// a terminal row in one direction only — never the other — because the two
// disagree for exactly one reason: turnBuffer.Finish seals the buffer first
// and issues FinishTurn last, so between those a turn is over in memory while
// the row still says running. Reporting running there is the safe read (the
// client waits and re-asks); the reverse — calling a registered, running turn
// finished because a row had not caught up — is the verdict-from-an-absence
// this endpoint exists to prevent.
func newTurnOutcomeResponse(rec *store.TurnOutcomeRecord, liveRunning bool) turnOutcomeResponse {
	out := turnOutcomeResponse{
		TurnID:        rec.TurnID,
		State:         turnStateFor(rec.Status),
		UserCommitted: rec.UserCommitted,
		Lossy:         rec.Lossy,
	}
	if liveRunning {
		out.State = turnStateRunning
	}
	if out.State == turnStateRunning {
		// A running turn has no outcome yet. Suppress any frame the ledger
		// happens to hold (a retained buffer's, mid-seal) rather than
		// advertising a reason for a turn that is still going.
		return out
	}
	out.Reason = turnReasonFor(rec.TerminalEvent)
	if json.Valid([]byte(rec.TerminalData)) {
		out.Detail = json.RawMessage(rec.TerminalData)
	}
	if rec.FinishedAt.Valid {
		out.FinishedAt = rec.FinishedAt.Int64
	}
	return out
}

// handleTurnOutcome answers GET /conversations/{id}/turns/{turn_id}.
//
// Ownership is the conversation's, resolved the same way every other
// conversation sub-route resolves it, and then folded into the turn lookup's
// own WHERE clause — so a caller cannot read a turn of a conversation they do
// not own even if this handler's check were later dropped (#1112).
func (s *Server) handleTurnOutcome(w http.ResponseWriter, r *http.Request, convID, turnID string) {
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

	entry, registered := s.getInflight(convID)
	liveRunning := registered && entry.turnID == turnID && entry.IsRunning()

	rec, err := s.store.LookupTurnOutcome(r.Context(), turnID, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rec == nil {
		if liveRunning {
			// Registered but not yet in the turns table: postChat creates the
			// row right after registering the buffer, and a probe landing in
			// that window must not report the turn missing.
			writeJSON(w, turnOutcomeResponse{TurnID: turnID, State: turnStateRunning})
			return
		}
		http.Error(w, "turn not found", http.StatusNotFound)
		return
	}
	writeJSON(w, newTurnOutcomeResponse(rec, liveRunning))
}
