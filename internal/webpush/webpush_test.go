package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/store"
)

// testConfig returns an enabled Config with a freshly generated VAPID pair —
// real keys so VAPID signing + RFC 8291 encryption actually run.
func testConfig(t *testing.T) Config {
	t.Helper()
	priv, pub, err := GenerateVAPIDKeys()
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

// fakeRelay is the HTTP seam every send goes through: it records every request
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
	if err := svc.sendToUser(context.Background(), "u@x.com", "t", "b", "", UrgencyNormal); err != nil {
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
		testSubscription(t, "https://fcm.googleapis.com/fcm/send/ep1"),
		testSubscription(t, "https://updates.push.services.mozilla.com/wpush/v2/ep2"),
	}}
	relay := &fakeRelay{}
	svc := newTestService(t, st, relay)

	if err := svc.sendToUser(context.Background(), "u@x.com", "secret-title", "secret-body", "https://fleet.example.com/x", UrgencyNormal); err != nil {
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
	if err := svc.sendToUser(context.Background(), "nobody@x.com", "t", "", "", UrgencyNormal); err != nil {
		t.Fatalf("SendToUser (no subs): %v", err)
	}
	if len(relay.requests) != 2 {
		t.Errorf("no-subscription send still hit the relay")
	}
}

// TestSendToUser_ExpiredSubscriptionDeleted: 404/410 from the relay retires
// the row; other failures do not, and never stop the fan-out.
func TestSendToUser_ExpiredSubscriptionDeleted(t *testing.T) {
	gone := testSubscription(t, "https://fcm.googleapis.com/fcm/send/gone")
	dead := testSubscription(t, "https://fcm.googleapis.com/fcm/send/dead")
	live := testSubscription(t, "https://fcm.googleapis.com/fcm/send/live")
	flaky := testSubscription(t, "https://fcm.googleapis.com/fcm/send/flaky")
	st := &fakeStore{subs: []store.PushSubscription{gone, dead, live, flaky}}
	relay := &fakeRelay{status: map[string]int{
		gone.Endpoint:  http.StatusGone,
		dead.Endpoint:  http.StatusNotFound,
		flaky.Endpoint: http.StatusInternalServerError,
	}}
	svc := newTestService(t, st, relay)

	if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
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

// The agent's mid-run notify tool also travels as StatusProgress, but the run
// carries on, so its note must not read as an ask-pause. The note itself
// (Message) stays off the lock screen.
func TestEventTitle_ProgressNote(t *testing.T) {
	ev := notify.Event{
		Status:  notify.StatusProgress,
		Kind:    notify.KindProgressNote,
		Name:    "nightly report",
		Message: "finished stage 1 of 3",
	}
	got, ok := eventTitle(ev)
	if !ok || got != "✎ Progress note: nightly report" {
		t.Errorf("eventTitle(progress note) = %q ok=%v", got, ok)
	}
}

// TestSendEvent_Gating covers delivery + the FLEET_PUSH_ON_TASK_COMPLETE /
// Audience gating.
func TestSendEvent_Gating(t *testing.T) {
	for _, status := range []notify.Status{notify.StatusSuccess, notify.StatusFailure, notify.StatusProgress} {
		st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://fcm.googleapis.com/fcm/send/ep")}}
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
	st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://fcm.googleapis.com/fcm/send/ep")}}
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
	st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://fcm.googleapis.com/fcm/send/ep")}}
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
	for in, want := range map[string]string{
		"https://fcm.googleapis.com/fcm/send/abc123secret":        "fcm.googleapis.com",
		"https://user:pw@relay.example:8443/ep?token=secret#frag": "relay.example:8443",
		"no-scheme-or-path": "(unparseable endpoint)",
		"https://%zz/ep":    "(unparseable endpoint)",
	} {
		if got := endpointHost(in); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The approval-finished and resume-skipped pushes: one send each, the
// urgency that matches whether the person is blocking anything, and the same
// FLEET_PUSH_ON_APPROVAL_REQUEST gate.
func TestNotifyApprovalFinishedAndResumeSkipped(t *testing.T) {
	st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://fcm.googleapis.com/fcm/send/ep")}}
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
			refused := testSubscription(t, "https://fcm.googleapis.com/fcm/send/old-key")
			st := &fakeStore{subs: []store.PushSubscription{refused}}
			relay := &fakeRelay{status: map[string]int{refused.Endpoint: code}}
			svc := newTestService(t, st, relay)
			var logged []string
			svc.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
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
// FLEET_VAPID_CONTACT. The VAPID signer adds the scheme itself, so the documented
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
		{"HTTPS", "HTTPS://ops.example.com/abuse", "https://ops.example.com/abuse"}, // the signer checks for lower-case https: only
		{"Https", "Https://ops.example.com/abuse", "https://ops.example.com/abuse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://web.push.apple.com/QGd2EBk7PvmwJ")}}
			relay := &fakeRelay{}
			svc := newTestService(t, st, relay)
			svc.cfg.Contact = tc.contact

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
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

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
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

// ValidateEndpoint admits the push service each major browser subscribes with
// and nothing else: the endpoint is browser-supplied, and fleet POSTs to it
// from the host.
func TestValidateEndpoint(t *testing.T) {
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/abc:APA91b",             // Chrome
		"https://fcm.googleapis.com/wp/abc",                          // Chrome, newer path
		"https://jmt17.google.com/fcm/send/abc:APA91b",               // Chrome Beta/Dev/Canary, Chromium
		"https://updates.push.services.mozilla.com/wpush/v2/gAAAAAB", // Firefox
		"https://web.push.apple.com/QGd2EBk7PvmwJ",                   // Safari
		"https://wns2-par02p.notify.windows.com/w/?token=BQYAAAB",    // Edge
		"https://FCM.GoogleAPIs.com/fcm/send/abc",                    // case
		"https://" + strings.Repeat("a", 63) + ".push.apple.com/x",   // the longest label
	} {
		if err := ValidateEndpoint(ok); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"not a url",
		"https://%zz/ep",
		"https:fcm.googleapis.com/fcm/send/abc",  // opaque, no host
		"http://fcm.googleapis.com/fcm/send/abc", // not https
		"https://u:p@fcm.googleapis.com/fcm/send/a", // user info
		"https://fcm.googleapis.com:8443/fcm/send/a",
		"https://fcm.googleapis.com:443/fcm/send/a", // browsers send no port; ":443" would sign the wrong VAPID audience
		"https://fcm.googleapis.com:/fcm/send/a",
		"https://fcm.googleapis.com./fcm/send/a", // trailing dot: an empty last label
		"https://127.0.0.1/ep",
		"https://169.254.169.254/latest/meta-data/",
		"https://[::1]/ep",
		"https://10.0.0.5:9000/ep",
		"https://localhost/ep",
		"https://203.0.113.10/ep",                  // a public IP is still not a push service
		"https://push.example.net/ep",              // a self-hosted push server
		"https://storage.googleapis.com/bucket/ep", // googleapis.com is not all FCM
		"https://evilfcm.googleapis.com/ep",
		"https://fcm.googleapis.com.evil.example/ep",
		"https://www.google.com/ep", // google.com is not all FCM either
		"https://evil-jmt17.google.com/ep",
		"https://jmt17.google.com.evil.example/ep",
		"https://push.apple.com/ep", // a domain entry needs a subdomain
		"https://.push.apple.com/ep",
		"https://xpush.apple.com/ep",
		"https://web.push.apple.com.evil.example/ep",
		"https://evil-notify.windows.com/ep",
		"https://wns2.notify.windows.com.evil.example/ep",
		// Unicode lookalikes: strings.ToLower folds "İ" to "i", but net/http
		// dials the IDNA (punycode) form, a different, registrable domain.
		"https://fcm.googleapİs.com/fcm/send/x",
		"https://fcm.googleap%C4%B0s.com/fcm/send/x",
		"https://x.push.services.mozİlla.com/wpush/v2/x",
		"https://wns2.notify.wİndows.com/w/?token=x",
		"https://\u202eevil.push.apple.com/x", // a bidi override would also reach the log
		"https://x%25.push.apple.com/x",
		"https://[::1]:443/ep",
		// Not a DNS name, though under a listed domain: an empty label, a label
		// past 63 bytes, a name past 253.
		"https://x..push.apple.com/x",
		"https://..push.apple.com/x",
		"https://" + strings.Repeat("a", 64) + ".push.apple.com/x",
		"https://" + strings.Repeat("a.", 120) + "push.apple.com/x",
	} {
		if err := ValidateEndpoint(bad); err == nil {
			t.Errorf("ValidateEndpoint(%q) accepted, want refused", bad)
		}
	}
	// The error names the host and the rule for the operator's log, and never
	// the path, which is the endpoint's secret part.
	err := ValidateEndpoint("https://push.example.net/secret-token")
	if err == nil || !strings.Contains(err.Error(), `"push.example.net"`) || !strings.Contains(err.Error(), "not on a push service") || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error = %v, want the host and the rule without the path", err)
	}
	// An endpoint with no host says so, rather than quoting an empty one.
	if err := ValidateEndpoint("https:fcm.googleapis.com/fcm/send/abc"); err == nil || !strings.Contains(err.Error(), "has no host") {
		t.Errorf("error = %v, want the missing host named", err)
	}
	// The host is the member's to choose, so the error quotes at most a DNS
	// name's worth of it.
	if err := ValidateEndpoint("https://" + strings.Repeat("a", 1<<20) + ".push.apple.com/x"); err == nil || len(err.Error()) > 2*maxHostLen {
		t.Errorf("error is %d bytes, want the host cut to %d", len(fmt.Sprint(err)), maxHostLen)
	}
	// A port gets its own rule in the log, the one an operator can act on.
	for _, ep := range []string{"https://fcm.googleapis.com:443/fcm/send/a", "https://fcm.googleapis.com:/fcm/send/a"} {
		if err := ValidateEndpoint(ep); err == nil || !strings.Contains(err.Error(), "names a port") {
			t.Errorf("ValidateEndpoint(%q) = %v, want the port named", ep, err)
		}
	}
	// The host is quoted in the error, so a bidi override or line separator in
	// one never reaches a log line raw.
	if err := ValidateEndpoint("https://\u202eevil.push.apple.com/x"); err == nil || strings.ContainsRune(err.Error(), '\u202e') {
		t.Errorf("error = %q, want the host escaped", err)
	}
}

// A stored row that is not on a push service (stored before endpoints were
// checked, or written straight to the database) is never sent to, and is
// retired so it is not refused again on every send. The user's other
// browsers still get theirs.
func TestSendToUser_NeverSendsOffAPushServiceAndRetiresTheRow(t *testing.T) {
	good := testSubscription(t, "https://fcm.googleapis.com/fcm/send/good")
	internal := testSubscription(t, "http://127.0.0.1:8080/admin/secret-token")
	metadata := testSubscription(t, "https://169.254.169.254/latest/meta-data/")
	elsewhere := testSubscription(t, "https://push.example.net/ep")
	st := &fakeStore{subs: []store.PushSubscription{internal, good, metadata, elsewhere}}
	relay := &fakeRelay{}
	svc := newTestService(t, st, relay)
	var logged []string
	svc.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
		t.Fatalf("sendToUser: %v", err)
	}
	if len(relay.requests) != 1 || relay.requests[0].URL.String() != good.Endpoint {
		got := make([]string, 0, len(relay.requests))
		for _, r := range relay.requests {
			got = append(got, r.URL.String())
		}
		t.Fatalf("sent to %v, want only the push-service endpoint", got)
	}
	want := map[string]bool{internal.Endpoint: true, metadata.Endpoint: true, elsewhere.Endpoint: true}
	if len(st.deleted) != 3 || !want[st.deleted[0]] || !want[st.deleted[1]] || !want[st.deleted[2]] {
		t.Errorf("deleted %v, want exactly the three rows off a push service", st.deleted)
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"u@x.com", `"127.0.0.1:8080"`, `"push.example.net"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("log = %q, want it to name %s", joined, want)
		}
	}
	if strings.Contains(joined, "secret-token") {
		t.Errorf("log = %q carries an endpoint path", joined)
	}
}

// failingRelay fails every send the way net/http does: a *url.Error, whose
// text quotes the full request URL.
type failingRelay struct{}

func (failingRelay) Do(req *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("dial tcp: i/o timeout")}
}

// A transport error is logged with its cause and host, never the endpoint
// URL, which is a capability: anyone holding it can push to that browser.
func TestSendOne_TransportErrorLogsNoEndpoint(t *testing.T) {
	sub := testSubscription(t, "https://fcm.googleapis.com/fcm/send/secret-token")
	svc := newTestService(t, &fakeStore{subs: []store.PushSubscription{sub}}, nil)
	svc.client = failingRelay{}
	var logged []string
	svc.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
		t.Fatalf("sendToUser: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "fcm.googleapis.com") || !strings.Contains(logged[0], "i/o timeout") || strings.Contains(logged[0], "secret-token") {
		t.Errorf("log = %q, want the host and the cause without the endpoint", logged)
	}
}

// The sender's own client never follows a redirect, so a send cannot be
// bounced off a push-service host to an address ValidateEndpoint never saw,
// and it keeps the default transport, so the operator's HTTPS_PROXY applies.
func TestNewClient_NoRedirectsAndTheDefaultTransport(t *testing.T) {
	var landed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landed", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/landed", func(http.ResponseWriter, *http.Request) { landed.Store(true) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc := New(testConfig(t), &fakeStore{})
	c, ok := svc.client.(*http.Client)
	if !ok {
		t.Fatalf("New's client is %T, want *http.Client", svc.client)
	}
	resp, err := c.Post(srv.URL+"/send", "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || landed.Load() {
		t.Errorf("status %d, landed %v: want the 307 returned and the redirect not followed", resp.StatusCode, landed.Load())
	}
	if c.Transport != nil {
		t.Errorf("Transport = %T, want nil (http.DefaultTransport, which honors HTTPS_PROXY)", c.Transport)
	}
	if c.Timeout != sendTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, sendTimeout)
	}
}

// contactProblem flags a FLEET_VAPID_CONTACT that cannot be a mailto: address
// or an https: URL, and nothing else: a warning on a contact that works would
// teach operators to ignore it.
func TestContactProblem(t *testing.T) {
	for _, tc := range []struct{ contact, want string }{
		// Usable: what the docs and `fleet generate-vapid-keys` describe, in the
		// forms vapidSubscriber accepts, and addresses a stricter check would
		// wrongly refuse.
		{"mailto:ops@example.com", ""},
		{"MAILTO:ops@example.com", ""},
		{" mailto:ops@example.com ", ""},
		{"mailto: ops@example.com", ""}, // vapidSubscriber trims after the scheme
		{"ops@example.com", ""},
		{"mailto:ops+push@example.com", ""},
		{"mailto:ops@mail.eu.example.co.uk", ""},
		{"mailto:ops@bücher.example", ""},
		{"mailto:ops@xn--bcher-kva.example", ""},
		{"mailto:ops@example.com?subject=fleet%20push", ""},
		{"mailto:a@b", ""},
		{"https://ops.example.com/abuse", ""},
		{"HTTPS://ops.example.com/abuse", ""},
		{"https://example.com", ""},
		{"https://example.com:8443/contact", ""},
		{"https://例え.jp/contact", ""},

		// Unusable.
		{"", "is empty"},
		{"   ", "is empty"},
		{"mailto:", `has no address after "mailto:"`},
		{"mailto:   ", `has no address after "mailto:"`},
		{"MAILTO:", `has no address after "mailto:"`},
		{"http://example.com", `starts with "http:" rather than "mailto:" or "https:"`},
		{"HTTP://example.com/contact", `starts with "http:" rather than "mailto:" or "https:"`},
		{"ftp:x", `starts with "ftp:" rather than "mailto:" or "https:"`},
		{"tel:+15551234567", `starts with "tel:" rather than "mailto:" or "https:"`},
		{"mailto:mailto:ops@example.com", `has "mailto:" after "mailto:" where the address belongs`},
		{"mailto:https://example.com", `has "https:" after "mailto:" where the address belongs`},
		// Text before a ":" that is not a well-known scheme may be a token or
		// key pasted in by mistake: it is not quoted.
		{"AKIAIOSFODNN7EXAMPLE:wJalrXUtnFEMIK7MDENG", `starts with a scheme rather than "mailto:" or "https:"`},
		{"mailto:svc-token.v2:abc@example.com", `has a scheme after "mailto:" where the address belongs`},
		{"admin", `has no "@", so it is not an e-mail address`},
		{"example.com", `has no "@", so it is not an e-mail address`},
		{"mailto:admin", `has no "@", so it is not an e-mail address`},
		{"@example.com", `has nothing before the "@"`},
		{"mailto:ops@", `has nothing after the "@"`},
		{"mailto: <ops@example.com>", "contains a space or angle brackets"},
		{"Ops Team <ops@example.com>", "contains a space or angle brackets"},
		{"mailto:ops @example.com", "contains a space or angle brackets"},
		{"https://exa mple.com", "contains a space or angle brackets"},
		{"https:", "is an https: URL with no host"},
		{"https://", "is an https: URL with no host"},
		{"https:example.com", "is an https: URL with no host"},
		{"https:///contact", "is an https: URL with no host"},
		{"https://:443", "is an https: URL with no host"},
		{"https://example.com:port", "is not a valid https: URL"},
	} {
		if got := contactProblem(tc.contact); got != tc.want {
			t.Errorf("contactProblem(%q) = %q, want %q", tc.contact, got, tc.want)
			continue
		}
		if tc.want != "" {
			continue
		}
		// A usable contact reaches the token as a mailto: address or an
		// https:// URL (webpush-go adds "mailto:" to anything not "https:").
		sub := vapidSubscriber(tc.contact)
		if !strings.HasPrefix(sub, "https:") {
			sub = "mailto:" + sub
		}
		mailto := strings.HasPrefix(sub, "mailto:") && strings.Contains(sub, "@")
		if !mailto && !strings.HasPrefix(sub, "https://") {
			t.Errorf("contact %q passes but its claim is %q", tc.contact, sub)
		}
	}
}

// An unusable contact is named once, when push is enabled, through the
// package's log seam; push stays enabled, a send adds no second warning, and
// no key reaches the line, not even a private key a slipped env-file put in
// the contact.
func TestNew_WarnsOnceAboutAnUnusableContact(t *testing.T) {
	for _, tc := range []struct {
		name, contact, problem string
		pasteKey               bool // the contact is the private key
	}{
		{name: "empty mailto", contact: "mailto:", problem: `it has no address after "mailto:"`},
		{name: "http URL", contact: "http://ops.example.com/contact", problem: `it starts with "http:"`},
		{name: "private key pasted as the contact", pasteKey: true, problem: `it has no "@"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Contact = tc.contact
			if tc.pasteKey {
				cfg.Contact = cfg.VAPIDPrivateKey
			}
			var logged []string
			logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			st := &fakeStore{subs: []store.PushSubscription{testSubscription(t, "https://fcm.googleapis.com/fcm/send/ep")}}

			relay := &fakeRelay{}
			svc := newService(cfg, st, relay, logf)
			if !svc.Enabled() {
				t.Fatal("an unusable contact must warn, not turn push off")
			}
			if len(logged) != 1 {
				t.Fatalf("log = %q, want one warning", logged)
			}
			for _, want := range []string{"WARNING", "FLEET_VAPID_CONTACT is not a usable contact", tc.problem, "BadJwtToken", "mailto:you@example.com", "https://example.com/contact"} {
				if !strings.Contains(logged[0], want) {
					t.Errorf("warning = %q, want it to mention %q", logged[0], want)
				}
			}
			for name, secret := range map[string]string{"private key": cfg.VAPIDPrivateKey, "public key": cfg.VAPIDPublicKey} {
				if strings.Contains(logged[0], secret) {
					t.Errorf("warning carries the %s: %q", name, logged[0])
				}
			}

			if err := svc.sendToUser(context.Background(), "u@x.com", "t", "", "", UrgencyNormal); err != nil {
				t.Fatalf("sendToUser: %v", err)
			}
			if len(relay.requests) != 1 {
				t.Fatalf("got %d relay requests, want 1: push must still send", len(relay.requests))
			}
			if len(logged) != 1 {
				t.Errorf("log = %q; the warning belongs to startup, not to each send", logged)
			}
		})
	}

	// A usable contact, and push left off, log nothing.
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if svc := newService(testConfig(t), &fakeStore{}, nil, logf); svc == nil {
		t.Fatal("newService returned nil for an enabled config")
	}
	if svc := newService(Config{}, &fakeStore{}, nil, logf); svc != nil {
		t.Error("newService must return nil for a disabled config")
	}
	if len(logged) != 0 {
		t.Errorf("log = %q, want nothing", logged)
	}
}

// Both exported constructors build through newService, so each warns once on
// the process logger, and NewWithHTTPClient keeps the caller's client.
func TestNewAndNewWithHTTPClient_WarnOnTheProcessLogger(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	cfg := testConfig(t)
	cfg.Contact = "mailto:"
	relay := &fakeRelay{}
	for name, build := range map[string]func() *Service{
		"New":               func() *Service { return New(cfg, &fakeStore{}) },
		"NewWithHTTPClient": func() *Service { return NewWithHTTPClient(cfg, &fakeStore{}, relay) },
	} {
		buf.Reset()
		svc := build()
		if !svc.Enabled() {
			t.Fatalf("%s: push must stay on", name)
		}
		if n := strings.Count(buf.String(), "FLEET_VAPID_CONTACT is not a usable contact"); n != 1 {
			t.Errorf("%s logged the warning %d times, want once: %q", name, n, buf.String())
		}
		if name == "NewWithHTTPClient" && svc.client != relay {
			t.Errorf("NewWithHTTPClient replaced the caller's client with %T", svc.client)
		}
	}
}

// TestEventTitle_Blocked: a blocked run pushes "⚠ Blocked: <name>" with its
// duration; the recorded reason (a Message) never reaches the lock screen.
func TestEventTitle_Blocked(t *testing.T) {
	title, ok := eventTitle(notify.Event{Name: "refresh dash", Status: notify.StatusBlocked, DurationSeconds: 75, Message: "secret-ish reason"})
	if !ok || title != "⚠ Blocked: refresh dash (1m15s)" {
		t.Fatalf("eventTitle(blocked) = %q, %v", title, ok)
	}
}
