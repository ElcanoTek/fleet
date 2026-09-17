package config

import "testing"

// The admin override of the per-run cost ceiling is a separate value from the
// env-derived field: it wins while set, and an env reload that rewrites the
// field takes effect again the moment the override is cleared.
func TestMaxCostUSDOverridePrecedence(t *testing.T) {
	c := &Config{MaxCostUSD: 50}
	if got := c.LiveMaxCostUSD(); got != 50 {
		t.Fatalf("env default: %v", got)
	}
	if _, ok := c.MaxCostUSDOverride(); ok {
		t.Fatal("no override expected at boot")
	}

	c.SetMaxCostUSDOverride(8.5, true)
	if got := c.LiveMaxCostUSD(); got != 8.5 {
		t.Fatalf("override not effective: %v", got)
	}
	if v, ok := c.MaxCostUSDOverride(); !ok || v != 8.5 {
		t.Fatalf("override not reported: %v %v", v, ok)
	}
	// An env-file reload rewrites the field underneath; the override still wins.
	c.MaxCostUSD = 25
	if got := c.LiveMaxCostUSD(); got != 8.5 {
		t.Fatalf("reloaded env value must not displace the admin override: %v", got)
	}
	// Reset clears the override and the (reloaded) env value serves again.
	c.SetMaxCostUSDOverride(0, false)
	if got := c.LiveMaxCostUSD(); got != 25 {
		t.Fatalf("after reset the env-derived value must serve: %v", got)
	}
	// 0 is a legitimate override: no ceiling.
	c.SetMaxCostUSDOverride(0, true)
	if got := c.LiveMaxCostUSD(); got != 0 {
		t.Fatalf("an explicit 0 override means unlimited: %v", got)
	}
}
