package agentcore

import (
	"log"
	"strings"
	"sync"
)

// Default model identifiers + context-window resolution (reconciled from chat
// models.go + cutlass models.go/openrouter_models.go).
//
// Both repos pin Opus 4.8 as the advanced/max tier with an EXACT slug (never a
// `~latest` floating alias — fantasy drops thinking signatures for alias slugs,
// see isAliasModel). chat exported it as AdvancedModelSlug; cutlass as
// DefaultMaxModel. Both names are kept (same value) so downstream code in either
// mode resolves the same model.
const (
	// DefaultCoreModel is the cost-efficient primary (cutlass's default).
	DefaultCoreModel = "moonshotai/kimi-k2.6"
	// DefaultMaxModel is the advanced/fallback tier — Opus 4.8, 1M context via
	// the Anthropic long-context beta. Pinned, never a `~latest` alias.
	DefaultMaxModel = "anthropic/claude-opus-4.8"
	// AdvancedModelSlug is chat's name for the same advanced tier. Kept in sync
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
// Resolution order: OpenRouter-reported value (self-correcting cache, populated
// from provider context-too-large errors via recordContextMax) → static prefix
// table → default. The live /api/v1/models fetch is a P3 concern; here the cache
// is seeded only from observed provider errors so it never needs the network.
func contextWindowForModel(slug string) int {
	if n := contextLengthFromOpenRouter(slug); n > 0 {
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

// observedContextWindows is the process-wide cache of context windows learned
// from provider context-too-large errors (ground truth for the active slug).
var observedContextWindows = struct {
	mu sync.RWMutex
	m  map[string]int
}{m: make(map[string]int)}

// contextLengthFromOpenRouter returns the observed context_length for slug, or 0
// when unknown. The live network fetch is deferred to P3; this reads only the
// self-correcting cache populated by recordContextMax.
func contextLengthFromOpenRouter(slug string) int {
	key := strings.ToLower(strings.TrimSpace(slug))
	if key == "" {
		return 0
	}
	observedContextWindows.mu.RLock()
	defer observedContextWindows.mu.RUnlock()
	return observedContextWindows.m[key]
}

// recordContextMax writes an observed context_max back into the cache. Called
// from resilience.go when a provider error surfaces ContextMaxTokens — ground
// truth for the current request that self-corrects staleness.
func recordContextMax(slug string, tokens int) {
	if tokens <= 0 || strings.TrimSpace(slug) == "" {
		return
	}
	key := strings.ToLower(strings.TrimSpace(slug))
	observedContextWindows.mu.Lock()
	defer observedContextWindows.mu.Unlock()
	existing := observedContextWindows.m[key]
	if existing == tokens {
		return
	}
	observedContextWindows.m[key] = tokens
	log.Printf("📏 Recorded ContextMaxTokens for %s: %d (was %d)", key, tokens, existing)
}
