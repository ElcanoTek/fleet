package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	webpushgo "github.com/SherClockHolmes/webpush-go"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/store"
)

// testConfig returns an enabled Config with a freshly generated VAPID pair —
// real keys so webpush-go's VAPID signing + RFC 8291 encryption actually run.
func testConfig(t *testing.T) Config {
	t.Helper()
	priv, pub, err := webpushgo.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	return Config{
		VAPIDPublicKey:    pub,
		VAPIDPrivateKey:   priv,
		Contact:           "mailto:ops@example.com",
		OnTaskComplete:    true,
		OnApprovalRequest: true,
		PublicURLBase:     "https://fleet.example.com",
	}
}

// testSubscription fabricates a browser-shaped subscription: a valid P-256
// public point for p256dh and 16 random bytes for auth, so payload encryption
// succeeds exactly as it would against a real PushSubscription.
func testSubscription(t *testing.T, endpoint string) store.PushSubscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate p256dh: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth: %v", err)
	}
	return store.PushSubscription{
		UserEmail:  "u@x.com",
		Endpoint:   endpoint,
		KeysAuth:   base64.RawURLEncoding.EncodeToString(auth),
		KeysP256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
	}
}

// fakeStore is an in-memory SubscriptionStore recording deletes.
type fakeStore struct {
	mu      sync.Mutex
	subs    []store.PushSubscription
	deleted []string
}

func (f *fakeStore) ListPushSubscriptions(_ context.Context, userEmail string) ([]store.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.PushSubscription
	for _, s := range f.subs {
		if s.UserEmail == userEmail {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) DeletePushSubscription(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, endpoint)
	return nil
}

// fakeRelay is the HTTP seam handed to webpush-go: it records every request
// and answers with a per-endpoint status (default 201, a real relay's accept).
type fakeRelay struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	status   map[string]int // by endpoint URL; 0 → 201
}

func (f *fakeRelay) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	f.bodies = append(f.bodies, body)
	code := f.status[req.URL.String()]
	if code == 0 {
		code = http.StatusCreated
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// newTestService wires an enabled Service over the fakes, silencing logs.
func newTestService(t *testing.T, st *fakeStore, relay *fakeRelay) *Service {
	t.Helper()
	svc := New(testConfig(t), st)
	if svc == nil {
		t.Fatal("New returned nil for an enabled config")
	}
	svc.client = relay
	svc.logf = func(string, ...any) {}
	return svc
}

// TestConfigEnabled locks the gating contract: all three VAPID vars, or off.
func TestConfigEnabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("empty config must be disabled")
	}
	full := Config{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv", Contact: "mailto:a@b"}
	if !full.Enabled() {
		t.Error("fully configured must be enabled")
	}
	for _, drop := range []func(*Config){
		func(c *Config) { c.VAPIDPublicKey = "" },
		func(c *Config) { c.VAPIDPrivateKey = "" },
		func(c *Config) { c.Contact = "" },
	} {
		c := full
		drop(&c)
		if c.Enabled() {
			t.Errorf("config with a missing var must be disabled: %+v", c)
		}
	}
}

// TestLoad_DefaultOff: with no FLEET_VAPID_* set, the feature is off and New
// returns nil; the trigger flags still default true (opt-out semantics).
func TestLoad_DefaultOff(t *testing.T) {
	for _, k := range []string{
		"FLEET_VAPID_PUBLIC_KEY", "FLEET_VAPID_PRIVATE_KEY", "FLEET_VAPID_CONTACT",
		"FLEET_PUSH_ON_TASK_COMPLETE", "FLEET_PUSH_ON_APPROVAL_REQUEST", "FLEET_PUBLIC_URL",
	} {
		t.Setenv(k, "")
	}
	cfg := Load()
	if cfg.Enabled() {
		t.Error("Load with no env must be disabled")
	}
	if !cfg.OnTaskComplete || !cfg.OnApprovalRequest {
		t.Error("trigger flags must default true")
	}
	if svc := New(cfg, &fakeStore{}); svc != nil {
		t.Error("New must return nil for a disabled config")
	}
	t.Setenv("FLEET_PUSH_ON_TASK_COMPLETE", "false")
	t.Setenv("FLEET_PUSH_ON_APPROVAL_REQUEST", "0")
	cfg = Load()
	if cfg.OnTaskComplete || cfg.OnApprovalRequest {
		t.Error("explicit false must turn a trigger flag off")
	}
}

// TestNilServiceIsNoOp: every method on a nil *Service (feature off) is safe.
func TestNilServiceIsNoOp(t *testing.T) {
	var svc *Service
	if svc.Enabled() || svc.ApprovalPushEnabled() {
		t.Error("nil service must report disabled")
	}
	if svc.PublicKey() != "" {
		t.Error("nil service must have no public key")
	}
	if err := svc.SendToUser(context.Background(), "u@x.com", "t", "b", ""); err != nil {
		t.Errorf("nil SendToUser: %v", err)
	}
	if err := svc.SendEvent(context.Background(), notify.Event{Status: notify.StatusSuccess, Audience: "u@x.com"}); err != nil {
		t.Errorf("nil SendEvent: %v", err)
	}
	if err := svc.NotifyApprovalRequired(context.Background(), "u@x.com", "send_email"); err != nil {
		t.Errorf("nil NotifyApprovalRequired: %v", err)
	}
}

// TestSendToUser_FansOutEncrypted: one POST per subscription, VAPID-signed,
// TTL'd, and with the payload ENCRYPTED — the plaintext title must not appear
// on the wire (RFC 8291; the relay never sees content).
func TestSendToUser_FansOutEncrypted(t *testing.T) {
	st := &fakeStore{subs: []store.PushSubscription{
		testSubscription(t, "https://relay.example/ep1"),
		testSubscription(t, "https://relay.example/ep2"),
	}}
	relay := &fakeRelay{}
	svc := newTestService(t, st, relay)

	if err := svc.SendToUser(context.Background(), "u@x.com", "secret-title", "secret-body", "https://fleet.example.com/x"); err != nil {
		t.Fatalf("SendToUser: %v", err)
	}
	if len(relay.requests) != 2 {
		t.Fatalf("got %d relay requests, want 2", len(relay.requests))
	}
	for i, req := range relay.requests {
		if req.Method != http.MethodPost {
			t.Errorf("request %d: method %s, want POST", i, req.Method)
		}
		if auth := req.Header.Get("Authorization"); !strings.Contains(auth, "vapid") {
			t.Errorf("request %d: Authorization %q lacks VAPID", i, auth)
		}
		if ttl := req.Header.Get("TTL"); ttl != "3600" {
			t.Errorf("request %d: TTL %q, want 3600", i, ttl)
		}
		if bytes.Contains(relay.bodies[i], []byte("secret-title")) || bytes.Contains(relay.bodies[i], []byte("secret-body")) {
			t.Errorf("request %d: payload not encrypted — plaintext on the wire", i)
		}
	}
	if len(st.deleted) != 0 {
		t.Errorf("healthy sends must not delete subscriptions: %v", st.deleted)
	}
	// No subscriptions for another user: a clean no-op, no extra requests.
	if err := svc.SendToUser(context.Background(), "nobody@x.com", "t", "", ""); err != nil {
		t.Fatalf("SendToUser (no subs): %v", err)
	}
	if len(relay.requests) != 2 {
		t.Errorf("no-subscription send still hit the relay")
	}
}

// TestSendToUser_ExpiredSubscriptionDeleted: 404/410 from the relay retires
// the row; other failures do not, and never stop the fan-out.
func TestSendToUser_ExpiredSubscriptionDeleted(t *testing.T) {
	gone := testSubscription(t, "https://relay.example/gone")
	dead := testSubscription(t, "https://relay.example/dead")
	live := testSubscription(t, "https://relay.example/live")
	flaky := testSubscription(t, "https://relay.example/flaky")
	st := &fakeStore{subs: []store.PushSubscription{gone, dead, live, flaky}}
	relay := &fakeRelay{status: map[string]int{
		gone.Endpoint:  http.StatusGone,
		dead.Endpoint:  http.StatusNotFound,
		flaky.Endpoint: http.StatusInternalServerError,
	}}
	svc := newTestService(t, st, relay)

	if err := svc.SendToUser(context.Background(), "u@x.com", "t", "", ""); err != nil {
		t.Fatalf("SendToUser: %v", err)
	}
	if len(relay.requests) != 4 {
		t.Fatalf("got %d relay requests, want 4 (failures must not stop the fan-out)", len(relay.requests))
	}
	want := map[string]bool{gone.Endpoint: true, dead.Endpoint: true}
	if len(st.deleted) != 2 || !want[st.deleted[0]] || !want[st.deleted[1]] {
		t.Errorf("deleted %v, want exactly the 404/410 endpoints", st.deleted)
	}
}

// TestEventTitle locks the exact task-lifecycle render (#292 text). The
// payload is encrypted on the wire, so the text contract is asserted on the
// pure render function.
func TestEventTitle(t *testing.T) {
	ev := notify.Event{Name: "nightly report", DurationSeconds: 90}
	cases := []struct {
		status notify.Status
		want   string
	}{
		{notify.StatusSuccess, "✓ Task complete: nightly report (1m30s)"},
		{notify.StatusFailure, "✗ Task failed: nightly report (1m30s)"},
		{notify.StatusProgress, "⏸ Waiting for your answer: nightly report"},
	}
	for _, tc := range cases {
		ev.Status = tc.status
		got, ok := eventTitle(ev)
		if !ok || got != tc.want {
			t.Errorf("eventTitle(%s) = %q ok=%v, want %q", tc.status, got, ok, tc.want)
		}
	}
	ev.Status = notify.Status("weird")
	if _, ok := eventTitle(ev); ok {
		t.Error("unknown status must not render a push")
	}
}

// TestSendEvent_Gating covers delivery + the FLEET_PUSH_ON_TASK_COMPLETE /
// Audience gating.
func TestSendEvent_Gating(t *testing.T) {
	for _, status := range []notify.Status{notify.StatusSuccess, notify.StatusFailure, notify.StatusProgress} {
		st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://relay.example/ep")}}
		relay := &fakeRelay{}
		svc := newTestService(t, st, relay)
		ev := notify.Event{Status: status, Name: "nightly report", DurationSeconds: 90, Audience: "u@x.com", LogURL: "https://x/log"}
		if err := svc.SendEvent(context.Background(), ev); err != nil {
			t.Fatalf("SendEvent(%s): %v", status, err)
		}
		if len(relay.requests) != 1 {
			t.Fatalf("SendEvent(%s): %d relay requests, want 1", status, len(relay.requests))
		}
	}

	// Audience empty → no push (nothing to route to).
	st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://relay.example/ep")}}
	relay := &fakeRelay{}
	svc := newTestService(t, st, relay)
	if err := svc.SendEvent(context.Background(), notify.Event{Status: notify.StatusSuccess}); err != nil {
		t.Fatalf("SendEvent no audience: %v", err)
	}
	if len(relay.requests) != 0 {
		t.Error("audience-less event must not send")
	}

	// Flag off → no push even with an audience.
	svc.cfg.OnTaskComplete = false
	if err := svc.SendEvent(context.Background(), notify.Event{Status: notify.StatusSuccess, Audience: "u@x.com"}); err != nil {
		t.Fatalf("SendEvent flag off: %v", err)
	}
	if len(relay.requests) != 0 {
		t.Error("FLEET_PUSH_ON_TASK_COMPLETE=false must suppress the send")
	}
}

// TestNotifyApprovalRequired: high urgency, tool-name-only title, and the
// FLEET_PUSH_ON_APPROVAL_REQUEST gate.
func TestNotifyApprovalRequired(t *testing.T) {
	st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://relay.example/ep")}}
	relay := &fakeRelay{}
	svc := newTestService(t, st, relay)

	if !svc.ApprovalPushEnabled() {
		t.Fatal("ApprovalPushEnabled must be true for an enabled service")
	}
	if err := svc.NotifyApprovalRequired(context.Background(), "u@x.com", "mcp_sendgrid_send_email"); err != nil {
		t.Fatalf("NotifyApprovalRequired: %v", err)
	}
	if len(relay.requests) != 1 {
		t.Fatalf("got %d relay requests, want 1", len(relay.requests))
	}
	if urgency := relay.requests[0].Header.Get("Urgency"); urgency != "high" {
		t.Errorf("Urgency = %q, want high", urgency)
	}

	svc.cfg.OnApprovalRequest = false
	if svc.ApprovalPushEnabled() {
		t.Error("ApprovalPushEnabled must honor the flag")
	}
	if err := svc.NotifyApprovalRequired(context.Background(), "u@x.com", "x"); err != nil {
		t.Fatalf("NotifyApprovalRequired flag off: %v", err)
	}
	if len(relay.requests) != 1 {
		t.Error("FLEET_PUSH_ON_APPROVAL_REQUEST=false must suppress the send")
	}
}

// TestEndpointHost: the capability URL is reduced to its host for logs.
func TestEndpointHost(t *testing.T) {
	if got := endpointHost("https://fcm.googleapis.com/fcm/send/abc123secret"); got != "fcm.googleapis.com" {
		t.Errorf("endpointHost = %q", got)
	}
	if got := endpointHost("no-scheme-or-path"); got != "no-scheme-or-path" {
		t.Errorf("endpointHost fallback = %q", got)
	}
}
