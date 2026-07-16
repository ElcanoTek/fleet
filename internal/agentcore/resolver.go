package agentcore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

// ModelResolver is the exported, cached model loader the interactive driver
// (agent.Manager) and the scheduled boot path use. The server holds no default
// model — the frontend sends a slug per turn — so an empty slug is a hard error
// rather than a silent fallback. Loaded models are memoized so a given slug pays
// the load cost only once across the whole process.
//
// It resolves the slug to one of its configured providers (#289) and loads the
// model through that provider. With a single OpenRouter provider (the default,
// via NewModelResolver) it behaves exactly as the historical OpenRouter-only
// resolver. It is the one exported entry point for "give me a
// fantasy.LanguageModel for this slug".
type ModelResolver struct {
	providers         []ProviderConfig            // routing table, in precedence order
	built             map[string]fantasy.Provider // provider handle by name (eager)
	fallbackProviders []string                    // ordered provider names; empty = disabled

	mu    sync.RWMutex
	cache map[string]fantasy.LanguageModel // by ORIGINAL slug
}

type providerNamedModel struct {
	fantasy.LanguageModel
	providerName        string
	providerType        ProviderType
	contextWindowTokens int
}

func (m *providerNamedModel) fleetProviderName() string { return m.providerName }

// NewModelResolver builds a resolver backed by a single catch-all OpenRouter
// provider — the historical, backward-compatible default. An empty API key is a
// hard error (OpenRouter is the sole credential in this mode).
func NewModelResolver(apiKey string, headers ProviderHeaders) (*ModelResolver, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY required")
	}
	return NewModelResolverWithProviders([]ProviderConfig{{
		Name:   "openrouter",
		Type:   ProviderTypeOpenRouter,
		APIKey: apiKey,
	}}, headers)
}

// NewModelResolverWithProviders builds a resolver over an explicit, ordered set
// of providers (#289). Each provider is constructed eagerly (no network — just
// the client handle) so a misconfigured provider fails at boot, not on the first
// turn that needs it. Provider names must be non-empty and unique.
func NewModelResolverWithProviders(providers []ProviderConfig, headers ProviderHeaders) (*ModelResolver, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("at least one LLM provider is required")
	}
	built := make(map[string]fantasy.Provider, len(providers))
	seen := make(map[string]bool, len(providers))
	for i := range providers {
		name := strings.TrimSpace(providers[i].Name)
		if name == "" {
			return nil, fmt.Errorf("provider[%d]: name is required", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate provider name %q", name)
		}
		if providers[i].ContextWindowTokens < 0 {
			return nil, fmt.Errorf("provider %q: context window tokens must be positive when set", name)
		}
		seen[name] = true
		p, err := buildProvider(providers[i], headers)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		built[name] = p
	}
	var fallbackProviders []string
	for i := range providers {
		if len(providers[i].FallbackProviders) > 0 {
			fallbackProviders = append([]string(nil), providers[i].FallbackProviders...)
			break
		}
	}
	for i, name := range fallbackProviders {
		if !seen[name] {
			return nil, fmt.Errorf("fallback provider[%d] %q is not configured", i, name)
		}
	}
	return &ModelResolver{
		providers:         append([]ProviderConfig(nil), providers...),
		built:             built,
		fallbackProviders: fallbackProviders,
		cache:             map[string]fantasy.LanguageModel{},
	}, nil
}

// Resolve returns the LanguageModel for the given slug, selecting the provider
// that serves it (#289), then loading + caching the model on first use. An empty
// slug is an error, as is a slug no configured provider serves.
func (r *ModelResolver) Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("model slug required (frontend must send one)")
	}

	r.mu.RLock()
	if cached, ok := r.cache[slug]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()

	pc, modelSlug, err := selectProvider(r.providers, slug)
	if err != nil {
		return nil, err
	}
	provider, ok := r.built[pc.Name]
	if !ok {
		// Unreachable: every configured provider is built at construction.
		return nil, fmt.Errorf("provider %q not built", pc.Name)
	}

	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	mdl, err := provider.LanguageModel(loadCtx, modelSlug)
	if err != nil {
		return nil, fmt.Errorf("load model %q via provider %q: %w", modelSlug, pc.Name, err)
	}

	mdl = &providerNamedModel{
		LanguageModel: mdl, providerName: pc.Name, providerType: pc.Type,
		contextWindowTokens: resolvedProviderContextWindow(pc),
	}
	r.mu.Lock()
	r.cache[slug] = mdl
	r.mu.Unlock()
	return mdl, nil
}

// ResolveWithFallback resolves the normal primary plus the next eligible
// provider in the configured cross-provider chain. Explicit provider-prefixed
// slugs are isolation pins and therefore never acquire an implicit fallback.
func (r *ModelResolver) ResolveWithFallback(ctx context.Context, slug string) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
	primary, fallbacks, err := r.ResolveWithFallbacks(ctx, slug)
	if err != nil || len(fallbacks) == 0 {
		return primary, nil, err
	}
	return primary, fallbacks[0], nil
}

// ResolveWithFallbacks returns every eligible backend after the primary in
// chain order. The resilience loop consumes them one at a time.
func (r *ModelResolver) ResolveWithFallbacks(ctx context.Context, slug string) (fantasy.LanguageModel, []fantasy.LanguageModel, error) {
	primary, err := r.Resolve(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	pc, modelSlug, err := selectProvider(r.providers, strings.TrimSpace(slug))
	if err != nil {
		return nil, nil, err
	}
	if name, rest, ok := strings.Cut(strings.TrimSpace(slug), "/"); ok && rest != "" {
		for i := range r.providers {
			if r.providers[i].Name == name {
				return primary, nil, nil
			}
		}
	}
	start := -1
	for i, name := range r.fallbackProviders {
		if name == pc.Name {
			start = i
			break
		}
	}
	if start < 0 {
		return primary, nil, nil
	}
	var fallbacks []fantasy.LanguageModel
	for _, name := range r.fallbackProviders[start+1:] {
		var candidate *ProviderConfig
		for i := range r.providers {
			if r.providers[i].Name == name {
				candidate = &r.providers[i]
				break
			}
		}
		if candidate == nil || (len(candidate.Models) > 0 && !slices.Contains(candidate.Models, modelSlug)) {
			continue
		}
		fallback, loadErr := r.resolveProviderModel(ctx, *candidate, modelSlug)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		fallbacks = append(fallbacks, fallback)
	}
	return primary, fallbacks, nil
}

func (r *ModelResolver) resolveProviderModel(ctx context.Context, pc ProviderConfig, modelSlug string) (fantasy.LanguageModel, error) {
	key := pc.Name + "/" + modelSlug
	r.mu.RLock()
	if cached, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()
	provider := r.built[pc.Name]
	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	mdl, err := provider.LanguageModel(loadCtx, modelSlug)
	if err != nil {
		return nil, fmt.Errorf("load fallback model %q via provider %q: %w", modelSlug, pc.Name, err)
	}
	mdl = &providerNamedModel{
		LanguageModel: mdl, providerName: pc.Name, providerType: pc.Type,
		contextWindowTokens: resolvedProviderContextWindow(pc),
	}
	r.mu.Lock()
	r.cache[key] = mdl
	r.mu.Unlock()
	return mdl, nil
}

func resolvedProviderContextWindow(cfg ProviderConfig) int {
	if cfg.Type == ProviderTypeOpenRouter {
		// OpenRouter publishes per-model context_length in its live catalog and
		// provider context errors refine that value at runtime. A manifest-wide
		// declaration can only be less specific and, if inflated, would let an
		// oversized request pass the pre-provider budget. Keep the wrapper unset so
		// contextWindowForActiveModel must use authoritative model metadata.
		return 0
	}
	if cfg.ContextWindowTokens > 0 {
		return cfg.ContextWindowTokens
	}
	if cfg.Type == ProviderTypeOllama {
		// Ollama model ids do not appear in OpenRouter's context catalog and the
		// OpenAI-compatible model handle exposes no context metadata. Fail-safe at
		// Ollama's conservative baseline unless the operator declares the actual
		// num_ctx via context_window_tokens.
		return 4096
	}
	// Native and OpenAI-compatible handles do not expose a context limit and
	// their provider-local slugs need not match OpenRouter's catalog. Avoid
	// assuming Fleet's 200K OpenRouter fallback for an unknown 32K endpoint.
	// Operators should declare the exact limit above; 32K keeps an omitted
	// declaration safe for the common native baseline without pretending the
	// remote endpoint advertised metadata it did not.
	return 32_000
}

// ── exported stream-error classification (for the interactive turn.model_required path) ──

// StreamErrorReason is the machine-readable reason the interactive driver surfaces
// to the frontend's model picker when a turn fails in a way the user can fix by
// choosing a different model. Mirrors chat's ModelSelectionReason.
type StreamErrorReason string

const (
	// ReasonContextTooLarge: the prompt exceeded the model's context window.
	ReasonContextTooLarge StreamErrorReason = "context_too_large"
	// ReasonRetryExhausted: the provider failed repeatedly (rate limits, 5xx).
	ReasonRetryExhausted StreamErrorReason = "retry_exhausted"
	// ReasonFatal: a non-retryable provider failure.
	ReasonFatal StreamErrorReason = "fatal"
)

// ClassifyStreamErrorReason classifies a raw Agent.Stream error into the
// frontend-facing reason plus the HTTP status (0 when none). Cancellation is
// reported separately via the cancelled bool so the caller can skip the picker.
func ClassifyStreamErrorReason(err error) (reason StreamErrorReason, status int, cancelled bool) {
	class, providerErr := classifyStreamError(err)
	switch class {
	case streamErrorCancelled:
		return ReasonFatal, providerErrStatus(providerErr), true
	case streamErrorContextTooLarge:
		return ReasonContextTooLarge, providerErrStatus(providerErr), false
	case streamErrorRetryExhausted, streamErrorStreamBlip:
		return ReasonRetryExhausted, providerErrStatus(providerErr), false
	default:
		return ReasonFatal, providerErrStatus(providerErr), false
	}
}
