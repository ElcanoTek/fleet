package config

import "testing"

// The admin override of the per-run uncached-token ceiling mirrors the cost
// ceiling's: a separate value that wins while set, with the env-derived field
// (and any env reload of it) serving again the moment it is cleared.
func TestMaxTotalTokensOverridePrecedence(t *testing.T) {
	c := &Config{MaxTotalTokens: 10_000_000}
	if got := c.LiveMaxTotalTokens(); got != 10_000_000 {
		t.Fatalf("env default: %v", got)
	}
	if _, ok := c.MaxTotalTokensOverride(); ok {
		t.Fatal("no override expected at boot")
	}

	c.SetMaxTotalTokensOverride(30_000_000, true)
	if got := c.LiveMaxTotalTokens(); got != 30_000_000 {
		t.Fatalf("override not effective: %v", got)
	}
	if v, ok := c.MaxTotalTokensOverride(); !ok || v != 30_000_000 {
		t.Fatalf("override not reported: %v %v", v, ok)
	}
	// An env-file reload rewrites the field underneath; the override still wins.
	c.MaxTotalTokens = 5_000_000
	if got := c.LiveMaxTotalTokens(); got != 30_000_000 {
		t.Fatalf("reloaded env value must not displace the admin override: %v", got)
	}
	// Reset clears the override and the (reloaded) env value serves again.
	c.SetMaxTotalTokensOverride(0, false)
	if got := c.LiveMaxTotalTokens(); got != 5_000_000 {
		t.Fatalf("after reset the env-derived value must serve: %v", got)
	}
	// 0 is a legitimate override: no ceiling.
	c.SetMaxTotalTokensOverride(0, true)
	if got := c.LiveMaxTotalTokens(); got != 0 {
		t.Fatalf("an explicit 0 override means unlimited: %v", got)
	}
}
