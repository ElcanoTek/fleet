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
	// caches are per-upstream — so the implicit-cache discount (~80% on cached
	// input) almost never hit; and an alias slug defeats the send-side
	// reasoning reconstruction (see isAliasModel).
	//
	// Google serves this family themselves, so canonicalUpstream pins it STRICT
	// (Only, no fallbacks): there is no provider spread to degrade across, which
	// is also why it carries no serving-precision floor — the fp8 floor under
	// the previous DeepSeek default existed because 28 OpenRouter endpoints
	// served that family at fp4-to-fp8. One upstream also means one prompt
	// cache and one context length: the full 1,048,576.
	DefaultCoreModel = "google/gemini-3.7-flash"
	// DefaultMaxModel is the strong/fallback tier — the model escalation
	// (suggest_advanced_model) and task fallback resolve to. Exact slug, never a
	// `~latest` alias. Unlike the previous xAI occupant of this slot, this one
	// DOES match a canonicalUpstream entry (the `openai/` soft pin), so the
	// escalation path inherits the same per-upstream prompt-cache locality the
	// rest of that family gets.
	DefaultMaxModel = "openai/gpt-5.6-sol"
	// AdvancedModelSlug is chat's name for the same strong tier. Kept in sync
	// with DefaultMaxModel.
	AdvancedModelSlug = DefaultMaxModel
	// DefaultMaxCompletionTokens caps a single completion's output tokens.
	DefaultMaxCompletionTokens = 16384
	// SuggestAdvancedCooldownTurns is the chat suggest_advanced_model cooldown.
	SuggestAdvancedCooldownTurns = 3
)

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
	{"openai/gpt-4.1", 1_000_000},
	{"openai/o1", 200_000},
	{modelOpenAIGPT5, 400_000},
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
