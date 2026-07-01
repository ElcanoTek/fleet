package agent

import (
	"context"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// mcpGates returns the (allowlist, optional-set) gating pair under the gating
// RLock, so a caller sees a consistent snapshot even while ReloadMCPServers
// (#218) swaps them. Reload assigns fresh maps, so the returned refs are safe to
// use lock-free.
func (m *Manager) mcpGates() (mcpAllowlist, mcpOptionalSet) {
	m.mcpGatingMu.RLock()
	defer m.mcpGatingMu.RUnlock()
	return m.allowlist, m.optionalServers
}

// mcpRosterSnapshot returns the (optional-set, tool-roster) pair under the
// gating RLock. Same snapshot contract as mcpGates.
func (m *Manager) mcpRosterSnapshot() (mcpOptionalSet, []string) {
	m.mcpGatingMu.RLock()
	defer m.mcpGatingMu.RUnlock()
	return m.optionalServers, m.mcpToolRoster
}

// specsToServerDefs converts the enabled entries of a resolved spec map into the
// transport-agnostic mcp.ServerDef list the client's Reload diffs against. It
// mirrors BuildMCPClient's stdio/HTTP dispatch. Disabled specs are dropped so a
// server toggled off in the manifest is removed on reload. The synthetic inline
// http-tools server has no spec and is left untouched by Reload.
func specsToServerDefs(specs map[string]MCPServerSpec) []mcp.ServerDef {
	defs := make([]mcp.ServerDef, 0, len(specs))
	for name, spec := range specs {
		if !spec.Enabled {
			continue
		}
		switch {
		case spec.URL != "":
			defs = append(defs, mcp.ServerDef{Name: name, URL: spec.URL, Headers: spec.Headers, TLS: spec.TLS})
		case spec.Command != "":
			defs = append(defs, mcp.ServerDef{Name: name, Command: spec.Command, Args: spec.Args, Env: spec.Env, Dir: spec.Dir})
		}
	}
	return defs
}

// ReloadMCPServers hot-reloads the MCP catalog from a freshly re-read spec map
// (#218): it diffs newSpecs against the live client and applies the minimum set
// of server add/remove/restart mutations WITHOUT tearing down unchanged servers,
// then atomically swaps the spec-derived gating (allowlist / optional-set /
// tool-roster / picker metadata) so the next turn sees the new catalog. Existing
// in-flight turns and scheduled runs finish on their current roster; the change
// takes effect on the NEXT interactive turn (which rebuilds its tool set) and
// the next scheduled run. The synthetic inline http-tools catalog (#261) is not
// affected. Returns a summary of what changed.
//
// Only operator-configured (bundle-manifest) servers are managed here; per-user
// remote-MCP overlays (#443/#449) are built fresh per turn and are untouched.
func (m *Manager) ReloadMCPServers(ctx context.Context, newSpecs map[string]MCPServerSpec) (*mcp.ReloadSummary, error) {
	if m.mcpClient == nil {
		return &mcp.ReloadSummary{}, nil
	}
	// Serialize the whole reload so the client reload + gating swap land as a
	// unit; two overlapping reloads must not interleave into a client/gating
	// mismatch.
	m.mcpReloadMu.Lock()
	defer m.mcpReloadMu.Unlock()

	// Build the spec-derived gates (fresh maps, never mutating a published one).
	allow := mcpAllowlist{}
	optional := mcpOptionalSet{}
	for name, spec := range newSpecs {
		if !spec.Enabled {
			continue
		}
		if len(spec.ToolAllowlist) > 0 {
			allow[name] = spec.ToolAllowlist
		}
		if spec.Optional {
			optional[name] = true
		}
	}

	// Publish the allowlist + optional-set BEFORE the client gains new servers, so
	// a newly-added OPTIONAL server is gated before its tools go live — otherwise
	// a turn in the window would see the new tools as always-on (the #433 128-tool
	// ceiling regression). Capture the old gates to revert if the client reload
	// fails (its swap is all-or-nothing, so on error the client is unchanged and
	// the gates must match).
	m.mcpGatingMu.Lock()
	prevAllow, prevOptional := m.allowlist, m.optionalServers
	m.allowlist = allow
	m.optionalServers = optional
	m.mcpGatingMu.Unlock()

	summary, err := m.mcpClient.Reload(ctx, specsToServerDefs(newSpecs))
	if err != nil {
		m.mcpGatingMu.Lock()
		m.allowlist = prevAllow
		m.optionalServers = prevOptional
		m.mcpGatingMu.Unlock()
		return summary, err
	}

	// Refresh the roster (prefixed tool-name list for the system prompt) and the
	// picker metadata — both read the now-reloaded client — and swap them in.
	roster := m.computeMCPToolRoster(allow)
	metadata := m.buildOptionalServerMetadata(newSpecs)
	m.mcpGatingMu.Lock()
	m.mcpToolRoster = roster
	m.optionalServerMetadata = metadata
	m.mcpGatingMu.Unlock()

	return summary, nil
}
