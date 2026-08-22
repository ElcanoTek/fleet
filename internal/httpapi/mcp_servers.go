// MCP-server catalog surfaces: the startup Optional-server catalog
// (GET /mcp-servers), the opt-in whitelist shared with the chat path, and the
// per-conversation Tools-picker routes (GET/POST
// /conversations/{id}/mcp-servers). Split out of server.go (#1127). The
// trust-labeled directory endpoint lives in mcp_catalog.go.

package httpapi

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// ── /mcp-servers ───────────────────────────────────────────────────────────

// listMCPServerCatalog returns the Optional MCP server catalog without any
// per-conversation opt-in state. The frontend calls this on startup so the
// Tools picker can render for brand-new chats (before a conversation row
// exists). `enabled` reflects each server's EnabledByDefault (so default-on
// connectors like gamma start toggled on for a fresh chat); per-conversation
// state is fetched from /conversations/{id}/mcp-servers once a conversation
// is open and overrides this seed.
func (s *Server) listMCPServerCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	catalog := s.agent.MCPServerCatalog()
	// Availability layer (unified connector UX): the user's connections-page
	// prefs decide which bundled connectors this pre-conversation picker offers
	// and which start enabled. Best-effort — a prefs read failure falls back to
	// operator defaults.
	prefs, perr := s.store.ListConnectorPrefs(r.Context(), userFromCtx(r.Context()))
	if perr != nil {
		prefs = nil
	}
	servers := make([]map[string]any, 0, len(catalog))
	for _, info := range catalog {
		avail, defaultOn, _ := bundledPrefFor(prefs, info)
		if !avail {
			continue
		}
		servers = append(servers, map[string]any{
			"name":         info.Name,
			"display_name": info.DisplayName,
			"description":  info.Description,
			"tools":        info.Tools,
			"tool_count":   info.ToolCount,
			"enabled":      defaultOn,
			"beta":         info.Beta,
			// Separate group so adding this longer key doesn't re-align
			// the block above.
			"enabled_by_default": info.EnabledByDefault,
			// Manifest-declared external data this connector touches.
			"data_sources": info.DataSources,
			// Provisioned credential-account seat names (never secret values).
			"accounts": info.Accounts,
		})
	}
	// Per-user remote (hosted) MCP servers (#443): merge the caller's connected
	// servers into the Optional-server catalog so they show up in the Tools picker
	// as toggleable, default-on entries (gated per conversation like a bundle
	// Optional server). Best-effort — a lookup error never breaks the catalog.
	if s.remoteMCP != nil && s.remoteMCP.Enabled() {
		user := userFromCtx(r.Context())
		if conns, err := s.remoteMCP.ConnectedServersForUser(r.Context(), user); err == nil {
			for _, c := range conns {
				servers = append(servers, map[string]any{
					"name":               c.Name,
					"display_name":       c.Name,
					"description":        "Remote MCP server you connected (" + c.URL + ").",
					"tools":              []string{},
					"tool_count":         0,
					"enabled":            true,
					"beta":               false,
					"enabled_by_default": true,
					"accounts":           []string{},
					"remote":             true,
				})
			}
		} else {
			log.Printf("mcp catalog: remote server lookup failed for %s: %v", user, err)
		}
	}
	writeJSON(w, map[string]any{"servers": servers})
}

// optionalServerWhitelist returns the set of server names a conversation may
// opt into, keyed lowercase (the canonical form of the persisted opt-in
// list): the bundle Optional catalog (frozen at boot) merged with the remote
// (hosted) MCP servers the caller can use — their own rows (#443) plus rows
// shared with them (#443 follow-up). Without the remote legs, a usable
// server's name is silently dropped by the opt-in intersection, so it can
// never reach conv.OptionalMCPServersEnabled — and the overlay gate in
// RunTurn then skips it on every turn, making the connection unusable in
// chat. Remote rows are included regardless of status or connector prefs: a
// needs_reauth or prefs-disabled server must stay valid so a full-set POST
// from the Tools picker doesn't silently drop its enablement while the user
// re-authorizes or re-enables it (the run-time filter in
// ConnectedServersForUser still governs what actually reaches a turn).
// Best-effort on the remote legs — a lookup failure degrades to whatever did
// resolve rather than failing the caller's request.
func (s *Server) optionalServerWhitelist(ctx context.Context, user string) map[string]bool {
	catalog := s.agent.MCPServerCatalog()
	valid := make(map[string]bool, len(catalog))
	for _, info := range catalog {
		valid[strings.ToLower(info.Name)] = true
	}
	if s.remoteMCP != nil && s.remoteMCP.Enabled() && user != "" {
		if servers, err := s.remoteMCP.ListServers(ctx, user); err == nil {
			for _, srv := range servers {
				valid[strings.ToLower(srv.Name)] = true
			}
		} else {
			log.Printf("mcp opt-in: remote server lookup failed for %s: %v", user, err)
		}
		if shared, err := s.remoteMCP.SharedWithMe(ctx, user); err == nil {
			for _, srv := range shared {
				valid[strings.ToLower(srv.Name)] = true
			}
		} else {
			log.Printf("mcp opt-in: shared remote server lookup failed for %s: %v", user, err)
		}
	}
	return valid
}

// handleConversationMCPServersGet serves GET /conversations/{id}/mcp-servers.
//
// Per-conversation MCP-server catalog. Response shape:
//
//	{ "servers": [{ name, description, tools: [...], tool_count,
//	                enabled }, ...] }
//
// `enabled` is true when the conversation currently opted this
// server in. Non-optional servers are NOT listed — the UI only
// renders the toggle row for Optional ones. Reads from
// Manager.MCPServerCatalog() (frozen at server startup) + the
// conversation's opt-in list (fresh from Postgres).
func (s *Server) handleConversationMCPServersGet(w http.ResponseWriter, r *http.Request, user, _ string, conv *store.Conversation) {
	enabled := make(map[string]bool, len(conv.OptionalMCPServersEnabled))
	for _, n := range conv.OptionalMCPServersEnabled {
		enabled[n] = true
	}
	// Availability layer: a connector the user disabled on the connections
	// page never appears in the conversation picker. Best-effort — a prefs
	// read failure falls back to the full catalog rather than failing the
	// settings read.
	prefs, perr := s.store.ListConnectorPrefs(r.Context(), user)
	if perr != nil {
		prefs = nil
	}
	servers := make([]map[string]any, 0, len(s.agent.MCPServerCatalog()))
	for _, info := range s.agent.MCPServerCatalog() {
		if avail, _, _ := bundledPrefFor(prefs, info); !avail && !enabled[info.Name] {
			// Hidden unless the conversation already opted it in before the
			// user disabled it (then it stays visible so it can be turned off).
			continue
		}
		servers = append(servers, map[string]any{
			"name":         info.Name,
			"display_name": info.DisplayName,
			"description":  info.Description,
			"tools":        info.Tools,
			"tool_count":   info.ToolCount,
			"enabled":      enabled[info.Name],
			"beta":         info.Beta,
			// Separate group so adding this longer key doesn't re-align
			// the block above.
			"enabled_by_default": info.EnabledByDefault,
		})
	}
	// Per-user remote (hosted) MCP servers (#443): merge the caller's
	// connected servers so the per-conversation Tools picker can toggle
	// them — mirroring the startup catalog (listMCPServerCatalog), which
	// already lists them as toggleable. Without this merge an existing
	// conversation had no way to enable a remote server at all. Enabled
	// state is looked up lowercased: the persisted opt-in list is
	// canonical lowercase while remote names keep the case the user
	// typed. Best-effort — a lookup error never breaks the catalog.
	if s.remoteMCP != nil && s.remoteMCP.Enabled() {
		if conns, err := s.remoteMCP.ConnectedServersForUser(r.Context(), user); err == nil {
			for _, c := range conns {
				servers = append(servers, map[string]any{
					"name":               c.Name,
					"display_name":       c.Name,
					"description":        "Remote MCP server you connected (" + c.URL + ").",
					"tools":              []string{},
					"tool_count":         0,
					"enabled":            enabled[strings.ToLower(c.Name)],
					"beta":               false,
					"enabled_by_default": true,
					"remote":             true,
				})
			}
		} else {
			log.Printf("mcp catalog: remote server lookup failed for %s: %v", user, err)
		}
	}
	writeJSON(w, map[string]any{"servers": servers})
}

// handleConversationMCPServersSet serves POST /conversations/{id}/mcp-servers.
//
// Body: { "enabled_optional": ["gamma", ...] } — full set,
// replacing the previous opt-in list. Unknown / non-optional
// server names are dropped silently; the server's catalog is
// the authoritative whitelist.
func (s *Server) handleConversationMCPServersSet(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		EnabledOptional []string `json:"enabled_optional"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// Intersect with the known-optional whitelist (bundle catalog +
	// the caller's remote servers) so a bad frontend can't persist
	// garbage. Dedup + sort for a canonical payload.
	valid := s.optionalServerWhitelist(r.Context(), user)
	seen := make(map[string]bool, len(req.EnabledOptional))
	clean := make([]string, 0, len(req.EnabledOptional))
	for _, n := range req.EnabledOptional {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || !valid[n] || seen[n] {
			continue
		}
		seen[n] = true
		clean = append(clean, n)
	}
	sort.Strings(clean)
	if err := s.store.SetOptionalMCPServers(r.Context(), user, id, clean); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"enabled_optional": clean})
}
