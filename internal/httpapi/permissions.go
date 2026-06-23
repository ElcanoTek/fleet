// Permission UI: human-in-the-loop gate for an EXTERNAL ACP agent's
// session/request_permission.
//
// An external agent (Claude Code / Goose) self-executes in its sandbox. When it
// wants to do something it deems sensitive it calls session/request_permission
// over ACP. fleet's ExternalRuntime routes that to a PermissionBroker, which we
// implement here: it emits a `permission.requested` SSE event (rendered as an
// inline allow/deny prompt in the chat) and BLOCKS the agent's turn until the
// human decides — or the request times out and DEFAULT-DENIES. There is no
// "approve all": each request is decided on its own.
//
// The decision arrives via POST /conversations/{id}/permissions/{requestId}.
// Because the agent's turn runs in the turn goroutine while the decision arrives
// on a separate HTTP request, the pending request lives in a process-level
// registry keyed by conversation+request id, holding the channel the broker
// blocks on.

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// pendingPermission is one in-flight permission request awaiting a human
// decision. decision delivers the human's answer to the blocked broker; it is
// buffered (cap 1) so a decision POST never blocks even if the broker has
// already given up (timeout) and stopped reading.
type pendingPermission struct {
	decision chan agent.PermissionDecision
}

// permissionRegistry holds the in-flight permission requests across the server,
// keyed by conversationID + requestID. It is the rendezvous between the turn
// goroutine (broker, blocked in RequestDecision) and the decision HTTP handler.
type permissionRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingPermission
}

func newPermissionRegistry() *permissionRegistry {
	return &permissionRegistry{pending: map[string]*pendingPermission{}}
}

func permKey(convID, requestID string) string { return convID + "\x00" + requestID }

// register adds a pending request and returns its channel. A duplicate key
// (should not happen — request ids are per-turn unique) replaces the old entry.
func (r *permissionRegistry) register(convID, requestID string) *pendingPermission {
	p := &pendingPermission{decision: make(chan agent.PermissionDecision, 1)}
	r.mu.Lock()
	r.pending[permKey(convID, requestID)] = p
	r.mu.Unlock()
	return p
}

// unregister removes a pending request (called by the broker when it stops
// waiting — decided or timed out — so the map never leaks).
func (r *permissionRegistry) unregister(convID, requestID string) {
	r.mu.Lock()
	delete(r.pending, permKey(convID, requestID))
	r.mu.Unlock()
}

// resolve delivers a decision to a pending request. Returns false when no such
// request is in flight (already resolved, timed out, or unknown) so the handler
// can answer idempotently.
func (r *permissionRegistry) resolve(convID, requestID string, dec agent.PermissionDecision) bool {
	r.mu.Lock()
	p, ok := r.pending[permKey(convID, requestID)]
	r.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case p.decision <- dec:
		return true
	default:
		// A decision is already queued (double-click): treat as resolved.
		return true
	}
}

// permissionBroker implements agent.PermissionBroker for one turn. It emits the
// SSE prompt on the turn's sink and blocks on the registry until the human
// decides or ctx (the runtime's per-request timeout) fires. On timeout/cancel it
// returns a DENY — the default-deny contract.
type permissionBroker struct {
	registry       *permissionRegistry
	conversationID string
	sink           agent.EventSink
}

var _ agent.PermissionBroker = (*permissionBroker)(nil)

func (b *permissionBroker) RequestDecision(ctx context.Context, req agent.PermissionRequest) (agent.PermissionDecision, error) {
	pending := b.registry.register(b.conversationID, req.RequestID)
	defer b.registry.unregister(b.conversationID, req.RequestID)

	// Surface the request to the human as an inline allow/deny prompt. The
	// frontend renders req.Title/Locations/Options and POSTs the decision back.
	b.sink.Emit("permission.requested", map[string]any{
		"request_id":   req.RequestID,
		"tool_call_id": req.ToolCallID,
		"title":        req.Title,
		"kind":         req.Kind,
		"locations":    req.Locations,
		"raw_input":    req.RawInput,
		"options":      req.Options,
	})

	select {
	case dec := <-pending.decision:
		return dec, nil
	case <-ctx.Done():
		// Default-deny on timeout / cancellation. Never an allow.
		return agent.PermissionDecision{Allowed: false}, nil
	}
}

// handlePermissionDecision resolves a pending permission request:
// POST /conversations/{id}/permissions/{requestId}  body {"allowed":bool,"option_id":"..."}.
// Idempotent: a decision for an unknown/already-resolved request returns 200
// with resolved:false so a late or double click is harmless.
func (s *Server) handlePermissionDecision(w http.ResponseWriter, r *http.Request, convID, requestID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Owner-scoped, like every other conversation route: confirm the caller owns
	// this conversation before resolving its pending permission request.
	// Otherwise a member who learns another user's convID + a guessable perm-N
	// requestID could force-allow/deny that user's blocked external-agent action,
	// defeating the human-in-the-loop gate. (The permission registry is keyed
	// only on convID+requestID with no user binding, so there is no SQL backstop
	// here — the explicit Get is the gate.)
	conv, err := s.store.Get(r.Context(), userFromCtx(r.Context()), convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	var req struct {
		Allowed  bool   `json:"allowed"`
		OptionID string `json:"option_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	resolved := s.permissions.resolve(convID, requestID, agent.PermissionDecision{
		Allowed:  req.Allowed,
		OptionID: req.OptionID,
	})
	writeJSON(w, map[string]any{"resolved": resolved})
}
