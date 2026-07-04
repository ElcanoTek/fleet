package notify

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SendTest fires ONE real delivery attempt of a synthetic event over the named
// channel ("email" or "webhook") using cfg — the admin Test button behind
// POST /admin/notify-settings/test. It deliberately does not retry (the admin
// wants a fast, honest answer, not a 3×-backoff wait) and bounds the attempt
// with the config timeout. The returned error names only the failure mode
// (dial, auth, HTTP status), never a credential — the same contract as the
// live senders it reuses.
func SendTest(ctx context.Context, cfg Config, channel string) error {
	cfg.applyDefaults()
	ev := Event{
		TaskID:          "test",
		Name:            "Test notification from fleet",
		Status:          StatusSuccess,
		CostUSD:         "0.0000",
		DurationSeconds: 0,
		Message:         "This is a test notification sent from Settings → Admin → Notifications.",
	}
	attemptCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	switch strings.TrimSpace(strings.ToLower(channel)) {
	case "email":
		if !cfg.emailEnabled() {
			return fmt.Errorf("email is not configured (an SMTP host and at least one recipient are required)")
		}
		return sendEmail(attemptCtx, cfg, ev)
	case "webhook":
		if !cfg.webhookEnabled() {
			return fmt.Errorf("webhook is not configured (a webhook URL is required)")
		}
		// Validate the template up front so a bad template reads as a template
		// error, not a generic send failure.
		if _, err := RenderWebhookBody(cfg.WebhookBodyTemplate, ev); err != nil {
			return err
		}
		return sendWebhook(attemptCtx, newState(cfg), ev)
	default:
		return fmt.Errorf("unknown test channel %q (want email or webhook)", channel)
	}
}

// TestResult is the admin-facing outcome of a SendTest, secret-free by
// construction.
type TestResult struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	LatencyMS int64  `json:"latency_ms"`
}

// RunTest wraps SendTest into the admin-facing TestResult (timing + key-free
// detail), so the HTTP layer stays a thin translation.
func RunTest(ctx context.Context, cfg Config, channel string) TestResult {
	start := time.Now()
	err := SendTest(ctx, cfg, channel)
	res := TestResult{OK: err == nil, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Detail = err.Error()
	} else {
		res.Detail = fmt.Sprintf("test %s delivered", strings.TrimSpace(strings.ToLower(channel)))
	}
	return res
}
