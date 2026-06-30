// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"net/http"
)

// MCPServerCatalogEntry is one Optional MCP server in the orchestrator's
// task-form picker. It mirrors chat's GET /mcp-servers response shape so the
// SAME <McpServerPicker> renders in both the chat toolbar and the scheduled-task
// form. It NEVER carries secret values — `Accounts` is the per-server credential
// seat catalog (names only, derived from the <VAR>_<ACCOUNT> env suffix scan).
type MCPServerCatalogEntry struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description"`
	ToolCount   int      `json:"tool_count"`
	Enabled     bool     `json:"enabled"`
	Accounts    []string `json:"accounts"`
	// Remote marks a per-user remote (hosted) MCP server the caller connected via
	// OAuth (#443/#466), as opposed to a bundle Optional server. Remote servers
	// carry no credential seats (their auth is the brokered per-user token) and are
	// auto-applied to ALL the owner's scheduled runs by the run overlay, so the UI
	// surfaces them as connected/auto-available rather than a per-task toggle.
	Remote bool `json:"remote,omitempty"`
}

// MCPAccountEntry is one (server, account) credential seat — names only, never
// secret values. The flat companion to the catalog for the credential-account
// admin table.
type MCPAccountEntry struct {
	Server  string `json:"server"`
	Account string `json:"account"`
}

// SetMCPCatalogProvider wires the read-only Optional-MCP catalog the orchestrator
// serves to its task-form picker + credential-account admin table. cmd/fleet
// builds it once from the loaded client bundle's catalog + the credential-account
// suffix scan (creds.AccountsFor) and injects it here, keeping the handlers
// package decoupled from clientconfig/creds. nil provider → empty catalog.
func (h *Handlers) SetMCPCatalogProvider(p func() []MCPServerCatalogEntry) {
	h.mcpCatalog = p
}

// SetRemoteMCPServersProvider wires the per-user remote (hosted) MCP lookup
// (#443) so GetMCPServers can surface the caller's OAuth-connected servers in
// the task-form picker (#466). cmd/fleet injects it from the remotemcp Service
// (ConnectedServersForUser), keyed by the caller's email; nil (feature off) →
// no remote entries. Keeps the handlers package decoupled from remotemcp/agent.
func (h *Handlers) SetRemoteMCPServersProvider(p func(ctx context.Context, email string) []MCPServerCatalogEntry) {
	h.remoteMCPServers = p
}

// GetMCPServers returns the Optional-MCP catalog (read-only; never secret
// values), mirroring chat's GET /mcp-servers so the scheduled-task picker works.
// It merges the caller's per-user remote (hosted) MCP servers (#443/#466) after
// the bundle catalog, resolving the caller's email from the authenticated
// principal — so a server connected in chat shows up here too. An admin-API-key
// principal carries no user, so it sees only the bundle catalog.
// Response: { "servers": [ {name, display_name, description, tool_count, enabled, accounts[], remote?} ] }
func (h *Handlers) GetMCPServers(w http.ResponseWriter, r *http.Request) {
	servers := []MCPServerCatalogEntry{}
	if h.mcpCatalog != nil {
		servers = h.mcpCatalog()
	}
	// Merge the caller's connected remote MCP servers. The orchestrator username
	// IS the chat-side email for the elcano-auth/header-trust tier (see
	// ownerEmailResolver), which is the key the remote-MCP tokens are stored under.
	// Best-effort: a missing user (admin key) or a provider that returns nothing
	// simply yields the bundle catalog unchanged.
	if h.remoteMCPServers != nil {
		if user := GetUserFromContext(r.Context()); user != nil && user.Username != "" {
			servers = append(servers, h.remoteMCPServers(r.Context(), user.Username)...)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// GetMCPAccounts returns the flat (server, account) seat catalog — names only,
// derived from the same provider as GetMCPServers.
// Response: { "accounts": [ {server, account} ] }
func (h *Handlers) GetMCPAccounts(w http.ResponseWriter, _ *http.Request) {
	accounts := []MCPAccountEntry{}
	if h.mcpCatalog != nil {
		for _, s := range h.mcpCatalog() {
			for _, a := range s.Accounts {
				accounts = append(accounts, MCPAccountEntry{Server: s.Name, Account: a})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}
