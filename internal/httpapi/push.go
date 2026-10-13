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
// (sub.toJSON()); DELETE /push/unsubscribe and POST /push/status accept the
// same shape but only read Endpoint.
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
	// Method first, then readiness: a wrong verb is the client's mistake
	// whether or not push is configured, and answering it 501 on an
	// unconfigured box misreported a routing bug as a missing feature.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.pushReady(w) {
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
	// Method first, then readiness: a wrong verb is the client's mistake
	// whether or not push is configured, and answering it 501 on an
	// unconfigured box misreported a routing bug as a missing feature.
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.pushReady(w) {
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

// pushStatus handles POST /push/status: {"subscribed": true} when the caller
// holds the subscription for the given endpoint. The settings switch asks this
// about the browser's own subscription, because a browser can hold one that is
// not the signed-in user's — another account's on a shared browser, or one the
// server dropped — and the switch must not read "on" for it. The endpoint
// rides in the body, not the query string: it is a capability URL. A read, so
// no viewer-write guard; owner-scoped in the store like unsubscribe.
func (s *Server) pushStatus(w http.ResponseWriter, r *http.Request) {
	// Method first, then readiness, as for the other push routes.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.pushReady(w) {
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
	held, err := s.store.HasUserPushSubscription(r.Context(), userFromCtx(r.Context()), req.Endpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"subscribed": held})
}

// pushRenewRequest is a browser's new PushSubscription (sub.toJSON()) plus the
// endpoint of the one it replaces.
type pushRenewRequest struct {
	OldEndpoint string `json:"old_endpoint"`
	pushSubscribeRequest
}

// pushRenew handles POST /push/renew: move the caller's row from old_endpoint
// to the browser's new subscription, {"renewed": bool}. The browser renews
// after the operator rotates the VAPID keys — a subscription made with the old
// key is refused by the push service for every send signed with the new one.
// It answers false, changing nothing, unless the caller holds the old row, so
// a browser holding another account's subscription cannot hand it to whoever
// is signed in now; the browser then drops the subscription it just made.
func (s *Server) pushRenew(w http.ResponseWriter, r *http.Request) {
	// Method first, then readiness, as for the other push routes.
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.pushReady(w) {
		return
	}
	var req pushRenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OldEndpoint) == "" || strings.TrimSpace(req.Endpoint) == "" || req.Keys.Auth == "" || req.Keys.P256dh == "" {
		http.Error(w, "old_endpoint, endpoint and keys (auth, p256dh) are required", http.StatusBadRequest)
		return
	}
	renewed, err := s.store.RenewPushSubscription(r.Context(), userFromCtx(r.Context()), req.OldEndpoint, req.Endpoint, req.Keys.Auth, req.Keys.P256dh)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"renewed": renewed})
}

// pushVAPIDPublicKey handles GET /push/vapid-public-key: the non-secret
// application server key the browser subscribes with. Served here so the
// client never needs a build-time NEXT_PUBLIC_* embed of it.
func (s *Server) pushVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	// Method first, then readiness: a wrong verb is the client's mistake
	// whether or not push is configured, and answering it 501 on an
	// unconfigured box misreported a routing bug as a missing feature.
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.pushReady(w) {
		return
	}
	writeJSON(w, map[string]string{"key": s.push.PublicKey()})
}
