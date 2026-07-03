package agentcore

import "testing"

// MergeLLMProviders composes bundle + admin provider tables. Load-bearing:
// the implicit env-OpenRouter catch-all appears exactly when the bundle is
// empty and a key is set, admin rows replace same-name base rows in place,
// and otherwise append (so a base catch-all keeps serving unlisted slugs).
func TestMergeLLMProviders(t *testing.T) {
	base := []ProviderConfig{
		{Name: "openrouter", Type: ProviderTypeOpenRouter, APIKey: "bundle-key"},
		{Name: "local", Type: ProviderTypeOllama, Models: []string{"llama3"}},
	}

	t.Run("no sources → empty", func(t *testing.T) {
		if got := MergeLLMProviders(nil, nil, ""); len(got) != 0 {
			t.Fatalf("got %d providers, want 0", len(got))
		}
	})

	t.Run("env key alone → implicit catch-all", func(t *testing.T) {
		got := MergeLLMProviders(nil, nil, "sk-or-x")
		if len(got) != 1 || got[0].Name != "openrouter" || got[0].APIKey != "sk-or-x" || len(got[0].Models) != 0 {
			t.Fatalf("got %+v, want single implicit openrouter catch-all", got)
		}
	})

	t.Run("admin row replaces same-name base row in place", func(t *testing.T) {
		got := MergeLLMProviders(base, []ProviderConfig{
			{Name: "openrouter", Type: ProviderTypeOpenRouter, APIKey: "rotated"},
		}, "")
		if len(got) != 2 || got[0].APIKey != "rotated" || got[1].Name != "local" {
			t.Fatalf("got %+v, want rotated key at position 0", got)
		}
	})

	t.Run("new admin rows append after the base", func(t *testing.T) {
		got := MergeLLMProviders(base, []ProviderConfig{
			{Name: "anthropic-direct", Type: ProviderTypeAnthropic, APIKey: "k", Models: []string{"claude-x"}},
		}, "")
		if len(got) != 3 || got[2].Name != "anthropic-direct" {
			t.Fatalf("got %+v, want admin row appended", got)
		}
	})

	t.Run("admin overlays the implicit catch-all too", func(t *testing.T) {
		got := MergeLLMProviders(nil, []ProviderConfig{
			{Name: "openrouter", Type: ProviderTypeOpenRouter, APIKey: "admin-key"},
		}, "env-key")
		if len(got) != 1 || got[0].APIKey != "admin-key" {
			t.Fatalf("got %+v, want admin key to win", got)
		}
	})
}
