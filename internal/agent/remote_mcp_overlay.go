package agent

import (
	"context"
	"log"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Per-user remote (hosted) MCP overlay (#443), shared by BOTH the interactive
// and scheduled drivers. A user's OAuth-connected remote servers are wired into
// a run WITHOUT touching the long-lived shared MCP client (process-wide; mutating
// it would leak one user's bearer to another). Instead a small per-run *mcp.Client
// holds only this user's servers, registered with a freshly-refreshed bearer over
// an SSRF-safe HTTP client, and a compositeBroker routes calls for those server
// names to it while everything else falls through to the base (shared) broker.
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

// maxOverlayServers caps how many remote servers one user can inject into a
// single run, a guard against blowing the 128-tool ceiling (and against a
// pathological number of per-turn handshakes). Excess servers are skipped with a
// logged warning rather than silently dropped.
const maxOverlayServers = 8

// RemoteMCPOverlay is the per-run wiring for a user's remote servers. The caller
// MUST Close Client at run end.
type RemoteMCPOverlay struct {
	Client  *mcp.Client      // per-run; Close() in the caller's defer
	Catalog []mcp.ServerTool // the overlay servers' tools, merged into the run catalog
	Servers map[string]bool  // registration names handled by the overlay broker
}

// Active reports whether the overlay actually registered any servers.
func (o *RemoteMCPOverlay) Active() bool {
	return o != nil && o.Client != nil && len(o.Servers) > 0
}

// Close tears down the overlay's per-run client (nil-safe).
func (o *RemoteMCPOverlay) Close() {
	if o != nil && o.Client != nil {
		_ = o.Client.Close()
	}
}

// BuildRemoteMCPOverlay registers a user's connected remote servers onto a fresh
// per-run client. shadowed is the set of server names already provided by the
// base catalog — an overlay server colliding with one is skipped so a user can
// never shadow a built-in tool. A server that fails to mint a token or initialize
// is skipped (graceful degradation), never fatal. Returns nil when there are no
// usable overlay servers. The caller MUST Close the returned overlay.
func BuildRemoteMCPOverlay(ctx context.Context, resolver RemoteMCPResolver, email string, shadowed map[string]bool) (*RemoteMCPOverlay, error) {
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
		if registered >= maxOverlayServers {
			log.Printf("remote-mcp: skipping %q and further servers for %s — overlay cap %d reached", conn.Name, email, maxOverlayServers)
			break
		}
		if shadowed[conn.Name] {
			log.Printf("remote-mcp: skipping remote server %q — name collides with a built-in server", conn.Name)
			continue
		}
		bearer, terr := resolver.AcquireTokenByID(ctx, email, conn.ID)
		if terr != nil {
			// needs-reauth / refresh failure: skip this server, keep the rest.
			log.Printf("remote-mcp: skipping server %q for %s — token unavailable: %v", conn.Name, email, terr)
			continue
		}
		opts := mcp.HTTPServerOptions{
			Headers:    map[string]string{"Authorization": "Bearer " + bearer},
			HTTPClient: httpClient,
		}
		if aerr := client.AddHTTPServerWithOptions(ctx, conn.Name, conn.URL, opts); aerr != nil {
			log.Printf("remote-mcp: skipping server %q for %s — failed to connect: %v", conn.Name, email, aerr)
			continue
		}
		overlay.Servers[conn.Name] = true
		registered++
	}

	if registered == 0 {
		_ = client.Close()
		return nil, nil
	}
	overlay.Catalog = client.GetAllTools()
	return overlay, nil
}

// ApplyMCPOverlay wires an overlay into a run's Deps: the run advertises the base
// client's catalog merged with the overlay's, and dispatches through a
// compositeBroker that routes the overlay's server names to the overlay client.
// A nil/empty overlay is a no-op, so callers can apply unconditionally. baseClient
// is the shared (or per-run bundle) client the run otherwise uses.
func ApplyMCPOverlay(deps *agentcore.Deps, baseClient *mcp.Client, overlay *RemoteMCPOverlay) {
	if deps == nil || baseClient == nil || !overlay.Active() {
		return
	}
	hints := agentcore.DefaultRemediationHints
	deps.MCPBroker = &compositeBroker{
		overlay:        agentcore.NewLocalMCPBroker(overlay.Client, hints),
		overlayServers: overlay.Servers,
		base:           agentcore.NewLocalMCPBroker(baseClient, hints),
	}
	merged := baseClient.GetAllTools()
	merged = append(merged, overlay.Catalog...)
	deps.MCPCatalog = merged
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
