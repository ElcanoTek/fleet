package agentcore

import (
	"testing"

	"charm.land/fantasy/providers/openrouter"
)

func TestSupportsExtendedThinking(t *testing.T) {
	cases := []struct {
		slug string
		want bool
	}{
		{"anthropic/claude-opus-4.8", true},
		{"anthropic/claude-sonnet-4.6", true},
		{"anthropic/claude-sonnet-4.5", true},
		{"ANTHROPIC/CLAUDE-OPUS-4.8", true},     // case-insensitive
		{"  anthropic/claude-opus-4.8  ", true}, // trimmed
		{"~anthropic/claude-opus-4.8", false},   // floating alias: signatures dropped
		{"anthropic/claude-3.5-sonnet", false},  // Claude 3.x: no extended thinking here
		{"google/gemini-2.5-pro", false},
		{"openai/gpt-5", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := supportsExtendedThinking(tc.slug); got != tc.want {
			t.Errorf("supportsExtendedThinking(%q) = %v, want %v", tc.slug, got, tc.want)
		}
	}
}

func TestClampThinkingBudget(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, MinThinkingBudgetTokens},
		{500, MinThinkingBudgetTokens},
		{1024, 1024},
		{10000, 10000},
		{100000, 100000},
		{250000, MaxThinkingBudgetTokens},
	}
	for _, tc := range cases {
		if got := ClampThinkingBudget(tc.in); got != tc.want {
			t.Errorf("ClampThinkingBudget(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestThinkingConfigForBudget(t *testing.T) {
	if got := ThinkingConfigForBudget(0); got != nil {
		t.Errorf("budget 0 should disable thinking (nil), got %+v", got)
	}
	if got := ThinkingConfigForBudget(-5); got != nil {
		t.Errorf("negative budget should disable thinking (nil), got %+v", got)
	}
	got := ThinkingConfigForBudget(10000)
	if got == nil || !got.Enabled || got.BudgetTokens != 10000 {
		t.Fatalf("budget 10000 → %+v, want enabled 10000", got)
	}
	// Out-of-range default is clamped.
	got = ThinkingConfigForBudget(500)
	if got == nil || got.BudgetTokens != MinThinkingBudgetTokens {
		t.Fatalf("budget 500 → %+v, want clamped to %d", got, MinThinkingBudgetTokens)
	}
}

// orForExtraBody pulls the openrouter ExtraBody out of a providerOptions result.
func orExtraBody(t *testing.T, e *engine, slug string) map[string]any {
	t.Helper()
	opts := e.providerOptions(slug)
	or, ok := opts[openrouter.Name].(*openrouter.ProviderOptions)
	if !ok || or == nil {
		t.Fatalf("no openrouter provider options in %#v", opts)
	}
	return or.ExtraBody
}

func TestProviderOptions_ThinkingActivation(t *testing.T) {
	// Enabled + thinking-capable Claude slug → thinking param present + clamped.
	e := &engine{thinkingConfig: &ThinkingConfig{Enabled: true, BudgetTokens: 500}}
	eb := orExtraBody(t, e, "anthropic/claude-opus-4.8")
	th, ok := eb["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking param missing for enabled Claude slug: %#v", eb)
	}
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if th["budget_tokens"] != MinThinkingBudgetTokens {
		t.Errorf("thinking.budget_tokens = %v, want clamped %d", th["budget_tokens"], MinThinkingBudgetTokens)
	}
}

func TestProviderOptions_ThinkingDisabledCases(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ThinkingConfig
		slug string
	}{
		{"nil config", nil, "anthropic/claude-opus-4.8"},
		{"disabled config", &ThinkingConfig{Enabled: false, BudgetTokens: 10000}, "anthropic/claude-opus-4.8"},
		{"non-Claude slug", &ThinkingConfig{Enabled: true, BudgetTokens: 10000}, "google/gemini-2.5-pro"},
		{"alias slug", &ThinkingConfig{Enabled: true, BudgetTokens: 10000}, "~anthropic/claude-opus-4.8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &engine{thinkingConfig: tc.cfg}
			if eb := orExtraBody(t, e, tc.slug); eb["thinking"] != nil {
				t.Errorf("thinking param should be absent (%s), got %#v", tc.name, eb["thinking"])
			}
		})
	}
}
