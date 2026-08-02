package agent

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/creds"
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

func (m *Manager) mcpCatalogSnapshot() []mcp.ServerTool {
	m.mcpGatingMu.RLock()
	defer m.mcpGatingMu.RUnlock()
	if m.mcpCatalog != nil {
		return cloneMCPCatalog(m.mcpCatalog)
	}
	if m.mcpClient != nil {
		return m.mcpClient.GetAllTools()
	}
	return nil
}

func (m *Manager) scopeSelection(optionalEnabled []string, defaults map[string]string) agentcore.MCPSelection {
	enabledOptional := make(map[string]bool, len(optionalEnabled))
	for _, name := range optionalEnabled {
		if name = strings.TrimSpace(name); name != "" {
			enabledOptional[name] = true
		}
	}
	m.mcpGatingMu.RLock()
	selection := make(agentcore.MCPSelection, 0, len(m.enabledMCPServers))
	for name := range m.enabledMCPServers {
		if m.optionalServers[name] && !enabledOptional[name] {
			continue
		}
		selection = append(selection, agentcore.MCPChoice{Server: name, Account: defaults[name]})
	}
	m.mcpGatingMu.RUnlock()
	sort.Slice(selection, func(i, j int) bool { return selection[i].Server < selection[j].Server })
	return selection
}

func cloneMCPAccounts(src map[string][]string) map[string][]string {
	dst := make(map[string][]string, len(src))
	for server, accounts := range src {
		dst[server] = append([]string(nil), accounts...)
	}
	return dst
}

func cloneMCPCatalog(src []mcp.ServerTool) []mcp.ServerTool {
	if src == nil {
		return nil
	}
	dst := make([]mcp.ServerTool, len(src))
	copy(dst, src)
	return dst
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
			// Expand the reserved ${FLEET_WORKSPACE} token exactly as
			// BuildMCPClient's spawn did (same shared dir), so the reload diff
			// compares like with like — a raw token here against the live
			// server's expanded env would force a spurious restart on every
			// reload. Resolved lazily: token-free catalogs touch no disk.
			env := spec.Env
			if agentcore.EnvReferencesWorkspace(env) {
				env = agentcore.ExpandWorkspaceEnv(env, agentcore.SharedMCPWorkspaceDir())
			}
			// Mirror the shared spawn's ${FLEET_TASK_ID} handling (dropped —
			// no task identity) so the diff compares like with like.
			env = agentcore.ExpandTaskIDEnv(env, "")
			defs = append(defs, mcp.ServerDef{Name: name, Command: spec.Command, Args: spec.Args, Env: env, Dir: spec.Dir})
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
	enabledServers := make(map[string]bool)
	accounts := make(map[string][]string)
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
		enabledServers[name] = true
		accounts[name] = creds.AccountsFor(spec.AccountVars)
	}

	// Publish the allowlist + optional-set BEFORE the client gains new servers, so
	// a newly-added OPTIONAL server is gated before its tools go live — otherwise
	// a turn in the window would see the new tools as always-on (the #433 128-tool
	// ceiling regression). Capture the old gates to revert if the client reload
	// fails (its swap is all-or-nothing, so on error the client is unchanged and
	// the gates must match).
	m.mcpGatingMu.Lock()
	prevAllow, prevOptional, prevEnabled := m.allowlist, m.optionalServers, m.enabledMCPServers
	m.allowlist = allow
	m.optionalServers = optional
	m.enabledMCPServers = enabledServers
	m.mcpGatingMu.Unlock()

	// Bound the reload like boot bounds initial registration (60s): a stdio
	// server that starts but never answers `initialize` would otherwise block
	// under reloadMu forever with the ctx being app-lifetime — wedging every
	// future reload AND leaving the just-published new gates running against
	// the old catalog indefinitely. On timeout the client reload fails
	// all-or-nothing and the gate revert below restores consistency.
	reloadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	summary, err := m.mcpClient.Reload(reloadCtx, specsToServerDefs(newSpecs))
	if err != nil {
		m.mcpGatingMu.Lock()
		m.allowlist = prevAllow
		m.optionalServers = prevOptional
		m.enabledMCPServers = prevEnabled
		m.mcpGatingMu.Unlock()
		return summary, err
	}
	m.mcpGatingMu.Lock()
	m.mcpCatalog = m.mcpClient.GetAllTools()
	m.mcpAccounts = accounts
	m.mcpGatingMu.Unlock()

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
