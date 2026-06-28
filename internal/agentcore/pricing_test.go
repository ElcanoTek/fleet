// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"math"
	"strings"
	"testing"
)

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

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
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
