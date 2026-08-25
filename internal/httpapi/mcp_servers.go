// MCP-server catalog surfaces: the startup Optional-server catalog
// (GET /mcp-servers), the opt-in whitelist shared with the chat path, and the
// per-conversation Tools-picker routes (GET/POST
// /conversations/{id}/mcp-servers). Split out of server.go (#1127). The
// trust-labeled directory endpoint lives in mcp_catalog.go.

package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
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
			for _, g := range agent.GroupRemoteMCPSeats(conns) {
				servers = append(servers, remoteCatalogEntry(g, true, ""))
			}
		} else {
			log.Printf("mcp catalog: remote server lookup failed for %s: %v", user, err)
		}
	}
	writeJSON(w, map[string]any{"servers": servers})
}

// remoteCatalogEntry renders one hosted connection NAME for the Tools picker
// (#988): its labeled seats, the default seat, and — on the per-conversation
// route — this conversation's override. Seats carry labels only, never
// credentials.
func remoteCatalogEntry(g agent.RemoteMCPSeatGroup, enabled bool, account string) map[string]any {
	return map[string]any{
		"name":               g.Name,
		"display_name":       g.Name,
		"description":        "Remote MCP server you connected (" + g.URL + ").",
		"tools":              []string{},
		"tool_count":         0,
		"enabled":            enabled,
		"beta":               false,
		"enabled_by_default": true,
		"accounts":           g.Accounts,
		"default_account":    g.DefaultAccount,
		"account":            account,
		"remote":             true,
	}
}

// remoteSeatCatalog is the seat universe a conversation may pin for hosted
// connections: every seat the user owns or was shared, regardless of status
// or prefs — mirroring optionalServerWhitelist, a needs_reauth seat must stay
// pinnable so a full-set POST during re-authorization does not silently drop
// the pin. Keyed by lowercased connection name → set of labels (plus "" for
// an unlabeled seat).
func (s *Server) remoteSeatCatalog(ctx context.Context, user string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if s.remoteMCP == nil || !s.remoteMCP.Enabled() || user == "" {
		return out
	}
	add := func(srv store.RemoteMCPServer) {
		key := strings.ToLower(srv.Name)
		if out[key] == nil {
			out[key] = map[string]bool{}
		}
		out[key][srv.Account] = true
	}
	if own, err := s.remoteMCP.ListServers(ctx, user); err == nil {
		for _, srv := range own {
			add(srv)
		}
	} else {
		log.Printf("mcp seats: remote server lookup failed for %s: %v", user, err)
	}
	if shared, err := s.remoteMCP.SharedWithMe(ctx, user); err == nil {
		for _, srv := range shared {
			add(srv)
		}
	} else {
		log.Printf("mcp seats: shared remote server lookup failed for %s: %v", user, err)
	}
	return out
}

// cleanMCPAccounts validates a per-conversation seat map (#988): each key must
// be an optional connector the user can pick (bundled catalog or their hosted
// connections, matched lowercased like the opt-in list) and each value a seat
// that connector actually has — a bundled connector's provisioned account, or a
// hosted connection's labeled seat. Empty values are dropped (they mean "the
// default"). Unlike the opt-in list, an unknown seat is an ERROR, not a silent
// drop: a user who believes they picked "personal" must not quietly run as
// "work".
func (s *Server) cleanMCPAccounts(ctx context.Context, user string, accounts map[string]string) (map[string]string, error) {
	clean := map[string]string{}
	if len(accounts) == 0 {
		return clean, nil
	}
	bundled := map[string]agent.OptionalServerInfo{}
	for _, info := range s.agent.MCPServerCatalog() {
		bundled[strings.ToLower(info.Name)] = info
	}
	var remote map[string]map[string]bool
	for rawName, rawAcct := range accounts {
		name := strings.ToLower(strings.TrimSpace(rawName))
		acct := strings.TrimSpace(rawAcct)
		if name == "" || acct == "" {
			continue
		}
		if info, ok := bundled[name]; ok {
			if !slices.Contains(info.Accounts, acct) {
				return nil, fmt.Errorf("unknown account %q for connector %q", rawAcct, rawName)
			}
			clean[info.Name] = acct
			continue
		}
		if remote == nil {
			remote = s.remoteSeatCatalog(ctx, user)
		}
		seats, ok := remote[name]
		if !ok {
			return nil, fmt.Errorf("unknown connector %q", rawName)
		}
		label, err := store.CanonicalRemoteMCPAccount(acct)
		if err != nil {
			return nil, err
		}
		if label == "" || !seats[label] {
			return nil, fmt.Errorf("unknown account %q for connection %q", rawAcct, rawName)
		}
		clean[name] = label
	}
	return clean, nil
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
			// Seat pick (#988): provisioned seats + this conversation's
			// override ("" = the user's connections-page default).
			"accounts": info.Accounts,
			"account":  conv.MCPAccounts[info.Name],
		})
	}
	// Per-user remote (hosted) MCP servers (#443): merge the caller's
	// connected servers so the per-conversation Tools picker can toggle
	// them — mirroring the startup catalog (listMCPServerCatalog), which
	// already lists them as toggleable. Without this merge an existing
	// conversation had no way to enable a remote server at all. Enabled
	// state is looked up lowercased: the persisted opt-in list is
	// canonical lowercase while remote names keep the case the user
	// typed. One entry per connection NAME with its seats (#988). Best-effort
	// — a lookup error never breaks the catalog.
	if s.remoteMCP != nil && s.remoteMCP.Enabled() {
		if conns, err := s.remoteMCP.ConnectedServersForUser(r.Context(), user); err == nil {
			for _, g := range agent.GroupRemoteMCPSeats(conns) {
				servers = append(servers, remoteCatalogEntry(g, enabled[strings.ToLower(g.Name)], conv.MCPAccounts[strings.ToLower(g.Name)]))
			}
		} else {
			log.Printf("mcp catalog: remote server lookup failed for %s: %v", user, err)
		}
	}
	writeJSON(w, map[string]any{"servers": servers})
}

// handleConversationMCPServersSet serves POST /conversations/{id}/mcp-servers.
//
// Body: { "enabled_optional": ["gamma", ...], "accounts": {"gamma": "work"} }
// — both full sets, replacing the previous opt-in list and the previous seat
// overrides (#988). Unknown / non-optional server names are dropped
// silently; the server's catalog is the authoritative whitelist. An unknown
// SEAT, by contrast, is a 400: silently running as a different account is
// the failure this feature exists to prevent. "accounts" omitted = clear
// every override (a client that never sends it keeps pre-#988 behaviour:
// the user's defaults).
func (s *Server) handleConversationMCPServersSet(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		EnabledOptional []string          `json:"enabled_optional"`
		Accounts        map[string]string `json:"accounts"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	accounts, err := s.cleanMCPAccounts(r.Context(), user, req.Accounts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	if err := s.store.SetConversationMCPAccounts(r.Context(), user, id, accounts); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"enabled_optional": clean, "accounts": accounts})
}
