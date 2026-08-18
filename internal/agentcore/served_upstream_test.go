package agentcore

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

// Served-upstream attribution. A soft pin (Order + AllowFallbacks) states a
// PREFERENCE: OpenRouter is free to route elsewhere whenever the canonical
// upstream is busy, which costs the per-upstream prompt cache and can land the
// request on a different serving precision of the same slug. The run only knew
// the cost from the provider metadata and threw the rest away, so a turn
// degraded by a fallback route looked exactly like the model being bad. These
// pin that the run records which upstream actually served it.

func orMetadata(provider string) fantasy.ProviderMetadata {
	return fantasy.ProviderMetadata{
		openrouter.Name: &openrouter.ProviderMetadata{Provider: provider},
	}
}

func TestOpenrouterServedProvider(t *testing.T) {
	if got := openrouterServedProvider(orMetadata("DeepSeek")); got != "DeepSeek" {
		t.Errorf("served provider = %q, want %q", got, "DeepSeek")
	}
	// Whitespace is trimmed so it compares equal to the pin table's spelling.
	if got := openrouterServedProvider(orMetadata("  DeepSeek  ")); got != "DeepSeek" {
		t.Errorf("served provider = %q, want trimmed %q", got, "DeepSeek")
	}
	// Absent metadata is not an error — non-OpenRouter providers report none.
	if got := openrouterServedProvider(fantasy.ProviderMetadata{}); got != "" {
		t.Errorf("served provider = %q, want empty for absent metadata", got)
	}
}

// A run served by its canonical upstream is not a fallback.
func TestUpdateUsage_CanonicalUpstreamIsNotAFallback(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 50)
	o.updateUsage(DefaultCoreModel, fantasy.Usage{InputTokens: 10, OutputTokens: 5}, orMetadata("Google"))

	if o.ServedFallback {
		t.Error("ServedFallback = true for a step served by the pinned upstream")
	}
	if o.LastServedUpstream != "Google" {
		t.Errorf("LastServedUpstream = %q, want %q", o.LastServedUpstream, "Google")
	}
}

// A step served by anything other than the pinned upstream latches the flag —
// this is the signal that explains an otherwise inexplicable bad response.
func TestUpdateUsage_RecordsUpstreamFallback(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 50)
	o.updateUsage(DefaultCoreModel, fantasy.Usage{InputTokens: 10, OutputTokens: 5}, orMetadata("DeepInfra"))

	if !o.ServedFallback {
		t.Error("ServedFallback = false for a step served off the pinned upstream")
	}
	if o.LastServedUpstream != "DeepInfra" {
		t.Errorf("LastServedUpstream = %q, want %q", o.LastServedUpstream, "DeepInfra")
	}

	// The flag latches: a later step returning to the canonical upstream must
	// not erase the fact that part of the run was served elsewhere.
	o.updateUsage(DefaultCoreModel, fantasy.Usage{InputTokens: 10, OutputTokens: 5}, orMetadata("Google"))
	if !o.ServedFallback {
		t.Error("ServedFallback cleared after the run returned to the pinned upstream; it must latch")
	}
	if o.LastServedUpstream != "Google" {
		t.Errorf("LastServedUpstream = %q, want the most recent upstream %q", o.LastServedUpstream, "Google")
	}
}

// An UNPINNED family has no canonical upstream, so no route is a "fallback".
func TestUpdateUsage_UnpinnedFamilyNeverFlagsFallback(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 50)
	const unpinned = "x-ai/grok-4.6" // no canonicalUpstream entry: xAI is its only upstream
	o.updateUsage(unpinned, fantasy.Usage{InputTokens: 10, OutputTokens: 5}, orMetadata("xAI"))

	if o.ServedFallback {
		t.Errorf("ServedFallback = true for unpinned model %q", unpinned)
	}
	if o.LastServedUpstream != "xAI" {
		t.Errorf("LastServedUpstream = %q, want %q", o.LastServedUpstream, "xAI")
	}
}

// Providers that report no upstream must leave the attribution untouched
// rather than clobbering a previously recorded one with "".
func TestUpdateUsage_AbsentMetadataPreservesAttribution(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 50)
	o.updateUsage(DefaultCoreModel, fantasy.Usage{InputTokens: 10, OutputTokens: 5}, orMetadata("Google"))
	o.updateUsage(DefaultCoreModel, fantasy.Usage{InputTokens: 10, OutputTokens: 5}, fantasy.ProviderMetadata{})

	if o.LastServedUpstream != "Google" {
		t.Errorf("LastServedUpstream = %q, want the last known upstream %q", o.LastServedUpstream, "Google")
	}
}
