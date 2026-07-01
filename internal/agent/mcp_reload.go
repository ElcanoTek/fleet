package agent

import (
	"context"
	"time"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// mcpReloadDrainTimeout bounds how long a retired/restarted server waits for an
// in-flight tool call to finish before it is force-closed. A generous bound: a
// normal tool call is far shorter, and force-closing only affects the one server
// being retired.
const mcpReloadDrainTimeout = 30 * time.Second

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

	// 1. Reload the live client's servers first, so the roster + metadata we
	//    rebuild below reflect the new connection set.
	summary, err := m.mcpClient.Reload(ctx, specsToServerDefs(newSpecs), mcpReloadDrainTimeout)
	if err != nil {
		return summary, err
	}

	// 2. Rebuild the spec-derived gating from the new specs (fresh maps/slices,
	//    never a mutation of the published ones).
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
	roster := m.computeMCPToolRoster(allow)
	metadata := m.buildOptionalServerMetadata(newSpecs)

	// 3. Swap the gating atomically under the write lock.
	m.mcpGatingMu.Lock()
	m.allowlist = allow
	m.optionalServers = optional
	m.mcpToolRoster = roster
	m.optionalServerMetadata = metadata
	m.mcpGatingMu.Unlock()

	return summary, nil
}
