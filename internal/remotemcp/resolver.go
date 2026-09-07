package remotemcp

import (
	"context"
	"log"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// This file adapts *Service to the agent.RemoteMCPResolver interface so the chat
// manager (and, via the same shape, the scheduled runner) can wire a user's
// connected remote servers into a run without importing this package's concrete
// types. The dependency points remotemcp → agent (agent declares the interface),
// which avoids the store → agent → remotemcp cycle.

// ConnectedServersForUser returns the seats the user may USE that are ready:
// their own connections plus ones other users shared with them (directly or
// via the everyone wildcard), one entry per (name, account) seat (#988). The
// run path mounts exactly one seat per name — the pinned one, else the one
// flagged Default — so returning every seat here lets it choose without a
// second store read. On a (name, account) collision the user's own seat wins:
// the registration name is the broker routing key, so two same-named seats
// cannot coexist in one run; the shadowed shared seat is skipped with a log
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
	// excluded from their runs. A prefs read failure fails CLOSED: with the
	// prefs unreadable we cannot tell which connectors the user disabled, and
	// the old "keep the default (enabled)" fallback mounted every connected
	// server — the user's own and every one shared with them — including the
	// ones they had switched off, during any DB blip. The run proceeds with
	// no remote connectors this turn and the cause is logged.
	prefs, perr := s.store.ListConnectorPrefs(ctx, email)
	if perr != nil {
		log.Printf("remotemcp: connector prefs unreadable for %s — mounting no remote connectors this run: %v", email, perr)
		return nil, nil
	}
	enabledForMe := func(id string) bool {
		if p, ok := prefs[store.ConnectorPrefKey(store.ConnectorKindRemote, id)]; ok {
			return p.Enabled
		}
		return true
	}
	out := make([]agent.RemoteMCPConn, 0, len(servers)+len(shared))
	seats := map[string]bool{} // registration name → taken
	ownNames := map[string]bool{}
	for _, srv := range servers {
		if srv.Status != store.RemoteMCPStatusConnected || !enabledForMe(srv.ID) {
			continue
		}
		seats[agentcore.RegisteredMCPName(srv.Name, srv.Account)] = true
		ownNames[srv.Name] = true
		out = append(out, connFor(srv, ""))
	}
	for _, srv := range shared {
		if srv.Status != store.RemoteMCPStatusConnected || !enabledForMe(srv.ID) {
			continue
		}
		reg := agentcore.RegisteredMCPName(srv.Name, srv.Account)
		if seats[reg] {
			log.Printf("remotemcp: shared seat %q (owner %s) shadowed by %s's own seat of the same name; skipping", reg, srv.UserEmail, email)
			continue
		}
		seats[reg] = true
		conn := connFor(srv, srv.UserEmail)
		// A shared seat never becomes the default under a name the user owns
		// seats for: the owner of the run decides their own default.
		if ownNames[srv.Name] {
			conn.Default = false
		}
		out = append(out, conn)
	}
	return out, nil
}

func connFor(srv store.RemoteMCPServer, owner string) agent.RemoteMCPConn {
	return agent.RemoteMCPConn{
		ID:         srv.ID,
		Name:       srv.Name,
		Account:    srv.Account,
		Default:    srv.IsDefault,
		URL:        srv.URL,
		Owner:      owner,
		AuthHeader: srv.APIKeyHeader,
		AuthQuery:  srv.APIKeyQuery,
	}
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
		key, err := s.store.GetRemoteMCPAPIKey(ctx, server)
		if err == nil {
			// The unsealed key is a runtime-acquired credential exactly like a
			// minted bearer: register it for literal redaction (#1124).
			s.noteSecrets(key)
		}
		return key, err
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
