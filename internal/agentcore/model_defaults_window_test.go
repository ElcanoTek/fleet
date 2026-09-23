package agentcore

import "testing"

// The compiled-in tiers must resolve to their real windows from the static
// table alone (staticContextWindow, not the catalog-first lookup), so a cold boot (no OpenRouter catalog yet) doesn't compact the
// everyday or strong tier early. GPT-6 had no row before gpt-6-luna-pro became
// DefaultCoreModel; Opus 5.5 rides the claude-opus-5 prefix.
func TestDefaultTiersResolveTheirContextWindows(t *testing.T) {
	for _, tc := range []struct {
		slug string
		want int
	}{
		{DefaultCoreModel, 1_050_000},
		{DefaultMaxModel, 1_000_000},
		{"openai/gpt-6-luna", 1_050_000},
		{"openai/gpt-6-sol-pro", 1_050_000},
		{"anthropic/claude-opus-5", 1_000_000},
	} {
		if got := staticContextWindow(tc.slug); got != tc.want {
			t.Errorf("staticContextWindow(%q) = %d, want %d", tc.slug, got, tc.want)
		}
	}
}
