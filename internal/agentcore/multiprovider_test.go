package agentcore

import (
	"testing"
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
