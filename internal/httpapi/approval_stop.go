// Stopping a running approved call (docs/APPROVAL-STOP.md).
//
// For a tool the bundle lists in agent_policy.critical_tool_stop, the running
// card offers Stop. POST /conversations/{id}/approvals/{approvalId}/stop then:
//
//   - records the stop on the row (approvals.stop_requested_at, once, only
//     while the call is still executing): the durable audit record, and what
//     a reloaded card reads "Stopping" from;
//   - closes the call's stop signal, which the MCP client turns into the
//     spec's notifications/cancelled for that request (over the broker as a
//     methodStop frame) while it keeps waiting for the call's answer.
//
// Nothing else changes about the call: it is still the claimed approval's one
// execution, under its own budget, and its outcome is recorded by the same
// code as any approved call. A server that honours the stop finishes its
// current item and answers normally (its result says what was not run); one
// that hard-cancels answers an error, recorded as a failure; one that ignores
// it runs on to its end or its budget, exactly as an unstopped call. Fleet
// adds a labelled line after the result so the agent knows the person stopped
// it.

package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// approvalStopHandle is one running stoppable call's stop signal.
type approvalStopHandle struct {
	ch   chan struct{}
	once sync.Once
}

// stop closes the signal; a second call does nothing.
func (h *approvalStopHandle) stop() { h.once.Do(func() { close(h.ch) }) }

// stopped reports whether stop was called.
func (h *approvalStopHandle) stopped() bool {
	select {
	case <-h.ch:
		return true
	default:
		return false
	}
}

// errStoppedBeforeSend is the outcome of a call stopped before it was sent:
// nothing ran.
var errStoppedBeforeSend = errors.New("stopped before the call was sent to its server; nothing ran")

// approvalStoppable reports whether a running approved call of toolName offers
// Stop: an MCP tool in critical_tool_stop (handler-only cards never).
func approvalStoppable(toolName string) bool {
	return strings.HasPrefix(toolName, "mcp_") && !handlerOnlyApproval(toolName) && agentcore.ApprovalStop(toolName)
}

// armApprovalStop registers a claimed call's stop signal. It is called right
// after the claim, before the call starts, so a Stop that lands while the
// call's scope is still opening is not lost.
func (s *Server) armApprovalStop(approvalID string) {
	s.approvalStops.LoadOrStore(approvalID, &approvalStopHandle{ch: make(chan struct{})})
}

// approvalStopFor returns a running call's stop signal, or nil.
func (s *Server) approvalStopFor(approvalID string) *approvalStopHandle {
	v, ok := s.approvalStops.Load(approvalID)
	if !ok {
		return nil
	}
	h, _ := v.(*approvalStopHandle)
	return h
}

// disarmApprovalStop drops a call's stop signal once its outcome is recorded.
func (s *Server) disarmApprovalStop(approvalID string) {
	s.approvalStops.Delete(approvalID)
}

// approvalStopNote is fleet's line after a stopped call's result, in the
// history the model reads.
const approvalStopNote = "[fleet] The person pressed Stop while this call ran. Its server was asked to stop after the item it was working on; the result above says what ran and what did not. Do not run the remaining items unless the person asks. (Written by fleet, not the tool.)"

// withApprovalStop adds the stop fields to a card payload: `stoppable` for a
// tool in critical_tool_stop, `stopping` on a running call the person
// stopped, `stopped` on a settled one. Absent for every other card, so its
// payload is unchanged.
func withApprovalStop(payload map[string]any, a *store.Approval) map[string]any {
	if a == nil || !approvalStoppable(a.ToolName) {
		return payload
	}
	payload["stoppable"] = true
	if a.StopRequestedAt > 0 {
		if payload["executing"] == true {
			payload["stopping"] = true
		} else {
			payload["stopped"] = true
		}
	}
	return payload
}

// handleApprovalStop serves POST /conversations/{id}/approvals/{approvalId}/stop.
// It answers the card's state: still executing with `stopping: true` once the
// stop is recorded, or the settled outcome if the call already ended.
func (s *Server) handleApprovalStop(w http.ResponseWriter, r *http.Request, convID, approvalID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	a, err := s.store.GetApproval(r.Context(), user, approvalID)
	if err != nil {
		http.Error(w, "could not load approval", http.StatusInternalServerError)
		return
	}
	if a == nil || a.ConversationID != convID {
		http.Error(w, "approval not found", http.StatusNotFound)
		return
	}
	if !approvalStoppable(a.ToolName) {
		http.Error(w, "this action cannot be stopped once it runs", http.StatusBadRequest)
		return
	}
	if a.Status == "pending" {
		http.Error(w, "this action is not running: decline it instead", http.StatusConflict)
		return
	}
	if s.approvalOutcomeFlags(a)["executing"] != true {
		// Already settled: the stop has nothing to do; answer the outcome.
		writeJSON(w, s.approvalClientState(a))
		return
	}
	marked, err := s.store.MarkApprovalStopRequested(r.Context(), user, approvalID)
	if err != nil {
		http.Error(w, "could not record the stop", http.StatusInternalServerError)
		return
	}
	if h := s.approvalStopFor(approvalID); h != nil {
		h.stop()
	}
	if marked {
		//nolint:gosec // G706: every interpolated value is rendered with %q, which escapes any CR/LF.
		log.Printf("audit: %q stopped running approval %q (%q) in conversation %q", user, approvalID, a.ToolName, convID)
	}
	s.resolvedApprovalDecision(r.Context(), user, approvalID).write(w)
}
