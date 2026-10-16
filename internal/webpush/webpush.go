// Package webpush is fleet's host-side browser Web Push sender (#292). It
// delivers small, LOW-DETAIL notifications ("task complete", "approval
// needed", "waiting for your answer") to the browsers a user opted in from,
// even when the fleet tab is backgrounded or closed, via the Web Push
// protocol (RFC 8030/8291/8292, implemented on the standard library in
// protocol.go).
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
//   - The endpoint fleet POSTs to is BROWSER-supplied: any signed-in member
//     can store one, so it is not trusted as an address. ValidateEndpoint
//     admits only an https URL on the push service of a major browser, on
//     subscribe and renew and again before every send, so a member cannot
//     point the host's requests at a loopback, metadata, or internal address.
//     Fixing the HOST (not just refusing internal IPs) is what keeps the
//     sender on an ordinary client: the operator's HTTPS_PROXY still applies,
//     and a hostname cannot resolve somewhere fleet did not choose.
//
// Default OFF: with any of FLEET_VAPID_PUBLIC_KEY / FLEET_VAPID_PRIVATE_KEY /
// FLEET_VAPID_CONTACT unset, New returns nil and every Service method is a
// nil-safe no-op, so a deployment that never ran `fleet generate-vapid-keys`
// behaves exactly as before.
package webpush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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

// pushServiceHosts and pushServiceDomains are where fleet sends: the push
// service each major browser subscribes with. A host matches a pushServiceHosts
// entry exactly, or is a subdomain of a pushServiceDomains entry.
//
//   - fcm.googleapis.com: Firebase Cloud Messaging, for Chrome Stable and the
//     Chromium-based browsers that use Google's service. Exact: other
//     googleapis.com hosts serve unrelated APIs and user content.
//   - jmt17.google.com: the same service's staging host, which Chromium hands
//     out on every channel but Stable (Chrome Beta, Dev and Canary, and a
//     Chromium build that reports no channel): see GetGcmEndpointForChannel
//     in Chromium's components/push_messaging. Exact, like FCM's. (Behind a
//     Chromium feature flag those channels use fcm.googleapis.com/preprod/wp/
//     instead, which the first entry covers.)
//   - push.services.mozilla.com: Mozilla autopush, for Firefox
//     (updates.push.services.mozilla.com).
//   - push.apple.com: Apple's service, for Safari; Apple documents
//     *.push.apple.com as the hosts to allow.
//   - notify.windows.com: the Windows Push Notification Services, for Edge on
//     the desktop (wns2-*.notify.windows.com); Microsoft documents
//     notify.windows.com as the only domain to send to.
//
// A browser on any other push service (a self-hosted autopush, say) cannot turn
// notifications on; docs/PUSH-NOTIFICATIONS.md says so.
var (
	pushServiceHosts   = []string{"fcm.googleapis.com", "jmt17.google.com"}
	pushServiceDomains = []string{"push.services.mozilla.com", "push.apple.com", "notify.windows.com"}
)

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
	// use to reach the operator about misbehaving senders. A bare address is
	// taken as a mailto: one (see vapidSubscriber).
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
	// client is the HTTP seam every send goes through, overridable in tests so
	// no real relay is contacted. Defaults to newClient's.
	client HTTPClient
	// logf is the package logger seam, overridable in tests. It must never be
	// handed a secret — log lines carry only emails/endpoints hosts, statuses,
	// and error text from send (which never quotes a key or an endpoint).
	logf func(format string, args ...any)
}

// New builds a Service, or returns nil (feature off, all methods no-op) when
// cfg is not fully configured. st must be non-nil when cfg is enabled.
func New(cfg Config, st SubscriptionStore) *Service {
	return NewWithHTTPClient(cfg, st, nil)
}

// NewWithHTTPClient is New with the HTTP client supplied by the caller. It is a
// test seam: a fake push service, so a package outside this one can run a real
// send (VAPID signing, payload encryption) with no network. ValidateEndpoint
// still gates every send, but the redirect policy is the caller's client's, so
// production code uses New. A nil client gets New's.
func NewWithHTTPClient(cfg Config, st SubscriptionStore, client HTTPClient) *Service {
	if !cfg.Enabled() {
		return nil
	}
	if client == nil {
		client = newClient()
	}
	return &Service{
		cfg:    cfg,
		store:  st,
		client: client,
		logf:   log.Printf,
	}
}

// newClient is the sender's HTTP client. Its transport is the default one, so
// the operator's HTTPS_PROXY applies: ValidateEndpoint fixes the host to a
// push service, so there is no internal address for a dial-time guard to
// refuse. A push service answers a send itself (201) and has no reason to
// redirect one, so a redirect is never followed: every request stays on the
// host ValidateEndpoint checked, whatever some path on that host answers.
func newClient() *http.Client {
	return &http.Client{
		Timeout: sendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateEndpoint reports whether fleet may store and send to a
// browser-supplied push endpoint: an https URL with no port and no user info,
// whose host is a push service in pushServiceHosts or pushServiceDomains. The
// error names the host and the rule that failed, for the operator's log; the
// browser is told only that its push service is not supported.
//
// The host must be a plain ASCII DNS name (see isPlainHostname) before it is
// matched. net/http maps a non-ASCII host through IDNA to a punycode name,
// while strings.ToLower folds some letters to ASCII ("İ" to "i"), so a Unicode
// lookalike of a listed host would match here and dial a domain anyone can
// register. A push service never issues one. A port is refused too: browsers
// never send one, and the VAPID audience is signed from the host as
// written, so an explicit ":443" would make a row the push service refuses.
func ValidateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("the endpoint is not a valid URL")
	}
	var why string
	switch {
	case u.Scheme != "https":
		why = "is not an https URL"
	case u.User != nil:
		why = "carries user info"
	case u.Host == "":
		why = "has no host"
	case u.Port() != "" || strings.HasSuffix(u.Host, ":"):
		why = "names a port"
	case !isPlainHostname(u.Host):
		why = "has a host that is not a plain ASCII DNS name"
	case !isPushServiceHost(strings.ToLower(u.Host)):
		why = "is not on a push service fleet sends to"
	default:
		return nil
	}
	host := u.Host
	if len(host) > maxHostLen {
		// The host is the member's to choose and goes into the operator's log.
		host = host[:maxHostLen] + "…"
	}
	return fmt.Errorf("the endpoint on %q %s", host, why)
}

// maxHostLen is the longest DNS name (RFC 1035), and maxLabelLen its longest
// label.
const (
	maxHostLen  = 253
	maxLabelLen = 63
)

// isPlainHostname reports whether host is a DNS name in plain ASCII: labels of
// 1 to 63 letters, digits and hyphens, joined by dots, at most 253 bytes in
// all. So no port, no IP-literal brackets, no zone, no empty label, nothing
// that net/http would rewrite before it dials.
func isPlainHostname(host string) bool {
	if host == "" || len(host) > maxHostLen {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > maxLabelLen {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// isPushServiceHost reports whether a lowercased host is a push service (see
// pushServiceHosts; isPlainHostname has already held it to ASCII). A domain
// entry matches only a whole-label subdomain, so neither "xpush.apple.com"
// nor "push.apple.com.example" passes.
func isPushServiceHost(host string) bool {
	for _, h := range pushServiceHosts {
		if host == h {
			return true
		}
	}
	for _, d := range pushServiceDomains {
		if len(host) > len(d)+1 && strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
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

// sendToUser delivers a low-detail notification to EVERY subscription the
// user holds (each browser they opted in from). Per-subscription failures are
// logged and do not stop the fan-out; an endpoint the relay reports expired
// (404/410) is deleted so dead browsers age out. Returns the first hard error
// (store read, or a marshal failure) — send failures are best-effort.
// nil-safe no-op when the service is disabled.
func (s *Service) sendToUser(ctx context.Context, userEmail, title, body, deepLink string, urgency Urgency) error {
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
func (s *Service) sendOne(ctx context.Context, payload []byte, sub store.PushSubscription, urgency Urgency) {
	// A row stored before endpoints were checked, or written straight to the
	// database, is never sent to: the subscribe-time check is not the only
	// gate. It can never be delivered, so it is retired like an endpoint the
	// relay reported gone rather than refused again on every send.
	if err := ValidateEndpoint(sub.Endpoint); err != nil {
		s.logf("webpush: retiring a subscription of %s: %v", sub.UserEmail, err)
		if err := s.store.DeletePushSubscription(ctx, sub.Endpoint); err != nil {
			s.logf("webpush: delete refused subscription failed: %v", err)
		}
		return
	}
	resp, err := send(ctx, s.client, pushMessage{
		Endpoint:        sub.Endpoint,
		Auth:            sub.KeysAuth,
		P256dh:          sub.KeysP256dh,
		Payload:         payload,
		TTL:             payloadTTLSeconds,
		Urgency:         urgency,
		VAPIDPublicKey:  s.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.VAPIDPrivateKey,
		Subscriber:      vapidSubscriber(s.cfg.Contact),
	})
	if err != nil {
		// A transport error is a *url.Error whose text quotes the whole
		// endpoint, a capability URL. Log only its cause, next to the host.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
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
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		// The relay refused this server's VAPID authentication (RFC 8292 §4.2
		// leaves which of the two codes to the push service). After the
		// operator rotates FLEET_VAPID_* that is almost always a subscription
		// made with an earlier key — FCM answers 403 for it, other services
		// may answer 401 — so the row is kept, not deleted: the browser renews
		// it (POST /push/renew) the next time it loads fleet. Every send still
		// refused after browsers have reopened fleet points at the
		// configuration instead, so the line names both.
		s.logf("webpush: relay %s returned status %d%s: it refused this server's VAPID authentication. A subscription made with an earlier key is the usual cause after a key rotation, and the browser renews it the next time it opens fleet; if this persists, check that FLEET_VAPID_PUBLIC_KEY and FLEET_VAPID_PRIVATE_KEY are one pair, FLEET_VAPID_CONTACT is a mailto: or https: address, and the host clock is right", endpointHost(sub.Endpoint), resp.StatusCode, relayReason(resp.Body))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		s.logf("webpush: relay %s returned status %d%s", endpointHost(sub.Endpoint), resp.StatusCode, relayReason(resp.Body))
	}
}

// vapidSubscriber is the contact in the form the VAPID signer expects. The
// signer (vapidAuthorization, kept from webpush-go) writes the token's "sub"
// claim itself: it keeps a contact that starts with "https:" (lower case only)
// and puts "mailto:" in front of anything else.
// FLEET_VAPID_CONTACT is documented (and printed by `fleet generate-vapid-keys`)
// as a whole "mailto:" URI, so passing it through made the claim
// "mailto:mailto:…". FCM accepts that; Apple's push service refuses every such
// send (403, "BadJwtToken"), so Safari never got a notification. So a leading
// "mailto:" is dropped and an "https:" scheme is lowercased, in any case: the
// claim then carries exactly one scheme for a mailto: address, a bare address
// or an https: URL.
func vapidSubscriber(contact string) string {
	const mailto, https = "mailto:", "https:"
	c := strings.TrimSpace(contact)
	switch {
	case len(c) >= len(mailto) && strings.EqualFold(c[:len(mailto)], mailto):
		return strings.TrimSpace(c[len(mailto):])
	case len(c) >= len(https) && strings.EqualFold(c[:len(https)], https):
		return https + c[len(https):]
	}
	return c
}

// maxReasonBytes bounds how much of a refusal's body relayReason reads, and
// maxReasonLen how much of the reason reaches the log line.
const (
	maxReasonBytes = 4096
	maxReasonLen   = 100
)

// relayReason is the push service's stated reason for a refusal, formatted for
// the log line (` (reason "BadJwtToken")`), or "" when it gives none. Apple's
// push service answers a refusal with a JSON object whose "reason" names the
// cause; only that field is read, never the rest of the body, which is the
// push service's to word and could quote the request.
func relayReason(body io.Reader) string {
	var parsed struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxReasonBytes)).Decode(&parsed); err != nil {
		return ""
	}
	reason := strings.TrimSpace(parsed.Reason)
	if reason == "" {
		return ""
	}
	if len(reason) > maxReasonLen {
		// Cut on a character boundary: ToValidUTF8 drops a rune split by
		// the cut instead of leaving a stray byte for %q to print as \xNN.
		reason = strings.ToValidUTF8(reason[:maxReasonLen], "")
	}
	return fmt.Sprintf(" (reason %q)", reason)
}

// endpointHost reduces a subscription endpoint to its host for log lines: the
// full endpoint is a capability URL (anyone holding it can send to that
// browser), so it never goes to the log whole.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host // host[:port]; never the user info, path or query
	}
	return "(unparseable endpoint)"
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
	return s.sendToUser(ctx, ev.Audience, title, "", ev.LogURL, UrgencyNormal)
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
		switch ev.Kind {
		case notify.KindBudgetAlert:
			// A soft budget threshold crossed: a heads-up, nothing waits on an
			// answer. Name already reads "Budget alert: <scope> <principal>
			// (<window> window)".
			return "⚠ " + ev.Name, true
		case notify.KindProgressNote:
			// The agent's mid-run notify tool: the run carries on. The note
			// itself stays off the lock screen, like every Message. "✎" sits
			// beside ✓ and ✗ in Dingbats and stays a symbol in text fonts,
			// where "ℹ" falls back to a plain "i".
			return "✎ Progress note: " + ev.Name, true
		}
		// #510 ask-pause: the task wants the human.
		return "⏸ Waiting for your answer: " + ev.Name, true
	default:
		return "", false
	}
}

// NotifyApprovalRequired sends the chat-side "approval needed" push to the
// conversation owner — high urgency, since a staged approval blocks the turn
// until a human acts (#292). Only the TOOL NAME is included; the staged
// arguments never enter the payload. A click opens the conversation, where the
// pending card re-hydrates. Skipped when FLEET_PUSH_ON_APPROVAL_REQUEST is
// off. nil-safe no-op when disabled.
func (s *Service) NotifyApprovalRequired(ctx context.Context, userEmail, conversationID, toolName string) error {
	if !s.Enabled() || !s.cfg.OnApprovalRequest {
		return nil
	}
	return s.sendToUser(ctx, userEmail, "⚠ Approval needed: "+toolName, "", s.ConversationLink(conversationID), UrgencyHigh)
}

// NotifyApprovalFinished tells the conversation owner that an approved call
// they may have walked away from has finished (docs/APPROVAL-PROGRESS.md).
// label is the approval card's plain-words title when the bundle declared a
// describer, else the tool name; the call's arguments and result never enter
// the payload. Normal urgency: nothing is blocked on the person. Shares the
// FLEET_PUSH_ON_APPROVAL_REQUEST gate with the other approval pushes.
func (s *Service) NotifyApprovalFinished(ctx context.Context, userEmail, conversationID, label string, failed bool) error {
	if !s.Enabled() || !s.cfg.OnApprovalRequest {
		return nil
	}
	return s.sendToUser(ctx, userEmail, approvalFinishedTitle(label, failed), "", s.ConversationLink(conversationID), UrgencyNormal)
}

// NotifyResumeSkipped tells the conversation owner that fleet did not carry
// on by itself after their approvals (the hourly cap, or a full queue), so the
// task waits for them (docs/RESUME-AFTER-APPROVAL.md). High urgency: nothing
// happens until they reply. Same gate as the other approval pushes.
func (s *Service) NotifyResumeSkipped(ctx context.Context, userEmail, conversationID string) error {
	if !s.Enabled() || !s.cfg.OnApprovalRequest {
		return nil
	}
	return s.sendToUser(ctx, userEmail, "⏸ Waiting for you: the chat did not continue on its own", "", s.ConversationLink(conversationID), UrgencyHigh)
}

// approvalFinishedTitle renders the finished-call title. Pure, so the exact
// text contract is unit-testable.
func approvalFinishedTitle(label string, failed bool) string {
	if failed {
		return "✗ Not applied: " + label
	}
	return "✓ Done: " + label
}

// ConversationLink is the deep link a chat notification opens: the chat page
// with ?c=<conversation id>, which opens that conversation on load. Relative
// to the app origin when FLEET_PUBLIC_URL is unset (the service worker
// resolves it against the origin the subscription was created on); the app
// root when there is no conversation.
func (s *Service) ConversationLink(conversationID string) string {
	if s == nil {
		return ""
	}
	if conversationID == "" {
		return s.cfg.PublicURLBase
	}
	return s.cfg.PublicURLBase + "/chat?c=" + url.QueryEscape(conversationID)
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
