package agentcore

import "testing"

// Gemini 4 spends part of max_output_tokens on thinking, so a cap below its
// floor can return an empty answer. The floor raises a smaller cap, never
// lowers a larger one, and leaves every other family on the configured cap.
func TestCompletionTokensForModel(t *testing.T) {
	for _, tc := range []struct {
		slug       string
		configured int64
		want       int64
	}{
		{"google/gemini-4-argon", DefaultMaxCompletionTokens, 32_768},
		{"google/gemini-4-argon", 512, 32_768},
		{"~Google/Gemini-4-Argon", 1024, 32_768}, // alias sigil + case
		{"google/gemini-4-argon", 65_536, 65_536},
		{"google/gemini-3.8-flash", DefaultMaxCompletionTokens, DefaultMaxCompletionTokens},
		{DefaultCoreModel, DefaultMaxCompletionTokens, DefaultMaxCompletionTokens},
		{DefaultMaxModel, DefaultMaxCompletionTokens, DefaultMaxCompletionTokens},
		{"anthropic/claude-opus-5.5", 512, 512},
	} {
		if got := completionTokensForModel(tc.slug, tc.configured); got != tc.want {
			t.Errorf("completionTokensForModel(%q, %d) = %d, want %d", tc.slug, tc.configured, got, tc.want)
		}
	}
}
