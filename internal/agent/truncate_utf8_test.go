package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPromptTruncationNeverSplitsARune pins #1935: the agent's prompt-building
// clamps used to slice on a raw byte offset, so multibyte task or memory text
// (CJK, emoji) could be cut mid-rune and reach the model as invalid UTF-8.
func TestPromptTruncationNeverSplitsARune(t *testing.T) {
	long := strings.Repeat("日本語🙂", 4000) // 3- and 4-byte runes
	for name, got := range map[string]string{
		"truncate":                         truncate(long, 101),
		"truncateTaskForVerifier":          truncateTaskForVerifier(long),
		"truncateFinalResponseForVerifier": truncateFinalResponseForVerifier(long),
	} {
		if !utf8.ValidString(got) {
			t.Errorf("%s produced invalid UTF-8", name)
		}
		if len(got) >= len(long) {
			t.Errorf("%s did not truncate", name)
		}
	}
}
