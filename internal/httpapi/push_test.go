package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/webpush"
)

// When Web Push is unconfigured (s.push == nil), every /push endpoint must
// fail closed with 501 Not Implemented and a machine-readable reason pointing
// at `fleet generate-vapid-keys` — never panic or touch the store.
// DB-independent: the disabled check short-circuits first.
func TestPushEndpointsDisabledReturn501(t *testing.T) {
	s := &Server{} // push nil → feature disabled

	cases := []struct {
		name, method, path string
		handler            http.HandlerFunc
	}{
		{"subscribe", http.MethodPost, "/push/subscribe", s.pushSubscribe},
		{"unsubscribe", http.MethodDelete, "/push/unsubscribe", s.pushUnsubscribe},
		{"status", http.MethodPost, "/push/status", s.pushStatus},
		{"renew", http.MethodPost, "/push/renew", s.pushRenew},
		{"vapid-key", http.MethodGet, "/push/vapid-public-key", s.pushVAPIDPublicKey},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
		w := httptest.NewRecorder()
		tc.handler(w, req)
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s: status %d, want 501", tc.name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "push_disabled") {
			t.Errorf("%s: body %q missing push_disabled", tc.name, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "generate-vapid-keys") {
			t.Errorf("%s: body %q missing the setup hint", tc.name, w.Body.String())
		}
	}
}

// A wrong verb is the client's mistake whether or not push is configured:
// the method check runs BEFORE the readiness check, so an unconfigured box
// answers 405 (not the 501 that misreported a routing bug as a missing
// feature). DB-independent like the 501 test above.
func TestPushEndpointsWrongMethodIs405EvenWhenDisabled(t *testing.T) {
	s := &Server{} // push nil → feature disabled

	cases := []struct {
		name, method, path string
		handler            http.HandlerFunc
	}{
		{"subscribe", http.MethodGet, "/push/subscribe", s.pushSubscribe},
		{"unsubscribe", http.MethodGet, "/push/unsubscribe", s.pushUnsubscribe},
		{"status", http.MethodGet, "/push/status", s.pushStatus},
		{"renew", http.MethodGet, "/push/renew", s.pushRenew},
		{"vapid-key", http.MethodPost, "/push/vapid-public-key", s.pushVAPIDPublicKey},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
		w := httptest.NewRecorder()
		tc.handler(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", tc.name, w.Code)
		}
	}
}

// pushFixture wires a DB-backed Server with an ENABLED (placeholder-keyed)
// push service — enough for the subscription CRUD endpoints, which never
// contact a relay. The key values are obvious non-secrets.
func pushFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := serverFixture(t)
	s.push = webpush.New(webpush.Config{
		VAPIDPublicKey:  "test-public-key",
		VAPIDPrivateKey: "test-private-key",
		Contact:         "mailto:ops@example.com",
	}, s.concreteStore(t))
	if !s.push.Enabled() {
		t.Fatal("push fixture not enabled")
	}
	return s, s.Routes()
}

// TestPushSubscribeLifecycle drives the full HTTP lifecycle against Postgres:
// subscribe (204, row visible), re-subscribe (idempotent upsert), the
// user-scoped unsubscribe, and the public-key read.
func TestPushSubscribeLifecycle(t *testing.T) {
	s, h := pushFixture(t)
	ctx := context.Background()
	st := s.concreteStore(t)

	sub := map[string]any{
		"endpoint": "https://relay.example/ep1",
		"keys":     map[string]string{"auth": "auth-b64", "p256dh": "p256dh-b64"},
	}
	if w := do(t, h, http.MethodPost, "/push/subscribe", sub, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d body=%s", w.Code, w.Body.String())
	}
	// Idempotent re-subscribe.
	if w := do(t, h, http.MethodPost, "/push/subscribe", sub, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("re-subscribe: %d body=%s", w.Code, w.Body.String())
	}
	subs, err := st.ListPushSubscriptions(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 || subs[0].Endpoint != "https://relay.example/ep1" || subs[0].KeysAuth != "auth-b64" {
		t.Fatalf("stored rows: %+v", subs)
	}

	// The VAPID public key is served so the client needn't embed it.
	w := do(t, h, http.MethodGet, "/push/vapid-public-key", nil, "u@x.com")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"key":"test-public-key"`) {
		t.Fatalf("vapid-public-key: %d body=%s", w.Code, w.Body.String())
	}

	// Another user cannot unsubscribe the row (idempotent 204, row survives).
	if w := do(t, h, http.MethodDelete, "/push/unsubscribe", sub, "other@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("cross-user unsubscribe: %d", w.Code)
	}
	if subs, _ = st.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 1 {
		t.Fatal("cross-user unsubscribe removed the row")
	}

	// The owner unsubscribes.
	if w := do(t, h, http.MethodDelete, "/push/unsubscribe", sub, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: %d body=%s", w.Code, w.Body.String())
	}
	if subs, _ = st.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 0 {
		t.Fatalf("row survived unsubscribe: %+v", subs)
	}
}

// TestPushStatus is the check behind the settings switch: it answers for the
// caller's own rows only, so a browser holding another account's subscription
// (a shared browser) or one the server dropped reads as not subscribed.
func TestPushStatus(t *testing.T) {
	_, h := pushFixture(t)
	sub := map[string]any{
		"endpoint": "https://relay.example/ep-status",
		"keys":     map[string]string{"auth": "auth-b64", "p256dh": "p256dh-b64"},
	}
	status := func(user, endpoint string) string {
		t.Helper()
		w := do(t, h, http.MethodPost, "/push/status", map[string]any{"endpoint": endpoint}, user)
		if w.Code != http.StatusOK {
			t.Fatalf("status for %s: %d body=%s", user, w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}

	if got := status("u@x.com", "https://relay.example/ep-status"); got != `{"subscribed":false}` {
		t.Fatalf("before subscribing: %s", got)
	}
	if w := do(t, h, http.MethodPost, "/push/subscribe", sub, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d body=%s", w.Code, w.Body.String())
	}
	if got := status("u@x.com", "https://relay.example/ep-status"); got != `{"subscribed":true}` {
		t.Errorf("owner: %s", got)
	}
	// The same browser endpoint, asked about by another account.
	if got := status("other@x.com", "https://relay.example/ep-status"); got != `{"subscribed":false}` {
		t.Errorf("another account: %s", got)
	}
	// The owner's row is gone (unsubscribed, or reaped after a 404/410).
	if w := do(t, h, http.MethodDelete, "/push/unsubscribe", sub, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: %d", w.Code)
	}
	if got := status("u@x.com", "https://relay.example/ep-status"); got != `{"subscribed":false}` {
		t.Errorf("after unsubscribe: %s", got)
	}
	if w := do(t, h, http.MethodPost, "/push/status", map[string]any{}, "u@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("missing endpoint: %d, want 400", w.Code)
	}
}

// A viewer can ask whether this browser is subscribed: /push/status is a
// read, so the viewer-write guard must not sit in front of it, while the
// write routes keep it.
func TestPushStatus_ViewerCanAsk(t *testing.T) {
	s := memberFixture(t, "viewer@x.com")
	setRole(t, s, "viewer@x.com", "viewer", "")
	s.push = webpush.New(webpush.Config{
		VAPIDPublicKey:  "test-public-key",
		VAPIDPrivateKey: "test-private-key",
		Contact:         "mailto:ops@example.com",
	}, s.concreteStore(t))
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/push/status", map[string]any{"endpoint": "https://relay.example/v"}, "viewer@x.com")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"subscribed":false}` {
		t.Fatalf("viewer status: %d %s", w.Code, w.Body.String())
	}
	sub := map[string]any{"endpoint": "https://relay.example/v", "keys": map[string]string{"auth": "a", "p256dh": "p"}}
	if w := do(t, h, http.MethodPost, "/push/subscribe", sub, "viewer@x.com"); w.Code != http.StatusForbidden {
		t.Errorf("viewer subscribe: %d, want 403 (the write guard still applies)", w.Code)
	}
}

// TestPushRenew is the move a browser makes after a VAPID key rotation: the
// caller's row follows the new subscription, and a browser holding someone
// else's subscription cannot hand it to the account signed in now.
func TestPushRenew(t *testing.T) {
	s, h := pushFixture(t)
	st := s.concreteStore(t)
	keys := map[string]string{"auth": "auth-b64", "p256dh": "p256dh-b64"}
	if w := do(t, h, http.MethodPost, "/push/subscribe", map[string]any{"endpoint": "https://relay.example/old", "keys": keys}, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d body=%s", w.Code, w.Body.String())
	}
	renew := map[string]any{
		"old_endpoint": "https://relay.example/old",
		"endpoint":     "https://relay.example/new",
		"keys":         map[string]string{"auth": "auth-new", "p256dh": "p256dh-new"},
	}

	w := do(t, h, http.MethodPost, "/push/renew", renew, "other@x.com")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"renewed":false}` {
		t.Fatalf("another account: %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodPost, "/push/renew", renew, "u@x.com")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"renewed":true}` {
		t.Fatalf("owner: %d %s", w.Code, w.Body.String())
	}
	subs, err := st.ListPushSubscriptions(context.Background(), "u@x.com")
	if err != nil || len(subs) != 1 || subs[0].Endpoint != "https://relay.example/new" || subs[0].KeysAuth != "auth-new" {
		t.Fatalf("rows after renew: %+v (err %v)", subs, err)
	}

	delete(renew, "old_endpoint")
	if w := do(t, h, http.MethodPost, "/push/renew", renew, "u@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("missing old_endpoint: %d, want 400", w.Code)
	}
}

// TestApprovalStagerFirePushNotification exercises the approval-staged push
// trigger (#292): a stager without push wired is a no-op, and with push wired
// the detached send runs against the user's stored subscriptions (none here,
// so no relay is ever contacted) without blocking or panicking.
func TestApprovalStagerFirePushNotification(t *testing.T) {
	// nil push → guard short-circuits.
	bare := &approvalStager{userEmail: "u@x.com", conversationID: "c1"}
	bare.firePushNotification("send_email") // must not panic

	s, _ := pushFixture(t)
	stager := &approvalStager{
		store:          s.store,
		conversationID: "c1",
		userEmail:      "nobody-subscribed@x.com",
		push:           s.push,
	}
	stager.firePushNotification("send_email")
	// The send is fire-and-forget with no completion handle; give the detached
	// goroutine a moment to run its (empty-subscription) fan-out.
	time.Sleep(100 * time.Millisecond)
}

// TestPushSubscribeValidation rejects malformed bodies with 400 and wrong
// methods with 405.
func TestPushSubscribeValidation(t *testing.T) {
	_, h := pushFixture(t)

	// Missing keys.
	bad := map[string]any{"endpoint": "https://relay.example/ep"}
	if w := do(t, h, http.MethodPost, "/push/subscribe", bad, "u@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("subscribe missing keys: %d, want 400", w.Code)
	}
	// Missing endpoint on unsubscribe.
	if w := do(t, h, http.MethodDelete, "/push/unsubscribe", map[string]any{}, "u@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("unsubscribe missing endpoint: %d, want 400", w.Code)
	}
	// Wrong methods.
	if w := do(t, h, http.MethodGet, "/push/subscribe", nil, "u@x.com"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET subscribe: %d, want 405", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/push/vapid-public-key", map[string]any{}, "u@x.com"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST vapid-public-key: %d, want 405", w.Code)
	}
}
