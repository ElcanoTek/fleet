// Browser Web Push subscription endpoints (#292). A browser that opted in on
// the settings page POSTs its PushSubscription here; the server later sends
// low-detail notifications (task complete/failed, approval needed, waiting
// for an answer) to every subscription the user holds via internal/webpush.
// Every handler is behind auth+membership, so userFromCtx is a provisioned
// user's email and a user can only manage their own subscriptions.

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// pushReady reports whether Web Push is configured; handlers short-circuit
// with 501 Not Implemented otherwise (the issue's contract — the capability
// does not exist on this server until the operator generates VAPID keys), so
// the UI can render a "not configured" hint. Mirrors remoteMCPReady.
func (s *Server) pushReady(w http.ResponseWriter) bool {
	if !s.push.Enabled() { // nil-safe: a nil *webpush.Service is disabled
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"push_disabled","detail":"web push is not configured on this server (run 'fleet generate-vapid-keys' and set FLEET_VAPID_PUBLIC_KEY, FLEET_VAPID_PRIVATE_KEY, FLEET_VAPID_CONTACT)"}`))
		return false
	}
	return true
}

// pushSubscribeRequest is the browser's PushSubscription serialized to JSON
// (sub.toJSON()); DELETE /push/unsubscribe accepts the same shape but only
// reads Endpoint.
type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		Auth   string `json:"auth"`
		P256dh string `json:"p256dh"`
	} `json:"keys"`
}

// pushSubscribe handles POST /push/subscribe: upsert the caller's
// subscription keyed on the relay endpoint. 204 on success.
func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.pushReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pushSubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" || req.Keys.Auth == "" || req.Keys.P256dh == "" {
		http.Error(w, "endpoint and keys (auth, p256dh) are required", http.StatusBadRequest)
		return
	}
	user := userFromCtx(r.Context())
	if err := s.store.UpsertPushSubscription(r.Context(), user, req.Endpoint, req.Keys.Auth, req.Keys.P256dh); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushUnsubscribe handles DELETE /push/unsubscribe: remove the caller's
// subscription for the given endpoint. Owner-scoped in the store so a user
// can never remove another's row; idempotent 204 either way.
func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.pushReady(w) {
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pushSubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" {
		http.Error(w, "endpoint is required", http.StatusBadRequest)
		return
	}
	user := userFromCtx(r.Context())
	if err := s.store.DeleteUserPushSubscription(r.Context(), user, req.Endpoint); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushVAPIDPublicKey handles GET /push/vapid-public-key: the non-secret
// application server key the browser subscribes with. Served here so the
// client never needs a build-time NEXT_PUBLIC_* embed of it.
func (s *Server) pushVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	if !s.pushReady(w) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]string{"key": s.push.PublicKey()})
}
