package remotemcp

import (
	"context"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// This file adapts *Service to the agent.RemoteMCPResolver interface so the chat
// manager (and, via the same shape, the scheduled runner) can wire a user's
// connected remote servers into a run without importing this package's concrete
// types. The dependency points remotemcp → agent (agent declares the interface),
// which avoids the store → agent → remotemcp cycle.

// ConnectedServersForUser returns the user's servers that are ready to use.
func (s *Service) ConnectedServersForUser(ctx context.Context, email string) ([]agent.RemoteMCPConn, error) {
	servers, err := s.store.ListRemoteMCPServers(ctx, email)
	if err != nil {
		return nil, err
	}
	out := make([]agent.RemoteMCPConn, 0, len(servers))
	for _, srv := range servers {
		if srv.Status != store.RemoteMCPStatusConnected {
			continue
		}
		out = append(out, agent.RemoteMCPConn{ID: srv.ID, Name: srv.Name, URL: srv.URL})
	}
	return out, nil
}

// AcquireTokenByID mints a fresh bearer for one of the user's servers.
func (s *Service) AcquireTokenByID(ctx context.Context, email, serverID string) (string, error) {
	server, err := s.store.GetRemoteMCPServer(ctx, email, serverID)
	if err != nil {
		return "", err
	}
	return s.AcquireToken(ctx, server)
}

// SafeHTTPClient exposes the SSRF-safe client used to dial user-supplied servers
// (also reused as the data-plane transport for the overlay MCP client).
func (s *Service) SafeHTTPClient() *http.Client { return s.httpClient }

// Ensure *Service satisfies the agent-side resolver contract at compile time.
var _ agent.RemoteMCPResolver = (*Service)(nil)
