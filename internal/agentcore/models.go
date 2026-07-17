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
	// Operations Center default). No :nitro variant: throughput-priority
	// routing sprays requests across ~26 GLM providers, and prompt caches are
	// per-upstream — so the implicit-cache discount (~80% on cached input)
	// almost never hit. The plain slug is soft-pinned to the Z.AI upstream
	// (see canonicalUpstream) for cache locality, verified live 2026-07-09:
	// 3968/4018 prompt tokens served from cache on the second pinned call.
	DefaultCoreModel = "z-ai/glm-5.2"
	// DefaultMaxModel is the strong/fallback tier — the model escalation
	// (suggest_advanced_model) and task fallback resolve to. Pinned, never a
	// `~latest` alias.
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
	{"google/gemini-2.5-pro", 1_000_000},
	{"google/gemini-2.0", 1_000_000},
	{"google/gemini-1.5-pro", 1_000_000},
	{"moonshotai/kimi", 256_000},
	{"deepseek/", 128_000},
	{"openai/gpt-4.1", 1_000_000},
	{"openai/o1", 200_000},
	{modelOpenAIGPT5, 400_000},
	{"anthropic/claude", 200_000},
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
