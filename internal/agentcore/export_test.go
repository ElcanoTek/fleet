package agentcore

import (
	"context"
	"strings"
	"time"

	"charm.land/fantasy"
)

// Test-only hooks for the external agentcore_test package, which can import
// clientconfig (clientconfig imports agentcore, so an internal test cannot).

// ConfirmAuditToolForTest returns the confirm_audit tool wired to policy, as
// the scheduled tool roster registers it.
func ConfirmAuditToolForTest(policy Policy) fantasy.AgentTool {
	return buildConfirmAuditPolicyTool(policy)
}

// RestoreTestFixturePolicy reinstalls the agent policy TestMain installs.
func RestoreTestFixturePolicy() { ConfigureAgentPolicy(testFixturePolicy()) }

// Test-only helpers for this package's internal tests: thin wrappers and
// probes production code does not call, kept out of the shipped files.

// applyOutputCeiling is the pure text-only form of the output ceiling, used by
// low-level tests that do not have a ToolResponse/context. Production AgentTool routes
// use boundModelVisibleToolResponse so they also get structured JSON handling,
// artifact recovery, and media suppression. A non-positive limit selects the
// safe default; it never disables truncation.
func applyOutputCeiling(content string, limit int) (string, bool) {
	limit = normalizeToolOutputLimit(limit)
	encodedBinary := containsEncodedBinary(content)
	if len(content) <= limit && !encodedBinary {
		return content, false
	}
	env := toolOutputEnvelope{
		Tool:             "tool",
		OriginalBytes:    len(content),
		Truncated:        true,
		Format:           "text",
		BinarySuppressed: encodedBinary,
		RecoveryAction:   "Re-run the tool with narrower output.",
	}
	return renderTextEnvelope(env, content, limit), true
}

// noteProviderError records a provider error seen NOW whose window is exactly
// the backoff it bought. Idempotent in effect; the last status wins.
func (w *firstChunkWatchdog) noteProviderError(providerErr *fantasy.ProviderError, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	now := w.now()
	w.publishProviderErr(&providerErrRecord{
		from:             now.UnixNano(),
		until:            now.Add(delay + providerErrorGrace).UnixNano(),
		status:           reportableProviderStatus(providerErr),
		creditsExhausted: isCreditsExhausted(providerErr),
	})
}

// providerErrorLive reports whether a provider error is the explanation for
// the silence AT THIS MOMENT — i.e. the retry loop is inside the backoff that
// error bought. Once the next attempt has begun, silence is the model's own.
// Production never asks this live question: expire() forms its verdict and
// snapshots the record in one critical section (providerErrorStateAtExpiry).
func (w *firstChunkWatchdog) providerErrorLive() bool {
	now := w.now().UnixNano()
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := w.providerErr
	return rec != nil && rec.from <= now && now < rec.until
}

// providerErrorAtExpiry reports the provider-error state as it stood when the
// watchdog fired, and the status that went with it. One read, one instant.
func (w *firstChunkWatchdog) providerErrorAtExpiry() (seen bool, status int) {
	seen, status, _ = w.providerErrorStateAtExpiry()
	return seen, status
}

// proactiveCompact is proactiveCompactKeeping over a single history: the
// summary is written from, and the head and recent half are kept from, the
// same messages.
func (e *engine) proactiveCompact(ctx context.Context, messages []fantasy.Message) proactiveCompactResult {
	return e.proactiveCompactKeeping(ctx, messages, messages)
}

// pinServesOfficialWeightsOnly reports whether the exact slug (alias marker
// stripped) is listed in officialPoolSlugs.
func pinServesOfficialWeightsOnly(modelSlug string) bool {
	_, ok := officialPoolSlugs[strings.TrimPrefix(modelSlug, "~")]
	return ok
}

// resetToolSchemaRecordForTest empties the process-wide record.
func resetToolSchemaRecordForTest() {
	toolSchemaRecord.Lock()
	toolSchemaRecord.issues = map[string]ToolSchemaIssue{}
	toolSchemaRecord.Unlock()
}
