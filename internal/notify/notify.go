// Package notify is fleet's small, host-side outbound notifier for scheduled
// task completion (#208). When a scheduled task reaches a TERMINAL status the
// runner hands the run's outcome to a Notifier, which fans it out to the
// configured channels:
//
//   - an SMTP email sender (net/smtp, STARTTLS), and
//   - a signed/plain HTTP webhook sender.
//
// Both senders apply a per-attempt timeout and a small bounded retry so a slow
// or briefly-unreachable receiver does not strand the goroutine, yet a hard
// outage cannot wedge the runner: the Notifier is fired from a detached
// goroutine in the runner, errors are logged and NEVER affect task status.
//
// # Webhook signing (#316)
//
// When a per-endpoint signing secret is configured (WebhookSecret, sourced from
// the host env-file as FLEET_WEBHOOK_SECRET) each outbound webhook is signed with
// HMAC-SHA256 so a receiver can verify it really came from this fleet and was not
// replayed. The scheme — documented for receiver authors in
// docs/WEBHOOK-SIGNING.md — is:
//
//	timestamp     := strconv.FormatInt(time.Now().Unix(), 10)   // seconds since epoch
//	signedPayload := timestamp + "." + string(body)             // exact bytes sent
//	mac           := HMAC-SHA256(secret, signedPayload)
//	X-Fleet-Signature: v1=<hex(mac)>
//	X-Fleet-Timestamp: <timestamp>
//
// Binding the timestamp into the MAC lets the receiver enforce a replay window
// (reject when |now − timestamp| exceeds, say, 5 minutes) without trusting an
// unauthenticated header. The "v1=" prefix versions the scheme so it can evolve
// without silently breaking receivers. SignWebhook computes the canonical value
// and setSignatureHeader is the single seam that attaches both headers.
//
// Security posture (matches the project invariants):
//
//   - All configuration — recipients, the webhook URL, the SMTP credentials,
//     and the webhook signing secret — comes from the HOST process environment
//     (the operator's env-file). It is held host-side only; it is NEVER shipped
//     into the sandbox, the agent's model context, or the run log.
//   - Secrets are NEVER logged. The senders return errors that describe WHAT
//     failed (dial, auth, status code) without echoing the password, the bearer,
//     or the signing secret; the package's own log lines carry only the task ID
//     and the channel name. Only the DERIVED HMAC (not the secret) ever leaves
//     the process, in the X-Fleet-Signature header.
//
// Default OFF: an empty Config (no SMTP host AND no webhook URL) yields a
// Notifier whose Notify is a no-op, so a deployment that sets none of the
// FLEET_SMTP_*/FLEET_WEBHOOK_* env vars behaves exactly as before. A webhook with
// no secret configured is delivered UNSIGNED (neither signature header is set),
// preserving the pre-#316 behavior for receivers that do not verify.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// defaultTimeout bounds a single send attempt (one SMTP dial+send, or one HTTP
// round-trip). The issue specifies a 10s webhook timeout; we apply the same
// bound to email so neither channel can hang the retry loop.
const defaultTimeout = 10 * time.Second

// defaultRetries is the number of ADDITIONAL attempts after the first, so a send
// is tried up to defaultRetries+1 times. Kept small and bounded: notifications
// are best-effort, and the runner already fires this off-thread.
const defaultRetries = 2

// defaultRetryBackoff is the fixed pause between attempts. Short and constant —
// a completion notification has no value if it lands minutes late, and the
// runner is not waiting on it.
const defaultRetryBackoff = 2 * time.Second

// signatureHeader carries the versioned hex HMAC-SHA256 over the signed payload
// ("<timestamp>.<body>"), prefixed "v1=", so a receiver can verify provenance
// without TLS client certs. Sent only when a webhook secret is configured.
const signatureHeader = "X-Fleet-Signature"

// timestampHeader carries the Unix-seconds timestamp that is bound into the
// signature (see SignWebhook). The receiver uses it both to reconstruct the MAC
// and to enforce a replay window. Sent only when a webhook secret is configured.
const timestampHeader = "X-Fleet-Timestamp"

// signatureVersion prefixes the signature value so the scheme can evolve without
// silently breaking receivers (cf. Stripe's "v1="/GitHub's "sha256=").
const signatureVersion = "v1"

// Status is the terminal outcome a notification reports. It is a small closed
// set the runner maps from its terminal branches; the webhook body template and
// the email subject render it verbatim.
type Status string

const (
	// StatusSuccess is a task that completed successfully.
	StatusSuccess Status = "success"
	// StatusFailure is a task that reached a terminal failure (errored,
	// dead-lettered, or interrupted) — anything the operator would want paged on.
	StatusFailure Status = "failure"
	// StatusProgress is a NON-terminal, out-of-band update (#510 notify /
	// ask-pause): a heads-up mid-run, not a completion. Fires under the same
	// On-list filter as terminal statuses.
	StatusProgress Status = "progress"
)

// Event is the fully-resolved, secret-free outcome of one task run. The runner
// builds it from the task + the run's LogSession and hands it to Notify; the
// senders render it into the email body / webhook payload. It deliberately holds
// NO credentials and NO raw task internals beyond the truncated name, so it is
// safe to construct, render, and (the non-secret fields) log.
type Event struct {
	// TaskID is the task's UUID string.
	TaskID string
	// Name is a short human label for the task (the runner passes the first ~60
	// chars of the prompt). Rendered into the subject/body; never a secret.
	Name string
	// Status is the terminal outcome.
	Status Status
	// CostUSD is the run's cost formatted to 4 decimal places (e.g. "0.1234").
	// Pre-formatted as a string so the webhook template and email body share one
	// representation and a template author cannot accidentally reformat it.
	CostUSD string
	// DurationSeconds is the wall-clock run time, in whole seconds.
	DurationSeconds int
	// LogURL is the absolute link to the run's log in the orchestrator UI, or ""
	// when no public base URL is configured.
	LogURL string
	// Message is an optional free-text line for a StatusProgress event (#510):
	// the notify/ask text. Empty for terminal events. Rendered into the email
	// body; the default webhook template omits it (additive, no template churn).
	Message string
}

// shouldFire reports whether this status is one the config asked to be notified
// about. An empty On list means "all terminal statuses".
func (c Config) shouldFire(s Status) bool {
	if len(c.On) == 0 {
		return true
	}
	for _, want := range c.On {
		if Status(strings.TrimSpace(want)) == s || strings.TrimSpace(want) == "always" {
			return true
		}
	}
	return false
}

// Notifier fans a terminal Event out to the configured channels. It is built
// once at startup from Config and is safe for concurrent use (it holds only
// immutable config + the stdlib http/smtp clients, which are concurrency-safe).
type Notifier struct {
	cfg    Config
	client *http.Client
	// logf is the package logger seam, overridable in tests to capture output and
	// assert no secret is ever written. Defaults to the stdlib logger.
	logf func(format string, args ...any)
}

// New builds a Notifier from cfg. When cfg configures nothing (Enabled() is
// false) the returned Notifier's Notify is a no-op, so callers can always wire a
// non-nil Notifier and let config decide whether anything fires.
func New(cfg Config) *Notifier {
	cfg.applyDefaults()
	return &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		logf:   stdLogf,
	}
}

// Enabled reports whether any channel is configured. Default OFF: with neither
// an SMTP host + recipients nor a webhook URL, nothing fires.
func (n *Notifier) Enabled() bool { return n.cfg.Enabled() }

// ReplyEnabled reports whether email reply-back (#511) can send: an SMTP host and
// a From address are configured (a reply targets the inbound sender, so it needs
// no EmailTo list). The runner wires the EmailReplier only when this is true so a
// deployment without SMTP does no per-run reply lookups.
func (n *Notifier) ReplyEnabled() bool { return n != nil && n.cfg.replyEnabled() }

// Notify fans the event out to every configured channel that opted in to this
// status. Channels are independent: a failure on one is logged and does not
// prevent the other from being attempted. The error (a join of any channel
// failures) is returned for callers/tests that want it; the runner fires Notify
// from a detached goroutine and only logs it, so a notification failure NEVER
// affects task status.
//
// The passed context bounds the whole fan-out; each individual attempt is
// additionally bounded by cfg.Timeout. A nil/closed Notifier or a disabled
// config returns nil immediately.
func (n *Notifier) Notify(ctx context.Context, ev Event) error {
	if n == nil || !n.cfg.Enabled() || !n.cfg.shouldFire(ev.Status) {
		return nil
	}
	var errs []error
	if n.cfg.emailEnabled() {
		if err := n.retry(ctx, func(ctx context.Context) error { return n.sendEmail(ctx, ev) }); err != nil {
			n.logf("notify: email for task %s failed: %v", ev.TaskID, err)
			errs = append(errs, fmt.Errorf("email: %w", err))
		} else {
			n.logf("notify: email for task %s sent", ev.TaskID)
		}
	}
	if n.cfg.webhookEnabled() {
		if err := n.retry(ctx, func(ctx context.Context) error { return n.sendWebhook(ctx, ev) }); err != nil {
			n.logf("notify: webhook for task %s failed: %v", ev.TaskID, err)
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		} else {
			n.logf("notify: webhook for task %s sent", ev.TaskID)
		}
	}
	return joinErrs(errs)
}

// retry runs fn up to cfg.Retries+1 times with a fixed backoff between attempts,
// each attempt bounded by cfg.Timeout. It returns the last attempt's error (nil
// on success). The loop stops early if the parent ctx is cancelled. The returned
// error is the channel's own error, which by construction names only the failure
// mode (dial/auth/status), never a secret.
func (n *Notifier) retry(ctx context.Context, fn func(context.Context) error) error {
	var lastErr error
	attempts := n.cfg.Retries + 1
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(n.cfg.RetryBackoff):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, n.cfg.Timeout)
		lastErr = fn(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// sendEmail sends the completion email over SMTP with STARTTLS (the net/smtp
// SendMail path, which negotiates STARTTLS when the server advertises it). The
// SMTP password is read from the host config and handed to smtp.PlainAuth; it is
// never placed in the message, the error, or any log line.
func (n *Notifier) sendEmail(ctx context.Context, ev Event) error {
	c := n.cfg
	addr := net.JoinHostPort(c.SMTPHost, c.SMTPPort)
	msg := renderEmail(c.SMTPFrom, c.EmailTo, ev)

	var auth smtp.Auth
	if c.SMTPUsername != "" {
		auth = smtp.PlainAuth("", c.SMTPUsername, c.SMTPPassword, c.SMTPHost)
	}

	// net/smtp has no context-aware SendMail; run it on a goroutine and honor the
	// per-attempt ctx so a hung dial still returns within cfg.Timeout. The
	// detached SendMail finishes (or errors) on its own and is then GC'd.
	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(addr, auth, c.SMTPFrom, c.EmailTo, msg)
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("smtp send to %s: %w", addr, ctx.Err())
	case err := <-done:
		if err != nil {
			// smtp errors describe the dial/auth/server response, not the password.
			return fmt.Errorf("smtp send to %s: %w", addr, err)
		}
		return nil
	}
}

// ReplyToEmailEvent sends a reply to an inbound-email trigger's original sender
// (#511 reply-back), threaded to the original message via In-Reply-To. It reuses
// the SMTP sender's timeout+retry mechanics. A no-op (returns nil) when SMTP is
// not configured for sending or `to` is empty, so the runner can wire it
// unconditionally. Fired off-thread by the runner; its error is logged there and
// NEVER affects task status. The SMTP password is never placed in the message,
// the error, or any log line.
func (n *Notifier) ReplyToEmailEvent(ctx context.Context, to, subject, body, inReplyTo string) error {
	if n == nil || !n.cfg.replyEnabled() {
		return nil
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return nil
	}
	return n.retry(ctx, func(ctx context.Context) error {
		return n.sendReplyEmail(ctx, to, subject, body, inReplyTo)
	})
}

// sendReplyEmail sends one reply over SMTP to the single inbound sender. Mirrors
// sendEmail's context-honoring goroutine pattern, but addresses the reply to the
// event's sender (not the configured EmailTo list).
func (n *Notifier) sendReplyEmail(ctx context.Context, to, subject, body, inReplyTo string) error {
	c := n.cfg
	addr := net.JoinHostPort(c.SMTPHost, c.SMTPPort)
	msg := renderReplyEmail(c.SMTPFrom, to, subject, body, inReplyTo)

	var auth smtp.Auth
	if c.SMTPUsername != "" {
		auth = smtp.PlainAuth("", c.SMTPUsername, c.SMTPPassword, c.SMTPHost)
	}

	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(addr, auth, c.SMTPFrom, []string{to}, msg)
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("smtp reply to %s: %w", addr, ctx.Err())
	case err := <-done:
		if err != nil {
			return fmt.Errorf("smtp reply to %s: %w", addr, err)
		}
		return nil
	}
}

// sendWebhook renders the body template and POSTs (or uses the configured
// method) to the webhook URL, signing the body with HMAC-SHA256 when a secret is
// configured. A non-2xx response is an error (so retry can re-attempt); the
// error names only the status, never the URL's query or the signing secret.
func (n *Notifier) sendWebhook(ctx context.Context, ev Event) error {
	body, err := RenderWebhookBody(n.cfg.WebhookBodyTemplate, ev)
	if err != nil {
		return err
	}
	method := n.cfg.WebhookMethod
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setSignatureHeader(req, body, n.cfg.WebhookSecret)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	defer func() {
		// Drain so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// RenderWebhookBody renders tmpl against ev, falling back to a sensible default
// JSON payload when tmpl is empty. Exported so the construction is unit-testable
// without any network. The template is text/template (not html/template): the
// body is a webhook payload, not HTML, and the rendered Event fields are all
// non-secret, operator-or-fleet-controlled values.
func RenderWebhookBody(tmpl string, ev Event) ([]byte, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultWebhookTemplate
	}
	t, err := template.New("webhook").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("parse webhook body_template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ev); err != nil {
		return nil, fmt.Errorf("render webhook body_template: %w", err)
	}
	return buf.Bytes(), nil
}

// defaultWebhookTemplate is the fallback payload when no body template is
// configured. JSON-encoding-safe for the simple value types it interpolates
// (a UUID, a closed-set status, a numeric cost/duration, and a URL we build).
const defaultWebhookTemplate = `{"task_id":"{{.TaskID}}","name":"{{.Name}}","status":"{{.Status}}","cost_usd":{{.CostUSD}},"duration_seconds":{{.DurationSeconds}},"log_url":"{{.LogURL}}"}`

// SignWebhook returns the canonical X-Fleet-Signature value for body under
// secret, binding in timestamp (Unix seconds, as it appears in the
// X-Fleet-Timestamp header):
//
//	"v1=" + hex(HMAC-SHA256(secret, timestamp + "." + body))
//
// Returns "" when secret is empty (plain, unsigned webhook). Pure and exported
// so it is unit-testable with known-answer vectors and so receiver authors have
// one canonical definition to mirror. body and timestamp MUST be the exact
// bytes/string sent in the request, or the receiver's recomputed MAC will not
// match.
func SignWebhook(body []byte, secret, timestamp string) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	return signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

// setSignatureHeader is the single seam where the request gets its provenance
// signature. It stamps the current time, signs "<timestamp>.<body>", and sets
// the signature + timestamp headers together so the receiver can both verify the
// MAC and enforce a replay window. No-op when no secret is configured (plain
// webhook): neither header is set, preserving pre-#316 behavior.
func setSignatureHeader(req *http.Request, body []byte, secret string) {
	if secret == "" {
		return
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	if sig := SignWebhook(body, secret, timestamp); sig != "" {
		req.Header.Set(signatureHeader, sig)
		req.Header.Set(timestampHeader, timestamp)
	}
}

// joinErrs collapses a slice of channel errors into one (nil when empty).
func joinErrs(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
}
