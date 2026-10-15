package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	status   map[string]int    // by endpoint URL; 0 → 201
	reply    map[string]string // response body by endpoint URL; default empty
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
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(f.reply[req.URL.String()]))}, nil
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
	if err := svc.sendToUser(context.Background(), "u@x.com", "t", "b", "", webpushgo.UrgencyNormal); err != nil {
		t.Errorf("nil SendToUser: %v", err)
	}
	if err := svc.SendEvent(context.Background(), notify.Event{Status: notify.StatusSuccess, Audience: "u@x.com"}); err != nil {
		t.Errorf("nil SendEvent: %v", err)
	}
	if err := svc.NotifyApprovalRequired(context.Background(), "u@x.com", "conv-1", "send_email"); err != nil {
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

	if err := svc.sendToUser(context.Background(), "u@x.com", "secret-title", "secret-body", "https://fleet.example.com/x", webpushgo.UrgencyNormal); err != nil {
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
	if err := svc.sendToUser(context.Background(), "nobody@x.com", "t", "", "", webpushgo.UrgencyNormal); err != nil {
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

	if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", webpushgo.UrgencyNormal); err != nil {
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

// A budget soft alert travels as StatusProgress (the On-list filter and the
// webhook treat it as a heads-up) but nothing waits on an answer, so it must
// not read as an ask-pause.
func TestEventTitle_BudgetAlert(t *testing.T) {
	ev := notify.Event{
		Status: notify.StatusProgress,
		Kind:   notify.KindBudgetAlert,
		Name:   "Budget alert: user alice@example.com (day window)",
	}
	got, ok := eventTitle(ev)
	if !ok || got != "⚠ Budget alert: user alice@example.com (day window)" {
		t.Errorf("eventTitle(budget alert) = %q ok=%v", got, ok)
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
	if err := svc.NotifyApprovalRequired(context.Background(), "u@x.com", "conv-1", "mcp_sendgrid_send_email"); err != nil {
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
	if err := svc.NotifyApprovalRequired(context.Background(), "u@x.com", "conv-1", "x"); err != nil {
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

// The approval-finished and resume-skipped pushes: one send each, the
// urgency that matches whether the person is blocking anything, and the same
// FLEET_PUSH_ON_APPROVAL_REQUEST gate.
func TestNotifyApprovalFinishedAndResumeSkipped(t *testing.T) {
	st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://relay.example/ep")}}
	relay := &fakeRelay{}
	svc := newTestService(t, st, relay)

	if err := svc.NotifyApprovalFinished(context.Background(), "u@x.com", "conv-1", "Create 12 deals on PubMatic", false); err != nil {
		t.Fatalf("NotifyApprovalFinished: %v", err)
	}
	if err := svc.NotifyResumeSkipped(context.Background(), "u@x.com", "conv-1"); err != nil {
		t.Fatalf("NotifyResumeSkipped: %v", err)
	}
	if len(relay.requests) != 2 {
		t.Fatalf("got %d relay requests, want 2", len(relay.requests))
	}
	if u := relay.requests[0].Header.Get("Urgency"); u != "normal" {
		t.Errorf("finished Urgency = %q, want normal", u)
	}
	if u := relay.requests[1].Header.Get("Urgency"); u != "high" {
		t.Errorf("resume-skipped Urgency = %q, want high", u)
	}

	svc.cfg.OnApprovalRequest = false
	_ = svc.NotifyApprovalFinished(context.Background(), "u@x.com", "conv-1", "x", true)
	_ = svc.NotifyResumeSkipped(context.Background(), "u@x.com", "conv-1")
	if len(relay.requests) != 2 {
		t.Error("FLEET_PUSH_ON_APPROVAL_REQUEST=false must suppress both")
	}
	var nilSvc *Service
	if err := nilSvc.NotifyApprovalFinished(context.Background(), "u@x.com", "c", "x", false); err != nil {
		t.Errorf("nil NotifyApprovalFinished: %v", err)
	}
}

func TestApprovalFinishedTitle(t *testing.T) {
	if got := approvalFinishedTitle("Create 12 deals", false); got != "✓ Done: Create 12 deals" {
		t.Errorf("success title = %q", got)
	}
	if got := approvalFinishedTitle("mcp_x_execute_plan", true); got != "✗ Not applied: mcp_x_execute_plan" {
		t.Errorf("failure title = %q", got)
	}
}

// A chat notification opens its conversation (?c=, which the chat page
// consumes on load), escaped, under FLEET_PUBLIC_URL or relative to the app.
func TestConversationLink(t *testing.T) {
	svc := &Service{cfg: Config{PublicURLBase: "https://fleet.example.com"}}
	if got := svc.ConversationLink("abc-123"); got != "https://fleet.example.com/chat?c=abc-123" {
		t.Errorf("link = %q", got)
	}
	if got := svc.ConversationLink("a&b c"); got != "https://fleet.example.com/chat?c=a%26b+c" {
		t.Errorf("escaped link = %q", got)
	}
	if got := svc.ConversationLink(""); got != "https://fleet.example.com" {
		t.Errorf("no conversation = %q, want the app root", got)
	}
	if got := (&Service{}).ConversationLink("abc"); got != "/chat?c=abc" {
		t.Errorf("relative link = %q", got)
	}
}

// A relay that refuses this server's VAPID authentication keeps the row: after
// a key rotation that is usually a subscription made with an earlier key,
// which the browser renews. Push services differ on the code (FCM answers 403,
// others may answer 401), and the same refusal also comes from a broken key
// pair, so the log names both causes for either code.
func TestSendOne_RefusedVAPIDKeepsTheRowAndNamesBothCauses(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			refused := testSubscription(t, "https://relay.example/old-key")
			st := &fakeStore{subs: []store.PushSubscription{refused}}
			relay := &fakeRelay{status: map[string]int{refused.Endpoint: code}}
			svc := newTestService(t, st, relay)
			var logged []string
			svc.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", webpushgo.UrgencyNormal); err != nil {
				t.Fatalf("sendToUser: %v", err)
			}
			if len(st.deleted) != 0 {
				t.Errorf("deleted %v; a refused key must not retire the row", st.deleted)
			}
			if len(logged) != 1 {
				t.Fatalf("log = %q, want one line", logged)
			}
			for _, want := range []string{fmt.Sprintf("status %d", code), "earlier key", "renews", "FLEET_VAPID_PRIVATE_KEY"} {
				if !strings.Contains(logged[0], want) {
					t.Errorf("log = %q, want it to mention %q", logged[0], want)
				}
			}
		})
	}
}

// vapidSubject decodes the "sub" claim of the VAPID token a send carried
// (Authorization: vapid t=<jwt>, k=<key>).
func vapidSubject(t *testing.T, req *http.Request) string {
	t.Helper()
	auth := req.Header.Get("Authorization")
	tok, _, _ := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ",")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("Authorization %q carries no JWT", auth)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT claims: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parse JWT claims: %v", err)
	}
	return claims.Sub
}

// The VAPID token carries exactly one "mailto:", however the operator wrote
// FLEET_VAPID_CONTACT. webpush-go adds the scheme itself, so the documented
// "mailto:…" form used to go out as "mailto:mailto:…", which Apple's push
// service refuses (403 BadJwtToken): Safari got no notifications at all.
func TestSendOne_VAPIDSubjectHasOneScheme(t *testing.T) {
	for _, tc := range []struct{ name, contact, want string }{
		{"mailto", "mailto:ops@example.com", "mailto:ops@example.com"},
		{"MAILTO", "MAILTO:ops@example.com", "mailto:ops@example.com"},
		{"padded", " mailto:ops@example.com ", "mailto:ops@example.com"}, // Load trims; the send path must not depend on it
		{"space after scheme", "mailto: ops@example.com", "mailto:ops@example.com"},
		{"bare address", "ops@example.com", "mailto:ops@example.com"},
		{"https", "https://ops.example.com/abuse", "https://ops.example.com/abuse"},
		{"HTTPS", "HTTPS://ops.example.com/abuse", "https://ops.example.com/abuse"}, // webpush-go checks for lower-case https: only
		{"Https", "Https://ops.example.com/abuse", "https://ops.example.com/abuse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://web.push.apple.com/QGd2EBk7PvmwJ")}}
			relay := &fakeRelay{}
			svc := newTestService(t, st, relay)
			svc.cfg.Contact = tc.contact

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", webpushgo.UrgencyNormal); err != nil {
				t.Fatalf("sendToUser: %v", err)
			}
			if len(relay.requests) != 1 {
				t.Fatalf("got %d requests, want 1", len(relay.requests))
			}
			if got := vapidSubject(t, relay.requests[0]); got != tc.want {
				t.Errorf("VAPID sub = %q, want %q", got, tc.want)
			}
		})
	}
}

// A refusal's log line carries the reason the push service gives (Apple puts
// it in a JSON "reason" field), and nothing else from the body.
func TestSendOne_RefusalLogsThePushServicesReason(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"apple 403", `{"reason":"BadJwtToken"}`, ` (reason "BadJwtToken")`, http.StatusForbidden},
		{"other refusal", `{"reason":"TooManyRequests"}`, ` (reason "TooManyRequests")`, http.StatusTooManyRequests},
		{"no body", "", "", http.StatusForbidden},
		{"not JSON", "<html>secret-token</html>", "", http.StatusForbidden},
		{"JSON without a reason", `{"message":"secret-token"}`, "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := testSubscription(t, "https://web.push.apple.com/secret-token")
			st := &fakeStore{subs: []store.PushSubscription{sub}}
			relay := &fakeRelay{status: map[string]int{sub.Endpoint: tc.status}, reply: map[string]string{sub.Endpoint: tc.body}}
			svc := newTestService(t, st, relay)
			var logged []string
			svc.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", webpushgo.UrgencyNormal); err != nil {
				t.Fatalf("sendToUser: %v", err)
			}
			if len(logged) != 1 {
				t.Fatalf("log = %q, want one line", logged)
			}
			head := fmt.Sprintf("returned status %d%s", tc.status, tc.want)
			if !strings.Contains(logged[0], head) {
				t.Errorf("log = %q, want it to contain %q", logged[0], head)
			}
			if strings.Contains(logged[0], "secret-token") {
				t.Errorf("log = %q quotes the body or the endpoint path", logged[0])
			}
		})
	}
}

func TestRelayReason_BoundsTheReason(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := relayReason(strings.NewReader(`{"reason":"` + long + `"}`))
	if want := fmt.Sprintf(" (reason %q)", strings.Repeat("x", maxReasonLen)); got != want {
		t.Errorf("relayReason = %q, want the reason cut to %d bytes", got, maxReasonLen)
	}
	// A cut through a multi-byte character drops it rather than leaving half.
	if got, want := relayReason(strings.NewReader(`{"reason":"`+strings.Repeat("x", maxReasonLen-1)+`é"}`)), fmt.Sprintf(" (reason %q)", strings.Repeat("x", maxReasonLen-1)); got != want {
		t.Errorf("relayReason = %q, want %q", got, want)
	}
	if got, want := relayReason(strings.NewReader(`{"reason":"a\nb"}`)), ` (reason "a\nb")`; got != want {
		t.Errorf("relayReason = %q, want %q: a newline is escaped, never raw in the log", got, want)
	}
}
