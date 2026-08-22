// Package observability wires external error-tracking sinks. It is the single
// place fleet's process touches the Sentry SDK so the rest of the codebase stays
// SDK-agnostic: cmd/fleet calls Init at boot (gated on FLEET_SENTRY_DSN), and
// internal/runner + internal/agentcore call the breadcrumb/capture helpers that
// are no-ops when the SDK was never initialized.
//
// When FLEET_SENTRY_DSN is unset, Init is a complete no-op — no client, no
// transport, no goroutine, zero per-call overhead beyond a nil-check. The SDK's
// own "no client bound" guard makes CaptureException / AddBreadcrumb cheap
// no-ops in that state, so call sites do not need their own DSN-present branch.
//
// Secrets never leave the process via the Sentry transport: RedactEvent is
// installed as the SDK's BeforeSend hook and reuses the secret scrubber passed
// to Init (internal/agentcore.RedactSecrets in production) on every breadcrumb
// message, breadcrumb data field, context string value, and a set of
// known-sensitive request headers. BeforeSend is the last line of defence;
// call sites ALSO redact at attach time so a future SDK change cannot regress it.
//
// The redact dependency is INJECTED at Init (not imported) so agentcore — which
// owns the scrubber — can call the breadcrumb/capture helpers without forming an
// import cycle (agentcore → observability → [redact func, not agentcore]).
package observability

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/getsentry/sentry-go"
)

// Redactor scrubs recoverable secrets from a string. It is the signature of
// internal/agentcore.RedactSecrets, injected at Init so this package does not
// import agentcore (which would cycle, since agentcore calls AddBreadcrumb).
type Redactor func(string) string

// noopRedactor leaves input unchanged; the default before Init wires the real one.
func noopRedactor(s string) string { return s }

var redactor Redactor = noopRedactor

// Options configures Init. Zero values are inert except DSN, which is the
// gate: an empty DSN leaves Sentry fully disabled.
type Options struct {
	// DSN is the Sentry-protocol ingest endpoint. Empty → Sentry disabled.
	DSN string
	// Environment is the deployment tier tagged on every event
	// ("production" | "staging" | "dev"). Empty → "dev".
	Environment string
	// Release is the application version tagged on every event.
	Release string
	// Redact scrubs secrets from breadcrumb data + outbound events. When nil,
	// no redaction is applied (suitable only for tests). Production wires
	// agentcore.RedactSecrets so the same scrubber the session log, tool
	// output, and SSE stream use also guards the Sentry transport.
	Redact Redactor
}

// Init initializes the Sentry SDK when DSN is set and registers RedactEvent as
// the BeforeSend hook. It is safe to call at boot before any goroutine starts.
// A failed init is NON-FATAL: fleet stays up and logs to stderr/journald as
// before; only Sentry capture is lost. Returns true when Sentry is active.
//
// The deferred Flush belongs to the caller (cmd/fleet) so it can bind the
// shutdown deadline to its own grace budget; Init only sets up the client.
func Init(opts Options) bool {
	if opts.Redact != nil {
		redactor = opts.Redact
	}
	if opts.DSN == "" {
		log.Printf("sentry: disabled (FLEET_SENTRY_DSN not set)")
		return false
	}
	env := opts.Environment
	if env == "" {
		env = "dev"
	}
	// No SendDefaultPII / DataCollection field is set, and that is deliberate.
	// Leaving both at their zero values is what selects the SDK's STRICTEST
	// posture: sentry-go resolves a nil DataCollection with SendDefaultPII
	// false through legacyDataCollection(false), which turns UserInfo off,
	// collects no HTTP bodies, drops cookies entirely, deny-lists request and
	// response headers plus query params, AND installs extendedSensitiveTerms
	// (forwarded, -ip, remote-, via, -user) as an extra scrubbing deny list.
	//
	// Do NOT "modernize" this by populating DataCollection instead. That field
	// is the documented replacement for the now-deprecated SendDefaultPII, but
	// it CANNOT express what we get here: extendedSensitiveTerms is set through
	// the unexported DataCollection.sensitiveTerms, unreachable from outside the
	// package, and resolveDataCollection defaults UserInfo back to TRUE. So a
	// hand-written DataCollection would silently loosen PII scrubbing — the
	// opposite of the intent — which is why the explicit `SendDefaultPII: false`
	// was dropped rather than migrated when staticcheck flagged it deprecated.
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              opts.DSN,
		Environment:      env,
		Release:          opts.Release,
		BeforeSend:       RedactEvent,
		AttachStacktrace: true,
	}); err != nil {
		// Non-fatal: fleet keeps running; Sentry is just absent.
		log.Printf("sentry: init failed (non-fatal): %v", err)
		return false
	}
	log.Printf("sentry: initialized (env=%s)", env)
	return true
}

// Flush blocks for up to timeout waiting for queued events to drain to the
// transport. Safe to call when Sentry was never initialized (no-op). cmd/fleet
// defers this at boot so a SIGTERM doesn't drop in-flight events.
func Flush(timeout time.Duration) {
	sentry.Flush(timeout)
}

// CaptureException ships err to Sentry with the per-call scope tags set by fn.
// No-op (the SDK's own guard) when Sentry was never initialized. The scope
// callback runs BEFORE capture so callers can attach task_id / model / flavor
// tags without racing a concurrent goroutine: WithScope clones the current
// scope, mutates the clone, captures, then restores — so concurrent captures
// never clobber each other's tags.
func CaptureException(ctx context.Context, err error, scope func(*sentry.Scope)) {
	if err == nil {
		return
	}
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		hub = sentry.CurrentHub()
	}
	hub.WithScope(func(s *sentry.Scope) {
		if scope != nil {
			scope(s)
		}
		hub.CaptureException(err)
	})
}

// CapturePanicClass ships a pre-classified panic without accepting diagnostic
// content. Unknown values collapse to "unknown" so callers cannot accidentally
// pass a recovered message through the class argument.
func CapturePanicClass(ctx context.Context, class string, scope func(*sentry.Scope)) {
	class = normalizedPanicClass(class)
	CaptureException(ctx, errors.New("recovered panic ("+class+")"), scope)
}

// CapturePanicClassWithTags is the safe-package adapter for a recovery boundary
// that already discarded the raw value and retained only its class.
func CapturePanicClassWithTags(ctx context.Context, class string, tags map[string]string) {
	CapturePanicClass(ctx, class, func(scope *sentry.Scope) {
		for key, value := range tags {
			if value != "" {
				scope.SetTag(key, value)
			}
		}
	})
}

func normalizedPanicClass(class string) string {
	switch class {
	case "nil", "invalid", "bool", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64",
		"complex64", "complex128", "array", "chan", "func", "interface", "map", "ptr",
		"slice", "string", "struct", "unsafe.Pointer", "error":
		return class
	default:
		return "unknown"
	}
}

// AddBreadcrumb records a breadcrumb (no-op when Sentry is disabled — the SDK
// checks internally). data is redacted through the injected Redactor before
// attach so a secret-bearing breadcrumb data field never reaches BeforeSend as
// the primary defence; RedactEvent is the last line.
func AddBreadcrumb(ctx context.Context, category, message string, data map[string]string) {
	bc := &sentry.Breadcrumb{
		Category: category,
		Message:  message,
	}
	if len(data) > 0 {
		bc.Data = make(map[string]any, len(data))
		for k, v := range data {
			bc.Data[k] = redactor(v)
		}
	}
	if ctx != nil {
		if hub := sentry.GetHubFromContext(ctx); hub != nil {
			hub.AddBreadcrumb(bc, nil)
			return
		}
	}
	sentry.AddBreadcrumb(bc)
}

// RedactEvent is the Sentry BeforeSend hook: it scrubs every outbound event so
// no credential ever leaves the host via the Sentry transport. It reuses the
// injected Redactor (internal/agentcore.RedactSecrets in production — the same
// scrubber the session log, tool output, and SSE stream apply) on every field
// that can carry a free-form, secret-bearing string: the top-level message, the
// exception values (an error's .Error() text — e.g. a DB DSN with a password or
// an upstream HTTP error echoing an Authorization value — is the highest-risk
// path because runner.go ships arbitrary run errors here), breadcrumb messages
// and data fields, the "extra" context bucket, and the request query string /
// cookies. It then force-filters a set of known-sensitive request headers (MCP
// broker calls, httpapi) as a second defence. Returning nil would drop the event
// entirely; this hook always returns the (redacted) event so error visibility is
// preserved while secrets are not.
func RedactEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil {
		return nil
	}
	// Top-level message (CaptureMessage) and every captured exception's Value —
	// the error's .Error() string, which the runner ships verbatim via
	// CaptureException, so a secret embedded in an error message is scrubbed
	// before it leaves the process. Stack-frame data is not redacted because the
	// Go SDK does not capture local variable values.
	event.Message = redactor(event.Message)
	for i := range event.Exception {
		event.Exception[i].Value = redactor(event.Exception[i].Value)
	}
	for _, bc := range event.Breadcrumbs {
		bc.Message = redactor(bc.Message)
		for k, v := range bc.Data {
			if s, ok := v.(string); ok {
				bc.Data[k] = redactor(s)
			}
		}
	}
	// Scrub string values nested under the "extra" context bucket (the
	// conventional home for ad-hoc key/value extras in the Sentry UI).
	if extras, ok := event.Contexts["extra"]; ok {
		for k, v := range extras {
			if s, ok := v.(string); ok {
				extras[k] = redactor(s)
			}
		}
	}
	if event.Request != nil {
		// Query string and cookies can carry tokens (?token=…, session=…); run
		// them through the same scrubber rather than dropping them wholesale so
		// non-secret request context survives.
		event.Request.QueryString = redactor(event.Request.QueryString)
		event.Request.Cookies = redactor(event.Request.Cookies)
		// Known-sensitive headers are wholesale secrets, so hard-filter them
		// (MCP broker calls, httpapi). The SDK lowercases header keys, so the
		// check is case-insensitive in effect.
		if event.Request.Headers != nil {
			for _, h := range []string{"authorization", "x-fleet-token", "x-api-key", "cookie"} {
				if _, ok := event.Request.Headers[h]; ok {
					event.Request.Headers[h] = "[Filtered]"
				}
			}
		}
	}
	return event
}
