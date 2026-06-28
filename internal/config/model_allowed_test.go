package config

import "testing"

func TestModelAllowed_ExactAndWildcard(t *testing.T) {
	patterns := []string{
		"openai/gpt-4o",   // exact
		"anthropic/*",     // whole-provider glob
		"google/gemini-*", // prefix glob
	}
	cases := []struct {
		slug string
		want bool
	}{
		{"openai/gpt-4o", true},                   // exact match
		{"openai/gpt-4o-mini", false},             // exact pattern must not prefix-match
		{"anthropic/claude-sonnet-4-5", true},     // provider glob
		{"anthropic/claude-opus-4-1", true},       // provider glob
		{"google/gemini-2.5-pro", true},           // prefix glob
		{"google/gemma-3", false},                 // outside the gemini- prefix
		{"moonshotai/kimi-k2", false},             // no pattern
		{"  anthropic/claude-sonnet-4-5  ", true}, // trimmed before matching
		{"", false}, // empty slug never matches
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			if got := ModelAllowed(tc.slug, patterns); got != tc.want {
				t.Errorf("ModelAllowed(%q) = %v, want %v", tc.slug, got, tc.want)
			}
		})
	}
}

func TestModelAllowed_EmptyListNeverMatches(t *testing.T) {
	if ModelAllowed("anything/at-all", nil) {
		t.Error("empty allow-list must not match (callers decide what empty means)")
	}
}

func TestModelAllowed_MalformedPatternIsNoMatch(t *testing.T) {
	// An unterminated character class is a malformed glob; it must not match
	// (path.Match errors), rather than being treated as a wildcard.
	if ModelAllowed("anthropic/claude", []string{"anthropic/[claude"}) {
		t.Error("malformed glob must not match")
	}
}

func TestLockdownAllows_SupportsWildcards(t *testing.T) {
	cfg := &Config{LockdownAllowedModels: []string{"anthropic/*"}}
	if !cfg.LockdownAllows("anthropic/claude-sonnet-4-5") {
		t.Error("expected wildcard lockdown allow-list to match an anthropic slug")
	}
	if cfg.LockdownAllows("openai/gpt-4o") {
		t.Error("wildcard scoped to anthropic must not match openai")
	}
}
