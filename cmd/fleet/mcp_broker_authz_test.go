package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
)

// brokerAuthzServerScript is a two-tool stdio MCP server: the bundle allowlist
// decides which of the two a scope may reach.
const brokerAuthzServerScript = `
import json, sys

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    rid = req.get("id")
    method = req.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":rid,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"authz","version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":rid,"result":{"tools":[
            {"name":"safe_read","description":"","inputSchema":{"type":"object"}},
            {"name":"danger_write","description":"","inputSchema":{"type":"object"}},
        ]}})
    elif method == "tools/call":
        name = req.get("params", {}).get("name", "")
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"ran " + name}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

func newBrokerBackendForAuthzTest(t *testing.T, bundleAllow map[string][]string) *brokerBackend {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	client := mcp.NewClient()
	return &brokerBackend{
		MCPBroker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:    client,
		bases: map[string]agentcore.MCPServerBase{
			"gated": {
				Command: "python3",
				Args:    []string{"-u", "-c", brokerAuthzServerScript},
			},
		},
		bundleAllow: bundleAllow,
		enabled:     map[string]bool{"gated": true},
		scopes:      make(map[string]*brokerScope),
	}
}

func toolNames(tools []mcpbroker.ToolDescriptor) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Server+"."+tool.Tool)
	}
	return out
}

// The child's own bundle allowlist is AUTHORITATIVE: a parent that claims a
// tool the bundle never allowed gets a refusal, not the call. This is the
// backstop the Gate-2 regression (#960) had none of — there, activating the
// production boundary scrubbed the very cfg.MCPServers the parent read its
// gates from, and every scheduled run silently lost its allowlist.
func TestBrokerScope_ChildRefusesToolTheParentClaimsButTheBundleDenies(t *testing.T) {
	b := newBrokerBackendForAuthzTest(t, map[string][]string{"gated": {"safe_read"}})
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, tools, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "gated"}},
		// A parent that lost (or was made to forget) its own gates: it asserts
		// the dangerous tool is permitted.
		Policy: &mcpbroker.ScopePolicy{Tools: map[string][]string{"gated": {"safe_read", "danger_write"}}},
	})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}

	if got := toolNames(tools); len(got) != 1 || got[0] != "gated.safe_read" {
		t.Fatalf("scope catalog = %v, want only the bundle-allowed tool", got)
	}

	text, isErr, err := b.CallMCPInScope(ctx, id, "gated", "danger_write", nil)
	if err != nil {
		t.Fatalf("CallMCPInScope returned a transport error: %v", err)
	}
	if !isErr || !strings.Contains(text, brokerPolicyDenied) {
		t.Fatalf("denied call = (%q, isErr=%v), want a tool-level %s refusal", text, isErr, brokerPolicyDenied)
	}

	text, isErr, err = b.CallMCPInScope(ctx, id, "gated", "safe_read", nil)
	if err != nil || isErr {
		t.Fatalf("permitted call = (%q, %v, %v), want success", text, isErr, err)
	}
	if !strings.Contains(text, "ran safe_read") {
		t.Fatalf("permitted call text = %q", text)
	}
}

// The parent may still NARROW below the bundle floor — persona/task gating that
// the child has no source of truth for.
func TestBrokerScope_ParentPolicyNarrowsBelowTheBundleAllowlist(t *testing.T) {
	b := newBrokerBackendForAuthzTest(t, nil) // no bundle allowlist: every tool exported
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, tools, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "gated"}},
		Policy:    &mcpbroker.ScopePolicy{Tools: map[string][]string{"gated": {"safe_read"}}},
	})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	if got := toolNames(tools); len(got) != 1 || got[0] != "gated.safe_read" {
		t.Fatalf("scope catalog = %v, want the parent-narrowed set", got)
	}
	text, isErr, _ := b.CallMCPInScope(ctx, id, "gated", "danger_write", nil)
	if !isErr || !strings.Contains(text, brokerPolicyDenied) {
		t.Fatalf("narrowed call = (%q, isErr=%v), want a refusal", text, isErr)
	}
}

// Gate-3 rides the same helper the in-process loop uses, so a per-task
// credential allowlist is enforced on the credential owner's side too.
func TestBrokerScope_EnforcesCredentialAllowlistChildSide(t *testing.T) {
	b := newBrokerBackendForAuthzTest(t, nil)
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, _, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "gated"}},
		// Non-nil but empty: deny every pair (the #184 "restrict to nothing"
		// answer, distinct from nil "inherit global").
		Policy: &mcpbroker.ScopePolicy{RestrictCredentials: true},
	})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	text, isErr, err := b.CallMCPInScope(ctx, id, "gated", "safe_read", nil)
	if err != nil {
		t.Fatalf("CallMCPInScope: %v", err)
	}
	if !isErr || !strings.Contains(text, "credential_allowlist_denied") {
		t.Fatalf("call = (%q, isErr=%v), want the Gate-3 denial", text, isErr)
	}
}

// A selection naming a server this credential owner does not have enabled is an
// authorization refusal, not a spawn accident.
func TestBrokerScope_RefusesSelectionOutsideTheChildsEnabledSet(t *testing.T) {
	b := newBrokerBackendForAuthzTest(t, nil)
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "not-in-this-bundle"}},
	})
	if err == nil || !strings.Contains(err.Error(), "does not have enabled") {
		t.Fatalf("OpenScope error = %v, want a refusal naming the unknown server", err)
	}
	if len(b.scopes) != 0 {
		t.Fatalf("refused open left %d scope(s) behind", len(b.scopes))
	}
}

// The unscoped shared client is the default-seat client carrying every enabled
// server. Production agent turns no longer reach it, but what does must still
// be bounded by the bundle allowlist.
func TestBrokerUnscopedCall_AppliesTheBundleAllowlist(t *testing.T) {
	b := newBrokerBackendForAuthzTest(t, map[string][]string{"gated": {"safe_read"}})
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	text, isErr, err := b.CallMCP(ctx, "gated", "danger_write", nil)
	if err != nil {
		t.Fatalf("CallMCP: %v", err)
	}
	if !isErr || !strings.Contains(text, brokerPolicyDenied) {
		t.Fatalf("unscoped denied call = (%q, isErr=%v), want a refusal", text, isErr)
	}
	if text, isErr, _ := b.CallMCP(ctx, "not-enabled", "anything", nil); !isErr || !strings.Contains(text, brokerPolicyDenied) {
		t.Fatalf("unscoped call to a disabled server = (%q, isErr=%v), want a refusal", text, isErr)
	}
}

func TestNarrowToolAllowlist(t *testing.T) {
	tests := []struct {
		name           string
		bundle, parent []string
		want           []string
		wantNil        bool
	}{
		{name: "neither restricts", wantNil: true},
		{name: "bundle only", bundle: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "parent only", parent: []string{"a"}, want: []string{"a"}},
		{name: "intersection", bundle: []string{"a", "b"}, parent: []string{"b", "c"}, want: []string{"b"}},
		{name: "disjoint denies everything", bundle: []string{"a"}, parent: []string{"z"}, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := narrowToolAllowlist(tc.bundle, tc.parent)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %v, want nil (no restriction)", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want a non-nil restriction")
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScopeCredentialAllowlist_DistinguishesInheritFromDenyAll(t *testing.T) {
	if got := scopeCredentialAllowlist(nil); got != nil {
		t.Fatalf("nil policy = %v, want nil (inherit global)", got)
	}
	if got := scopeCredentialAllowlist(&mcpbroker.ScopePolicy{}); got != nil {
		t.Fatalf("policy without RestrictCredentials = %v, want nil (inherit global)", got)
	}
	got := scopeCredentialAllowlist(&mcpbroker.ScopePolicy{RestrictCredentials: true})
	if got == nil || len(got) != 0 {
		t.Fatalf("RestrictCredentials with no pairs = %v, want a non-nil empty allowlist (deny all)", got)
	}
}

// The parent's side of the contract: serialize the effective sets, send no
// policy when there is nothing to narrow, and never conflate a nil credential
// allowlist with an empty one.
func TestBrokerScopePolicy_SerializesEffectiveSetsOnly(t *testing.T) {
	if got := brokerScopePolicy(agent.MCPScopePolicy{}); got != nil {
		t.Fatalf("empty policy = %+v, want nil (no narrowing to send)", got)
	}
	// An allowlist entry with no tools adds no narrowing in-process; it must not
	// cross as one either.
	if got := brokerScopePolicy(agent.MCPScopePolicy{
		ToolAllowlist: agentcore.MCPAllowlist{"gated": nil},
	}); got != nil {
		t.Fatalf("empty per-server list = %+v, want nil", got)
	}
	got := brokerScopePolicy(agent.MCPScopePolicy{
		ToolAllowlist:       agentcore.MCPAllowlist{"gated": {"safe_read"}},
		CredentialAllowlist: agentcore.CredentialAllowlist{},
	})
	if got == nil {
		t.Fatal("populated policy serialized to nil")
	}
	if len(got.Tools["gated"]) != 1 || got.Tools["gated"][0] != "safe_read" {
		t.Fatalf("tools = %v", got.Tools)
	}
	if !got.RestrictCredentials || len(got.Credentials) != 0 {
		t.Fatalf("empty (deny-all) credential allowlist did not survive: restrict=%v pairs=%v",
			got.RestrictCredentials, got.Credentials)
	}
}

// The registered-name projection must match BindMCPSelection's, or a
// named-account scope would gate the wrong key and deny every call.
func TestScopeAuthorizer_KeysOnRegisteredAccountVariantNames(t *testing.T) {
	authz := newScopeAuthorizer(
		agentcore.MCPSelection{{Server: "gated", Account: "client-a"}},
		map[string][]string{"gated": {"safe_read"}},
		nil,
	)
	if !authz.permits("gated_client_a", "safe_read") {
		t.Fatal("account-variant seat was not permitted its allowlisted tool")
	}
	if authz.permits("gated_client_a", "danger_write") {
		t.Fatal("account-variant seat was permitted a tool outside the allowlist")
	}
	if authz.permits("gated", "safe_read") {
		t.Fatal("the default seat was permitted in a named-account scope")
	}
}
