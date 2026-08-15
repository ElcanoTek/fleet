package agent

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
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
	Name string // registration name (also the broker routing key + mcp_<name>_* prefix)
	URL  string
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

// RemoteMCPOverlayOpener binds one user's public remote-server selection to a
// per-run broker overlay. Implementations may keep the historical in-process
// client or open a scope in a credential-owning subprocess; callers see only
// public tool metadata and the agentcore call seam.
type RemoteMCPOverlayOpener func(ctx context.Context, email string, shadowed, enabled map[string]bool) (*RemoteMCPOverlay, error)

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
	// CloseScope releases a broker-owned scope. It is called with a fresh,
	// bounded context so cancellation of the run cannot suppress cleanup.
	CloseScope func(context.Context) error
	// Skipped names servers that were selected but could not be wired this run —
	// today only because their token is unavailable (needs re-auth) or the server
	// failed to connect. Callers surface these to the owner (a needs-reauth server
	// silently doing nothing is a correctness trap, especially for headless runs).
	Skipped []string
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
// never shadow a built-in tool. enabled, when non-nil, restricts wiring to the
// servers whose name is in it (the conversation's per-turn opt-in set); nil means
// "wire all connected servers" (the scheduled default). A server that fails to
// mint a token (needs re-auth) or connect is recorded in Skipped (graceful
// degradation), never fatal. The returned overlay is non-nil whenever there are
// connected servers (so the caller can read Skipped even when none registered);
// its Active() reports whether any server is actually wired. The caller MUST
// Close it.
func BuildRemoteMCPOverlay(ctx context.Context, resolver RemoteMCPResolver, email string, shadowed, enabled map[string]bool) (*RemoteMCPOverlay, error) {
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
	overlay := &RemoteMCPOverlay{Client: client, Servers: map[string]bool{}}
	registered := 0
	for _, conn := range conns {
		// Opt-in gate (chat): only wire servers the conversation selected. A
		// non-selected server is not "skipped" — the user chose not to enable it.
		// The persisted opt-in list is canonically lowercase (the HTTP layer
		// normalizes on write) while remote server names keep the case the user
		// typed, so check the lowercased form too — exact match first so any
		// pre-existing list entry keeps working.
		if enabled != nil && !enabled[conn.Name] && !enabled[strings.ToLower(conn.Name)] {
			continue
		}
		if registered >= maxOverlayServers {
			log.Printf("remote-mcp: skipping %q and further servers for %s — overlay cap %d reached", conn.Name, email, maxOverlayServers)
			break
		}
		if shadowed[conn.Name] {
			log.Printf("remote-mcp: skipping remote server %q — name collides with a built-in server", conn.Name)
			continue
		}
		if conn.Owner != "" {
			// Attribution for shared connections: the run belongs to email, but
			// tool calls authenticate with the OWNER's token host-side.
			log.Printf("remote-mcp: run for %s uses shared server %q owned by %s", email, conn.Name, conn.Owner)
		}
		bearer, terr := resolver.AcquireTokenByID(ctx, email, conn.ID)
		if terr != nil {
			// needs-reauth / refresh failure: skip this server, keep the rest, and
			// record it so the caller can tell the owner.
			log.Printf("remote-mcp: skipping server %q for %s — token unavailable", conn.Name, email)
			overlay.Skipped = append(overlay.Skipped, conn.Name)
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
		if aerr := client.AddHTTPServerWithOptions(ctx, conn.Name, conn.URL, opts); aerr != nil {
			log.Printf("remote-mcp: skipping server %q for %s — failed to connect", conn.Name, email)
			overlay.Skipped = append(overlay.Skipped, conn.Name)
			continue
		}
		overlay.Servers[conn.Name] = true
		registered++
	}

	overlay.Catalog = client.GetAllTools()
	return overlay, nil
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
