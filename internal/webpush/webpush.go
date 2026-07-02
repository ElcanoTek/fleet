// Package webpush is fleet's host-side browser Web Push sender (#292). It
// delivers small, LOW-DETAIL notifications ("task complete", "approval
// needed", "waiting for your answer") to the browsers a user opted in from,
// even when the fleet tab is backgrounded or closed, via the Web Push
// protocol (RFC 8030/8291/8292, github.com/SherClockHolmes/webpush-go).
//
// Security posture (matches the project invariants):
//
//   - Payloads are deliberately low-detail BY DESIGN: a title, a short body,
//     and a deep-link URL. No raw model output, no tool arguments, no PII
//     beyond the task's truncated display name, and never a credential —
//     the payload transits a third-party push relay (encrypted end-to-end to
//     the browser under RFC 8291, but the discipline holds regardless).
//   - The VAPID private key is host-side configuration (the operator's
//     env-file). It is never logged, never shipped into the sandbox, and
//     never sent to a client; only VAPID-signed JWTs derived from it reach
//     the push relay. The PUBLIC key is non-secret and is served to browsers
//     via GET /push/vapid-public-key.
//   - Subscriptions are per-user rows in the chat store; the HTTP layer
//     scopes every subscribe/unsubscribe by the authenticated email.
//
// Default OFF: with any of FLEET_VAPID_PUBLIC_KEY / FLEET_VAPID_PRIVATE_KEY /
// FLEET_VAPID_CONTACT unset, New returns nil and every Service method is a
// nil-safe no-op, so a deployment that never ran `fleet generate-vapid-keys`
// behaves exactly as before.
package webpush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	webpushgo "github.com/SherClockHolmes/webpush-go"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Compile-time proof the concrete chat store satisfies the sender's seam, and
// that the Service is a valid notify push backend.
var (
	_ SubscriptionStore = (*store.Store)(nil)
	_ notify.PushSender = (*Service)(nil)
)

// sendTimeout bounds one HTTP round-trip to a push relay. Push delivery is
// best-effort and always fired off the hot path, so a short bound beats a
// hung goroutine.
const sendTimeout = 10 * time.Second

// payloadTTLSeconds is how long the relay may hold an undelivered
// notification for an offline browser (the issue's suggested 1h).
const payloadTTLSeconds = 3600

// Config is the host-side Web Push configuration, sourced from the operator's
// env-file via Load (all names admitted by the config allowlist).
type Config struct {
	// VAPIDPublicKey / VAPIDPrivateKey are the base64url P-256 pair from
	// `fleet generate-vapid-keys`. The private key is a SECRET — never logged.
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	// Contact is the RFC 8292 subject (mailto: or https: URL) push relays may
	// use to reach the operator about misbehaving senders.
	Contact string
	// OnTaskComplete gates the runner-side task lifecycle pushes (terminal
	// success/failure + paused-awaiting-input/progress). Default ON when the
	// keys are configured; FLEET_PUSH_ON_TASK_COMPLETE=false turns it off.
	OnTaskComplete bool
	// OnApprovalRequest gates the chat-side "approval needed" push. Default ON
	// when the keys are configured; FLEET_PUSH_ON_APPROVAL_REQUEST=false turns
	// it off.
	OnApprovalRequest bool
	// PublicURLBase (FLEET_PUBLIC_URL, shared with notify) builds the deep
	// links the notification click opens. Empty = the service worker falls
	// back to the app origin the subscription was created on.
	PublicURLBase string
}

// Enabled reports whether the three VAPID vars are all set. Default OFF.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.VAPIDPublicKey) != "" &&
		strings.TrimSpace(c.VAPIDPrivateKey) != "" &&
		strings.TrimSpace(c.Contact) != ""
}

// Load builds a Config from the host process environment. With none of the
// FLEET_VAPID_* vars set the returned Config is disabled and Web Push is OFF —
// the default, behavior-unchanged posture. The trigger flags default ON so
// configuring the keys is the single opt-in step.
func Load() Config {
	return Config{
		VAPIDPublicKey:    strings.TrimSpace(os.Getenv("FLEET_VAPID_PUBLIC_KEY")),
		VAPIDPrivateKey:   os.Getenv("FLEET_VAPID_PRIVATE_KEY"),
		Contact:           strings.TrimSpace(os.Getenv("FLEET_VAPID_CONTACT")),
		OnTaskComplete:    envBoolDefaultTrue("FLEET_PUSH_ON_TASK_COMPLETE"),
		OnApprovalRequest: envBoolDefaultTrue("FLEET_PUSH_ON_APPROVAL_REQUEST"),
		PublicURLBase:     strings.TrimRight(strings.TrimSpace(os.Getenv("FLEET_PUBLIC_URL")), "/"),
	}
}

// envBoolDefaultTrue parses a boolean env var that defaults to TRUE when
// unset or unparseable — the trigger flags are opt-out, not opt-in.
func envBoolDefaultTrue(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return b
}

// SubscriptionStore is the narrow persistence seam the sender needs: the
// concrete *store.Store satisfies it, and tests inject an in-memory fake.
// (Subscribe/unsubscribe writes go through the HTTP layer's own store
// interface, not this one.)
type SubscriptionStore interface {
	ListPushSubscriptions(ctx context.Context, userEmail string) ([]store.PushSubscription, error)
	// DeletePushSubscription retires an endpoint the relay reported gone
	// (404/410) — endpoint-keyed, not user-scoped, because the relay's
	// response is authoritative for that endpoint regardless of owner.
	DeletePushSubscription(ctx context.Context, endpoint string) error
}

// Service sends Web Push notifications for one deployment. Safe for
// concurrent use (immutable config + a stdlib HTTP client); every method is
// nil-safe so callers can hold a nil *Service when the feature is off.
type Service struct {
	cfg   Config
	store SubscriptionStore
	// client is the HTTP seam handed to webpush-go, overridable in tests so no
	// real relay is contacted. Defaults to a timeout-bounded http.Client.
	client webpushgo.HTTPClient
	// logf is the package logger seam, overridable in tests. It must never be
	// handed a secret — log lines carry only emails/endpoints hosts, statuses,
	// and error text from webpush-go (which does not echo keys).
	logf func(format string, args ...any)
}

// New builds a Service, or returns nil (feature off, all methods no-op) when
// cfg is not fully configured. st must be non-nil when cfg is enabled.
func New(cfg Config, st SubscriptionStore) *Service {
	if !cfg.Enabled() {
		return nil
	}
	return &Service{
		cfg:    cfg,
		store:  st,
		client: &http.Client{Timeout: sendTimeout},
		logf:   log.Printf,
	}
}

// Enabled reports whether the service is configured and usable.
func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled() }

// PublicKey returns the non-secret VAPID public key browsers subscribe with
// ("" when disabled). Served by GET /push/vapid-public-key so the client
// never needs it baked in at build time.
func (s *Service) PublicKey() string {
	if s == nil {
		return ""
	}
	return s.cfg.VAPIDPublicKey
}

// pushPayload is the encrypted notification body the service worker
// (web/public/sw.js) decodes: title + short body + the URL a click opens.
// Deliberately nothing else — see the package comment.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
}

// SendToUser delivers a low-detail notification to EVERY subscription the
// user holds (each browser they opted in from). Per-subscription failures are
// logged and do not stop the fan-out; an endpoint the relay reports expired
// (404/410) is deleted so dead browsers age out. Returns the first hard error
// (store read, or a marshal failure) — send failures are best-effort.
// nil-safe no-op when the service is disabled.
func (s *Service) SendToUser(ctx context.Context, userEmail, title, body, deepLink string) error {
	return s.sendToUser(ctx, userEmail, title, body, deepLink, webpushgo.UrgencyNormal)
}

func (s *Service) sendToUser(ctx context.Context, userEmail, title, body, deepLink string, urgency webpushgo.Urgency) error {
	if !s.Enabled() || strings.TrimSpace(userEmail) == "" {
		return nil
	}
	subs, err := s.store.ListPushSubscriptions(ctx, userEmail)
	if err != nil {
		return fmt.Errorf("list push subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	payload, err := json.Marshal(pushPayload{Title: title, Body: body, URL: deepLink})
	if err != nil {
		return fmt.Errorf("marshal push payload: %w", err)
	}
	for _, sub := range subs {
		s.sendOne(ctx, payload, sub, urgency)
	}
	return nil
}

// sendOne pushes one payload to one subscription, retiring the row when the
// relay says the endpoint is gone. Errors are logged, never returned — one
// dead browser must not mask delivery to the user's other browsers.
func (s *Service) sendOne(ctx context.Context, payload []byte, sub store.PushSubscription, urgency webpushgo.Urgency) {
	resp, err := webpushgo.SendNotificationWithContext(ctx, payload, &webpushgo.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpushgo.Keys{Auth: sub.KeysAuth, P256dh: sub.KeysP256dh},
	}, &webpushgo.Options{
		HTTPClient:      s.client,
		Subscriber:      s.cfg.Contact,
		TTL:             payloadTTLSeconds,
		Urgency:         urgency,
		VAPIDPublicKey:  s.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.VAPIDPrivateKey,
	})
	if err != nil {
		s.logf("webpush: send to %s failed: %v", endpointHost(sub.Endpoint), err)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The browser unsubscribed or the endpoint expired — the relay's answer
		// is authoritative, so retire the row. Best-effort: on failure the next
		// send retries the delete.
		if err := s.store.DeletePushSubscription(ctx, sub.Endpoint); err != nil {
			s.logf("webpush: delete expired subscription failed: %v", err)
		}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		s.logf("webpush: relay %s returned status %d", endpointHost(sub.Endpoint), resp.StatusCode)
	}
}

// endpointHost reduces a subscription endpoint to its host for log lines: the
// full endpoint is a capability URL (anyone holding it can send to that
// browser), so it never goes to the log whole.
func endpointHost(endpoint string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// SendEvent adapts a notify.Event into a task-lifecycle push, making the
// Service a notify.PushSender backend (Brad's #292 routing: Web Push is one
// notify channel, not a parallel path). The Event is already secret-free by
// construction; the push renders even LESS of it — title + deep link only,
// so the free-text Message (which may carry model output) stays out of the
// payload. Skipped when FLEET_PUSH_ON_TASK_COMPLETE is off or the event has
// no Audience (no owner email to route to).
func (s *Service) SendEvent(ctx context.Context, ev notify.Event) error {
	if !s.Enabled() || !s.cfg.OnTaskComplete {
		return nil
	}
	title, ok := eventTitle(ev)
	if !ok {
		return nil
	}
	return s.sendToUser(ctx, ev.Audience, title, "", ev.LogURL, webpushgo.UrgencyNormal)
}

// eventTitle renders the notification title for a task-lifecycle event, or
// ok=false for a status the push channel doesn't cover. Pure, so the exact
// text contract is unit-testable.
func eventTitle(ev notify.Event) (title string, ok bool) {
	switch ev.Status {
	case notify.StatusSuccess:
		return fmt.Sprintf("✓ Task complete: %s (%s)", ev.Name, formatDuration(ev.DurationSeconds)), true
	case notify.StatusFailure:
		return fmt.Sprintf("✗ Task failed: %s (%s)", ev.Name, formatDuration(ev.DurationSeconds)), true
	case notify.StatusProgress:
		// #510 ask-pause (and the agent's mid-run notify tool, which shares the
		// StatusProgress path): the task wants the human.
		return "⏸ Waiting for your answer: " + ev.Name, true
	default:
		return "", false
	}
}

// NotifyApprovalRequired sends the chat-side "approval needed" push to the
// conversation owner — high urgency, since a staged approval blocks the turn
// until a human acts (#292). Only the TOOL NAME is included; the staged
// arguments never enter the payload. Skipped when
// FLEET_PUSH_ON_APPROVAL_REQUEST is off. nil-safe no-op when disabled.
func (s *Service) NotifyApprovalRequired(ctx context.Context, userEmail, toolName string) error {
	if !s.Enabled() || !s.cfg.OnApprovalRequest {
		return nil
	}
	// Deep link to the app root: the chat UI has no per-conversation URL yet
	// (the pending card re-hydrates on load), so the root is the honest target.
	return s.sendToUser(ctx, userEmail, "⚠ Approval needed: "+toolName, "", s.cfg.PublicURLBase, webpushgo.UrgencyHigh)
}

// ApprovalPushEnabled reports whether the approval trigger would fire —
// consulted before spawning the fire-and-forget goroutine so a disabled
// deployment pays nothing.
func (s *Service) ApprovalPushEnabled() bool { return s.Enabled() && s.cfg.OnApprovalRequest }

// formatDuration renders whole seconds as a compact human duration ("42s",
// "12m30s", "1h2m5s") for notification titles.
func formatDuration(seconds int) string {
	return (time.Duration(seconds) * time.Second).String()
}
