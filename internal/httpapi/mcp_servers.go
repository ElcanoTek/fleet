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
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/store"
)

// ── /mcp-servers ───────────────────────────────────────────────────────────

// listMCPServerCatalog returns Optional controls plus locked always-on status
// without any per-conversation opt-in state. The frontend calls this on startup
// so the Tools picker can render for brand-new chats. For Optional rows,
// `enabled` reflects EnabledByDefault; for always-on rows it reflects live
// discovery availability. Per-conversation state later overrides only the
// Optional seed.
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
	// Always-on bundle servers are status rows, not opt-ins. Keep them in the
	// same response so chat can report the actual live discovery state beside
	// the optional controls without ever persisting them as conversation
	// choices.
	cats := s.connectorCategories()
	servers := s.alwaysOnMCPServerCatalogEntries()
	for _, info := range catalog {
		avail, defaultOn, seat := bundledPrefFor(prefs, info)
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
			// The user's Settings > Connections default seat ("" = the
			// deployment's own), so the picker names what "default" means.
			"default_account": seat,
			"category":        categoryFor(cats, info.Name),
			"status":          optionalConnectorStatus(info),
		})
	}
	servers = append(servers, s.needsSetupCatalogEntries(servers)...)
	// Per-user remote (hosted) MCP servers (#443): merge the caller's connected
	// servers into the Optional-server catalog so they show up in the Tools picker
	// as toggleable, default-on entries (gated per conversation like a bundle
	// Optional server). Best-effort — a lookup error never breaks the catalog.
	if s.remoteMCP != nil && s.remoteMCP.Enabled() {
		user := userFromCtx(r.Context())
		if conns, err := s.remoteMCP.ConnectedServersForUser(r.Context(), user); err == nil {
			for _, g := range agent.GroupRemoteMCPSeats(conns) {
				servers = append(servers, s.remoteCatalogEntry(g, true, ""))
			}
		} else {
			log.Printf("mcp catalog: remote server lookup failed for %s: %v", user, err)
		}
	}
	// What a chat turn can call: interactive_hidden_tools are not offered
	// (mcp_servers_hidden.go).
	servers = hideInteractiveHiddenTools(servers)
	writeJSON(w, map[string]any{"servers": servers, "categories": s.clientConfig.ConnectorCategories()})
}

// defaultEnabledOptionalMCPServers returns the canonical (lowercased, sorted,
// deduplicated) optional MCP server seed for a brand-new conversation created
// outside the pre-chat composer (e.g. POST /conversations from Settings →
// Personas "Start patrol" or Orchestrator "Discuss this run"): the bundled
// optional connectors that are available and default-on under the caller's
// connector preferences, plus the caller's connected remote MCP servers.
// Mirrors the `enabled && !always_on` seed that listMCPServerCatalog hands the
// pre-chat Tools picker.
func (s *Server) defaultEnabledOptionalMCPServers(ctx context.Context, user string) []string {
	var prefs map[string]store.ConnectorPref
	if s.store != nil && user != "" {
		if list, err := s.store.ListConnectorPrefs(ctx, user); err == nil {
			prefs = list
		}
	}
	var catalog []agent.OptionalServerInfo
	if s.agent != nil {
		catalog = chatVisibleConnectors(s.agent.MCPServerCatalog(), agentcore.InteractiveHiddenTools())
	}
	seen := make(map[string]bool, len(catalog))
	var out []string
	add := func(name string) {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	for _, info := range catalog {
		avail, defaultOn, _ := bundledPrefFor(prefs, info)
		if !avail || !defaultOn {
			continue
		}
		add(info.Name)
	}
	if s.remoteMCP != nil && s.remoteMCP.Enabled() && user != "" {
		if conns, err := s.remoteMCP.ConnectedServersForUser(ctx, user); err == nil {
			for _, g := range agent.GroupRemoteMCPSeats(conns) {
				add(g.Name)
			}
		} else {
			log.Printf("mcp defaults: remote server lookup failed for %s: %v", user, err)
		}
	}
	sort.Strings(out)
	return out
}

// Connection statuses for the Tools picker. "available" connectors get a
// switch (or the always-on lock); the other two never do. "needs_setup" is a
// manifest connector whose credentials are not set on this box, so it never
// launched; "unavailable" launched (or was meant to) but exposes no tools.
const (
	connectorStatusAvailable   = "available"
	connectorStatusNeedsSetup  = "needs_setup"
	connectorStatusUnavailable = "unavailable"
)

// optionalConnectorStatus is an Optional connector's picker status: a bundled
// server that started but exposed no tools has nothing to turn on. The native
// image-generation entry has no MCP tools by design and is always available.
func optionalConnectorStatus(info agent.OptionalServerInfo) string {
	if info.ToolCount == 0 && info.Name != agent.OptionalNativeImageGenName {
		return connectorStatusUnavailable
	}
	return connectorStatusAvailable
}

// connectorMetaProvider is the Manager's live connector metadata, refreshed by
// an MCP reload. Narrow like alwaysOnMCPServerCatalogProvider so transport
// fakes need not grow it.
type connectorMetaProvider interface {
	ConnectorMeta() []clientconfig.ConnectorMeta
}

// connectorMeta is every manifest connector's picker metadata: the Manager's
// reload-fresh snapshot when it has one, else the boot bundle's.
func (s *Server) connectorMeta() []clientconfig.ConnectorMeta {
	if p, ok := s.agent.(connectorMetaProvider); ok {
		if meta := p.ConnectorMeta(); meta != nil {
			return meta
		}
	}
	if s.clientConfig != nil {
		return s.clientConfig.ConnectorMeta()
	}
	return nil
}

// connectorCategories maps each bundled connector to its picker category
// (Misc Tools when untagged; untagged connectors are warned about at
// startup). Built once per request; look names up with categoryFor.
func (s *Server) connectorCategories() map[string]string {
	out := map[string]string{}
	for _, m := range s.connectorMeta() {
		out[m.Name] = m.Category
	}
	return out
}

// categoryFor is a connector's category from connectorCategories, else Misc
// Tools (the native image entry, or a server the bundle does not declare).
func categoryFor(cats map[string]string, name string) string {
	if c, ok := cats[name]; ok {
		return c
	}
	return clientconfig.MiscConnectorCategory
}

// needsSetupCatalogEntries lists the manifest connectors whose credentials
// are not set on this box. They never launched, so they have no switch and no
// opt-in; the picker shows them only in search (and the library always), with
// copy that depends on whether the viewer is an admin. Tool names are the
// manifest allowlist, the only ones known without launching the server.
// listed is the rows already built; a name there is never repeated.
func (s *Server) needsSetupCatalogEntries(listed []map[string]any) []map[string]any {
	seen := make(map[string]bool, len(listed))
	for _, e := range listed {
		if name, _ := e["name"].(string); name != "" {
			seen[name] = true
		}
	}
	var out []map[string]any
	for _, m := range s.connectorMeta() {
		// A connector that is running is configured, whatever the snapshot says.
		if m.Configured || seen[m.Name] {
			continue
		}
		tools := append([]string{}, m.Tools...)
		sort.Strings(tools)
		out = append(out, map[string]any{
			"name":         m.Name,
			"display_name": m.DisplayName,
			"description":  m.Description,
			"tools":        tools,
			"tool_count":   len(tools),
			"enabled":      false,
			"always_on":    !m.Optional,
			"category":     m.Category,
			"status":       connectorStatusNeedsSetup,
		})
	}
	return out
}

// remoteConnectionCategory groups a hosted connection under the directory
// category of the catalog entry it was added from (matched by endpoint), so
// it shows under the same heading the Connections directory uses. A URL the
// directory does not list, or a slug outside the picker's list, is Misc Tools.
func (s *Server) remoteConnectionCategory(url string) string {
	if s.clientConfig == nil {
		return clientconfig.MiscConnectorCategory
	}
	canon := func(u string) string {
		u = strings.TrimSpace(u)
		if c, err := mcpoauth.CanonicalResourceURI(u); err == nil {
			return c
		}
		return u
	}
	want := canon(url)
	for _, e := range s.clientConfig.RemoteMCPCatalog {
		match := canon(e.URL) == want
		for _, v := range e.URLVariants {
			match = match || canon(v.URL) == want
		}
		if match {
			return clientconfig.ConnectorCategoryOr(e.Category)
		}
	}
	return clientconfig.MiscConnectorCategory
}

// alwaysOnMCPServerCatalogProvider is deliberately narrower than turnEngine.
// Manager implements it, while existing transport fakes do not have to grow a
// method for a read-only UI capability they do not exercise.
type alwaysOnMCPServerCatalogProvider interface {
	AlwaysOnMCPServerCatalog() []agent.AlwaysOnServerInfo
}

// alwaysOnMCPServerCatalogEntries returns locked status rows for chat's Tools
// picker. Available comes from live MCP discovery; a manifest-enabled server
// that exposed no tools is returned unchecked/unavailable rather than being
// painted permanently on. Tool names are intentionally omitted: the public
// status snapshot exposes only the allowlisted count.
func (s *Server) alwaysOnMCPServerCatalogEntries() []map[string]any {
	provider, ok := s.agent.(alwaysOnMCPServerCatalogProvider)
	if !ok {
		return []map[string]any{}
	}
	catalog := provider.AlwaysOnMCPServerCatalog()
	cats := s.connectorCategories()
	hidden := agentcore.InteractiveHiddenTools()
	servers := make([]map[string]any, 0, len(catalog))
	for _, info := range catalog {
		status := connectorStatusAvailable
		if !info.Available {
			status = connectorStatusUnavailable
		}
		// What a chat turn can call (mcp_servers_hidden.go): the tools
		// interactive_hidden_tools keeps out of chat are not counted, and a
		// connector with none left is not listed. Names never leave the server.
		count := info.ToolCount - chatHiddenCount(hidden, info.Name, info.Tools)
		if info.ToolCount > 0 && count <= 0 {
			continue
		}
		servers = append(servers, map[string]any{
			"name":         info.Name,
			"display_name": info.DisplayName,
			"description":  info.Description,
			"tools":        []string{},
			"tool_count":   count,
			"enabled":      info.Available,
			"always_on":    true,
			"category":     categoryFor(cats, info.Name),
			"status":       status,
		})
	}
	return servers
}

// remoteCatalogEntry renders one hosted connection NAME for the Tools picker
// (#988): its labeled seats, the default seat, and — on the per-conversation
// route — this conversation's override. Seats carry labels only, never
// credentials.
func (s *Server) remoteCatalogEntry(g agent.RemoteMCPSeatGroup, enabled bool, account string) map[string]any {
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
		"category":           s.remoteConnectionCategory(g.URL),
		"status":             connectorStatusAvailable,
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
		log.Printf("mcp seats: remote server lookup failed for %s: %s", logSafe(user), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
	}
	if shared, err := s.remoteMCP.SharedWithMe(ctx, user); err == nil {
		for _, srv := range shared {
			add(srv)
		}
	} else {
		log.Printf("mcp seats: shared remote server lookup failed for %s: %s", logSafe(user), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
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
// `enabled` is true when the conversation currently opted an Optional server
// in. Non-optional servers are also listed as locked `always_on` status rows;
// their enabled state is live discovery availability, never conversation
// state. Reads from Manager's frozen catalogs + the conversation's fresh
// opt-in list from Postgres.
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
	cats := s.connectorCategories()
	servers := s.alwaysOnMCPServerCatalogEntries()
	for _, info := range s.agent.MCPServerCatalog() {
		avail, _, seat := bundledPrefFor(prefs, info)
		if !avail && !enabled[info.Name] {
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
			"accounts":        info.Accounts,
			"account":         conv.MCPAccounts[info.Name],
			"default_account": seat,
			"category":        categoryFor(cats, info.Name),
			"status":          optionalConnectorStatus(info),
		})
	}
	servers = append(servers, s.needsSetupCatalogEntries(servers)...)
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
				servers = append(servers, s.remoteCatalogEntry(g, enabled[strings.ToLower(g.Name)], conv.MCPAccounts[strings.ToLower(g.Name)]))
			}
		} else {
			log.Printf("mcp catalog: remote server lookup failed for %s: %v", user, err)
		}
	}
	// What a chat turn can call: interactive_hidden_tools are not offered
	// (mcp_servers_hidden.go).
	servers = hideInteractiveHiddenTools(servers)
	writeJSON(w, map[string]any{"servers": servers, "categories": s.clientConfig.ConnectorCategories()})
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
	unlock := s.connectorLocks.lock(user + "\x00" + id)
	defer unlock()

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
		writeConversationMutationError(w, err)
		return
	}
	if err := s.store.SetConversationMCPAccounts(r.Context(), user, id, accounts); err != nil {
		writeConversationMutationError(w, err)
		return
	}
	writeJSON(w, map[string]any{"enabled_optional": clean, "accounts": accounts})
}
