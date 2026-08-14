package main

import (
	"fmt"
	"slices"
	"sort"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
)

// Child-side authorization for the out-of-process MCP broker (issue #167,
// residual 1).
//
// Before this, the broker transported but never validated: it bound whatever
// {server, account} selection it was sent and ran whatever tool name arrived on
// a call frame. Every gate — the bundle tool allowlist, the per-task credential
// allowlist, persona narrowing — was enforced parent-side, i.e. inside the very
// address space the broker boundary exists to distrust. A parent-side bug was
// therefore a TOTAL gating bypass, and the Gate-2 regression that shipped when
// the production boundary scrubbed cfg.MCPServers out from under the scheduled
// agent (#960) had no backstop here at all.
//
// The rule this file implements: the credential owner enforces the gates it can
// re-derive from ITS OWN bundle, and treats anything the parent sends as
// further narrowing only.
//
//   - Gate-2 (per-server tool allowlist) is AUTHORITATIVE child-side. It comes
//     from the bundle this process loaded and reloads, never from the wire, so
//     a parent that forgets (or is made to forget) its allowlist cannot widen a
//     scope past it.
//   - Gate-3 (per-task {server, account} credential allowlist) has no
//     child-side source of truth — it is task data. The parent serializes the
//     effective pairs and the child enforces them with the same
//     agentcore.GateMCPBrokerWithAllowlist the in-process loop uses.
//   - Gate-1 (optional-server opt-in) is already structural here: only the
//     selected servers are bound into a scope's client, and scopeAuthorizer
//     rejects a dispatch to anything outside that set.
//
// Persona narrowing (Gate-4) stays a parent-side registration filter. It
// governs which tools a model may SEE, is resolved from persona definitions the
// parent owns, and can only subtract from what the gates above already permit —
// so it is not a credential boundary and is deliberately not duplicated here.

// brokerPolicyDenied is the stable, value-free marker a child-side refusal
// carries. Like the in-process credential-allowlist denial it is a TOOL-LEVEL
// error (isError=true), not a transport failure: the model sees a governance
// message as a tool result and the parent records it for audit.
const brokerPolicyDenied = "mcp_broker_policy_denied"

// scopeAuthorizer is one scope's effective authorization, computed at scope
// open from the child's own bundle intersected with the parent's snapshot.
type scopeAuthorizer struct {
	// tools maps a REGISTERED server name (what dispatch keys on) to the tool
	// names permitted on it. A nil value means "every tool this server
	// exports"; a non-nil empty value means "none".
	tools map[string][]string
	// servers is the exhaustive set of registered server names this scope may
	// dispatch against. A call naming anything else is refused even if the
	// underlying client would have routed it.
	servers map[string]bool
}

// permits reports whether server.tool may run in this scope.
func (a *scopeAuthorizer) permits(server, tool string) bool {
	if a == nil {
		return true
	}
	if !a.servers[server] {
		return false
	}
	list, ok := a.tools[server]
	if !ok || list == nil {
		return true
	}
	return slices.Contains(list, tool)
}

// denyPolicy renders the refusal a denied call returns. It names only public
// configuration identifiers, so it is safe to cross the credential boundary.
func denyPolicy(server, tool string) string {
	return fmt.Sprintf(
		"%s: the credential owner does not permit tool %q on server %q for this scope. "+
			"Check the bundle's tool_allowlist for that server and the run's selection.",
		brokerPolicyDenied, tool, server)
}

// filterTools drops every catalog entry this scope may not call, so a tool the
// child would refuse is never advertised to the parent in the first place. The
// parent's roster and the model's tool list then agree with what dispatch will
// actually allow.
func (a *scopeAuthorizer) filterTools(tools []mcpbroker.ToolDescriptor) []mcpbroker.ToolDescriptor {
	if a == nil {
		return tools
	}
	out := make([]mcpbroker.ToolDescriptor, 0, len(tools))
	for _, tool := range tools {
		if a.permits(tool.Server, tool.Tool) {
			out = append(out, tool)
		}
	}
	return out
}

// newScopeAuthorizer computes a scope's effective gates.
//
// bundleAllow is the child's own manifest allowlist keyed by BUNDLE server
// name; policy is the parent's optional narrowing. extraServers names servers
// bound into the scope that are not part of the selection — today only the
// synthetic inline http-tools server, which has no manifest entry to gate on.
func newScopeAuthorizer(
	selection agentcore.MCPSelection,
	bundleAllow map[string][]string,
	policy *mcpbroker.ScopePolicy,
	extraServers ...string,
) *scopeAuthorizer {
	authz := &scopeAuthorizer{
		tools:   make(map[string][]string, len(selection)+len(extraServers)),
		servers: make(map[string]bool, len(selection)+len(extraServers)),
	}
	var parentTools map[string][]string
	if policy != nil {
		parentTools = policy.Tools
	}
	for _, choice := range selection {
		registered := agentcore.RegisteredMCPName(choice.Server, choice.Account)
		authz.servers[registered] = true
		authz.tools[registered] = narrowToolAllowlist(bundleAllow[choice.Server], parentTools[choice.Server])
	}
	for _, name := range extraServers {
		if name == "" {
			continue
		}
		authz.servers[name] = true
		authz.tools[name] = narrowToolAllowlist(bundleAllow[name], parentTools[name])
	}
	return authz
}

// newRemoteScopeAuthorizer gates a per-user hosted-MCP scope. Remote servers
// have no bundle manifest, so there is no child-side allowlist to be
// authoritative about: the scope is bound to exactly the servers the child's
// own resolver connected, and the parent may narrow the tools on them.
func newRemoteScopeAuthorizer(servers []string, policy *mcpbroker.ScopePolicy) *scopeAuthorizer {
	authz := &scopeAuthorizer{
		tools:   make(map[string][]string, len(servers)),
		servers: make(map[string]bool, len(servers)),
	}
	var parentTools map[string][]string
	if policy != nil {
		parentTools = policy.Tools
	}
	for _, name := range servers {
		authz.servers[name] = true
		authz.tools[name] = narrowToolAllowlist(nil, parentTools[name])
	}
	return authz
}

// narrowToolAllowlist intersects the child's authoritative bundle allowlist
// with the parent's snapshot. Both sides use the in-process Gate-2 convention —
// an empty list means "this side adds no restriction" — so the result is nil
// (allow every exported tool) only when NEITHER side restricts.
func narrowToolAllowlist(bundle, parent []string) []string {
	switch {
	case len(bundle) == 0 && len(parent) == 0:
		return nil
	case len(bundle) == 0:
		return append([]string(nil), parent...)
	case len(parent) == 0:
		return append([]string(nil), bundle...)
	}
	out := make([]string, 0, min(len(bundle), len(parent)))
	for _, tool := range bundle {
		if slices.Contains(parent, tool) && !slices.Contains(out, tool) {
			out = append(out, tool)
		}
	}
	// Non-nil even when the intersection is empty: that is a real "deny every
	// tool on this server" answer, not "no restriction".
	return out
}

// scopeCredentialAllowlist converts the parent's Gate-3 snapshot into the
// agentcore type, so the child gates with the SAME helper the in-process loop
// uses rather than a second, subtly different implementation. A policy that
// does not restrict credentials returns nil ("inherit global"), which
// GateMCPBrokerWithAllowlist passes through untouched.
func scopeCredentialAllowlist(policy *mcpbroker.ScopePolicy) agentcore.CredentialAllowlist {
	if policy == nil || !policy.RestrictCredentials {
		return nil
	}
	allow := make(agentcore.CredentialAllowlist, 0, len(policy.Credentials))
	for _, pair := range policy.Credentials {
		allow = append(allow, agentcore.CredentialAllowlistEntry{Server: pair.Server, Account: pair.Account})
	}
	return allow
}

// validateScopeSelection refuses a selection naming a server this child does
// not have enabled. Binding would have failed anyway for a stdio server, but
// failing here makes the refusal an authorization decision with a public,
// deterministic message instead of a spawn-time accident — and it closes the
// case where a best-effort server is silently skipped, leaving a scope that
// looks bound but is not.
func validateScopeSelection(selection agentcore.MCPSelection, enabled map[string]bool) error {
	var unknown []string
	seen := map[string]bool{}
	for _, choice := range selection {
		if choice.Server == "" {
			return fmt.Errorf("mcp-broker: scope selection contains an empty server name")
		}
		if !enabled[choice.Server] && !seen[choice.Server] {
			seen[choice.Server] = true
			unknown = append(unknown, choice.Server)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("mcp-broker: scope selection names server(s) this credential owner does not have enabled: %v", unknown)
}
