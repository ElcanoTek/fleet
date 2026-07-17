package agentcore

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestSelectProvider(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "anthropic-direct", Type: ProviderTypeAnthropic, Models: []string{"claude-opus-4-8", "claude-sonnet-4-6"}},
		{Name: "openai-direct", Type: ProviderTypeOpenAI, Models: []string{"gpt-4o"}},
		{Name: "openrouter", Type: ProviderTypeOpenRouter}, // catch-all (no Models)
	}

	cases := []struct {
		name         string
		slug         string
		wantProvider string
		wantModel    string
		wantErr      bool
	}{
		{"explicit name prefix", "anthropic-direct/claude-opus-4-8", "anthropic-direct", "claude-opus-4-8", false},
		{"explicit prefix to openai", "openai-direct/gpt-4o", "openai-direct", "gpt-4o", false},
		{"implicit models-list match", "claude-sonnet-4-6", "anthropic-direct", "claude-sonnet-4-6", false},
		{"implicit match openai", "gpt-4o", "openai-direct", "gpt-4o", false},
		{"unknown slug falls to catch-all", "anthropic/claude-opus-4.8", "openrouter", "anthropic/claude-opus-4.8", false},
		{"bare unknown to catch-all", "some-new-model", "openrouter", "some-new-model", false},
		// A "/"-containing slug whose prefix is NOT a provider name is an OpenRouter
		// slug, not explicit routing — the whole slug goes to the catch-all.
		{"openrouter-style slug not treated as routing", "meta-llama/llama-3", "openrouter", "meta-llama/llama-3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, model, err := selectProvider(providers, tc.slug)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got provider %q", tc.slug, pc.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pc.Name != tc.wantProvider {
				t.Errorf("provider = %q, want %q", pc.Name, tc.wantProvider)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
		})
	}
}

// TestSelectProviderSpecificBeatsEarlierCatchAll proves a specifically-listed
// model routes to its provider even when a catch-all appears earlier in the list.
func TestSelectProviderSpecificBeatsEarlierCatchAll(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "openrouter", Type: ProviderTypeOpenRouter}, // catch-all FIRST
		{Name: "anthropic-direct", Type: ProviderTypeAnthropic, Models: []string{"claude-opus-4-8"}},
	}
	pc, model, err := selectProvider(providers, "claude-opus-4-8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.Name != "anthropic-direct" || model != "claude-opus-4-8" {
		t.Errorf("got provider %q model %q, want anthropic-direct/claude-opus-4-8 (specific match must beat earlier catch-all)", pc.Name, model)
	}
}

// TestSelectProviderNoCatchAll errors when no provider serves the slug and none
// is a catch-all.
func TestSelectProviderNoCatchAll(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "anthropic-direct", Type: ProviderTypeAnthropic, Models: []string{"claude-opus-4-8"}},
	}
	if _, _, err := selectProvider(providers, "gpt-4o"); err == nil {
		t.Fatal("expected error for a slug no provider serves and no catch-all")
	}
}

func TestBuildProvider(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ProviderConfig
		wantErr bool
	}{
		{"openrouter with key", ProviderConfig{Name: "or", Type: ProviderTypeOpenRouter, APIKey: "k"}, false},
		{"openrouter without key errors", ProviderConfig{Name: "or", Type: ProviderTypeOpenRouter}, true},
		{"anthropic with key", ProviderConfig{Name: "a", Type: ProviderTypeAnthropic, APIKey: "k"}, false},
		{"anthropic without key errors", ProviderConfig{Name: "a", Type: ProviderTypeAnthropic}, true},
		{"openai with key", ProviderConfig{Name: "o", Type: ProviderTypeOpenAI, APIKey: "k"}, false},
		{"openai without key errors", ProviderConfig{Name: "o", Type: ProviderTypeOpenAI}, true},
		{"ollama needs no key", ProviderConfig{Name: "l", Type: ProviderTypeOllama}, false},
		{"ollama with base url", ProviderConfig{Name: "l", Type: ProviderTypeOllama, BaseURL: "http://host:11434/v1"}, false},
		{"unknown type errors", ProviderConfig{Name: "x", Type: ProviderType("bogus")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildProvider(tc.cfg, DefaultProviderHeaders)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got provider %v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p == nil {
				t.Fatal("provider is nil")
			}
		})
	}
}

// TestNewModelResolverBackwardCompat proves the single-arg constructor still
// yields a one-provider (OpenRouter catch-all) resolver, byte-compatible with the
// historical behavior.
func TestNewModelResolverBackwardCompat(t *testing.T) {
	if _, err := NewModelResolver("", DefaultProviderHeaders); err == nil {
		t.Error("empty OpenRouter key must error (backward-compat)")
	}
	r, err := NewModelResolver("test-key", DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("NewModelResolver: %v", err)
	}
	if len(r.providers) != 1 || r.providers[0].Type != ProviderTypeOpenRouter || len(r.providers[0].Models) != 0 {
		t.Errorf("backward-compat resolver providers = %+v, want one catch-all openrouter", r.providers)
	}
	if _, ok := r.built["openrouter"]; !ok {
		t.Error("openrouter provider not built")
	}
	// Any slug routes to the single catch-all.
	pc, model, err := selectProvider(r.providers, "anthropic/claude-opus-4.8")
	if err != nil || pc.Type != ProviderTypeOpenRouter || model != "anthropic/claude-opus-4.8" {
		t.Errorf("catch-all routing = (%+v, %q, %v), want openrouter/full-slug", pc, model, err)
	}
}

func TestNewModelResolverWithProviders(t *testing.T) {
	t.Run("empty list errors", func(t *testing.T) {
		if _, err := NewModelResolverWithProviders(nil, DefaultProviderHeaders); err == nil {
			t.Error("empty provider list must error")
		}
	})
	t.Run("duplicate name errors", func(t *testing.T) {
		_, err := NewModelResolverWithProviders([]ProviderConfig{
			{Name: "dup", Type: ProviderTypeOpenRouter, APIKey: "k"},
			{Name: "dup", Type: ProviderTypeAnthropic, APIKey: "k"},
		}, DefaultProviderHeaders)
		if err == nil {
			t.Error("duplicate provider name must error")
		}
	})
	t.Run("missing name errors", func(t *testing.T) {
		_, err := NewModelResolverWithProviders([]ProviderConfig{
			{Type: ProviderTypeOpenRouter, APIKey: "k"},
		}, DefaultProviderHeaders)
		if err == nil {
			t.Error("missing provider name must error")
		}
	})
	t.Run("a bad provider fails construction at boot", func(t *testing.T) {
		_, err := NewModelResolverWithProviders([]ProviderConfig{
			{Name: "openrouter", Type: ProviderTypeOpenRouter, APIKey: "k"},
			{Name: "anthropic-direct", Type: ProviderTypeAnthropic}, // no key
		}, DefaultProviderHeaders)
		if err == nil {
			t.Error("a provider that fails to build must error at construction, not at first turn")
		}
	})
	t.Run("negative context window fails construction at boot", func(t *testing.T) {
		_, err := NewModelResolverWithProviders([]ProviderConfig{
			{Name: "local", Type: ProviderTypeOllama, ContextWindowTokens: -1},
		}, DefaultProviderHeaders)
		if err == nil {
			t.Error("a negative context window must fail resolver construction")
		}
	})
	t.Run("valid multi-provider builds all", func(t *testing.T) {
		r, err := NewModelResolverWithProviders([]ProviderConfig{
			{Name: "openrouter", Type: ProviderTypeOpenRouter, APIKey: "k"},
			{Name: "anthropic-direct", Type: ProviderTypeAnthropic, APIKey: "k", Models: []string{"claude-opus-4-8"}},
			{Name: "local", Type: ProviderTypeOllama},
		}, DefaultProviderHeaders)
		if err != nil {
			t.Fatalf("construction: %v", err)
		}
		for _, name := range []string{"openrouter", "anthropic-direct", "local"} {
			if _, ok := r.built[name]; !ok {
				t.Errorf("provider %q not built", name)
			}
		}
	})
}

func TestResolvedProviderContextWindow(t *testing.T) {
	if got := resolvedProviderContextWindow(ProviderConfig{Type: ProviderTypeOllama}); got != 4096 {
		t.Fatalf("unknown Ollama context window = %d, want conservative 4096", got)
	}
	if got := resolvedProviderContextWindow(ProviderConfig{Type: ProviderTypeOpenAI}); got != 32_000 {
		t.Fatalf("unknown native OpenAI context window = %d, want conservative 32000", got)
	}
	if got := resolvedProviderContextWindow(ProviderConfig{Type: ProviderTypeOllama, ContextWindowTokens: 32_768}); got != 32_768 {
		t.Fatalf("declared Ollama context window = %d, want 32768", got)
	}
	if got := resolvedProviderContextWindow(ProviderConfig{Type: ProviderTypeOpenRouter}); got != 0 {
		t.Fatalf("OpenRouter override = %d, want catalog resolution", got)
	}
	if got := resolvedProviderContextWindow(ProviderConfig{Type: ProviderTypeOpenRouter, ContextWindowTokens: 1_000_000}); got != 0 {
		t.Fatalf("inflated OpenRouter declaration = %d, want authoritative catalog resolution", got)
	}
}

func TestContextWindowForActiveModel_OpenRouterCatalogOverridesDeclaration(t *testing.T) {
	const slug = "review-fixture/catalog-authoritative-128k"
	t.Setenv("FLEET_DISABLE_OPENROUTER_MODELS", "0")
	base := &namedMockModel{name: slug}
	openRouter := &providerNamedModel{
		LanguageModel:       base,
		providerName:        "openrouter",
		providerType:        ProviderTypeOpenRouter,
		contextWindowTokens: 1_000_000, // deliberately inflated wrapper declaration
	}
	native := &providerNamedModel{
		LanguageModel:       base,
		providerName:        "local",
		providerType:        ProviderTypeOllama,
		contextWindowTokens: 32_768,
	}

	// Seed the already-fetched public catalog directly so this regression is
	// deterministic and never performs a network request.
	sharedModelsCache.mu.Lock()
	oldMap := sharedModelsCache.contextMap
	oldFetchedAt := sharedModelsCache.fetchedAt
	sharedModelsCache.contextMap = map[string]int{slug: 128 * 1024}
	sharedModelsCache.fetchedAt = time.Now()
	sharedModelsCache.mu.Unlock()
	type priorObservation struct {
		value int
		had   bool
	}
	keys := []observedContextKey{
		observedContextKeyForModel(openRouter),
		observedContextKeyForModel(native),
		{slug: slug}, // legacy unwrapped-model observation
	}
	prior := make(map[observedContextKey]priorObservation, len(keys))
	observedContextWindows.mu.Lock()
	for _, key := range keys {
		value, had := observedContextWindows.m[key]
		prior[key] = priorObservation{value: value, had: had}
		delete(observedContextWindows.m, key)
	}
	observedContextWindows.mu.Unlock()
	t.Cleanup(func() {
		sharedModelsCache.mu.Lock()
		sharedModelsCache.contextMap = oldMap
		sharedModelsCache.fetchedAt = oldFetchedAt
		sharedModelsCache.mu.Unlock()
		observedContextWindows.mu.Lock()
		for _, key := range keys {
			if old := prior[key]; old.had {
				observedContextWindows.m[key] = old.value
			} else {
				delete(observedContextWindows.m, key)
			}
		}
		observedContextWindows.mu.Unlock()
	})

	// Even the legacy unwrapped-model cache is isolated from a provider-wrapped
	// model. This exercises OpenRouter's static/live fallback path without
	// reintroducing a slug-only observation after the scoped lookup misses.
	recordContextMax(slug, 2_000_000)
	if got := contextWindowForActiveModel(openRouter); got != 128*1024 {
		t.Fatalf("OpenRouter active window = %d, want authoritative catalog 131072", got)
	}

	// Provider-local handles do not expose authoritative OpenRouter metadata;
	// their manifest declaration remains the active limit for the same slug.
	if got := contextWindowForActiveModel(native); got != 32_768 {
		t.Fatalf("native active window = %d, want declared 32768", got)
	}

	// Provider-reported ground truth is scoped to the exact provider+slug pair.
	// A large native observation must not inflate OpenRouter's 128K catalog
	// window, and a later OpenRouter observation must not shrink the native one.
	recordContextFromError(native, &fantasy.ProviderError{ContextMaxTokens: 1_000_000})
	if got := contextWindowForActiveModel(openRouter); got != 128*1024 {
		t.Fatalf("native same-slug observation contaminated OpenRouter: got %d, want 131072", got)
	}
	if got := contextWindowForActiveModel(native); got != 1_000_000 {
		t.Fatalf("native observed window = %d, want 1000000", got)
	}
	recordContextFromError(openRouter, &fantasy.ProviderError{ContextMaxTokens: 64 * 1024})
	if got := contextWindowForActiveModel(openRouter); got != 64*1024 {
		t.Fatalf("OpenRouter observed window = %d, want 65536", got)
	}
	if got := contextWindowForActiveModel(native); got != 1_000_000 {
		t.Fatalf("OpenRouter same-slug observation contaminated native: got %d, want 1000000", got)
	}
}

func TestResolveWithFallbackChainAndExplicitPin(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "primary", Type: ProviderTypeOpenAI, APIKey: "p", Models: []string{"gpt-test"}, FallbackProviders: []string{"primary", "secondary", "tertiary"}},
		{Name: "secondary", Type: ProviderTypeOpenAI, APIKey: "s"},
		{Name: "tertiary", Type: ProviderTypeOpenAI, APIKey: "t"},
	}
	r, err := NewModelResolverWithProviders(providers, DefaultProviderHeaders)
	if err != nil {
		t.Fatal(err)
	}
	primary, fallback, err := r.ResolveWithFallback(context.Background(), "gpt-test")
	if err != nil || primary == nil || fallback == nil {
		t.Fatalf("primary=%v fallback=%v err=%v", primary, fallback, err)
	}
	_, fallbacks, err := r.ResolveWithFallbacks(context.Background(), "gpt-test")
	if err != nil || len(fallbacks) != 2 {
		t.Fatalf("fallback chain len=%d err=%v", len(fallbacks), err)
	}
	_, pinnedFallback, err := r.ResolveWithFallback(context.Background(), "primary/gpt-test")
	if err != nil || pinnedFallback != nil {
		t.Fatalf("explicit pin fallback=%v err=%v", pinnedFallback, err)
	}
}

func TestResolverRejectsUnknownFallbackProvider(t *testing.T) {
	_, err := NewModelResolverWithProviders([]ProviderConfig{{
		Name: "primary", Type: ProviderTypeOpenAI, APIKey: "p", FallbackProviders: []string{"primary", "missing"},
	}}, DefaultProviderHeaders)
	if err == nil {
		t.Fatal("expected unknown fallback provider error")
	}
}
