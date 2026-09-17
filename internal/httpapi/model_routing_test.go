package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

type routingTestEngine struct {
	turnEngine
	*agentcore.ModelResolver
}

func TestModelDiscoveryUsesActiveResolver(t *testing.T) {
	base := []agentcore.ProviderConfig{{
		Name: "bundle-openai", Type: agentcore.ProviderTypeOpenAI,
		APIKey: "test-key-not-for-disclosure", BaseURL: "https://private-endpoint.invalid/v1",
		Models: []string{"gpt-4o"},
	}}
	// No store at all: discovery must come from the active merged resolver,
	// including bundle providers that never had an admin DB row.
	resolver, err := agentcore.NewModelResolverWithProviders(base, agentcore.DefaultProviderHeaders)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{agent: &routingTestEngine{ModelResolver: resolver}}
	w := httptest.NewRecorder()
	s.handleLLMProviderModels(w, httptest.NewRequest("GET", "/llm-provider-models", nil))
	var got struct {
		RoutingKnown bool                          `json:"routing_known"`
		Providers    []agentcore.ModelProviderInfo `json:"providers"`
		Models       []struct{ ID string }         `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.RoutingKnown || len(got.Providers) != 1 || len(got.Models) != 1 || got.Models[0].ID != "bundle-openai/gpt-4o" {
		t.Fatalf("discovery = %s", w.Body.String())
	}
	for _, secret := range []string{"test-key-not-for-disclosure", "private-endpoint", "api_key", "base_url"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("discovery disclosed %q", secret)
		}
	}
	// Mutating a caller's snapshot must not change execution's model list.
	gotProviders := resolver.ModelProviders()
	gotProviders[0].Models[0] = "google/gemini-3.8-flash"
	if _, err := resolver.CheckModelRoute("gpt-4o"); err != nil {
		t.Fatalf("snapshot mutated resolver: %v", err)
	}

	for _, tc := range []struct {
		slug    string
		allowed bool
	}{
		{"google/gemini-3.8-flash", false},
		{"openai/gpt-4o", false},
		{"bundle-openai/gpt-4o", true},
		{"gpt-4o", true},
		{"", false},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleLLMProviderModels(w, httptest.NewRequest("GET", "/llm-provider-models?slug="+tc.slug, nil))
			var result struct {
				Allowed      bool   `json:"allowed"`
				ProviderType string `json:"provider_type"`
				Message      string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Allowed != tc.allowed || (!tc.allowed && result.Message == "") || (tc.allowed && result.ProviderType != "openai") {
				t.Fatalf("route = %s", w.Body.String())
			}
		})
	}
}
