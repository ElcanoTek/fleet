package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// #587 regression tests: one cache-token convention across every usage signal.
//
// fantasy (v0.33.x) normalizes ALL providers before usage reaches updateUsage:
// InputTokens is the UNCACHED (fresh) prompt input and CacheReadTokens is the
// cache-read subset. OpenRouter reports prompt_tokens INCLUDING cached and
// fantasy's openrouter hooks subtract the subset; the native Anthropic provider
// reports input_tokens already excluding cache reads. So an "OpenRouter-shaped"
// step of prompt_tokens=1000 with cached_tokens=800 arrives here as
// {InputTokens: 200, CacheReadTokens: 800} — identical to the native shape.
// These tests pin that every consumer counts each prompt token exactly ONCE:
// the context-pressure signal sees the true 1000-token prompt (not 1800), and
// the ceiling/budget math charges the 200 uncached tokens (not 200-800 = a
// negative that let the token ceiling never fire).

// TestUpdateUsage_CacheTokensCountedOnceAcrossSignals feeds one cached-heavy
// step through updateUsage and checks every derived signal.
func TestUpdateUsage_CacheTokensCountedOnceAcrossSignals(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 50)
	// OpenRouter-shaped: prompt_tokens=1000, cached_tokens=800 → fantasy delivers
	// InputTokens=200 (uncached) + CacheReadTokens=800.
	o.updateUsage("test/cached-model",
		fantasy.Usage{InputTokens: 200, OutputTokens: 50, CacheReadTokens: 800},
		fantasy.ProviderMetadata{})

	// The compaction/pressure signal is the TRUE prompt size: 1000 — not
	// prompt+cached (1800), and not uncached-only (200, which would hide a huge
	// cached prefix from the window check).
	if got := o.logSession.LastStepPromptTokens; got != 1000 {
		t.Fatalf("LastStepPromptTokens = %d, want 1000 (true prompt size; 1800 double-counts the cache)", got)
	}
	if got := o.LastStepInputTokens; got != 1000 {
		t.Fatalf("LastStepInputTokens = %d, want 1000 (context-window fill includes cache reads)", got)
	}

	// Cumulative accumulators follow the LogSession contract: PromptTokens
	// INCLUDES cache reads, CachedTokens is the cached subset.
	if o.PromptTokens != 1000 || o.CachedTokens != 800 {
		t.Fatalf("PromptTokens/CachedTokens = %d/%d, want 1000/800", o.PromptTokens, o.CachedTokens)
	}
	if o.logSession.PromptTokens != 1000 || o.logSession.CachedTokens != 800 {
		t.Fatalf("logSession PromptTokens/CachedTokens = %d/%d, want 1000/800",
			o.logSession.PromptTokens, o.logSession.CachedTokens)
	}
	if rate := o.logSession.CumulativeCacheHitRate(); rate < 79.0 || rate > 81.0 {
		t.Fatalf("cache hit rate = %.1f%%, want ~80%% (>100%% means PromptTokens excluded the cache)", rate)
	}

	// Ceiling/budget math charges uncached spend once: 200 fresh + 50 completion.
	if got := o.budgetState().SpentTokens; got != 250 {
		t.Fatalf("budgetState().SpentTokens = %d, want 250 (200 uncached + 50 completion)", got)
	}
}

// TestCheckCeilings_TokenCeilingFiresOnUncachedSpend pins the governance side:
// before #587 updateUsage accumulated bare InputTokens while checkCeilings
// subtracted CachedTokens from it, double-discounting the cache — this step's
// "total" computed to 200 - 800 + 50 = -550, so a 250-token ceiling never
// fired. The ceiling must fire exactly at the true uncached spend.
func TestCheckCeilings_TokenCeilingFiresOnUncachedSpend(t *testing.T) {
	usage := fantasy.Usage{InputTokens: 200, OutputTokens: 50, CacheReadTokens: 800}

	at := newOrchestrationState(NewLogSession(), 50)
	at.maxTotalTokens = 250 // == uncached spend → ceiling met
	at.updateUsage("test/cached-model", usage, fantasy.ProviderMetadata{})
	blocked, msg := at.checkCeilings()
	if !blocked {
		t.Fatal("token ceiling of 250 should block after 250 uncached tokens (cache double-discount regression)")
	}
	if !strings.Contains(msg, "TOKEN_CEILING_REACHED") {
		t.Fatalf("expected TOKEN_CEILING_REACHED, got: %s", msg)
	}

	under := newOrchestrationState(NewLogSession(), 50)
	under.maxTotalTokens = 251 // one above the uncached spend → not met
	under.updateUsage("test/cached-model", usage, fantasy.ProviderMetadata{})
	if blocked, msg := under.checkCeilings(); blocked {
		t.Fatalf("ceiling of 251 must not block at 250 uncached tokens (cached tokens are not charged): %s", msg)
	}
}

// TestCheckContextPressure_TrueUsageFractionWithHotCache wires updateUsage into
// the real pre-round pressure check: a step whose true prompt is half the
// window must stay silent even when most of it is cache reads. A signal that
// added the cached subset onto an already-inclusive total would report 0.90 of
// the window here and fire a premature proactive compaction (dropping the
// oldest half of the conversation) — the #587 failure scenario.
func TestCheckContextPressure_TrueUsageFractionWithHotCache(t *testing.T) {
	slug := "ctx587-hot-cache"
	recordContextMax(slug, testContextWindow) // 1000-token window
	model := &namedMockModel{name: slug}
	e := newMockEngine(t, model)
	obs := &captureObserver{}
	sink := newStreamSink(obs)
	orch := newOrchestrationState(e.logSession, 0)

	// True prompt: 500 of 1000 (0.50) — 400 of it cache reads. Double-counting
	// the cache would report 900 (0.90 ≥ the compaction threshold).
	orch.updateUsage(slug,
		fantasy.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 400},
		fantasy.ProviderMetadata{})
	if got := e.logSession.LastStepPromptTokens; got != 500 {
		t.Fatalf("LastStepPromptTokens = %d, want 500", got)
	}

	msgs := fillerMessages(4, 20)
	res := e.checkContextPressure(context.Background(), msgs, model, sink, false)
	if res.warned || len(obs.events) != 0 {
		t.Fatalf("true usage 0.50 of the window must neither warn nor compact; events=%v warned=%v",
			obs.events, res.warned)
	}
}
