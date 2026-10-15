package agentcore

import "testing"

// The compiled-in tiers must resolve to their real windows from the static
// table alone (staticContextWindow, not the catalog-first lookup), so a cold boot (no OpenRouter catalog yet) doesn't compact the
// everyday or strong tier early. Claude Haiku 5 (DefaultCoreModel since
// 2026-10-15) needs its own 1M row ahead of the generic 200K Claude line;
// Gemini 4 Argon (DefaultMaxModel) has an exact-slug row.
func TestDefaultTiersResolveTheirContextWindows(t *testing.T) {
	for _, tc := range []struct {
		slug string
		want int
	}{
		{DefaultCoreModel, 1_000_000},
		{"anthropic/claude-haiku-4.5", 200_000}, // the Haiku 5 row must not swallow Haiku 4.5
		{"openai/gpt-6-luna-pro", 1_050_000},
		{DefaultMaxModel, 1_048_576},
		{"anthropic/claude-opus-5.5", 1_000_000},
		{"openai/gpt-6-luna", 1_050_000},
		{"openai/gpt-6-sol-pro", 1_050_000},
		{"anthropic/claude-opus-5", 1_000_000},
	} {
		if got := staticContextWindow(tc.slug); got != tc.want {
			t.Errorf("staticContextWindow(%q) = %d, want %d", tc.slug, got, tc.want)
		}
	}
}
