package agentcore

import (
	"log"
	"strings"
	"sync"

	"charm.land/fantasy"
)

// Default model identifiers + context-window resolution (reconciled from chat
// models.go + cutlass models.go/openrouter_models.go).
//
// Both repos pin the strong/max tier with an EXACT slug (never a `~latest`
// floating alias — fantasy drops thinking signatures for alias slugs, see
// isAliasModel). chat exported it as AdvancedModelSlug; cutlass as
// DefaultMaxModel. Both names are kept (same value) so downstream code in
// either mode resolves the same model.
const (
	// DefaultCoreModel is the cost-efficient primary (scheduled tasks + the
	// Operations Center default). No :nitro variant and no `~…-latest` alias:
	// throughput-priority routing sprays requests across providers, and prompt
	// caches are per-upstream — so the cache discount almost never hit; and an
	// alias slug defeats the send-side reasoning reconstruction (see
	// isAliasModel).
	//
	// Claude Haiku 5.5 (2026-10-15, replacing openai/gpt-6-luna-pro): the
	// everyday tier moves to Anthropic for speed. Luna Pro is a reasoning
	// ("pro") model that thinks before every visible token; on a live
	// benchmark from a production box (2026-10-14) it took ~5.7s per call on
	// a short prompt and ~10.3s on a ~25K-token one, and a data-heavy chat
	// turn makes one such call per tool step. Haiku 5.5 is priced the same
	// ($0.10/M in, $0.50/M out), with a 1,000,000-token window and 128K
	// output, served by Anthropic and the official resellers of its weights
	// (Claude Platform on AWS, Amazon Bedrock, Azure, Google Vertex) — the
	// `anthropic/` soft pin below keeps the prompt cache on one upstream, and
	// Anthropic's explicit cache breakpoints are already pinned by the live
	// prompt-cache check (docs/PROMPT-CACHE-CONTRACT.md). See
	// docs/MODEL-DEFAULTS.md.
	//
	// GPT-6 Luna Pro (2026-09-22, replacing openai/gpt-5.6-luna-pro): the
	// GPT-6 generation of the same everyday tier, $0.10/M in, $0.50/M out,
	// cache reads at $0.01/M, a 1,050,000-token window, and OpenAI's own three
	// endpoints (openai, openai/flex, openai/fast) — so the `openai/` soft pin
	// below still holds the prompt cache on one upstream. See
	// docs/MODEL-DEFAULTS.md.
	//
	// GPT-5.6 Luna Pro (2026-09-21, replacing google/gemini-3.8-flash): the
	// same Luna model served with reasoning.mode=pro, $0.20/M in, $1.20/M out,
	// cache reads at a tenth of that, a 1,050,000-token window and OpenAI's
	// single-family endpoints. The switch was measured on production's
	// scheduled jobs on one day: gemini-3.8-flash averaged 9.4M prompt tokens
	// at a 46% cache-hit rate and $4.76 per run with seven dead-letters
	// (Google's implicit cache kept going cold across provider hops); the same
	// jobs on Luna Pro ran in 4–10 minutes at $0.16–0.62 with 89–95% cache
	// hits. The `openai/` soft pin in canonicalUpstream keeps the prompt cache
	// on one upstream with graceful degradation.
	DefaultCoreModel = "anthropic/claude-haiku-5.5"
	// DefaultMaxModel is the strong tier — the model escalation
	// (suggest_advanced_model, chat's "advanced model") and the default task
	// tier resolve to. Exact slug, never a `~latest` alias. Claude Opus 5.5
	// (2026-10-28, replacing google/gemini-4-argon): 1,000,000-token window and
	// eleven official endpoints across Anthropic, Claude Platform on AWS,
	// Amazon Bedrock, Azure and Google Vertex, at $4/M in and $20/M out.
	// Gemini 4 Argon's strict single-provider `google/` pin had no upstream
	// fallback and suffered repeated mid-stream 504 idle timeouts on long
	// tool-heavy turns (such as Pages dashboard builds); Opus 5.5 restores the
	// multi-cloud official-weights pool and explicit prompt-cache breakpoints.
	// History: Gemini 4 Argon (2026-10-14, replacing anthropic/claude-opus-5.5);
	// Claude Opus 5.5 (2026-09-22, replacing anthropic/claude-opus-5); Claude
	// Opus 5 (2026-09-21, replacing openai/gpt-5.6-sol). The scheduled-task
	// FALLBACK is configured separately (FLEET_TASK_FALLBACK_MODEL; production
	// uses deepseek/deepseek-v4.1-flash).
	DefaultMaxModel = "anthropic/claude-opus-5.5"
	// AdvancedModelSlug is chat's name for the same strong tier. Kept in sync
	// with DefaultMaxModel.
	AdvancedModelSlug = DefaultMaxModel
	// DefaultMaxCompletionTokens caps a single completion's output tokens.
	DefaultMaxCompletionTokens = 16384
	// SuggestAdvancedCooldownTurns is the chat suggest_advanced_model cooldown.
	SuggestAdvancedCooldownTurns = 3
)

// minCompletionTokens lists slug prefixes whose per-call output allowance must
// not drop below a floor. Gemini 4 counts thinking against max_output_tokens and
// reasons at medium effort by default, so a cap below the floor can be spent
// entirely on thinking and return a truncated or empty answer; Google's Gemini 4
// Argon migration guide says to raise any limit under 32K to at least 32,768.
// The floor only RAISES a smaller configured cap — it never lowers one — and the
// run's cost/token ceilings still bound spend. Longest prefix first.
var minCompletionTokens = []struct {
	prefix string
	tokens int
}{
	{"google/gemini-4", 32_768},
}

// completionTokensForModel returns the per-call output allowance for slug: the
// configured cap, raised to the model's minCompletionTokens floor when it is
// below it. Callers use the SAME value for the request's MaxOutputTokens and
// for the context reserve (contextAccounting), so the reserve stays exact.
func completionTokensForModel(slug string, configured int64) int64 {
	m := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(slug)), "~")
	for _, f := range minCompletionTokens {
		if strings.HasPrefix(m, f.prefix) {
			return max(configured, int64(f.tokens))
		}
	}
	return configured
}

// modelOpenAIGPT5 is hoisted because the context-window table and several test
// fixtures assert on this exact slug.
const modelOpenAIGPT5 = "openai/gpt-5"

// defaultModelContextWindow is the fallback context window when the slug isn't
// in the lookup. 200K matches Anthropic's Claude 4 family.
const defaultModelContextWindow = 200_000

// modelContextWindows maps an OpenRouter-style slug prefix to its upstream
// context window (tokens). Prefix match, longest-first.
var modelContextWindows = []struct {
	prefix string
	tokens int
}{
	// The everyday default (DefaultCoreModel). An exact-slug entry, not a
	// `google/gemini-3` family prefix: the Nano Banana image variants in that
	// family are 65K-131K, and an over-large window is worse than a missing one
	// — a missing entry falls back to the conservative 200K default and merely
	// compacts early, while an over-large one feeds the upstream more than it
	// accepts and hard-errors. Cold-start/offline only; a running fleet gets
	// this from the live OpenRouter catalog.
	//
	// Gemini 4 Argon is the strong tier (DefaultMaxModel, 2026-10-14); exact
	// slug for the same reason.
	//
	// 3.7 keeps its row alongside 3.8: the slug stays
	// selectable (and stays the fallback an admin-configured tier may point
	// at), and both serve the same 1,048,576 window.
	{"google/gemini-4-argon", 1_048_576},
	{"google/gemini-3.8-flash", 1_048_576},
	{"google/gemini-3.7-flash", 1_048_576},
	{"google/gemini-2.5-pro", 1_000_000},
	{"google/gemini-2.0", 1_000_000},
	{"google/gemini-1.5-pro", 1_000_000},
	{"moonshotai/kimi", 256_000},
	// The V4 family is 1M, an order of magnitude past the V3 line below it.
	// Longer prefix first: this table returns the FIRST match, so ordering is
	// what makes "longest-first" true. Cold-start/offline only — a running
	// fleet gets this from the live OpenRouter catalog — but getting it wrong
	// means compacting a 1M-window default at 128K on every cold boot.
	{"deepseek/deepseek-v4", 1_048_576},
	{"deepseek/", 128_000},
	// The 5.6 family (sol / luna / terra, and their -pro variants) is 1,050,000
	// across the board. This must stay AHEAD of the generic `openai/gpt-5` row
	// below: that row is 400K, and this table returns the FIRST match, so
	// without this line the strong tier — the slug users escalate to for their
	// LARGEST problems — would cold-boot compacting at 38% of its real window.
	{"openai/gpt-5.6", 1_050_000},
	// GPT-6 (sol / luna / astra and their -pro variants; gpt-6-luna-pro was
	// DefaultCoreModel until 2026-10-15) is 1,050,000 too. No generic openai/gpt-6 row existed, so
	// without this the everyday tier would fall through to the default window.
	{"openai/gpt-6", 1_050_000},
	{"openai/gpt-4.1", 1_000_000},
	{"openai/o1", 200_000},
	{modelOpenAIGPT5, 400_000},
	// Claude 5 ships a 1M window (this prefix also covers Opus 5.5, the
	// DefaultMaxModel until 2026-10-14); the generic row
	// below keeps the Claude 4 family at 200K. Longest prefix first.
	{"anthropic/claude-opus-5", 1_000_000},
	{"anthropic/claude-sonnet-5", 1_000_000},
	// Haiku 5 (DefaultCoreModel is claude-haiku-5.5, 2026-10-15) is 1M as
	// well; without this row the everyday tier would cold-boot on the 200K
	// Claude 4 line below and compact at a fifth of its real window. The
	// prefix stops at "haiku-5" so claude-haiku-4.5 (200K) keeps the
	// generic row.
	{"anthropic/claude-haiku-5", 1_000_000},
	{"anthropic/claude", 200_000},
	// 4.6 is 500K; the generic grok entry below is the old 131K line and still
	// covers earlier builds. Longer prefix first, as above. (4.6 held the strong
	// tier for one release; the rows stay because the slugs stay selectable.)
	{"x-ai/grok-4.6", 500_000},
	{"x-ai/grok", 131_072},
}

// contextWindowForModel returns the upstream context window (tokens) for a slug.
// Resolution order:
//  1. observed cache (recordContextMax write-backs from provider
//     context-too-large errors) — per-request ground truth;
//  2. live OpenRouter /api/v1/models cache (openrouter_models.go) — refreshed
//     every 24h, the authoritative source for any slug OpenRouter knows;
//  3. static prefix table (below) — cold-start / offline fallback;
//  4. defaultModelContextWindow.
func contextWindowForModel(slug string) int {
	if n := contextLengthFromOpenRouter(slug); n > 0 {
		return n
	}
	return contextWindowForOpenRouterCatalog(slug)
}

// contextWindowForOpenRouterCatalog resolves only OpenRouter's per-model
// metadata and conservative static fallback. Provider-wrapped OpenRouter models
// use this after their provider-scoped observation lookup; consulting the
// legacy unscoped observed cache here would reintroduce same-slug contamination.
func contextWindowForOpenRouterCatalog(slug string) int {
	if n := contextLengthFromOpenRouterLive(slug); n > 0 {
		return n
	}
	return staticContextWindow(slug)
}

// staticContextWindow is the compiled-in fallback alone: the first matching
// modelContextWindows prefix, else defaultModelContextWindow. It is what a cold
// boot with no OpenRouter catalog sees.
func staticContextWindow(slug string) int {
	m := strings.ToLower(strings.TrimSpace(slug))
	for _, entry := range modelContextWindows {
		if strings.HasPrefix(m, entry.prefix) {
			return entry.tokens
		}
	}
	return defaultModelContextWindow
}

func contextWindowForActiveModel(model fantasy.LanguageModel) int {
	if model == nil {
		return defaultModelContextWindow
	}
	// Provider context errors are request-ground-truth and override both an
	// operator declaration and the conservative Ollama fallback. The lookup is
	// scoped to provider identity as well as slug: fallback providers commonly
	// expose the same slug with different limits.
	if observed := observedContextWindowForModel(model); observed > 0 {
		return observed
	}
	if named, ok := model.(*providerNamedModel); ok {
		if named.providerType == ProviderTypeOpenRouter {
			// Never let a generic manifest declaration outrank OpenRouter's
			// per-model live catalog. This branch also protects manually composed
			// wrappers; the production resolver leaves the OpenRouter declaration
			// unset in resolvedProviderContextWindow.
			return contextWindowForOpenRouterCatalog(model.Model())
		}
		if named.contextWindowTokens > 0 {
			return named.contextWindowTokens
		}
	}
	return contextWindowForModel(model.Model())
}

const (
	// defaultContextPressureWarnThreshold is the fraction of a model's context
	// window at which the run loop emits a fleet.context_pressure warning (#209).
	defaultContextPressureWarnThreshold = 0.75
	// defaultContextCompactionThreshold is the fraction at which the run loop
	// proactively compacts the oldest history and emits fleet.context_compacted.
	defaultContextCompactionThreshold = 0.90
)

// contextPressureWarnThreshold resolves FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD
// (with the CHAT_/CUTLASS_ aliases the EnvPrefix machinery already honors),
// clamped to (0,1]. An unset, unparseable, or out-of-range value falls back to
// the default.
func contextPressureWarnThreshold(p EnvPrefix) float64 {
	return clampFraction(
		p.lookupFloatDefault("CONTEXT_PRESSURE_WARN_THRESHOLD", defaultContextPressureWarnThreshold),
		defaultContextPressureWarnThreshold,
	)
}

// contextCompactionThreshold resolves FLEET_CONTEXT_COMPACTION_THRESHOLD the
// same way, clamped to (0,1].
func contextCompactionThreshold(p EnvPrefix) float64 {
	return clampFraction(
		p.lookupFloatDefault("CONTEXT_COMPACTION_THRESHOLD", defaultContextCompactionThreshold),
		defaultContextCompactionThreshold,
	)
}

// defaultBudgetWindDownFraction is the fraction of the cost/token ceiling at
// which the run starts receiving a request-local wrap-up notice (#990,
// borrowed from Prime Agent's goal budget wind-down): the model is told to
// stop starting substantive work and report progress/remaining/blockers
// BEFORE the hard ceiling cuts it off mid-thought.
const defaultBudgetWindDownFraction = 0.8

// budgetWindDownFraction resolves FLEET_BUDGET_WINDDOWN_FRACTION, clamped to
// (0,1] like the context thresholds. Setting it to 1 disables the notice in
// practice: at 100% the hard ceiling (budgetGuardedStep) fires first.
func budgetWindDownFraction(p EnvPrefix) float64 {
	return clampFraction(
		p.lookupFloatDefault("BUDGET_WINDDOWN_FRACTION", defaultBudgetWindDownFraction),
		defaultBudgetWindDownFraction,
	)
}

// defaultContextResendBudgetTokens is the cost-aware compaction trigger for
// SCHEDULED runs (#1534): the prompt a run resends on every provider call is
// compacted once it exceeds this many tokens, independent of the model's
// context window. A 1M-window model never reaches the window-pressure
// threshold in a 100-turn run, yet every one of those turns pays for the whole
// transcript again — the Reklaim health scan resent ~115K tokens per call for
// ~100 calls at an 11% cache-hit rate. 0 disables the trigger.
const defaultContextResendBudgetTokens = 80_000

// contextResendBudgetTokens resolves FLEET_CONTEXT_RESEND_BUDGET_TOKENS (with
// the CHAT_/CUTLASS_ aliases): 0 disables, a negative or unparseable value
// falls back to the default.
func contextResendBudgetTokens(p EnvPrefix) int {
	v := p.lookupFloatDefault("CONTEXT_RESEND_BUDGET_TOKENS", float64(defaultContextResendBudgetTokens))
	if v < 0 {
		return defaultContextResendBudgetTokens
	}
	return int(v)
}

// contextElideBeforeSummary resolves FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY
// (#1742): on unless set to a false value. When on, a scheduled run's
// compaction first replaces stale bulky tool results with stubs and buys the
// LLM summary only if that did not free enough (see elideBeforeSummary).
func contextElideBeforeSummary(p EnvPrefix) bool {
	return p.lookupBoolDefault("CONTEXT_ELIDE_BEFORE_SUMMARY", true)
}

// clampFraction returns v when it lies in (0,1]; otherwise def. A misconfigured
// threshold must not silently compact every round (≤0) or never fire (>1).
func clampFraction(v, def float64) float64 {
	if v <= 0 || v > 1 {
		return def
	}
	return v
}

// observedContextWindows is the process-wide cache of context windows learned
// from provider context-too-large errors (ground truth for the active slug).
type observedContextKey struct {
	providerName string
	providerType ProviderType
	slug         string
}

var observedContextWindows = struct {
	mu sync.RWMutex
	m  map[observedContextKey]int
}{m: make(map[observedContextKey]int)}

func observedContextKeyForModel(model fantasy.LanguageModel) observedContextKey {
	if model == nil {
		return observedContextKey{}
	}
	key := observedContextKey{slug: strings.ToLower(strings.TrimSpace(model.Model()))}
	if named, ok := model.(*providerNamedModel); ok {
		key.providerName = strings.ToLower(strings.TrimSpace(named.providerName))
		key.providerType = named.providerType
	}
	return key
}

func observedContextWindowForModel(model fantasy.LanguageModel) int {
	return observedContextWindow(observedContextKeyForModel(model))
}

func observedContextWindow(key observedContextKey) int {
	if key.slug == "" {
		return 0
	}
	observedContextWindows.mu.RLock()
	defer observedContextWindows.mu.RUnlock()
	return observedContextWindows.m[key]
}

// contextLengthFromOpenRouter returns the observed context_length for slug, or 0
// when unknown for an unwrapped historical OpenRouter model. Provider-wrapped
// production models use observedContextWindowForModel so same-slug fallbacks
// cannot contaminate each other's learned limits.
func contextLengthFromOpenRouter(slug string) int {
	return observedContextWindow(observedContextKey{slug: strings.ToLower(strings.TrimSpace(slug))})
}

// recordContextMax writes an observed context_max back into the cache. Called
// directly only by historical unwrapped-model tests. Production provider errors
// call recordContextMaxForModel so the provider identity is part of the key.
func recordContextMax(slug string, tokens int) {
	recordObservedContextWindow(observedContextKey{slug: strings.ToLower(strings.TrimSpace(slug))}, tokens)
}

func recordContextMaxForModel(model fantasy.LanguageModel, tokens int) {
	recordObservedContextWindow(observedContextKeyForModel(model), tokens)
}

func recordObservedContextWindow(key observedContextKey, tokens int) {
	if tokens <= 0 || key.slug == "" {
		return
	}
	observedContextWindows.mu.Lock()
	defer observedContextWindows.mu.Unlock()
	existing := observedContextWindows.m[key]
	if existing == tokens {
		return
	}
	observedContextWindows.m[key] = tokens
	log.Printf("📏 Recorded ContextMaxTokens for provider=%s type=%s model=%s: %d (was %d)",
		key.providerName, key.providerType, key.slug, tokens, existing)
}
