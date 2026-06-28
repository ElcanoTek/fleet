package agentcore

import "testing"

// Sub-agent budget seam (#175, part b): the read (Budget) + charge-back
// (ChargeChildUsage) primitives a parent run uses to make its ceiling a hard wall
// across all descendants. These are pure accounting; the spawn-side enforcement
// (slicing, depth/fan-out caps) lives in internal/agent.

// TestBudgetState_RemainingAndUnlimited pins the remaining-budget math and the
// 0 == unlimited convention checkCeilings shares.
func TestBudgetState_RemainingAndUnlimited(t *testing.T) {
	// Finite: $1.00 ceiling, $0.30 spent → $0.70 remaining; 1000 tok, 400 spent.
	b := BudgetState{MaxCostUSD: 1.0, SpentCostUSD: 0.30, MaxTotalTokens: 1000, SpentTokens: 400}
	if got := b.RemainingCostUSD(); got < 0.70-1e-9 || got > 0.70+1e-9 {
		t.Fatalf("RemainingCostUSD = %v, want 0.70", got)
	}
	if got := b.RemainingTokens(); got != 600 {
		t.Fatalf("RemainingTokens = %d, want 600", got)
	}

	// Unlimited (0 ceiling) reports -1 on both axes.
	u := BudgetState{}
	if u.RemainingCostUSD() != -1 || u.RemainingTokens() != -1 {
		t.Fatalf("unlimited budget should report -1 remaining, got cost=%v tok=%d", u.RemainingCostUSD(), u.RemainingTokens())
	}

	// Over-budget never reports a negative slice (clamped to 0).
	over := BudgetState{MaxCostUSD: 1.0, SpentCostUSD: 1.5, MaxTotalTokens: 100, SpentTokens: 250}
	if over.RemainingCostUSD() != 0 || over.RemainingTokens() != 0 {
		t.Fatalf("over-budget should clamp remaining to 0, got cost=%v tok=%d", over.RemainingCostUSD(), over.RemainingTokens())
	}
}

// TestChargeChildUsage_FoldsIntoParentCeiling proves charging a child's usage into
// the parent makes the parent's OWN ceiling check (checkCeilings) account for it —
// the linchpin that makes the parent ceiling un-breachable by sub-agent spend.
func TestChargeChildUsage_FoldsIntoParentCeiling(t *testing.T) {
	// Parent policy with a 1000-uncached-token ceiling, nothing spent yet.
	p := NewScheduledPolicy(NewLogSession(), 50, 0, 1000)

	// Under budget: a benign tool is allowed.
	if blocked, _ := p.BeforeToolCall("read_file", "c1", "{}"); blocked {
		t.Fatal("blocked before any spend")
	}

	// Charge a child run that consumed 900 prompt + 200 completion = 1100 uncached.
	p.ChargeChildUsage(RunUsage{PromptTokens: 900, CompletionTokens: 200})

	// The parent's budget snapshot now reflects the child's spend.
	b := p.Budget()
	if b.SpentTokens != 1100 {
		t.Fatalf("parent SpentTokens = %d after charge-back, want 1100", b.SpentTokens)
	}
	if b.RemainingTokens() != 0 {
		t.Fatalf("parent RemainingTokens = %d, want 0 (over ceiling)", b.RemainingTokens())
	}

	// And the parent's OWN ceiling now fires — proving the child's spend counts
	// against the parent wall, not just a sibling-spawn read.
	if blocked, _ := p.BeforeToolCall("read_file", "c2", "{}"); !blocked {
		t.Fatal("parent ceiling must fire once a child's charged-back spend crosses it")
	}
}

// TestChargeChildUsage_CostAccumulates pins the cost axis of the charge-back.
func TestChargeChildUsage_CostAccumulates(t *testing.T) {
	p := NewScheduledPolicy(NewLogSession(), 50, 0.10, 0)
	p.ChargeChildUsage(RunUsage{CostUSD: 0.04})
	p.ChargeChildUsage(RunUsage{CostUSD: 0.04})
	if b := p.Budget(); b.SpentCostUSD < 0.08-1e-9 || b.SpentCostUSD > 0.08+1e-9 {
		t.Fatalf("two children charging $0.04 each → SpentCostUSD = %v, want 0.08", b.SpentCostUSD)
	}
	// A third child would push to $0.12 > $0.10 ceiling; the parent ceiling fires.
	p.ChargeChildUsage(RunUsage{CostUSD: 0.04})
	if blocked, _ := p.BeforeToolCall("read_file", "c1", "{}"); !blocked {
		t.Fatal("parent cost ceiling must fire once charged-back child cost crosses it")
	}
}
