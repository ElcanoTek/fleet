// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import "testing"

// The 300-second window is not 300 seconds of the user's attention: the card
// lands whenever the agent reaches it, often many minutes into a run they
// started and then reasonably stopped watching. So the thing it most reliably
// blocked was the final, wanted action of a long analysis (#1153).
func TestApprovalModeForTool(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })

	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"deploy_page", "create_deal", "page"},
		CriticalToolModes: map[string]string{
			"deploy_page": ApprovalModeNotify,
			"create_deal": ApprovalModeApprove,
			// A short suffix that a longer one must outrank.
			"page": ApprovalModeApprove,
		},
		CriticalToolUndoHints: map[string]string{
			"deploy_page": "Undo with mcp_pages_rollback_page(slug, version_id).",
		},
	})

	mode, hint := ApprovalModeForTool("mcp_pages_deploy_page")
	if mode != ApprovalModeNotify {
		t.Errorf("deploy_page mode = %q, want %q", mode, ApprovalModeNotify)
	}
	// "we can always roll back" is only true in practice if the card says how,
	// and fleet does not know any client's reversal verb.
	if hint == "" {
		t.Error("a notify tool must carry the bundle's undo line for the record card")
	}
	if mode, _ := ApprovalModeForTool("mcp_ssp_create_deal"); mode != ApprovalModeApprove {
		t.Errorf("create_deal mode = %q, want %q — audit-gated writes stay blocking", mode, ApprovalModeApprove)
	}
	// An undeclared tool is unchanged, so adding modes is a no-op for any bundle
	// that does not use them.
	if mode, hint := ApprovalModeForTool("mcp_other_do_thing"); mode != ApprovalModeApprove || hint != "" {
		t.Errorf("undeclared tool = (%q, %q), want (%q, \"\")", mode, hint, ApprovalModeApprove)
	}
	// Longest suffix wins, exactly like the timeout table.
	if mode, _ := ApprovalModeForTool("mcp_pages_deploy_page"); mode != ApprovalModeNotify {
		t.Errorf("longest-suffix match failed: got %q", mode)
	}
}

// The entire case for running without asking is "we can always roll it back".
// For outbound email that is simply false, so a bundle cannot buy its way out of
// the review step — whatever the manifest says.
func TestNotifyIsRefusedForOutboundEmail(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolModes: map[string]string{
			"send_email":          ApprovalModeNotify,
			"send_template_email": ApprovalModeNotify,
		},
	})
	for _, tool := range []string{"mcp_sendgrid_send_email", "send_email", "mcp_x_send_template_email"} {
		if mode, _ := ApprovalModeForTool(tool); mode != ApprovalModeApprove {
			t.Errorf("%s mode = %q, want %q: a sent message has no undo", tool, mode, ApprovalModeApprove)
		}
	}
}

// A typo in a manifest must not silently become a policy. An unknown mode is
// dropped, which leaves the tool blocking — the safe direction.
func TestUnknownModeFallsBackToApprove(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolModes: map[string]string{"deploy_page": "auto", "  ": ApprovalModeNotify},
	})
	if mode, _ := ApprovalModeForTool("mcp_pages_deploy_page"); mode != ApprovalModeApprove {
		t.Errorf("mode = %q, want %q for an unrecognized declaration", mode, ApprovalModeApprove)
	}
}

// Reconfiguring fully replaces the policy: a mode left behind by a previous
// bundle would be a permission nobody granted.
func TestModesAreReplacedNotMerged(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolModes: map[string]string{"deploy_page": ApprovalModeNotify}})
	ConfigureAgentPolicy(AgentPolicy{})
	if mode, hint := ApprovalModeForTool("mcp_pages_deploy_page"); mode != ApprovalModeApprove || hint != "" {
		t.Errorf("after reconfigure = (%q, %q), want the default blocking mode", mode, hint)
	}
}
