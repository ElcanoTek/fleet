// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agent

import "testing"

// A task's own ceilings (#1533) ride the same override fields a spawned
// child's sliced budget uses, so enforcement stays the one checkCeilings path.
func TestNewAgentAppliesPerTaskCeilings(t *testing.T) {
	a := NewAgent(Options{MaxCostUSD: 12.5, MaxTotalTokens: 30_000_000})
	if a.costCeilingOverride != 12.5 || a.tokenCeilingOverride != 30_000_000 {
		t.Fatalf("overrides = (%v, %d), want (12.5, 30000000)", a.costCeilingOverride, a.tokenCeilingOverride)
	}
	if b := NewAgent(Options{}); b.costCeilingOverride != 0 || b.tokenCeilingOverride != 0 {
		t.Fatalf("omitted ceilings must inherit (0), got (%v, %d)", b.costCeilingOverride, b.tokenCeilingOverride)
	}
}
