package main

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// TestToAgentcoreProviders covers the #289 bundle→resolver translation: nil/empty
// bundles preserve the single-OpenRouter default (nil), and a populated block is
// translated with the API key resolved HOST-SIDE from the env var the manifest
// names.
func TestToAgentcoreProviders(t *testing.T) {
	t.Run("nil bundle → nil", func(t *testing.T) {
		if got := toAgentcoreProviders(nil); got != nil {
			t.Errorf("nil bundle => %v, want nil", got)
		}
	})
	t.Run("empty providers → nil (default preserved)", func(t *testing.T) {
		if got := toAgentcoreProviders(&clientconfig.Bundle{}); got != nil {
			t.Errorf("empty bundle => %v, want nil", got)
		}
	})
	t.Run("populated block translated with host-side key resolution", func(t *testing.T) {
		t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-secret")
		bundle := &clientconfig.Bundle{Providers: []clientconfig.ProviderDef{
			{Name: "  anthropic-direct  ", Type: " anthropic ", APIKeyEnv: "TEST_ANTHROPIC_KEY", Models: []string{"claude-opus-4-8"}},
			{Name: "local", Type: "ollama", BaseURL: " http://localhost:11434/v1 "}, // no key env
		}}
		got := toAgentcoreProviders(bundle)
		if len(got) != 2 {
			t.Fatalf("got %d providers, want 2", len(got))
		}
		// Fields are trimmed; the key is resolved from the env var, not the name.
		if got[0].Name != "anthropic-direct" || got[0].Type != agentcore.ProviderTypeAnthropic {
			t.Errorf("provider[0] name/type = %q/%q, want anthropic-direct/anthropic", got[0].Name, got[0].Type)
		}
		if got[0].APIKey != "sk-ant-secret" {
			t.Errorf("provider[0] APIKey = %q, want the resolved env value", got[0].APIKey)
		}
		if len(got[0].Models) != 1 || got[0].Models[0] != "claude-opus-4-8" {
			t.Errorf("provider[0] Models = %v, want [claude-opus-4-8]", got[0].Models)
		}
		// Ollama entry has no api_key_env → empty APIKey, base URL trimmed.
		if got[1].APIKey != "" {
			t.Errorf("provider[1] APIKey = %q, want empty (no api_key_env)", got[1].APIKey)
		}
		if got[1].BaseURL != "http://localhost:11434/v1" {
			t.Errorf("provider[1] BaseURL = %q, want trimmed", got[1].BaseURL)
		}
	})
	t.Run("unset api_key_env yields empty key", func(t *testing.T) {
		bundle := &clientconfig.Bundle{Providers: []clientconfig.ProviderDef{
			{Name: "x", Type: "anthropic", APIKeyEnv: "DEFINITELY_UNSET_KEY_XYZ"},
		}}
		got := toAgentcoreProviders(bundle)
		if len(got) != 1 || got[0].APIKey != "" {
			t.Errorf("unset env => APIKey %q, want empty", got[0].APIKey)
		}
	})
}
