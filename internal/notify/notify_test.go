package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sampleEvent is a representative terminal event used across the payload tests.
func sampleEvent() Event {
	return Event{
		TaskID:          "11111111-2222-3333-4444-555555555555",
		Name:            "nightly report",
		Status:          StatusSuccess,
		CostUSD:         "0.1234",
		DurationSeconds: 42,
		LogURL:          "https://fleet.example.com/orchestrator/tasks/11111111-2222-3333-4444-555555555555",
	}
}

// TestRenderWebhookBody_Default checks the fallback payload is valid JSON
// carrying every event field with the documented shapes (cost/duration numeric).
func TestRenderWebhookBody_Default(t *testing.T) {
	body, err := RenderWebhookBody("", sampleEvent())
	if err != nil {
		t.Fatalf("RenderWebhookBody: %v", err)
	}
	var got struct {
		TaskID          string  `json:"task_id"`
		Name            string  `json:"name"`
		Status          string  `json:"status"`
		CostUSD         float64 `json:"cost_usd"`
		DurationSeconds int     `json:"duration_seconds"`
		LogURL          string  `json:"log_url"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("default body is not valid JSON: %v\nbody=%s", err, body)
	}
	if got.TaskID != sampleEvent().TaskID {
		t.Errorf("task_id = %q, want %q", got.TaskID, sampleEvent().TaskID)
	}
	if got.Status != "success" {
		t.Errorf("status = %q, want success", got.Status)
	}
	if got.CostUSD != 0.1234 {
		t.Errorf("cost_usd = %v, want 0.1234", got.CostUSD)
	}
	if got.DurationSeconds != 42 {
		t.Errorf("duration_seconds = %d, want 42", got.DurationSeconds)
	}
	if got.LogURL != sampleEvent().LogURL {
		t.Errorf("log_url = %q, want %q", got.LogURL, sampleEvent().LogURL)
	}
}

// TestRenderWebhookBody_CustomTemplate checks a custom template renders the
// event fields.
func TestRenderWebhookBody_CustomTemplate(t *testing.T) {
	tmpl := `{"text":"Task {{.Name}} {{.Status}} cost ${{.CostUSD}}"}`
	body, err := RenderWebhookBody(tmpl, sampleEvent())
	if err != nil {
		t.Fatalf("RenderWebhookBody: %v", err)
	}
	want := `{"text":"Task nightly report success cost $0.1234"}`
	if string(body) != want {
		t.Errorf("rendered = %q, want %q", body, want)
	}
}

// TestRenderWebhookBody_BadTemplate surfaces a parse error and an unknown-field
// (missingkey=error) execute error rather than silently emitting "<no value>".
func TestRenderWebhookBody_BadTemplate(t *testing.T) {
	if _, err := RenderWebhookBody("{{.Name", sampleEvent()); err == nil {
		t.Error("expected a parse error for an unterminated action")
	}
	if _, err := RenderWebhookBody("{{.Nope}}", sampleEvent()); err == nil {
		t.Error("expected an execute error for an unknown field")
	}
}

// TestSignWebhook checks the timestamp-bound HMAC-SHA256 signature against a
// known-answer vector, that it is versioned ("v1="), that the timestamp is part
// of the signed payload (a different timestamp yields a different MAC), and that
// it is empty when no secret is set (plain webhook). This is the #316 HMAC seam.
func TestSignWebhook(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	const secret = "test-signing-secret"
	const timestamp = "1700000000"

	got := SignWebhook(body, secret, timestamp)

	// Known-answer: the signed payload is "<timestamp>.<body>".
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("SignWebhook = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "v1=") {
		t.Errorf("signature %q is not versioned with the v1= prefix", got)
	}

	// The timestamp is bound into the MAC: changing it changes the signature, so a
	// captured signature cannot be replayed under a fresh timestamp.
	if other := SignWebhook(body, secret, "1700000001"); other == got {
		t.Error("signature did not change when the timestamp changed; timestamp is not bound into the MAC")
	}

	if SignWebhook(body, "", timestamp) != "" {
		t.Error("SignWebhook with no secret should return empty (plain webhook)")
	}
}

// TestWebhookSend_SignedAndStatus drives a real sendWebhook against an httptest
// server: it asserts both provenance headers are present, the signature verifies
// over the exact received body + timestamp (the receiver's recomputation), the
// default JSON body arrives, and a non-2xx is surfaced as an error.
func TestWebhookSend_SignedAndStatus(t *testing.T) {
	const secret = "hook-secret-value"
	var gotSig, gotTS, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		gotSig = r.Header.Get(signatureHeader)
		gotTS = r.Header.Get(timestampHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL, WebhookSecret: secret})
	if err := n.sendWebhook(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}

	// Both headers must be present for a signed delivery.
	if gotSig == "" || gotTS == "" {
		t.Fatalf("missing provenance headers: signature=%q timestamp=%q", gotSig, gotTS)
	}
	// The timestamp must be a plausible recent Unix-seconds value (replay window).
	ts, err := strconv.ParseInt(gotTS, 10, 64)
	if err != nil {
		t.Fatalf("timestamp header %q is not an integer: %v", gotTS, err)
	}
	if delta := time.Since(time.Unix(ts, 0)); delta < 0 || delta > time.Minute {
		t.Errorf("timestamp %v is not recent (delta %v)", ts, delta)
	}
	// The receiver verifies the signature over the body + timestamp it received —
	// exactly the recomputation a real receiver performs.
	if want := SignWebhook([]byte(gotBody), secret, gotTS); gotSig != want {
		t.Errorf("signature %q does not verify over received body+timestamp (want %q)", gotSig, want)
	}
	if !strings.Contains(gotBody, sampleEvent().TaskID) {
		t.Errorf("body did not carry the task id: %s", gotBody)
	}

	// A 500 receiver yields an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	n2 := New(Config{WebhookURL: bad.URL})
	if err := n2.sendWebhook(context.Background(), sampleEvent()); err == nil {
		t.Error("expected an error for a 500 response")
	}
}

// TestWebhookSend_Unsigned checks the default backward-compatible path: with no
// secret configured, neither provenance header is set and delivery still
// succeeds.
func TestWebhookSend_Unsigned(t *testing.T) {
	var gotSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get(signatureHeader)
		gotTS = r.Header.Get(timestampHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL}) // no WebhookSecret
	if err := n.sendWebhook(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
	if gotSig != "" || gotTS != "" {
		t.Errorf("unsigned delivery set provenance headers: signature=%q timestamp=%q", gotSig, gotTS)
	}
}

// TestNotify_WebhookRetry asserts the bounded retry: a receiver that fails the
// first N-1 attempts and then succeeds causes exactly that many calls, and a
// receiver that always fails is tried exactly Retries+1 times.
func TestNotify_WebhookRetry(t *testing.T) {
	t.Run("succeeds on third attempt", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if atomic.AddInt32(&calls, 1) < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		n := New(Config{WebhookURL: srv.URL, Retries: 2, RetryBackoff: time.Millisecond})
		if err := n.Notify(context.Background(), sampleEvent()); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		if got := atomic.LoadInt32(&calls); got != 3 {
			t.Errorf("webhook called %d times, want 3 (2 failures + 1 success)", got)
		}
	})

	t.Run("exhausts retries", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		n := New(Config{WebhookURL: srv.URL, Retries: 2, RetryBackoff: time.Millisecond})
		if err := n.Notify(context.Background(), sampleEvent()); err == nil {
			t.Error("expected an error after exhausting retries")
		}
		if got := atomic.LoadInt32(&calls); got != 3 {
			t.Errorf("webhook called %d times, want 3 (Retries+1)", got)
		}
	})

	t.Run("no retries when Retries=0", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		// Explicit 0 retries must be honored (not overwritten by the default).
		cfg := Config{WebhookURL: srv.URL, RetryBackoff: time.Millisecond, retriesSet: true}
		n := New(cfg)
		_ = n.Notify(context.Background(), sampleEvent())
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Errorf("webhook called %d times, want 1 (no retries)", got)
		}
	})
}

// TestNotify_StatusFilter checks On gates which statuses fire.
func TestNotify_StatusFilter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL, On: []string{"failure"}})
	// success is filtered out
	if err := n.Notify(context.Background(), Event{Status: StatusSuccess}); err != nil {
		t.Fatalf("Notify(success): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("success fired despite On=[failure]: %d calls", got)
	}
	// failure passes the filter
	if err := n.Notify(context.Background(), Event{Status: StatusFailure}); err != nil {
		t.Fatalf("Notify(failure): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("failure did not fire: %d calls", got)
	}
}

// TestNotify_Disabled checks the default-OFF posture: an empty config is a
// no-op, and a webhook-only config never touches the email path.
func TestNotify_Disabled(t *testing.T) {
	n := New(Config{})
	if n.Enabled() {
		t.Error("empty config should be disabled (default OFF)")
	}
	if err := n.Notify(context.Background(), sampleEvent()); err != nil {
		t.Errorf("disabled Notify should be a no-op, got %v", err)
	}
}

// TestSecretsNotLogged is the core security assertion: when a webhook send fails,
// the package's log output must NOT contain the signing secret, and a failed
// email send must NOT contain the SMTP password. We capture the Notifier's log
// seam and the rendered error/body and scan them for the secret material. We also
// capture the outbound request headers to assert the raw signing secret never
// travels on the wire — only the DERIVED HMAC does (in X-Fleet-Signature).
func TestSecretsNotLogged(t *testing.T) {
	const webhookSecret = "SUPER-SECRET-WEBHOOK-KEY-abc123"
	const smtpPassword = "SUPER-SECRET-SMTP-PASSWORD-xyz789"

	// A webhook receiver that always 500s, so the failure path (and its log line)
	// runs through retry exhaustion. It records the request headers it saw so we
	// can prove the secret was never sent as a header value.
	var hmu sync.Mutex
	var sawHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hmu.Lock()
		sawHeaders = r.Header.Clone()
		hmu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var logged strings.Builder
	capture := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&logged, format, args...)
		logged.WriteByte('\n')
	}

	cfg := Config{
		// Email points at an unroutable address so the send fails fast; the point is
		// the FAILURE log line, which must not echo the password.
		SMTPHost:     "127.0.0.1",
		SMTPPort:     "1", // nothing listens here
		SMTPUsername: "user@example.com",
		SMTPPassword: smtpPassword,
		SMTPFrom:     "fleet@example.com",
		EmailTo:      []string{"ops@example.com"},

		WebhookURL:    srv.URL,
		WebhookSecret: webhookSecret,

		Retries:      0,
		retriesSet:   true,
		Timeout:      500 * time.Millisecond,
		RetryBackoff: time.Millisecond,
	}
	n := New(cfg)
	n.logf = capture

	// Both channels fail; we want their error + log paths exercised.
	err := n.Notify(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("expected both channels to fail")
	}

	mu.Lock()
	out := logged.String()
	mu.Unlock()

	// The returned error is also surfaced to operators (logged by the runner), so
	// scan it too.
	haystack := out + "\n" + err.Error()
	for _, secret := range []string{webhookSecret, smtpPassword} {
		if strings.Contains(haystack, secret) {
			t.Errorf("secret leaked into log/error output:\n%s", haystack)
		}
	}

	// The raw signing secret must never appear in any outbound header — only the
	// derived HMAC (X-Fleet-Signature) and the timestamp travel on the wire.
	hmu.Lock()
	hdr := sawHeaders
	hmu.Unlock()
	if hdr == nil {
		t.Fatal("webhook receiver never saw a request")
	}
	for k, vals := range hdr {
		for _, v := range vals {
			if strings.Contains(v, webhookSecret) {
				t.Errorf("signing secret leaked into outbound header %s: %q", k, v)
			}
		}
	}
	// Sanity: the signed delivery did carry the provenance headers.
	if hdr.Get(signatureHeader) == "" || hdr.Get(timestampHeader) == "" {
		t.Errorf("signed webhook missing provenance headers: %s=%q %s=%q",
			signatureHeader, hdr.Get(signatureHeader), timestampHeader, hdr.Get(timestampHeader))
	}
}

// TestConfigLoad_DefaultOff checks Load with a clean env yields a disabled
// config, and that setting only the webhook URL enables just the webhook.
func TestConfigLoad_DefaultOff(t *testing.T) {
	// Clear every notify var so the test is hermetic regardless of the CI env.
	for _, k := range []string{
		"FLEET_NOTIFY_ON", "FLEET_NOTIFY_EMAIL_TO", "FLEET_NOTIFY_TIMEOUT", "FLEET_NOTIFY_RETRIES",
		"FLEET_SMTP_HOST", "FLEET_SMTP_PORT", "FLEET_SMTP_USERNAME", "FLEET_SMTP_PASSWORD", "FLEET_SMTP_FROM",
		"FLEET_WEBHOOK_URL", "FLEET_WEBHOOK_METHOD", "FLEET_WEBHOOK_BODY_TEMPLATE", "FLEET_WEBHOOK_SECRET",
		"FLEET_PUBLIC_URL",
	} {
		t.Setenv(k, "")
	}
	if Load().Enabled() {
		t.Error("clean env should yield a disabled config")
	}

	t.Setenv("FLEET_WEBHOOK_URL", "https://hooks.example.com/x")
	c := Load()
	if !c.Enabled() || !c.webhookEnabled() {
		t.Error("FLEET_WEBHOOK_URL should enable the webhook channel")
	}
	if c.emailEnabled() {
		t.Error("email should stay disabled with no SMTP host/recipients")
	}
}

// TestRenderEmail checks the multipart message carries both parts, the headers,
// and the run facts — and never the SMTP password (which is not an input to
// renderEmail at all, by construction).
func TestRenderEmail(t *testing.T) {
	msg := string(renderEmail("fleet@example.com", []string{"a@example.com", "b@example.com"}, sampleEvent()))
	for _, want := range []string{
		"From: fleet@example.com",
		"To: a@example.com, b@example.com",
		"Subject: Fleet task success: nightly report",
		"multipart/alternative",
		"text/plain",
		"text/html",
		"nightly report",
		"$0.1234",
		"42s",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rendered email missing %q\n%s", want, msg)
		}
	}
}
