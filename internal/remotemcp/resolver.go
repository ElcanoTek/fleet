package remotemcp

import (
	"context"
	"log"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// This file adapts *Service to the agent.RemoteMCPResolver interface so the chat
// manager (and, via the same shape, the scheduled runner) can wire a user's
// connected remote servers into a run without importing this package's concrete
// types. The dependency points remotemcp → agent (agent declares the interface),
// which avoids the store → agent → remotemcp cycle.

// ConnectedServersForUser returns the servers the user may USE that are ready:
// their own connections plus ones other users shared with them (directly or
// via the everyone wildcard). On a name collision the user's own server wins —
// the registration name is the broker routing key, so two same-named servers
// cannot coexist in one run; the shadowed shared server is skipped with a log
// line rather than silently.
func (s *Service) ConnectedServersForUser(ctx context.Context, email string) ([]agent.RemoteMCPConn, error) {
	servers, err := s.store.ListRemoteMCPServers(ctx, email)
	if err != nil {
		return nil, err
	}
	shared, err := s.store.ListRemoteMCPServersSharedWith(ctx, email)
	if err != nil {
		return nil, err
	}
	// Availability layer (unified connector UX): a connection the user turned
	// off on the connections page — their own or one shared with them — is
	// excluded from their runs. Best-effort: a prefs read failure keeps the
	// default (enabled) rather than failing the run.
	prefs, perr := s.store.ListConnectorPrefs(ctx, email)
	if perr != nil {
		prefs = nil
	}
	enabledForMe := func(id string) bool {
		if p, ok := prefs[store.ConnectorPrefKey(store.ConnectorKindRemote, id)]; ok {
			return p.Enabled
		}
		return true
	}
	out := make([]agent.RemoteMCPConn, 0, len(servers)+len(shared))
	names := map[string]bool{}
	for _, srv := range servers {
		if srv.Status != store.RemoteMCPStatusConnected || !enabledForMe(srv.ID) {
			continue
		}
		names[srv.Name] = true
		out = append(out, agent.RemoteMCPConn{ID: srv.ID, Name: srv.Name, URL: srv.URL, AuthHeader: srv.APIKeyHeader, AuthQuery: srv.APIKeyQuery})
	}
	for _, srv := range shared {
		if srv.Status != store.RemoteMCPStatusConnected || !enabledForMe(srv.ID) {
			continue
		}
		if names[srv.Name] {
			log.Printf("remotemcp: shared server %q (owner %s) shadowed by %s's own server of the same name; skipping", srv.Name, srv.UserEmail, email)
			continue
		}
		names[srv.Name] = true
		out = append(out, agent.RemoteMCPConn{ID: srv.ID, Name: srv.Name, URL: srv.URL, Owner: srv.UserEmail, AuthHeader: srv.APIKeyHeader, AuthQuery: srv.APIKeyQuery})
	}
	return out, nil
}

// AcquireTokenByID mints a fresh credential for a server the user may use —
// their own or one shared with them. The returned row keeps the OWNER's email,
// so the refresh/decrypt path opens and reseals secrets under the owner's AAD;
// the grantee never handles the credential. For api_key servers the credential
// is the sealed static key; for open servers it is empty (no header at all).
func (s *Service) AcquireTokenByID(ctx context.Context, email, serverID string) (string, error) {
	server, err := s.store.GetRemoteMCPServerForUse(ctx, email, serverID)
	if err != nil {
		return "", err
	}
	if server.AuthKind == store.RemoteMCPAuthAPIKey {
		return s.store.GetRemoteMCPAPIKey(ctx, server)
	}
	if server.Issuer == "" {
		return "", nil
	}
	return s.AcquireToken(ctx, server)
}

// SafeHTTPClient exposes the SSRF-safe client used to dial user-supplied servers
// (also reused as the data-plane transport for the overlay MCP client).
func (s *Service) SafeHTTPClient() *http.Client { return s.httpClient }

// Ensure *Service satisfies the agent-side resolver contract at compile time.
var _ agent.RemoteMCPResolver = (*Service)(nil)
