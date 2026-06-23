// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import "net/http"

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

// GetMCPServers returns the Optional-MCP catalog (read-only; never secret
// values), mirroring chat's GET /mcp-servers so the scheduled-task picker works.
// Response: { "servers": [ {name, display_name, description, tool_count, enabled, accounts[]} ] }
func (h *Handlers) GetMCPServers(w http.ResponseWriter, _ *http.Request) {
	servers := []MCPServerCatalogEntry{}
	if h.mcpCatalog != nil {
		servers = h.mcpCatalog()
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
