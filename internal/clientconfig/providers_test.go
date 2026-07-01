package clientconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateProviders(t *testing.T) {
	valid := ProviderDef{Name: "anthropic-direct", Type: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"}
	cases := []struct {
		name    string
		provs   []ProviderDef
		wantErr string // substring; "" = expect success
	}{
		{"empty is ok (single-openrouter default)", nil, ""},
		{"valid anthropic", []ProviderDef{valid}, ""},
		{"valid openrouter catch-all", []ProviderDef{{Name: "or", Type: "openrouter", APIKeyEnv: "OPENROUTER_API_KEY"}}, ""},
		{"ollama needs no key", []ProviderDef{{Name: "local", Type: "ollama"}}, ""},
		{"blank name", []ProviderDef{{Type: "anthropic", APIKeyEnv: "K"}}, "name is required"},
		{"duplicate name", []ProviderDef{valid, valid}, "duplicate provider name"},
		{"missing type", []ProviderDef{{Name: "x", APIKeyEnv: "K"}}, "type is required"},
		{"unknown type", []ProviderDef{{Name: "x", Type: "bogus", APIKeyEnv: "K"}}, "unknown type"},
		{"missing api_key_env for non-ollama", []ProviderDef{{Name: "x", Type: "anthropic"}}, "api_key_env is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{Providers: tc.provs}
			err := b.validateProviders()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestEnvVarNamesIncludesProviderKeys proves a provider's api_key_env survives the
// .env-file allowlist (registered via EnvVarNames), the same as an MCP credential.
func TestEnvVarNamesIncludesProviderKeys(t *testing.T) {
	b := &Bundle{Providers: []ProviderDef{
		{Name: "a", Type: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
		{Name: "o", Type: "openai", APIKeyEnv: "OPENAI_API_KEY"},
		{Name: "local", Type: "ollama"}, // no key env — nothing to add
	}}
	names := b.EnvVarNames()
	for _, want := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvVarNames = %v, want it to include %q", names, want)
		}
	}
}
