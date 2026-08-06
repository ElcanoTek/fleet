package agentcore

import (
	"strings"
	"testing"
)

// stagerFake records Stage calls and returns a canned id, so the tests can
// assert what the interactive critical-tool gate stages without a real
// approvals table/SSE sink.
type stagerFake struct {
	staged []string
	ret    string // approval id or a Pre*Sentinel
	err    error
}

func (s *stagerFake) Stage(toolName, _, _ string) (string, error) {
	s.staged = append(s.staged, toolName)
	if s.err != nil {
		return "", s.err
	}
	if s.ret == "" {
		return "appr-1", nil
	}
	return s.ret, nil
}

func (s *stagerFake) StageSuggestion(string) (string, string, error) { return "", "", nil }

// TestInteractiveCriticalToolApproval pins the interactive counterpart of the
// scheduled confirm_audit gate: a bundle-declared critical suffix
// (agent_policy.critical_tools) routes through the approval-card UX before
// execution in interactive mode — staged, blocked with APPROVAL_REQUIRED, and
// honoring the session pre-approval sentinels — while non-critical tools and
// the tools owned by dedicated gates pass through untouched.
func TestInteractiveCriticalToolApproval(t *testing.T) {
	// Restore the package-wide fixture policy (TestMain) after the override.
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"create_deal"}})

	newPolicy := func(sink ApprovalStager) *InteractivePolicy {
		return NewInteractivePolicy(0, 0, sink, nil)
	}

	t.Run("critical MCP tool is staged and blocked", func(t *testing.T) {
		sink := &stagerFake{}
		p := newPolicy(sink)
		blocked, msg := p.BeforeToolCall("mcp_pubmatic_mcp_create_deal", "tc-1", `{"deal":"x"}`)
		if !blocked {
			t.Fatal("bundle-critical tool must be blocked pending approval")
		}
		if !strings.Contains(msg, "APPROVAL_REQUIRED") || !strings.Contains(msg, "appr-1") {
			t.Errorf("message should carry APPROVAL_REQUIRED + the approval id, got: %s", msg)
		}
		if len(sink.staged) != 1 || sink.staged[0] != "mcp_pubmatic_mcp_create_deal" {
			t.Errorf("staged = %v, want the critical tool", sink.staged)
		}
	})

	t.Run("account-variant tool name still matches the suffix", func(t *testing.T) {
		sink := &stagerFake{}
		p := newPolicy(sink)
		if blocked, _ := p.BeforeToolCall("mcp_pubmatic_mcp_acme_create_deal", "tc-2", `{}`); !blocked {
			t.Error("variant-registered critical tool must be gated too")
		}
	})

	t.Run("non-critical tool passes", func(t *testing.T) {
		sink := &stagerFake{}
		p := newPolicy(sink)
		if blocked, msg := p.BeforeToolCall("mcp_pubmatic_mcp_list_deals", "tc-3", `{}`); blocked {
			t.Errorf("non-critical tool must not be gated: %s", msg)
		}
		if len(sink.staged) != 0 {
			t.Errorf("nothing should be staged, got %v", sink.staged)
		}
	})

	t.Run("send_email stays owned by the email gate", func(t *testing.T) {
		sink := &stagerFake{}
		p := newPolicy(sink)
		blocked, msg := p.BeforeToolCall("mcp_sendgrid_send_email", "tc-4", `{"to":"a@b.c","content":"hi"}`)
		if !blocked {
			t.Fatal("send_email must still be staged (by checkEmailSafety)")
		}
		// One stage only — the critical gate must not double-stage it.
		if len(sink.staged) != 1 {
			t.Errorf("send_email staged %d times, want exactly 1 (email gate only): %v", len(sink.staged), sink.staged)
		}
		if !strings.Contains(msg, "send_email") {
			t.Errorf("unexpected message: %s", msg)
		}
	})

	t.Run("session pre-approval runs without a card", func(t *testing.T) {
		sink := &stagerFake{ret: PreApprovedSentinel}
		p := newPolicy(sink)
		if blocked, msg := p.BeforeToolCall("mcp_pubmatic_mcp_create_deal", "tc-5", `{}`); blocked {
			t.Errorf("pre-approved critical tool must run: %s", msg)
		}
	})

	t.Run("session pre-denial blocks without a card", func(t *testing.T) {
		sink := &stagerFake{ret: PreDeniedSentinel}
		p := newPolicy(sink)
		blocked, msg := p.BeforeToolCall("mcp_pubmatic_mcp_create_deal", "tc-6", `{}`)
		if !blocked || !strings.Contains(msg, "APPROVAL_DENIED") {
			t.Errorf("pre-denied critical tool must be blocked with APPROVAL_DENIED, got blocked=%v msg=%s", blocked, msg)
		}
	})

	t.Run("no approval sink is inert", func(t *testing.T) {
		p := newPolicy(nil)
		if blocked, msg := p.BeforeToolCall("mcp_pubmatic_mcp_create_deal", "tc-7", `{}`); blocked {
			t.Errorf("without a sink the gate mirrors checkBashSafety (inert): %s", msg)
		}
	})

	t.Run("base send_template_email suffix is gated", func(t *testing.T) {
		// send_template_email is a BASE critical suffix that is NOT matched by
		// isEmailSendTool — before this gate it executed un-staged interactively.
		sink := &stagerFake{}
		p := newPolicy(sink)
		if blocked, _ := p.BeforeToolCall("mcp_sendgrid_send_template_email", "tc-8", `{}`); !blocked {
			t.Error("send_template_email must be staged by the critical gate")
		}
		if len(sink.staged) != 1 {
			t.Errorf("expected exactly one stage, got %v", sink.staged)
		}
	})
}

// TestOneStagedWritePerServer pins the fix for a production data loss: an agent
// staged mcp_pages_patch_page for approval, read the "Do NOT retry" in the
// result as a rule about that tool name, rebuilt the same change as a full-file
// upload, and staged mcp_pages_deploy_page_upload for the SAME page. Both cards
// carried frozen arguments; the human approved both; the patch landed as version
// 145 and the upload — still carrying expected_version 143 from before the patch
// existed — was rejected as stale after every expensive step was already paid
// for. A second write to the same server must be refused at stage time.
func TestOneStagedWritePerServer(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{
		"patch_page", "deploy_page_upload", "create_datastream", "create_deal",
	}})

	t.Run("a different write on the same server is refused, not staged", func(t *testing.T) {
		sink := &stagerFake{}
		p := NewInteractivePolicy(0, 0, sink, nil)

		blocked, msg := p.BeforeToolCall("mcp_pages_patch_page", "tc-1", `{"slug":"energizer-weather-kpi"}`)
		if !blocked || !strings.Contains(msg, "APPROVAL_REQUIRED") {
			t.Fatalf("first write should stage: blocked=%v msg=%s", blocked, msg)
		}
		// The message must close the loophole the model actually walked through.
		if !strings.Contains(msg, "different tool") {
			t.Errorf("staging message must forbid re-routing through another tool, got: %s", msg)
		}

		blocked, msg = p.BeforeToolCall("mcp_pages_deploy_page_upload", "tc-2", `{"expected_version":"143"}`)
		if !blocked {
			t.Fatal("a second competing write must be blocked")
		}
		if !strings.Contains(msg, "APPROVAL_BLOCKED") {
			t.Errorf("want APPROVAL_BLOCKED, got: %s", msg)
		}
		if !strings.Contains(msg, "mcp_pages_patch_page") || !strings.Contains(msg, "appr-1") {
			t.Errorf("message must name the pending action and its approval id, got: %s", msg)
		}
		if len(sink.staged) != 1 {
			t.Errorf("the second write must never reach the sink, staged = %v", sink.staged)
		}
	})

	t.Run("repeating the same tool stays allowed for batch flows", func(t *testing.T) {
		sink := &stagerFake{}
		p := NewInteractivePolicy(0, 0, sink, nil)
		p.BeforeToolCall("mcp_pubmatic_mcp_create_deal", "tc-1", `{"id":"1"}`)
		if blocked, msg := p.BeforeToolCall("mcp_pubmatic_mcp_create_deal", "tc-2", `{"id":"2"}`); !blocked ||
			!strings.Contains(msg, "APPROVAL_REQUIRED") {
			t.Fatalf("N independent records on one tool must each stage: blocked=%v msg=%s", blocked, msg)
		}
		if len(sink.staged) != 2 {
			t.Errorf("both batch records should stage, got %v", sink.staged)
		}
	})

	t.Run("a write on a different server stays allowed", func(t *testing.T) {
		sink := &stagerFake{}
		p := NewInteractivePolicy(0, 0, sink, nil)
		p.BeforeToolCall("mcp_pages_patch_page", "tc-1", `{}`)
		if blocked, msg := p.BeforeToolCall("mcp_keel_create_datastream", "tc-2", `{}`); !blocked ||
			!strings.Contains(msg, "APPROVAL_REQUIRED") {
			t.Fatalf("unrelated server must still stage: blocked=%v msg=%s", blocked, msg)
		}
		if len(sink.staged) != 2 {
			t.Errorf("staged = %v, want one per server", sink.staged)
		}
	})

	t.Run("pre-approved and pre-denied calls do not reserve the server", func(t *testing.T) {
		// Neither leaves a card pending, so neither can be overtaken by a later
		// write. Recording them would block legitimate follow-up work.
		for _, sentinel := range []string{PreApprovedSentinel, PreDeniedSentinel} {
			sink := &stagerFake{ret: sentinel}
			p := NewInteractivePolicy(0, 0, sink, nil)
			p.BeforeToolCall("mcp_pages_patch_page", "tc-1", `{}`)
			sink.ret = ""
			blocked, msg := p.BeforeToolCall("mcp_pages_deploy_page_upload", "tc-2", `{}`)
			if !blocked || strings.Contains(msg, "APPROVAL_BLOCKED") {
				t.Errorf("%s must not reserve the server, got blocked=%v msg=%s", sentinel, blocked, msg)
			}
		}
	})
}
