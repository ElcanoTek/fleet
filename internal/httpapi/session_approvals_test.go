package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

func TestSessionApprovalRegistry_Match(t *testing.T) {
	r := NewSessionApprovalRegistry()

	// approve-all for one tool in one conversation.
	r.Register("c1", "mcp_sendgrid_send_email", SessionApprovalPolicy{Mode: "approve"})
	if p, ok := r.Match("c1", "mcp_sendgrid_send_email", `{"to":"a@b.com"}`); !ok || p.Mode != "approve" {
		t.Fatalf("approve-all should match: ok=%v mode=%q", ok, p.Mode)
	}
	// Scoped to the conversation: another conv does not match.
	if _, ok := r.Match("c2", "mcp_sendgrid_send_email", `{}`); ok {
		t.Error("policy must not leak to another conversation")
	}
	// Scoped to the tool: another tool does not match.
	if _, ok := r.Match("c1", "mcp_other", `{}`); ok {
		t.Error("policy must not leak to another tool")
	}

	// Pattern-scoped: glob on a named argument.
	r.Register("c1", "mcp_x", SessionApprovalPolicy{Mode: "approve", Pattern: "to=*.company.com"})
	if _, ok := r.Match("c1", "mcp_x", `{"to":"a@evil.com"}`); ok {
		t.Error("pattern must not match a non-conforming arg")
	}
	if _, ok := r.Match("c1", "mcp_x", `{"to":"a@x.company.com"}`); !ok {
		t.Error("pattern must match a conforming arg")
	}

	// Deny wins over a co-registered approve.
	r.Register("c1", "mcp_y", SessionApprovalPolicy{Mode: "approve"})
	r.Register("c1", "mcp_y", SessionApprovalPolicy{Mode: "deny"})
	if p, ok := r.Match("c1", "mcp_y", `{}`); !ok || p.Mode != "deny" {
		t.Fatalf("deny must win: ok=%v mode=%q", ok, p.Mode)
	}
}

func TestMaybeRegisterSessionPolicy(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)

	// session approve → policy present, mode approve.
	s.maybeRegisterSessionPolicy("c1", "u@e.com", "mcp_t", approvalRequest{Approved: true, Scope: "session"})
	if p, ok := s.sessionApprovals.Match("c1", "mcp_t", `{}`); !ok || p.Mode != "approve" {
		t.Errorf("session approve should register an approve policy: ok=%v mode=%q", ok, p.Mode)
	}

	// session deny → mode deny.
	s.maybeRegisterSessionPolicy("c1", "u@e.com", "mcp_d", approvalRequest{Approved: false, Scope: "session"})
	if p, ok := s.sessionApprovals.Match("c1", "mcp_d", `{}`); !ok || p.Mode != "deny" {
		t.Errorf("session deny should register a deny policy: ok=%v mode=%q", ok, p.Mode)
	}

	// "once" (and empty) → nothing registered.
	s.maybeRegisterSessionPolicy("c1", "u@e.com", "mcp_o", approvalRequest{Approved: true, Scope: "once"})
	if _, ok := s.sessionApprovals.Match("c1", "mcp_o", `{}`); ok {
		t.Error(`scope "once" must not register a session policy`)
	}

	// pattern scope carries the glob.
	s.maybeRegisterSessionPolicy("c1", "u@e.com", "mcp_p", approvalRequest{Approved: true, Scope: "pattern", Pattern: "to=*@ok.com"})
	if _, ok := s.sessionApprovals.Match("c1", "mcp_p", `{"to":"x@ok.com"}`); !ok {
		t.Error("pattern policy should match a conforming call")
	}
	if _, ok := s.sessionApprovals.Match("c1", "mcp_p", `{"to":"x@no.com"}`); ok {
		t.Error("pattern policy should not match a non-conforming call")
	}
}
