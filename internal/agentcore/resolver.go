package agentcore

import (
	"context"
	"fmt"
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
	providers []ProviderConfig            // routing table, in precedence order
	built     map[string]fantasy.Provider // provider handle by name (eager)

	mu    sync.RWMutex
	cache map[string]fantasy.LanguageModel // by ORIGINAL slug
}

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
		seen[name] = true
		p, err := buildProvider(providers[i], headers)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		built[name] = p
	}
	return &ModelResolver{
		providers: append([]ProviderConfig(nil), providers...),
		built:     built,
		cache:     map[string]fantasy.LanguageModel{},
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

	r.mu.Lock()
	r.cache[slug] = mdl
	r.mu.Unlock()
	return mdl, nil
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
