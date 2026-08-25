package agent

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Per-user remote (hosted) MCP overlay (#443), shared by BOTH the interactive
// and scheduled drivers. A user's OAuth-connected remote servers are wired into
// a run WITHOUT touching the long-lived shared MCP client (process-wide; mutating
// it would leak one user's bearer to another). The overlay may be an in-process
// compatibility client or a child-owned broker scope; a compositeBroker routes
// its server names to that per-run target while everything else falls through
// to the base broker.
// ApplyMCPOverlay sets the run's Deps to advertise the merged catalog and
// dispatch via that composite — the SAME governed loop, just a different broker
// seam impl. The overlay client is closed by the caller at run end.
//
// The agent package can't import internal/remotemcp (that package imports
// internal/store, which imports this package — a cycle), so the dependency is
// inverted: this file declares the small RemoteMCPResolver interface that
// remotemcp.Service satisfies, and cmd/fleet injects the concrete service.

// RemoteMCPConn is a connected remote MCP server, in the store-agnostic shape
// the overlay needs.
type RemoteMCPConn struct {
	ID   string
	Name string // connection name: the picker/opt-in key and the mcp_<name>_* prefix of the unlabeled seat
	URL  string
	// Account is this seat's public label under Name (#988); "" is the
	// unlabeled seat. The run registers the seat under
	// agentcore.RegisteredMCPName(Name, Account) — the bundle seat formula —
	// so a labeled seat's tools read mcp_<name>_<account>_*.
	Account string
	// Default marks the seat a run mounts for Name when nothing pins another.
	Default bool
	// Owner is the connection owner's email when this server was SHARED with
	// the running user (empty for the user's own servers). Used for audit
	// attribution: tool calls authenticate with the owner's token host-side.
	Owner string
	// AuthQuery is the query-parameter NAME the credential is sent under for
	// api_key connections that authenticate in the URL (Browserbase). The
	// transport attaches it per-request; the registered URL stays clean.
	AuthQuery string
	// AuthHeader is the header NAME the credential is sent under for api_key
	// connections (e.g. "X-API-Key"). Empty means the default OAuth/bearer
	// shape: "Authorization: Bearer <credential>".
	AuthHeader string
}

// RemoteMCPResolver supplies a user's connected remote servers and mints fresh
// bearer tokens for them. remotemcp.Service implements it.
type RemoteMCPResolver interface {
	// ConnectedServersForUser returns the user's servers currently in the
	// "connected" state (ready to use).
	ConnectedServersForUser(ctx context.Context, email string) ([]RemoteMCPConn, error)
	// AcquireTokenByID returns a valid bearer for the server, refreshing under a
	// lock if needed. A needs-reauth/expired connection returns an error so the
	// caller skips the server gracefully.
	AcquireTokenByID(ctx context.Context, email, serverID string) (string, error)
	// SafeHTTPClient is the SSRF-safe client used to dial user-supplied servers.
	SafeHTTPClient() *http.Client
}

// RemoteMCPSelection says which of a user's hosted connections a run mounts,
// and on which seat (#988). It carries public names and labels only.
type RemoteMCPSelection struct {
	// Filter restricts mounting to the names in Enabled (the interactive
	// per-conversation opt-in). False mounts every connected name — the
	// scheduled default — each on its default seat unless Accounts pins one.
	Filter  bool
	Enabled map[string]bool
	// Accounts pins a seat per connection name (name → label). An absent
	// key, or "" when !Exact, mounts the name's default seat.
	Accounts map[string]string
	// Exact makes every mounted name use Accounts[name] literally — "" is
	// then the UNLABELED seat, not the default. Approval re-execution uses it
	// to reopen the seat a card recorded even if the default has since moved.
	Exact bool
}

// wants reports whether sel mounts the connection name at all. The persisted
// opt-in list is canonically lowercase (the HTTP layer normalizes on write)
// while remote names keep the case the user typed, so check the lowercased
// form too — exact match first so any pre-existing list entry keeps working.
func (sel RemoteMCPSelection) wants(name string) bool {
	if !sel.Filter {
		return true
	}
	return sel.Enabled[name] || sel.Enabled[strings.ToLower(name)]
}

// pinned returns the seat label sel demands for name and whether it demands
// one at all (false = "the default seat").
func (sel RemoteMCPSelection) pinned(name string) (string, bool) {
	acct, ok := sel.Accounts[name]
	if !ok {
		acct, ok = sel.Accounts[strings.ToLower(name)]
	}
	if sel.Exact {
		return acct, true
	}
	if !ok || acct == "" {
		return "", false
	}
	return acct, true
}

// RemoteMCPAllConnected is the scheduled default: every connected name on its
// default seat.
var RemoteMCPAllConnected = RemoteMCPSelection{}

// RemoteMCPEnabledOnly builds the interactive selection: only the names the
// conversation opted into, on the seat accounts pins (or the default).
func RemoteMCPEnabledOnly(enabledNames []string, accounts map[string]string) RemoteMCPSelection {
	enabled := make(map[string]bool, len(enabledNames))
	for _, name := range enabledNames {
		if n := strings.TrimSpace(name); n != "" {
			enabled[n] = true
		}
	}
	return RemoteMCPSelection{Filter: true, Enabled: enabled, Accounts: accounts}
}

// selectRemoteSeats picks exactly one seat per wanted connection name. A
// pinned label that no connected seat carries yields the name in missing
// (public registered name) instead of falling back to another seat — a run
// must never silently transact as a different account. Order follows conns.
func selectRemoteSeats(conns []RemoteMCPConn, sel RemoteMCPSelection) (chosen []RemoteMCPConn, missing []string) {
	var order []string
	byName := map[string][]RemoteMCPConn{}
	for _, c := range conns {
		if _, seen := byName[c.Name]; !seen {
			order = append(order, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], c)
	}
	for _, name := range order {
		if !sel.wants(name) {
			continue
		}
		seats := byName[name]
		if acct, pin := sel.pinned(name); pin {
			found := false
			for _, c := range seats {
				if c.Account == acct {
					chosen = append(chosen, c)
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, agentcore.RegisteredMCPName(name, acct))
			}
			continue
		}
		chosen = append(chosen, defaultRemoteSeat(seats))
	}
	return chosen, missing
}

// defaultRemoteSeat is the seat a name mounts when nothing pins one: the one
// flagged default, else the unlabeled seat, else the first connected.
// (Legacy single-row connections are flagged default by the #988 migration,
// so they resolve exactly as before.)
func defaultRemoteSeat(seats []RemoteMCPConn) RemoteMCPConn {
	for _, c := range seats {
		if c.Default {
			return c
		}
	}
	for _, c := range seats {
		if c.Account == "" {
			return c
		}
	}
	return seats[0]
}

// RemoteMCPSeatGroup is one connection NAME with its seats, the shape the
// pickers (chat Tools picker, task modal) present: the user toggles/pins by
// name, then picks a seat (#988).
type RemoteMCPSeatGroup struct {
	Name string
	URL  string // the default seat's URL (display only)
	// Accounts lists the labeled seats (non-empty labels), sorted. The
	// unlabeled seat is not listed — it is reachable only as the default.
	Accounts []string
	// DefaultAccount is the label of the seat a run mounts when nothing pins
	// one ("" = the unlabeled seat).
	DefaultAccount string
	// Owner is non-empty when EVERY seat of this name was shared with the
	// user (display attribution); mixed groups report the user's own.
	Owner string
}

// GroupRemoteMCPSeats collapses per-seat connections into one group per name,
// preserving first-seen order. Callers that need per-seat rows (Settings →
// Connections) keep using the flat list.
func GroupRemoteMCPSeats(conns []RemoteMCPConn) []RemoteMCPSeatGroup {
	var order []string
	byName := map[string][]RemoteMCPConn{}
	for _, c := range conns {
		if _, seen := byName[c.Name]; !seen {
			order = append(order, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], c)
	}
	out := make([]RemoteMCPSeatGroup, 0, len(order))
	for _, name := range order {
		seats := byName[name]
		def := defaultRemoteSeat(seats)
		g := RemoteMCPSeatGroup{Name: name, URL: def.URL, DefaultAccount: def.Account, Accounts: []string{}, Owner: def.Owner}
		for _, c := range seats {
			if c.Account != "" {
				g.Accounts = append(g.Accounts, c.Account)
			}
			if c.Owner == "" {
				g.Owner = ""
			}
		}
		sort.Strings(g.Accounts)
		out = append(out, g)
	}
	return out
}

// RemoteMCPOverlayOpener binds one user's public remote-server selection to a
// per-run broker overlay. Implementations may keep the historical in-process
// client or open a scope in a credential-owning subprocess; callers see only
// public tool metadata and the agentcore call seam.
type RemoteMCPOverlayOpener func(ctx context.Context, email string, shadowed map[string]bool, sel RemoteMCPSelection) (*RemoteMCPOverlay, error)

// maxOverlayServers caps how many remote servers one user can inject into a
// single run, a guard against blowing the 128-tool ceiling (and against a
// pathological number of per-turn handshakes). Excess servers are skipped with a
// logged warning rather than silently dropped.
const maxOverlayServers = 8

// RemoteMCPOverlay is the per-run wiring for a user's remote servers. Client is
// retained for the in-process compatibility path; Broker and CloseScope allow
// the same overlay to be owned across a process boundary. The caller MUST close
// the overlay at run end.
type RemoteMCPOverlay struct {
	Client  *mcp.Client // per-run compatibility client
	Broker  agentcore.MCPBroker
	Catalog []mcp.ServerTool // the overlay servers' tools, merged into the run catalog
	Servers map[string]bool  // registration names handled by the overlay broker
	// Seats maps each registration name in Servers to the public
	// {connection name, account label} it mounted (#988), so approval staging
	// can record the exact seat a later approval must reopen.
	Seats map[string]agentcore.MCPChoice
	// CloseScope releases a broker-owned scope. It is called with a fresh,
	// bounded context so cancellation of the run cannot suppress cleanup.
	CloseScope func(context.Context) error
	// Skipped names servers that were selected but could not be wired this run —
	// today only because their token is unavailable (needs re-auth) or the server
	// failed to connect. Callers surface these to the owner (a needs-reauth server
	// silently doing nothing is a correctness trap, especially for headless runs).
	Skipped []string
}

// skippedNames is a nil-safe read of Skipped for error text.
func (o *RemoteMCPOverlay) skippedNames() []string {
	if o == nil {
		return nil
	}
	return o.Skipped
}

// Active reports whether the overlay actually registered any servers.
func (o *RemoteMCPOverlay) Active() bool {
	return o != nil && (o.Broker != nil || o.Client != nil) && len(o.Servers) > 0
}

// Validate checks the ownership contract an injected opener must satisfy. A
// broker-backed overlay always represents a per-run scope and therefore needs
// an explicit release function; selected routing names need a call target.
func (o *RemoteMCPOverlay) Validate() error {
	if o == nil {
		return nil
	}
	if o.Broker != nil && o.CloseScope == nil {
		return errors.New("remote MCP broker overlay has no close function")
	}
	if len(o.Servers) > 0 && o.Broker == nil && o.Client == nil {
		return errors.New("remote MCP overlay has routing names but no call broker")
	}
	return nil
}

const remoteMCPOverlayCloseTimeout = 5 * time.Second

// Close tears down the overlay's per-run client or broker scope (nil-safe). It
// deliberately uses a fresh bounded context: callers commonly defer it from a
// run whose context may already be cancelled.
func (o *RemoteMCPOverlay) Close() {
	if o == nil {
		return
	}
	if o.CloseScope != nil {
		ctx, cancel := context.WithTimeout(context.Background(), remoteMCPOverlayCloseTimeout)
		defer cancel()
		if err := o.CloseScope(ctx); err != nil {
			log.Printf("remote-mcp: close overlay scope: %v", err)
		}
	}
	if o.Client != nil {
		_ = o.Client.Close()
	}
}

// BuildRemoteMCPOverlay registers a user's connected remote servers onto a fresh
// per-run client. shadowed is the set of server names already provided by the
// base catalog — an overlay server colliding with one is skipped so a user can
// never shadow a built-in tool. sel says which connection names mount (the
// conversation's opt-in set, or every connected name for a scheduled run) and
// which seat each one uses (#988): exactly ONE seat per name is mounted,
// registered under agentcore.RegisteredMCPName(name, account), and a pinned
// seat that is not connected is reported in Skipped rather than replaced by
// another account. A server that fails to mint a token (needs re-auth) or
// connect is likewise recorded in Skipped (graceful degradation), never
// fatal. The returned overlay is non-nil whenever there are connected servers
// (so the caller can read Skipped even when none registered); its Active()
// reports whether any server is actually wired. The caller MUST Close it.
func BuildRemoteMCPOverlay(ctx context.Context, resolver RemoteMCPResolver, email string, shadowed map[string]bool, sel RemoteMCPSelection) (*RemoteMCPOverlay, error) {
	if resolver == nil || email == "" {
		return nil, nil
	}
	conns, err := resolver.ConnectedServersForUser(ctx, email)
	if err != nil {
		return nil, err
	}
	if len(conns) == 0 {
		return nil, nil
	}

	client := mcp.NewClient()
	httpClient := resolver.SafeHTTPClient()
	overlay := &RemoteMCPOverlay{Client: client, Servers: map[string]bool{}, Seats: map[string]agentcore.MCPChoice{}}
	chosen, missing := selectRemoteSeats(conns, sel)
	for _, name := range missing {
		// A pinned seat that is not connected: never fall back to another
		// account under the same name — surface it so the owner can connect
		// (or re-pin) it.
		log.Printf("remote-mcp: skipping %q for %s — the pinned seat is not connected", name, email)
		overlay.Skipped = append(overlay.Skipped, name)
	}
	registered := 0
	for _, conn := range chosen {
		regName := agentcore.RegisteredMCPName(conn.Name, conn.Account)
		if registered >= maxOverlayServers {
			log.Printf("remote-mcp: skipping %q and further servers for %s — overlay cap %d reached", regName, email, maxOverlayServers)
			break
		}
		if shadowed[conn.Name] || shadowed[regName] {
			log.Printf("remote-mcp: skipping remote server %q — name collides with a built-in server", regName)
			continue
		}
		if conn.Owner != "" {
			// Attribution for shared connections: the run belongs to email, but
			// tool calls authenticate with the OWNER's token host-side.
			log.Printf("remote-mcp: run for %s uses shared server %q owned by %s", email, regName, conn.Owner)
		}
		bearer, terr := resolver.AcquireTokenByID(ctx, email, conn.ID)
		if terr != nil {
			// needs-reauth / refresh failure: skip this server, keep the rest, and
			// record it so the caller can tell the owner.
			log.Printf("remote-mcp: skipping server %q for %s — token unavailable", regName, email)
			overlay.Skipped = append(overlay.Skipped, regName)
			continue
		}
		opts := mcp.HTTPServerOptions{HTTPClient: httpClient}
		if bearer != "" {
			switch {
			case conn.AuthQuery != "":
				// Query-authenticated vendor: attach the key in the transport so
				// the registered URL (and every log/error that embeds it) stays
				// credential-free.
				opts.HTTPClient = mcp.WithQueryParam(httpClient, conn.AuthQuery, bearer)
			case conn.AuthHeader != "":
				// api_key connection with a vendor-specific header: the raw key,
				// no Bearer scheme.
				opts.Headers = map[string]string{conn.AuthHeader: bearer}
			default:
				opts.Headers = map[string]string{"Authorization": "Bearer " + bearer}
			}
		}
		if aerr := client.AddHTTPServerWithOptions(ctx, regName, conn.URL, opts); aerr != nil {
			log.Printf("remote-mcp: skipping server %q for %s — failed to connect", regName, email)
			overlay.Skipped = append(overlay.Skipped, regName)
			continue
		}
		overlay.Servers[regName] = true
		overlay.Seats[regName] = agentcore.MCPChoice{Server: conn.Name, Account: conn.Account}
		registered++
	}

	overlay.Catalog = client.GetAllTools()
	return overlay, nil
}

// SeatSelection returns the public {connection name, account} of every seat
// the overlay mounted, ordered by registration name, for approval staging
// (#988): the stager keys seats by agentcore.RegisteredMCPName, which is
// exactly the name each seat registered under.
func (o *RemoteMCPOverlay) SeatSelection() agentcore.MCPSelection {
	if o == nil || len(o.Seats) == 0 {
		return nil
	}
	names := make([]string, 0, len(o.Seats))
	for name := range o.Seats {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(agentcore.MCPSelection, 0, len(names))
	for _, name := range names {
		out = append(out, o.Seats[name])
	}
	return out
}

// ComposeWith returns the call seam + public catalog a run sees once this
// overlay is layered over a base broker/catalog: overlay names route to the
// overlay, everything else to the base. Shared by ApplyMCPOverlayWithBase and
// the approval stager rebind so staging resolves remote tools against the
// same composite the loop dispatches on. An inactive overlay returns the base
// unchanged.
func (o *RemoteMCPOverlay) ComposeWith(baseBroker agentcore.MCPBroker, baseCatalog []mcp.ServerTool) (agentcore.MCPBroker, []mcp.ServerTool) {
	if !o.Active() {
		return baseBroker, baseCatalog
	}
	merged := append([]mcp.ServerTool(nil), baseCatalog...)
	merged = append(merged, o.Catalog...)
	if baseBroker == nil {
		return o.CallBroker(), merged
	}
	return &compositeBroker{
		overlay:        o.callBroker(agentcore.DefaultRemediationHints),
		overlayServers: o.Servers,
		base:           baseBroker,
	}, merged
}

// CallBroker is the overlay's own call seam (a broker-owned scope, or the
// in-process compatibility client wrapped as a local broker).
func (o *RemoteMCPOverlay) CallBroker() agentcore.MCPBroker {
	return o.callBroker(agentcore.DefaultRemediationHints)
}

// ApplyMCPOverlayWithBase composes a per-user remote overlay with either the
// historical local base client or an injected out-of-process base broker and
// catalog. A remote server can still never shadow a base server; only the base
// call location changes.
func ApplyMCPOverlayWithBase(
	deps *agentcore.Deps,
	baseClient *mcp.Client,
	baseBroker agentcore.MCPBroker,
	baseCatalog []mcp.ServerTool,
	overlay *RemoteMCPOverlay,
) {
	if deps == nil || !overlay.Active() {
		return
	}
	hints := agentcore.DefaultRemediationHints
	if baseBroker == nil {
		if baseClient == nil {
			return
		}
		baseBroker = agentcore.NewLocalMCPBroker(baseClient, hints)
	}
	if baseCatalog == nil && baseClient != nil {
		baseCatalog = baseClient.GetAllTools()
	}
	deps.MCPBroker = &compositeBroker{
		overlay:        overlay.callBroker(hints),
		overlayServers: overlay.Servers,
		base:           baseBroker,
	}
	merged := append([]mcp.ServerTool(nil), baseCatalog...)
	merged = append(merged, overlay.Catalog...)
	deps.MCPCatalog = merged
}

func (o *RemoteMCPOverlay) callBroker(hints agentcore.RemediationHints) agentcore.MCPBroker {
	if o.Broker != nil {
		return o.Broker
	}
	return agentcore.NewLocalMCPBroker(o.Client, hints)
}

// compositeBroker routes an MCP call to the per-user overlay broker when the
// server name belongs to the overlay, and to the base (shared) broker otherwise.
// It implements agentcore.MCPBroker so it slots into Deps.MCPBroker without
// forking the governed loop.
type compositeBroker struct {
	overlay        agentcore.MCPBroker
	overlayServers map[string]bool
	base           agentcore.MCPBroker
}

func (b *compositeBroker) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	if b.overlayServers[server] {
		return b.overlay.CallMCP(ctx, server, tool, args)
	}
	return b.base.CallMCP(ctx, server, tool, args)
}

// browserbaseHost is the vendor host of the Browserbase hosted MCP endpoint.
// The connector is matched on its URL rather than its registration name because
// the name is whatever the user typed when they added it ("bb", "Browserbase",
// anything) — the URL is what actually identifies the vendor.
const browserbaseHost = "browserbase.com"

// browserbaseKeyFunc returns a resolver for THIS user's Browserbase connector
// credential, or nil when one is not genuinely reachable for this turn.
//
// Returning nil matters twice over, because a non-nil func is what registers
// browserbase_live_view:
//
//   - It keeps a permanently-failing tool away from the majority of users, who
//     have no Browserbase connection at all.
//   - It keeps the credential, and the session enumeration it enables, inside the
//     per-conversation connector gate. openRemoteOverlay restricts remote servers
//     to the conversation's opt-in set; a key resolver that ignored that set would
//     let a chat with Browserbase switched OFF still unseal the key and list every
//     running session in the account.
//
// The cost is one extra ConnectedServersForUser read per turn (the overlay does
// its own later). That is a store read, not a decrypt: the key itself is unsealed
// only if the tool is actually called.
func (m *Manager) browserbaseKeyFunc(ctx context.Context, email string, enabledOptional []string) tools.BrowserbaseKeyFunc {
	if m.remoteMCP == nil || strings.TrimSpace(email) == "" {
		return nil
	}
	conns, err := m.remoteMCP.ConnectedServersForUser(ctx, email)
	if err != nil {
		return nil
	}
	// Same opt-in semantics as openRemoteOverlay: nil and empty both mean "no
	// connectors on" (RunTurnInput.OptionalMCPServersEnabled documents exactly
	// that, and openRemoteOverlay builds its filter map unconditionally). This
	// function's only caller is the interactive turn — scheduled runs pass a nil
	// key func straight to NewTurnTools — so there is no "unfiltered" caller,
	// and treating nil as unfiltered would unseal the key in a conversation the
	// overlay wires no connectors into (e.g. one whose opt-in list was never
	// seeded).
	enabled := make(map[string]bool, len(enabledOptional))
	for _, n := range enabledOptional {
		if n = strings.TrimSpace(n); n != "" {
			enabled[n] = true
			enabled[strings.ToLower(n)] = true
		}
	}
	for _, c := range conns {
		if !isBrowserbaseURL(c.URL) {
			continue
		}
		if !enabled[c.Name] && !enabled[strings.ToLower(c.Name)] {
			continue // connector switched off for this conversation
		}
		id := c.ID
		return func(ctx context.Context) (string, error) {
			// Own connections and ones shared with this user both work: the
			// credential is the owner's, applied host-side, exactly as it is for
			// the connector's own tool calls.
			return m.remoteMCP.AcquireTokenByID(ctx, email, id)
		}
	}
	return nil
}

// isBrowserbaseURL reports whether a connection's URL points at Browserbase.
func isBrowserbaseURL(raw string) bool {
	host := strings.ToLower(strings.TrimSpace(raw))
	// Hostname(), not Host: an explicit port (":443") must not defeat the match.
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	return host == browserbaseHost || strings.HasSuffix(host, "."+browserbaseHost)
}
