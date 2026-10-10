package agentcore

import (
	"testing"
	"time"
)

// TestApprovedCallBudget pins the approved-card call budget: a server that
// declares no approved_call_timeout_seconds keeps the flat 60 s with or
// without deal_ids; a declared server gets its own budget, raised to
// deal_ids × the server's batch pace and capped at 30 minutes; and the
// declaration governs the server's named-account variants. The expected
// values are written out by hand, not computed with the helpers under test.
func TestApprovedCallBudget(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		ApprovedCallTimeoutSeconds: map[string]int{
			"deals_mcp":       300,
			"deals_mcp_slow":  900,
			"paced_mcp":       120,
			"zero_mcp":        0,
			"negative_mcp":    -5,
			"whole_cap_mcp":   1800,
			"tiny_budget_mcp": 1,
		},
		BatchSecondsPerDeal: map[string]int{
			"paced_mcp": 45,
			// A batch pace alone opts nothing in on the approval path.
			"batch_only_mcp": 45,
		},
	})
	cases := []struct {
		name, server, input string
		want                time.Duration
		declared            bool
	}{
		// Not opted in: exactly the old flat 60 s.
		{"undeclared: no deal_ids", "sendgrid", `{"to":"a@example.com"}`, 60 * time.Second, false},
		{"undeclared: deal_ids do not scale", "openx_mcp", dealIDsInput(59), 60 * time.Second, false},
		{"undeclared: batch pace alone does not opt in", "batch_only_mcp", dealIDsInput(59), 60 * time.Second, false},
		{"undeclared: zero is ignored", "zero_mcp", dealIDsInput(59), 60 * time.Second, false},
		{"undeclared: negative is ignored", "negative_mcp", `{}`, 60 * time.Second, false},
		{"undeclared: look-alike name", "deals_mcpx", `{}`, 60 * time.Second, false},

		// Opted in.
		{"declared: base", "deals_mcp", `{"name":"one deal"}`, 300 * time.Second, true},
		{"declared: unparseable input keeps the base", "deals_mcp", `{"deal_ids":`, 300 * time.Second, true},
		{"declared: empty deal_ids keeps the base", "deals_mcp", `{"deal_ids":[]}`, 300 * time.Second, true},
		{"declared: small batch stays at the base", "deals_mcp", dealIDsInput(10), 300 * time.Second, true},     // 10 × 20 s = 200 s
		{"declared: batch above the base scales", "deals_mcp", dealIDsInput(59), 1180 * time.Second, true},      // 59 × 20 s
		{"declared: batch is capped at 30 minutes", "deals_mcp", dealIDsInput(200), 30 * time.Minute, true},     // 200 × 20 s = 4000 s
		{"declared: the server's batch pace is used", "paced_mcp", dealIDsInput(12), 540 * time.Second, true},   // 12 × 45 s
		{"declared: pace below the base keeps the base", "paced_mcp", dealIDsInput(2), 120 * time.Second, true}, // 2 × 45 s = 90 s
		{"declared: a one-second budget is honoured", "tiny_budget_mcp", `{}`, time.Second, true},
		{"declared: the 1800 s ceiling itself", "whole_cap_mcp", dealIDsInput(1), 30 * time.Minute, true},

		// Named-account variants.
		{"variant inherits its base", "deals_mcp_tunnl", `{}`, 300 * time.Second, true},
		{"variant inherits scaling", "deals_mcp_tunnl", dealIDsInput(59), 1180 * time.Second, true},
		{"variant's own key wins", "deals_mcp_slow", `{}`, 900 * time.Second, true},
		{"variant of the paced server uses the inherited pace", "paced_mcp_reklaim", dealIDsInput(12), 540 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, declared := ApprovedCallBudget(tc.server, tc.input)
			if got != tc.want || declared != tc.declared {
				t.Fatalf("ApprovedCallBudget(%q) = (%v, %v), want (%v, %v)", tc.server, got, declared, tc.want, tc.declared)
			}
		})
	}
}

// TestApprovedCallBudgetDefaultPolicy: with no bundle policy installed at
// all (the generic bundle), every approved call is on the old 60 s.
func TestApprovedCallBudgetDefaultPolicy(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{})
	if got, declared := ApprovedCallBudget("deals_mcp", dealIDsInput(59)); got != DefaultApprovedCallBudget || declared {
		t.Fatalf("ApprovedCallBudget with no policy = (%v, %v), want (60s, false)", got, declared)
	}
	if DefaultApprovedCallBudget != 60*time.Second {
		t.Fatalf("DefaultApprovedCallBudget = %v; the undeclared budget must stay the historical 60 s", DefaultApprovedCallBudget)
	}
}
