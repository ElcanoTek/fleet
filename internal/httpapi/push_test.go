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
