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
	"context"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

// approvalStopHandle is one running stoppable call's stop signal.
type approvalStopHandle struct {
	ch   chan struct{}
	once sync.Once
	refs int // guarded by approvalStopRegistry.mu
	// mu orders accepting a stop (the durable mark plus the signal) against
	// finalizing the outcome: whichever takes it first decides, so the row's
	// stop mark and the outcome's stop line always agree.
	mu        sync.Mutex
	finalized bool
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
// nothing ran. The MCP transports refuse to send a call whose stop already
// fired (mcp.ErrStoppedBeforeSend); this is the same refusal one step
// earlier, before its scope's budget even starts.
var errStoppedBeforeSend = mcp.ErrStoppedBeforeSend

// approvalStoppable reports whether a running approved call of toolName offers
// Stop: an MCP tool in critical_tool_stop (handler-only cards never). A tool
// of a synthetic server (inline http_tools, a2a peers) never does, whatever
// key matches it: those transports neither observe the stop signal nor send a
// cancellation (Notify is a no-op), so the call would always run on while the
// card and the model's line said its server was asked to stop.
func approvalStoppable(toolName string) bool {
	return strings.HasPrefix(toolName, "mcp_") && !handlerOnlyApproval(toolName) &&
		!onSyntheticTransport(toolName) && agentcore.ApprovalStop(toolName)
}

// approvalStopRegistry holds the stop signal of each running stoppable call,
// by approval id. A signal is shared by everyone who armed it and lives until
// the last of them disarms it: the request that wins the claim keeps it for
// the call, while a concurrent request that loses the claim drops only its
// own reference, never the winner's. The zero value is ready.
type approvalStopRegistry struct {
	mu sync.Mutex
	m  map[string]*approvalStopHandle
}

// armApprovalStop takes a reference on a call's stop signal, creating it if
// needed. decideApproval arms it before the claim makes the row executing,
// so the stop route always finds it for a running call.
func (s *Server) armApprovalStop(approvalID string) *approvalStopHandle {
	r := &s.approvalStops
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]*approvalStopHandle)
	}
	h, ok := r.m[approvalID]
	if !ok {
		h = &approvalStopHandle{ch: make(chan struct{})}
		r.m[approvalID] = h
	}
	h.refs++
	return h
}

// approvalStopFor returns a running call's stop signal, or nil.
func (s *Server) approvalStopFor(approvalID string) *approvalStopHandle {
	r := &s.approvalStops
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[approvalID]
}

// disarmApprovalStop drops one reference; the signal is forgotten with the
// last one (the call's outcome is recorded, or the claim was lost).
func (s *Server) disarmApprovalStop(approvalID string) {
	r := &s.approvalStops
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.m[approvalID]; ok {
		if h.refs--; h.refs <= 0 {
			delete(r.m, approvalID)
		}
	}
}

// acceptApprovalStop records and signals a Stop unless the call's outcome was
// already finalized without one. accepted is whether the stop took (now or by
// an earlier request); marked is whether this request wrote the durable mark.
func (s *Server) acceptApprovalStop(ctx context.Context, h *approvalStopHandle, user, approvalID string) (marked, accepted bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finalized {
		return false, h.stopped(), nil
	}
	marked, err = s.store.MarkApprovalStopRequested(ctx, user, approvalID)
	if err != nil {
		return false, false, err
	}
	h.stop()
	return marked, true, nil
}

// finalizeApprovalStop closes a call's stop window as its outcome is about to
// be recorded and reports whether a stop was accepted before it. A Stop that
// arrives later is refused (acceptApprovalStop), so the outcome's stop line
// and the row's stop mark cannot diverge.
func (s *Server) finalizeApprovalStop(approvalID string) bool {
	h := s.approvalStopFor(approvalID)
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finalized = true
	return h.stopped()
}

// approvalStopNote is fleet's line after a stopped call's result, in the
// history the model reads (shared with the restart recovery).
const approvalStopNote = store.ApprovalStopNote

// approvalStoppedBeforeSendNote is fleet's line for a call the person stopped
// before it was sent: nothing reached the server, so nothing ran.
// approvalStopTrailerReserve is the room a stop line may take beside the
// narrowing line under the same result: the longest of the three stop lines
// (running, before send, after a restart) plus its separator for a stoppable
// tool, else 0. narrowApproval reserves it, so a narrowed call that is later
// stopped still fits the output limit with every line whole.
func approvalStopTrailerReserve(toolName string) int {
	if !approvalStoppable(toolName) {
		return 0
	}
	return max(len(approvalStopNote), len(approvalStoppedBeforeSendNote), len(store.ApprovalStopRecoveredNote)) + 2
}

const approvalStoppedBeforeSendNote = "[fleet] The person pressed Stop before this call was sent, so nothing was sent to its server and nothing ran. Do not run it unless the person asks. (Written by fleet, not the tool.)"

// withApprovalStop adds the stop fields to a card payload: `stoppable` for a
// tool in critical_tool_stop, `stopping` on a running call the person
// stopped, `stopped` on a settled one. Absent for every other card, so its
// payload is unchanged.
func withApprovalStop(payload map[string]any, a *store.Approval) map[string]any {
	if a == nil {
		return payload
	}
	executing := payload["executing"] == true
	// A settled stop is history: it follows the stored mark, whatever the
	// policy says now (an operator may have removed the tool from
	// critical_tool_stop since).
	if a.StopRequestedAt > 0 && !executing {
		payload["stopped"] = true
	}
	// Offering Stop, and a stop still in progress, follow the current policy.
	if !approvalStoppable(a.ToolName) {
		return payload
	}
	payload["stoppable"] = true
	if a.StopRequestedAt > 0 && executing {
		payload["stopping"] = true
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
	// The call's signal is armed before its claim and dropped only after its
	// outcome is recorded, so a running call has one. None means the outcome
	// is being or has been recorded: nothing to stop, answer the state.
	h := s.approvalStopFor(approvalID)
	if h == nil {
		s.resolvedApprovalDecision(r.Context(), user, approvalID).write(w)
		return
	}
	marked, accepted, err := s.acceptApprovalStop(r.Context(), h, user, approvalID)
	if err != nil {
		http.Error(w, "could not record the stop", http.StatusInternalServerError)
		return
	}
	if marked {
		//nolint:gosec // G706: every interpolated value is rendered with %q, which escapes any CR/LF.
		log.Printf("audit: %q stopped running approval %q (%q) in conversation %q", user, approvalID, a.ToolName, convID)
	}
	if !accepted {
		// The call finished first: its outcome is being recorded without a
		// stop, and the row is not marked.
		s.resolvedApprovalDecision(r.Context(), user, approvalID).write(w)
		return
	}
	s.resolvedApprovalDecision(r.Context(), user, approvalID).write(w)
}
