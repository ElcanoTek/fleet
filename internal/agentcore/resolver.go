package agentcore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

// ModelResolver is the exported, cached OpenRouter model loader the interactive
// driver (agent.Manager) and the scheduled boot path use. The server holds no
// default model — the frontend sends a slug per turn — so an empty slug is a
// hard error rather than a silent fallback. Loaded models are memoized so a
// given slug pays the load cost only once across the whole process.
//
// This wraps the package-private fantasy provider + upstream-pin plumbing so
// the driver package can resolve models without re-importing openrouter or
// duplicating the cache; it is the one exported entry point for "give me a
// fantasy.LanguageModel for this slug".
type ModelResolver struct {
	provider fantasy.Provider

	mu    sync.RWMutex
	cache map[string]fantasy.LanguageModel
}

// NewModelResolver dials the OpenRouter provider with the given API key +
// identity headers and returns a cached resolver.
func NewModelResolver(apiKey string, headers ProviderHeaders) (*ModelResolver, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY required")
	}
	provider, err := newOpenRouterProvider(apiKey, headers)
	if err != nil {
		return nil, fmt.Errorf("openrouter provider: %w", err)
	}
	return &ModelResolver{provider: provider, cache: map[string]fantasy.LanguageModel{}}, nil
}

// Resolve returns the LanguageModel for the given slug, loading + caching it on
// first use. An empty slug is an error.
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

	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	mdl, err := r.provider.LanguageModel(loadCtx, slug)
	if err != nil {
		return nil, fmt.Errorf("load model %q: %w", slug, err)
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
