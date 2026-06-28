package agentcore

import (
	"context"
	"fmt"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// Per-task credential allowlist (#184): which (server, account) MCP pairs a run
// is permitted to call. This is Gate-3 — least-privilege scoping layered on top
// of Gate-1 (server opt-in) and Gate-2 (per-server tool allowlist).
//
// Enforcement lives at the MCPBroker seam (credentialGatedBroker), NOT at tool
// registration: the broker is the SINGLE seam every MCP call routes through —
// the in-process mcpTool and any out-of-process broker alike (issue #167).
// Gating it there means every caller enforces the allowlist identically (the
// out-of-process broker is the real credential boundary), rather than only the
// in-process tool-build path. A denied call never reaches the credentialed
// client; it returns a governance message as a tool-level error (isError=true,
// surfaced to the model as a tool result, not a transport failure) which the
// caller records through the Policy seam for the audit trail.
//
// The type is mirrored in internal/sched/models (the storage/JSON shape) and
// converted at the scheduled-driver boundary. Only the scheduled driver sets it
// today; the interactive driver leaves it nil.

// CredentialAllowlistEntry names one permitted (server, account) pair.
// Account=="" means the default/shared seat only.
type CredentialAllowlistEntry struct {
	Server  string `json:"server"`
	Account string `json:"account,omitempty"`
}

// CredentialAllowlist is the per-run list of permitted (server, account) pairs.
//
//   - nil          → no restriction (inherit global; the prior behaviour).
//   - non-nil empty → deny ALL MCP calls.
//   - populated     → only the listed pairs are permitted.
type CredentialAllowlist []CredentialAllowlistEntry

// Permits reports whether the (server, account) pair is allowed. Account names
// are canonicalized on both sides so `client-a` and `client_a` resolve to one
// seat (matching the env-suffix folding the credential store uses).
func (al CredentialAllowlist) Permits(server, account string) bool {
	if al == nil {
		return true // nil = global inherit
	}
	account = creds.CanonicalAccount(account)
	for _, e := range al {
		if e.Server == server && creds.CanonicalAccount(e.Account) == account {
			return true
		}
	}
	return false
}

// permittedRegisteredNames projects the allowlist onto the set of REGISTERED MCP
// server names a run may dispatch against — `server` for a default-seat entry,
// `server_account` (canonical account) for a variant — using the EXACT formula
// BindMCPSelection registers under. The broker dispatches by registered server
// name, so the Gate-3 check is a plain membership test, with no fragile
// reparsing of names that may themselves contain underscores. Returns nil for a
// nil allowlist (caller must treat nil as "inherit global", not "deny all").
func (al CredentialAllowlist) permittedRegisteredNames() map[string]bool {
	if al == nil {
		return nil
	}
	out := make(map[string]bool, len(al))
	for _, e := range al {
		if e.Server == "" {
			continue
		}
		name := e.Server
		if acct := creds.CanonicalAccount(e.Account); acct != "" {
			name = e.Server + "_" + acct
		}
		out[name] = true
	}
	return out
}

// credentialGatedBroker wraps an MCPBroker and denies any call whose registered
// server name is not on the task's allowlist (Gate-3) before it reaches the
// credentialed client.
type credentialGatedBroker struct {
	inner     MCPBroker
	permitted map[string]bool
}

// GateMCPBrokerWithAllowlist wraps broker with Gate-3 enforcement for the given
// allowlist. A nil allowlist (inherit global) returns the broker unchanged, so
// there is zero overhead and zero behaviour change on the default path. Exported
// so an out-of-process broker (#167) can gate its broker the same way the
// in-process loop gates its own.
func GateMCPBrokerWithAllowlist(broker MCPBroker, al CredentialAllowlist) MCPBroker {
	permitted := al.permittedRegisteredNames()
	if permitted == nil {
		return broker
	}
	return &credentialGatedBroker{inner: broker, permitted: permitted}
}

func (b *credentialGatedBroker) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	if !b.permitted[server] {
		// Tool-level error (isError=true), NOT a transport error: the model sees a
		// governance denial as a tool result and the caller records it for audit.
		// The "credential_allowlist_denied" marker lands in the audit text.
		return fmt.Sprintf(
			"credential_allowlist_denied: server %q is not permitted for this task. "+
				"Contact the operator to update the task's credential_allowlist.", server), true, nil
	}
	return b.inner.CallMCP(ctx, server, tool, args)
}
