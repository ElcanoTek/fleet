package agentcore

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
)

// Multi-provider LLM support (#289). Fleet routed every model through OpenRouter
// as a singleton gateway. This adds native provider backends — Anthropic, OpenAI,
// and Ollama (via its OpenAI-compatible API) — alongside OpenRouter, selected by
// a client-bundle `providers:` block. The engine itself is already
// provider-agnostic (it holds fantasy.LanguageModel interfaces); the coupling was
// entirely in how those models were constructed (always the OpenRouter provider).
// This layer resolves the RIGHT provider for a given model slug and builds the
// model through it.
//
// Backward compatibility is the hard requirement: a bundle with NO providers:
// block behaves exactly as before — a single catch-all OpenRouter provider — so
// existing deployments that set only OPENROUTER_API_KEY are byte-for-byte
// unchanged (see NewModelResolver).

// ProviderType enumerates the supported LLM provider backends.
type ProviderType string

const (
	// ProviderTypeOpenRouter routes through OpenRouter (the historical default,
	// and a catch-all that accepts any slug). Honors the OPENROUTER_BASE_URL E2E
	// fake-LLM seam.
	ProviderTypeOpenRouter ProviderType = "openrouter"
	// ProviderTypeAnthropic calls the Anthropic API directly.
	ProviderTypeAnthropic ProviderType = "anthropic"
	// ProviderTypeOpenAI calls the OpenAI API directly.
	ProviderTypeOpenAI ProviderType = "openai"
	// ProviderTypeOllama reaches a local (or remote) Ollama server through its
	// OpenAI-compatible endpoint — there is no dedicated fantasy Ollama provider,
	// so it is the OpenAI provider pointed at the Ollama base URL.
	ProviderTypeOllama ProviderType = "ollama"
)

// defaultOllamaBaseURL is the OpenAI-compatible endpoint a local Ollama server
// exposes when a provider entry omits base_url.
const defaultOllamaBaseURL = "http://localhost:11434/v1"

// ProviderConfig is one resolved LLM provider. The API-key VALUE is resolved
// host-side (from the env var the manifest names) BEFORE this struct is built and
// handed to agentcore — the secret never lives in the manifest or in agentcore's
// config surface, only in the process env and this transient field. Models lists
// the slugs the provider serves; empty means catch-all (matches any slug).
type ProviderConfig struct {
	Name    string
	Type    ProviderType
	APIKey  string
	BaseURL string
	Models  []string
}

// selectProvider picks the provider that serves slug and returns it plus the
// provider-local model slug. Routing precedence:
//
//  1. Explicit "<provider-name>/<model>" — when the segment before the first "/"
//     exactly matches a CONFIGURED provider name, that provider is used and the
//     remainder is the model slug. (An OpenRouter slug like
//     "anthropic/claude-opus-4.8" is NOT explicit routing unless a provider is
//     literally named "anthropic"; "anthropic" there is an OpenRouter upstream,
//     not a fleet provider name.)
//  2. Implicit models-list match — the first provider (in manifest order) whose
//     Models list contains the slug.
//  3. Catch-all — the first provider with an empty Models list.
//
// A specifically-listed model always beats a catch-all regardless of order, so a
// catch-all OpenRouter entry can sit anywhere in the list.
func selectProvider(providers []ProviderConfig, slug string) (ProviderConfig, string, error) {
	// 1. Explicit name-prefixed routing.
	if name, rest, ok := strings.Cut(slug, "/"); ok && strings.TrimSpace(rest) != "" {
		for i := range providers {
			if providers[i].Name == name {
				return providers[i], rest, nil
			}
		}
	}
	// 2. Explicit models-list match (in manifest order).
	for i := range providers {
		if len(providers[i].Models) > 0 && slices.Contains(providers[i].Models, slug) {
			return providers[i], slug, nil
		}
	}
	// 3. First catch-all.
	for i := range providers {
		if len(providers[i].Models) == 0 {
			return providers[i], slug, nil
		}
	}
	return ProviderConfig{}, "", fmt.Errorf("no configured provider serves model %q", slug)
}

// buildProvider constructs the fantasy.Provider for a resolved ProviderConfig.
// It performs no network I/O — it builds the client handle; model loading (which
// may touch the network) happens later in Resolve. OpenRouter reuses
// newOpenRouterProvider so the OPENROUTER_BASE_URL E2E seam keeps working.
func buildProvider(cfg ProviderConfig, headers ProviderHeaders) (fantasy.Provider, error) {
	switch cfg.Type {
	case ProviderTypeOpenRouter:
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("openrouter provider requires an API key")
		}
		return newOpenRouterProvider(cfg.APIKey, headers)
	case ProviderTypeAnthropic:
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("anthropic provider requires an API key")
		}
		opts := []anthropic.Option{anthropic.WithAPIKey(cfg.APIKey)}
		if b := strings.TrimSpace(cfg.BaseURL); b != "" {
			opts = append(opts, anthropic.WithBaseURL(b))
		}
		return anthropic.New(opts...)
	case ProviderTypeOpenAI:
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, fmt.Errorf("openai provider requires an API key")
		}
		opts := []openai.Option{openai.WithAPIKey(cfg.APIKey)}
		if b := strings.TrimSpace(cfg.BaseURL); b != "" {
			opts = append(opts, openai.WithBaseURL(b))
		}
		return openai.New(opts...)
	case ProviderTypeOllama:
		// Ollama speaks the OpenAI wire format; reach it via the OpenAI provider
		// pointed at the Ollama endpoint. Local Ollama needs no API key, but the
		// OpenAI SDK insists on a non-empty one, so a placeholder is used when none
		// is configured (Ollama ignores it).
		base := strings.TrimSpace(cfg.BaseURL)
		if base == "" {
			base = defaultOllamaBaseURL
		}
		key := strings.TrimSpace(cfg.APIKey)
		if key == "" {
			key = "ollama"
		}
		return openai.New(openai.WithAPIKey(key), openai.WithBaseURL(base))
	default:
		return nil, fmt.Errorf("unknown provider type %q (want openrouter|anthropic|openai|ollama)", cfg.Type)
	}
}
