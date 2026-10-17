// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openrouter"
)

// generatePathMetadata is the shape fantasy's non-streaming Generate returns
// for an OpenRouter model: the OpenRouter adapter wraps the OpenAI provider,
// whose Response is keyed by openai.Name, so the typed OpenRouter metadata
// sits under "openai", not "openrouter".
func generatePathMetadata(cost float64, provider string) fantasy.ProviderMetadata {
	return fantasy.ProviderMetadata{
		openai.Name: &openrouter.ProviderMetadata{
			Provider: provider,
			Usage:    openrouter.UsageAccounting{Cost: cost},
		},
	}
}

func TestOpenRouterCost_GeneratePathKeyedUnderOpenAI(t *testing.T) {
	got := openrouterCost(generatePathMetadata(0.12, ""))
	if got == nil || *got != 0.12 {
		t.Fatalf("openrouterCost on Generate-path metadata = %v, want 0.12", got)
	}
	if got := OpenRouterCost(generatePathMetadata(0.12, "")); got == nil || *got != 0.12 {
		t.Fatalf("OpenRouterCost on Generate-path metadata = %v, want 0.12", got)
	}
}

func TestOpenRouterCost_StreamingPathAndAbsent(t *testing.T) {
	if got := openrouterCost(openRouterMetadataWithCost(0.5)); got == nil || *got != 0.5 {
		t.Fatalf("openrouterCost on streaming-path metadata = %v, want 0.5", got)
	}
	if got := openrouterCost(nil); got != nil {
		t.Fatalf("openrouterCost(nil) = %v, want nil", *got)
	}
	if got := openrouterCost(fantasy.ProviderMetadata{openai.Name: &openai.ProviderMetadata{}}); got != nil {
		t.Fatalf("openrouterCost on plain OpenAI metadata = %v, want nil", *got)
	}
}

func TestOpenRouterServedProvider_GeneratePath(t *testing.T) {
	if got := openrouterServedProvider(generatePathMetadata(0, `"Google"`)); got != "Google" {
		t.Fatalf("served provider on Generate-path metadata = %q, want Google", got)
	}
}

// TestNewAuxUsageRecord_PricesGenerateResult pins the end-to-end symptom: an
// aux Generate call (verifier, error analysis) recorded cost_usd 0 because
// its metadata is keyed under "openai".
func TestNewAuxUsageRecord_PricesGenerateResult(t *testing.T) {
	ConfigurePricing(PricingConfig{})
	t.Cleanup(func() { ConfigurePricing(PricingConfig{}) })
	res := &fantasy.AgentResult{
		Response:   fantasy.Response{ProviderMetadata: generatePathMetadata(0.12, "")},
		TotalUsage: fantasy.Usage{InputTokens: 100, OutputTokens: 10},
	}
	rec := NewAuxUsageRecord(AuxUsageEndOfRunVerifier, "anthropic/claude-opus-5.5", res)
	if rec.CostUSD != 0.12 {
		t.Fatalf("aux record cost = %v, want 0.12", rec.CostUSD)
	}
}
