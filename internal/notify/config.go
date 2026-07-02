package notify

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the host-side notification configuration. Every field is sourced
// from the operator's env-file via Load; the struct is also constructed directly
// in tests. The secret-bearing fields (SMTPPassword, WebhookSecret) are held
// host-side only and never logged, shipped into the sandbox, or placed in the
// model context.
type Config struct {
	// On filters which terminal statuses fire a notification. Empty = all
	// terminal statuses. Values are notify Status strings ("success"/"failure")
	// or the special "always".
	On []string

	// ── email (SMTP) ──
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string // SECRET — never logged
	SMTPFrom     string
	// EmailTo is the recipient list. Email fires only when both SMTPHost and a
	// non-empty EmailTo are set.
	EmailTo []string

	// ── webhook ──
	WebhookURL          string
	WebhookMethod       string // default POST
	WebhookBodyTemplate string
	WebhookSecret       string // SECRET — HMAC signing key, never logged

	// ── shared ──
	// PublicURLBase is the absolute base (scheme+host) used to build the
	// per-task LogURL in notifications, e.g. https://fleet.example.com. Empty =
	// notifications omit the log link.
	PublicURLBase string
	// Timeout bounds a single send attempt. 0 → defaultTimeout.
	Timeout time.Duration
	// Retries is the number of ADDITIONAL attempts after the first. <0 → 0;
	// 0 (unset) → defaultRetries.
	Retries int
	// RetryBackoff is the fixed pause between attempts. 0 → defaultRetryBackoff.
	RetryBackoff time.Duration
	// retriesSet records whether Retries was explicitly provided, so an explicit
	// 0 (no retries) is honored rather than overwritten by the default. Set by
	// Load and by applyDefaults when Retries is already >0.
	retriesSet bool
}

// Enabled reports whether any channel is configured. Default OFF.
func (c Config) Enabled() bool { return c.emailEnabled() || c.webhookEnabled() }

// emailEnabled reports whether email can be sent (an SMTP host AND at least one
// recipient). A host with no recipients — or recipients with no host — is inert.
func (c Config) emailEnabled() bool {
	return strings.TrimSpace(c.SMTPHost) != "" && len(c.EmailTo) > 0
}

// webhookEnabled reports whether a webhook URL is configured.
func (c Config) webhookEnabled() bool { return strings.TrimSpace(c.WebhookURL) != "" }

// replyEnabled reports whether an email REPLY can be sent (#511 reply-back). A
// reply goes to the inbound sender rather than the configured EmailTo list, so —
// unlike emailEnabled — it needs only an SMTP host and a From address, not a
// recipient list. Inert (no-op) when SMTP is not configured for sending.
func (c Config) replyEnabled() bool {
	return strings.TrimSpace(c.SMTPHost) != "" && strings.TrimSpace(c.SMTPFrom) != ""
}

// applyDefaults fills the timing/retry knobs in place. Called by New.
func (c *Config) applyDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = defaultRetryBackoff
	}
	if c.Retries < 0 {
		c.Retries = 0
		c.retriesSet = true
	}
	if !c.retriesSet && c.Retries == 0 {
		c.Retries = defaultRetries
	}
}

// Load builds a Config from the host process environment (#208). It reads the
// FLEET_SMTP_*, FLEET_WEBHOOK_*, FLEET_NOTIFY_*, and FLEET_PUBLIC_URL variables
// the operator set in their env-file (already admitted by the config allowlist
// and loaded into the process env by config.Load). With none of them set, the
// returned Config is disabled (Enabled() == false) and notifications are OFF —
// the default, behavior-unchanged posture.
func Load() Config {
	c := Config{
		On: splitCSV(os.Getenv("FLEET_NOTIFY_ON")),

		SMTPHost:     strings.TrimSpace(os.Getenv("FLEET_SMTP_HOST")),
		SMTPPort:     strings.TrimSpace(os.Getenv("FLEET_SMTP_PORT")),
		SMTPUsername: os.Getenv("FLEET_SMTP_USERNAME"),
		SMTPPassword: os.Getenv("FLEET_SMTP_PASSWORD"),
		SMTPFrom:     strings.TrimSpace(os.Getenv("FLEET_SMTP_FROM")),
		EmailTo:      splitCSV(os.Getenv("FLEET_NOTIFY_EMAIL_TO")),

		WebhookURL:          strings.TrimSpace(os.Getenv("FLEET_WEBHOOK_URL")),
		WebhookMethod:       strings.TrimSpace(os.Getenv("FLEET_WEBHOOK_METHOD")),
		WebhookBodyTemplate: os.Getenv("FLEET_WEBHOOK_BODY_TEMPLATE"),
		WebhookSecret:       os.Getenv("FLEET_WEBHOOK_SECRET"),

		PublicURLBase: strings.TrimRight(strings.TrimSpace(os.Getenv("FLEET_PUBLIC_URL")), "/"),
	}
	if c.SMTPPort == "" {
		c.SMTPPort = "587" // STARTTLS submission port, the common default
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_NOTIFY_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Timeout = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_NOTIFY_RETRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Retries = n
			c.retriesSet = true
		}
	}
	return c
}

// splitCSV parses a comma-separated env value into a trimmed, non-empty slice
// (nil for empty input).
func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
