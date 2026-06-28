// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"math"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

func ptr(f float64) *float64 { return &f }

// openRouterMetadataWithCost builds provider metadata carrying an
// OpenRouter-reported cost, the shape openrouterCost reads.
func openRouterMetadataWithCost(cost float64) fantasy.ProviderMetadata {
	return fantasy.ProviderMetadata{
		openrouter.Name: &openrouter.ProviderMetadata{
			Usage: openrouter.UsageAccounting{Cost: cost},
		},
	}
}

// approxEqual compares two costs with a tolerance generous enough to absorb
// float rounding but tight enough to catch a wrong rate or token-class mistake.
func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestLookupModelPrice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		slug    string
		wantOK  bool
		wantIn  float64
		wantOut float64
	}{
		{name: "known exact", slug: "anthropic/claude-sonnet-4-5", wantOK: true, wantIn: 3.00, wantOut: 15.00},
		{name: "known case-insensitive + whitespace", slug: "  ANTHROPIC/Claude-Sonnet-4-5 ", wantOK: true, wantIn: 3.00, wantOut: 15.00},
		{name: "unknown slug", slug: "anthropic/claude-imaginary-9", wantOK: false},
		{name: "empty slug", slug: "   ", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			price, ok := LookupModelPrice(tc.slug)
			if ok != tc.wantOK {
				t.Fatalf("LookupModelPrice(%q) ok = %v, want %v", tc.slug, ok, tc.wantOK)
			}
			if !tc.wantOK {
				if price != (ModelPrice{}) {
					t.Fatalf("expected zero price on miss, got %+v", price)
				}
				return
			}
			if price.InputPerM != tc.wantIn || price.OutputPerM != tc.wantOut {
				t.Fatalf("price = %+v, want in=%v out=%v", price, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	t.Parallel()

	// 40-char system prompt -> 10 tokens; 20-char task -> 5 tokens; 2 tools -> 600.
	sys := strings.Repeat("a", 40)
	task := strings.Repeat("b", 20)
	systemToks, toolToks, promptToks := EstimateTokens(sys, task, 2)
	if systemToks != 10 {
		t.Errorf("systemToks = %d, want 10", systemToks)
	}
	if toolToks != 2*tokensPerMCPTool {
		t.Errorf("toolToks = %d, want %d", toolToks, 2*tokensPerMCPTool)
	}
	if want := 10 + 2*tokensPerMCPTool + 5; promptToks != want {
		t.Errorf("promptToks = %d, want %d", promptToks, want)
	}
}

func TestEstimateTokensNoTools(t *testing.T) {
	t.Parallel()
	systemToks, toolToks, promptToks := EstimateTokens("", "", 0)
	if systemToks != 0 || toolToks != 0 || promptToks != 0 {
		t.Fatalf("empty estimate = (%d,%d,%d), want all zero", systemToks, toolToks, promptToks)
	}
	// Negative tool counts must not subtract tokens.
	_, toolToks, _ = EstimateTokens("", "", -5)
	if toolToks != 0 {
		t.Fatalf("negative tool count produced %d tool tokens, want 0", toolToks)
	}
}

func TestForecastCostKnownModel(t *testing.T) {
	t.Parallel()

	// sonnet-4-5: $3/M in, $15/M out. prompt=1,000,000 tokens, output=DefaultForecastOutputTokens.
	const promptToks = 1_000_000
	fc := ForecastCost("anthropic/claude-sonnet-4-5", 800_000, 200_000, promptToks, 10, 0)

	if !fc.PricingKnown {
		t.Fatal("expected PricingKnown=true for a known model")
	}
	if fc.PerIterationCostUSD == nil || fc.EstimatedTotalCostUSD == nil || fc.EstimatedTotalRange == nil {
		t.Fatal("expected non-nil cost fields for a known model")
	}

	// per-iter = (1e6/1e6)*3 + (800/1e6)*15 = 3 + 0.012 = 3.012
	wantPerIter := 3.00 + float64(DefaultForecastOutputTokens)/1e6*15.00
	if !approxEqual(*fc.PerIterationCostUSD, wantPerIter) {
		t.Errorf("per-iter cost = %v, want %v", *fc.PerIterationCostUSD, wantPerIter)
	}
	wantTotal := wantPerIter * 10
	if !approxEqual(*fc.EstimatedTotalCostUSD, wantTotal) {
		t.Errorf("total cost = %v, want %v", *fc.EstimatedTotalCostUSD, wantTotal)
	}
	if !approxEqual(fc.EstimatedTotalRange.MinUSD, wantTotal*forecastRangeMinMultiplier) {
		t.Errorf("range min = %v, want %v", fc.EstimatedTotalRange.MinUSD, wantTotal*forecastRangeMinMultiplier)
	}
	if !approxEqual(fc.EstimatedTotalRange.MaxUSD, wantTotal*forecastRangeMaxMultiplier) {
		t.Errorf("range max = %v, want %v", fc.EstimatedTotalRange.MaxUSD, wantTotal*forecastRangeMaxMultiplier)
	}
	if fc.AvgOutputTokens != DefaultForecastOutputTokens {
		t.Errorf("AvgOutputTokens = %d, want %d", fc.AvgOutputTokens, DefaultForecastOutputTokens)
	}
}

func TestForecastCostUnknownModelFallback(t *testing.T) {
	t.Parallel()

	fc := ForecastCost("anthropic/claude-imaginary-9", 100, 50, 200, 5, 1.0)
	if fc.PricingKnown {
		t.Fatal("expected PricingKnown=false for an unknown model")
	}
	if fc.PerIterationCostUSD != nil || fc.EstimatedTotalCostUSD != nil || fc.EstimatedTotalRange != nil || fc.Price != nil {
		t.Fatal("expected all cost fields nil for an unknown model")
	}
	// Token estimates must still be carried through.
	if fc.EstimatedPromptTokens != 200 || fc.SystemPromptTokens != 100 || fc.ToolDefinitionsTokens != 50 {
		t.Fatalf("token fields not preserved: %+v", fc)
	}
	if fc.WouldHitCeiling {
		t.Fatal("unknown pricing cannot determine a ceiling breach")
	}
	if !strings.Contains(fc.Note, "unknown") {
		t.Errorf("note should explain unknown pricing, got %q", fc.Note)
	}
}

func TestForecastCostWouldHitCeiling(t *testing.T) {
	t.Parallel()

	// per-iter ~= 3.012 (see above); 10 iters ~= 30.12.
	const promptToks = 1_000_000

	// Ceiling well below the median -> breach.
	fc := ForecastCost("anthropic/claude-sonnet-4-5", 0, 0, promptToks, 10, 1.0)
	if !fc.WouldHitCeiling {
		t.Errorf("expected WouldHitCeiling=true when median %.4f > ceiling 1.0", *fc.EstimatedTotalCostUSD)
	}

	// Ceiling above the median -> no breach.
	fc = ForecastCost("anthropic/claude-sonnet-4-5", 0, 0, promptToks, 10, 1000.0)
	if fc.WouldHitCeiling {
		t.Errorf("expected WouldHitCeiling=false when median %.4f <= ceiling 1000", *fc.EstimatedTotalCostUSD)
	}

	// Ceiling of 0 disables the comparison entirely.
	fc = ForecastCost("anthropic/claude-sonnet-4-5", 0, 0, promptToks, 10, 0)
	if fc.WouldHitCeiling {
		t.Error("expected WouldHitCeiling=false when ceiling is 0 (disabled)")
	}
}

func TestForecastCostClampsIterations(t *testing.T) {
	t.Parallel()

	fc := ForecastCost("anthropic/claude-sonnet-4-5", 0, 0, 1_000_000, 0, 0)
	if fc.MaxIterations != 1 {
		t.Fatalf("MaxIterations = %d, want 1 (clamped)", fc.MaxIterations)
	}
	// With 1 iteration, median == per-iteration.
	if !approxEqual(*fc.EstimatedTotalCostUSD, *fc.PerIterationCostUSD) {
		t.Fatalf("median %v should equal per-iter %v at 1 iteration", *fc.EstimatedTotalCostUSD, *fc.PerIterationCostUSD)
	}
}

// TestComputeCostFromUsage_OverrideMatch exercises the primary path: a configured
// override matches the model slug, so cost is computed LOCALLY from the token
// counts at the operator's rates — the OpenRouter-returned cost is ignored.
func TestComputeCostFromUsage_OverrideMatch(t *testing.T) {
	cfg := PricingConfig{
		Overrides: []PricingOverride{{
			Model:                          "anthropic/claude-opus-4-8",
			InputCostPerMillionTokens:      7.50,
			OutputCostPerMillionTokens:     22.50,
			CacheReadCostPerMillionTokens:  0.75,
			CacheWriteCostPerMillionTokens: 1.875,
		}},
		Fallback: PricingFallbackOpenRouter,
	}
	usage := fantasy.Usage{
		InputTokens:         1_000_000,
		OutputTokens:        2_000_000,
		CacheReadTokens:     500_000,
		CacheCreationTokens: 100_000,
	}
	// 1M*7.50 + 2M*22.50 + 0.5M*0.75 + 0.1M*1.875
	// = 7.50 + 45.00 + 0.375 + 0.1875 = 53.0625
	want := 53.0625
	// A wildly different OR cost must be IGNORED when an override matches.
	got := computeCostFromUsage("anthropic/claude-opus-4-8", usage, ptr(999.0), cfg)
	if !approxEqual(got, want) {
		t.Fatalf("override cost = %v, want %v", got, want)
	}
}

// TestComputeCostFromUsage_OverrideCaseInsensitive confirms the slug match is
// case-insensitive and trims surrounding whitespace on both sides.
func TestComputeCostFromUsage_OverrideCaseInsensitive(t *testing.T) {
	cfg := PricingConfig{Overrides: []PricingOverride{{
		Model:                     "  Anthropic/Claude-Opus-4-8  ",
		InputCostPerMillionTokens: 10,
	}}}
	usage := fantasy.Usage{InputTokens: 2_000_000}
	got := computeCostFromUsage("ANTHROPIC/claude-opus-4-8", usage, ptr(1.0), cfg)
	if !approxEqual(got, 20.0) {
		t.Fatalf("case-insensitive override cost = %v, want 20.0", got)
	}
}

// TestComputeCostFromUsage_NoMatchOpenRouterFallback is the default behavior: no
// override matches, fallback is "openrouter" (and the empty fallback resolves the
// same way), so the OpenRouter-returned cost is used verbatim.
func TestComputeCostFromUsage_NoMatchOpenRouterFallback(t *testing.T) {
	usage := fantasy.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	// Explicit openrouter fallback.
	cfg := PricingConfig{
		Overrides: []PricingOverride{{Model: "some/other-model", InputCostPerMillionTokens: 1}},
		Fallback:  PricingFallbackOpenRouter,
	}
	if got := computeCostFromUsage("openai/gpt-4o", usage, ptr(0.0185), cfg); !approxEqual(got, 0.0185) {
		t.Fatalf("openrouter-fallback cost = %v, want 0.0185 (the OR cost)", got)
	}

	// Empty fallback resolves to the openrouter default.
	if got := computeCostFromUsage("openai/gpt-4o", usage, ptr(0.0185), PricingConfig{}); !approxEqual(got, 0.0185) {
		t.Fatalf("empty-fallback cost = %v, want 0.0185 (the OR cost)", got)
	}

	// No OR cost available (nil) under the openrouter fallback → 0, no accrual,
	// exactly the pre-#297 behavior.
	if got := computeCostFromUsage("openai/gpt-4o", usage, nil, PricingConfig{}); got != 0 {
		t.Fatalf("nil OR cost under openrouter fallback = %v, want 0", got)
	}
}

// TestComputeCostFromUsage_NoMatchZeroFallback confirms the "zero" fallback
// suppresses cost for unlisted models even when the provider returned one.
func TestComputeCostFromUsage_NoMatchZeroFallback(t *testing.T) {
	cfg := PricingConfig{Fallback: PricingFallbackZero}
	usage := fantasy.Usage{InputTokens: 5_000_000, OutputTokens: 5_000_000}
	if got := computeCostFromUsage("openai/gpt-4o", usage, ptr(123.45), cfg); got != 0 {
		t.Fatalf("zero-fallback cost = %v, want 0 (unlisted model suppressed)", got)
	}
}

// TestComputeCostFromUsage_ZeroFallbackStillHonorsOverride confirms a listed
// model is still priced from its override even when the fallback is "zero".
func TestComputeCostFromUsage_ZeroFallbackStillHonorsOverride(t *testing.T) {
	cfg := PricingConfig{
		Fallback: PricingFallbackZero,
		Overrides: []PricingOverride{{
			Model:                      "private/model",
			InputCostPerMillionTokens:  2,
			OutputCostPerMillionTokens: 4,
		}},
	}
	usage := fantasy.Usage{InputTokens: 1_000_000, OutputTokens: 500_000}
	// 1M*2 + 0.5M*4 = 2 + 2 = 4
	if got := computeCostFromUsage("private/model", usage, nil, cfg); !approxEqual(got, 4.0) {
		t.Fatalf("override-under-zero-fallback cost = %v, want 4.0", got)
	}
}

// TestComputeCostFromUsage_EmptySlug falls through to the fallback (an empty slug
// can never match an override) — guards the finalize-hook path where the active
// model may be nil.
func TestComputeCostFromUsage_EmptySlug(t *testing.T) {
	cfg := PricingConfig{Overrides: []PricingOverride{{Model: "x", InputCostPerMillionTokens: 1}}}
	usage := fantasy.Usage{InputTokens: 1_000_000}
	if got := computeCostFromUsage("", usage, ptr(0.5), cfg); !approxEqual(got, 0.5) {
		t.Fatalf("empty-slug cost = %v, want 0.5 (fallback to OR cost)", got)
	}
}

// TestComputeCostFromUsage_FirstMatchWins confirms duplicate entries resolve to
// the first (manifest order), matching the documented behavior.
func TestComputeCostFromUsage_FirstMatchWins(t *testing.T) {
	cfg := PricingConfig{Overrides: []PricingOverride{
		{Model: "dup/model", InputCostPerMillionTokens: 1},
		{Model: "dup/model", InputCostPerMillionTokens: 99},
	}}
	usage := fantasy.Usage{InputTokens: 1_000_000}
	if got := computeCostFromUsage("dup/model", usage, nil, cfg); !approxEqual(got, 1.0) {
		t.Fatalf("first-match cost = %v, want 1.0", got)
	}
}

// TestUpdateUsage_UsesOverrideForCeiling is the end-to-end accounting check: with
// an override installed process-wide, updateUsage accrues cost at the OVERRIDE
// rate (not the OpenRouter figure), and the cost ceiling fires against that
// overridden spend. This is the behavior the issue's budget/ceiling criterion
// asks for.
func TestUpdateUsage_UsesOverrideForCeiling(t *testing.T) {
	// Install a high override rate, restore the default after the test so the
	// process-wide config doesn't leak into other tests in the package.
	t.Cleanup(func() { ConfigurePricing(PricingConfig{}) })
	ConfigurePricing(PricingConfig{
		Overrides: []PricingOverride{{
			Model:                      "private/expensive",
			InputCostPerMillionTokens:  100, // $100/M input
			OutputCostPerMillionTokens: 200, // $200/M output
		}},
	})

	// $0.50 ceiling.
	p := NewScheduledPolicy(NewLogSession(), 50, 0.50, 0)

	// One step: 1000 input + 1000 output tokens.
	// override cost = (1000/1e6)*100 + (1000/1e6)*200 = 0.10 + 0.20 = 0.30
	p.orch.updateUsage("private/expensive", fantasy.Usage{InputTokens: 1000, OutputTokens: 1000}, fantasy.ProviderMetadata{})
	if !approxEqual(p.orch.CostUSD, 0.30) {
		t.Fatalf("CostUSD after one override-priced step = %v, want 0.30", p.orch.CostUSD)
	}
	// Under the ceiling so far.
	if blocked, _ := p.BeforeToolCall("read_file", "c1", "{}"); blocked {
		t.Fatal("blocked before exceeding the ceiling")
	}

	// A second identical step pushes accrued cost to 0.60 >= 0.50.
	p.orch.updateUsage("private/expensive", fantasy.Usage{InputTokens: 1000, OutputTokens: 1000}, fantasy.ProviderMetadata{})
	if !approxEqual(p.orch.CostUSD, 0.60) {
		t.Fatalf("CostUSD after two override-priced steps = %v, want 0.60", p.orch.CostUSD)
	}
	blocked, msg := p.BeforeToolCall("read_file", "c2", "{}")
	if !blocked {
		t.Fatalf("expected the cost ceiling to fire at override-priced spend %.2f >= 0.50", p.orch.CostUSD)
	}
	if msg == "" {
		t.Fatal("ceiling block returned an empty message")
	}
}

// TestUpdateUsage_DefaultUsesOpenRouterCost is the no-override regression guard:
// with the default (empty) pricing config, updateUsage accrues exactly the
// OpenRouter-returned cost — byte-identical to the pre-#297 path.
func TestUpdateUsage_DefaultUsesOpenRouterCost(t *testing.T) {
	t.Cleanup(func() { ConfigurePricing(PricingConfig{}) })
	ConfigurePricing(PricingConfig{}) // explicit default

	p := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	md := openRouterMetadataWithCost(0.0042)
	p.orch.updateUsage("openai/gpt-4o", fantasy.Usage{InputTokens: 100, OutputTokens: 50}, md)
	if !approxEqual(p.orch.CostUSD, 0.0042) {
		t.Fatalf("default-path CostUSD = %v, want 0.0042 (the OR cost)", p.orch.CostUSD)
	}
}
