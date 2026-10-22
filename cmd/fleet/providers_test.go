package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/store"
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
			{Name: "local", Type: "ollama", BaseURL: " http://localhost:11434/v1 ", ContextWindowTokens: 32_768}, // no key env
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
		if got[1].ContextWindowTokens != 32_768 {
			t.Errorf("provider[1] ContextWindowTokens = %d, want 32768", got[1].ContextWindowTokens)
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

// TestLoadDBLLMProvidersCarriesTheContextWindow: an admin row's declared
// context window (migration 082) reaches the resolver's routing table, which
// both the boot merge and the hot reload build through loadDBLLMProviders.
// Without it every non-OpenRouter admin row ran on the resolver's fallback,
// below a turn's fixed reserve.
func TestLoadDBLLMProvidersCarriesTheContextWindow(t *testing.T) {
	chatDsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if chatDsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is required; skipping Postgres-backed test")
	}
	chat, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chat.Close() })
	ctx := context.Background()
	// loadDBLLMProviders decrypts every enabled row, and this store has no
	// cipher: a keyed row left by another package's test would fail the read.
	if err := chat.TruncateAllForTest(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	name := "ctxwin-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	row, err := chat.CreateLLMProvider(ctx, store.LLMProviderInput{
		Name: name, Type: "ollama", Models: []string{"qwen3-coder"}, Enabled: true, ContextWindowTokens: 65536,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = chat.DeleteLLMProvider(context.Background(), row.ID) })
	// A row saved before migration 082 (stored directly: the API would refuse
	// it enabled) still loads, on the fallback, with a warning naming it.
	legacy, err := chat.CreateLLMProvider(ctx, store.LLMProviderInput{
		Name: name + "-legacy", Type: "ollama", Models: []string{"llama3.2"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	t.Cleanup(func() { _ = chat.DeleteLLMProvider(context.Background(), legacy.ID) })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	got, err := loadDBLLMProviders(ctx, chat)
	if err != nil {
		t.Fatalf("loadDBLLMProviders: %v", err)
	}
	windows := map[string]int{}
	for _, p := range got {
		windows[p.Name] = p.ContextWindowTokens
	}
	if w, ok := windows[name]; !ok || w != 65536 {
		t.Fatalf("declared row window = %d (present %v), want 65536", w, ok)
	}
	if w, ok := windows[name+"-legacy"]; !ok || w != 0 {
		t.Fatalf("legacy row window = %d (present %v), want it loaded with 0", w, ok)
	}
	out := buf.String()
	if !strings.Contains(out, `admin provider "`+name+`-legacy" (type ollama) declares no context window`) ||
		strings.Contains(out, `"`+name+`" (type`) {
		t.Fatalf("log = %q, want one warning, for the legacy row only", out)
	}
}

// TestToAgentcoreProvidersWarnsOnAnUndeclaredWindow: a bundle entry with no
// context_window_tokens still loads (failing boot would break existing
// manifests) but is named in a warning; OpenRouter and declared entries are
// not.
func TestToAgentcoreProvidersWarnsOnAnUndeclaredWindow(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	got := toAgentcoreProviders(&clientconfig.Bundle{Providers: []clientconfig.ProviderDef{
		{Name: "openrouter", Type: "openrouter", APIKeyEnv: "OPENROUTER_API_KEY"},
		{Name: "anthropic-direct", Type: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
		{Name: "local", Type: "ollama", ContextWindowTokens: 65536},
	}})
	if len(got) != 3 {
		t.Fatalf("got %d providers, want all 3 loaded", len(got))
	}
	out := buf.String()
	if !strings.Contains(out, `bundle provider "anthropic-direct" (type anthropic) declares no context window`) ||
		strings.Contains(out, `"openrouter"`) || strings.Contains(out, `"local"`) {
		t.Fatalf("log = %q, want one warning, for anthropic-direct only", out)
	}
}
