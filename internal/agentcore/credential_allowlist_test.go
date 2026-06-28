package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Tests for the per-task credential allowlist (#184): Permits / the registered-
// name projection, and the Gate-3 enforcement at the MCPBroker seam (which is
// the SINGLE path every MCP call routes through, so gating it there enforces the
// allowlist for every caller).

func TestCredentialAllowlist_Permits(t *testing.T) {
	tests := []struct {
		name    string
		al      CredentialAllowlist
		server  string
		account string
		want    bool
	}{
		{"nil inherits global — anything permitted", nil, "github", "client_a", true},
		{"empty denies all", CredentialAllowlist{}, "github", "", false},
		{"exact pair permitted", CredentialAllowlist{{Server: "github", Account: "client_a"}}, "github", "client_a", true},
		{"default seat denied when only a variant is listed", CredentialAllowlist{{Server: "github", Account: "client_a"}}, "github", "", false},
		{"other server denied", CredentialAllowlist{{Server: "github", Account: "client_a"}}, "sendgrid", "", false},
		{"account canonicalization (hyphen folds to underscore)", CredentialAllowlist{{Server: "github", Account: "client-a"}}, "github", "client_a", true},
		{"default-seat entry matches empty account", CredentialAllowlist{{Server: "sendgrid"}}, "sendgrid", "", true},
		{"default-seat entry does not match a variant", CredentialAllowlist{{Server: "sendgrid"}}, "sendgrid", "client_b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.al.Permits(tt.server, tt.account); got != tt.want {
				t.Errorf("Permits(%q,%q) = %v, want %v", tt.server, tt.account, got, tt.want)
			}
		})
	}
}

func TestCredentialAllowlist_PermittedRegisteredNames(t *testing.T) {
	if CredentialAllowlist(nil).permittedRegisteredNames() != nil {
		t.Error("nil allowlist must project to nil (inherit global), not an empty set")
	}
	got := CredentialAllowlist{
		{Server: "github", Account: "client-a"},
		{Server: "sendgrid"},
	}.permittedRegisteredNames()
	want := map[string]bool{"github_client_a": true, "sendgrid": true}
	if len(got) != len(want) {
		t.Fatalf("registered names = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing registered name %q in %v", k, got)
		}
	}
	// Empty (non-nil) allowlist → empty (non-nil) set: deny all, not inherit.
	if names := (CredentialAllowlist{}).permittedRegisteredNames(); names == nil || len(names) != 0 {
		t.Errorf("empty allowlist must project to a non-nil empty set, got %v", names)
	}
}

func TestGateMCPBrokerWithAllowlist(t *testing.T) {
	// nil allowlist → inherit global → the broker is returned UNWRAPPED (no overhead).
	inner := &recordingBroker{text: "ok"}
	if got := GateMCPBrokerWithAllowlist(inner, nil); got != MCPBroker(inner) {
		t.Error("nil allowlist must return the broker unchanged")
	}

	// Allowlist scoped to github_client_a: that pair passes through to the inner
	// broker; any other (server, account) is denied at the seam.
	al := CredentialAllowlist{{Server: "github", Account: "client-a"}}

	t.Run("permitted pair reaches the inner broker", func(t *testing.T) {
		inner := &recordingBroker{text: "result", isErr: false}
		gated := GateMCPBrokerWithAllowlist(inner, al)
		text, isErr, err := gated.CallMCP(context.Background(), "github_client_a", "lookup", nil)
		if err != nil || isErr || text != "result" {
			t.Fatalf("permitted call should pass through: text=%q isErr=%v err=%v", text, isErr, err)
		}
		if inner.calls != 1 {
			t.Errorf("inner broker should be called once, got %d", inner.calls)
		}
	})

	for _, server := range []string{"github", "sendgrid", "github_client_b"} {
		t.Run("denied: "+server, func(t *testing.T) {
			inner := &recordingBroker{text: "SHOULD NOT RUN"}
			gated := GateMCPBrokerWithAllowlist(inner, al)
			text, isErr, err := gated.CallMCP(context.Background(), server, "lookup", nil)
			if err != nil {
				t.Fatalf("denial must be a tool-level error, not a transport error: %v", err)
			}
			if !isErr {
				t.Error("denied call must return isError=true")
			}
			if !strings.Contains(text, "credential_allowlist_denied") || !strings.Contains(text, server) {
				t.Errorf("denial message missing marker/server: %q", text)
			}
			if inner.calls != 0 {
				t.Errorf("denied call must NOT reach the credentialed inner broker, got %d calls", inner.calls)
			}
		})
	}

	// Empty (non-nil) allowlist denies everything, including a default seat.
	t.Run("empty allowlist denies all", func(t *testing.T) {
		inner := &recordingBroker{}
		gated := GateMCPBrokerWithAllowlist(inner, CredentialAllowlist{})
		_, isErr, _ := gated.CallMCP(context.Background(), "anything", "t", nil)
		if !isErr || inner.calls != 0 {
			t.Errorf("empty allowlist must deny all; isErr=%v calls=%d", isErr, inner.calls)
		}
	})
}

// End-to-end through mcpTool: a denied pair surfaces the governance message to
// the model as an error response (not a transport error) and records the denial
// through the Policy seam for the audit trail — covering BOTH flavors, since
// both dispatch their MCP calls through this gated broker.
func TestMCPTool_OverGatedBroker_DeniesAndAudits(t *testing.T) {
	policy := &gatePolicy{}
	inner := &recordingBroker{text: "SHOULD NOT RUN"}
	gated := GateMCPBrokerWithAllowlist(inner, CredentialAllowlist{{Server: "github", Account: "client-a"}})
	tool := &mcpTool{serverName: "sendgrid", tool: mcp.Tool{Name: "send", Description: "d"}, broker: gated, policy: policy}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: "{}"})
	if err != nil {
		t.Fatalf("Run returned a transport error, want a clean error response: %v", err)
	}
	if !resp.IsError {
		t.Error("denied tool call should be an error response")
	}
	if !strings.Contains(resp.Content, "credential_allowlist_denied") {
		t.Errorf("denial message missing: %q", resp.Content)
	}
	if inner.calls != 0 {
		t.Errorf("the credentialed inner broker must not be reached, got %d calls", inner.calls)
	}
	if !policy.recorded || policy.recordOK {
		t.Errorf("denial must record a FAILED tool result; recorded=%v ok=%v", policy.recorded, policy.recordOK)
	}
}
