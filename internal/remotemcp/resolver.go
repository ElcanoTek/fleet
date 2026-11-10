package remotemcp

import (
	"context"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/logging"
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
	return s.serversForUser(ctx, email, false)
}

// OverlayServersForUser returns the user's enabled remote MCP seats for
// BuildRemoteMCPOverlay and CheckTaskMCPSelection: connected seats first, plus
// login_required / needs_reauth seats marked with NeedsReauth: true so a
// scheduled task pinning an unauthenticated connection skips it gracefully
// instead of dead-lettering as an unknown server.
func (s *Service) OverlayServersForUser(ctx context.Context, email string) ([]agent.RemoteMCPConn, error) {
	return s.serversForUser(ctx, email, true)
}

func isOverlayStatus(status string, includeNeedsReauth bool) (ok bool, needsReauth bool) {
	switch status {
	case store.RemoteMCPStatusConnected:
		return true, false
	case store.RemoteMCPStatusLoginRequired, store.RemoteMCPStatusNeedsReauth:
		return includeNeedsReauth, true
	default:
		return false, false
	}
}

func (s *Service) serversForUser(ctx context.Context, email string, includeNeedsReauth bool) ([]agent.RemoteMCPConn, error) {
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
		logging.From(ctx).Warn("remotemcp: connector prefs unreadable — mounting no remote connectors this run", "user", email, "err", perr)
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
		ok, needsReauth := isOverlayStatus(srv.Status, includeNeedsReauth)
		if !ok || !enabledForMe(srv.ID) {
			continue
		}
		seats[agentcore.RegisteredMCPName(srv.Name, srv.Account)] = true
		ownNames[srv.Name] = true
		conn := connFor(srv, "")
		conn.NeedsReauth = needsReauth
		out = append(out, conn)
	}
	for _, srv := range shared {
		ok, needsReauth := isOverlayStatus(srv.Status, includeNeedsReauth)
		if !ok || !enabledForMe(srv.ID) {
			continue
		}
		reg := agentcore.RegisteredMCPName(srv.Name, srv.Account)
		if seats[reg] {
			logging.From(ctx).Warn("remotemcp: shared seat shadowed by the user's own seat of the same name; skipping", "reg", reg, "owner", srv.UserEmail, "user", email)
			continue
		}
		seats[reg] = true
		conn := connFor(srv, srv.UserEmail)
		conn.NeedsReauth = needsReauth
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
	var customTools []agent.RemoteMCPCustomTool
	if len(srv.CustomTools) > 0 {
		customTools = make([]agent.RemoteMCPCustomTool, 0, len(srv.CustomTools))
		for _, ct := range srv.CustomTools {
			var steps []agent.RemoteMCPCustomStep
			if len(ct.Steps) > 0 {
				steps = make([]agent.RemoteMCPCustomStep, 0, len(ct.Steps))
				for _, st := range ct.Steps {
					steps = append(steps, agent.RemoteMCPCustomStep{
						Action:    st.Action,
						Selector:  st.Selector,
						Value:     st.Value,
						Mode:      st.Mode,
						Attribute: st.Attribute,
						TimeoutMs: st.TimeoutMs,
						Optional:  st.Optional,
					})
				}
			}
			customTools = append(customTools, agent.RemoteMCPCustomTool{
				Name:        ct.Name,
				Description: ct.Description,
				Method:      ct.Method,
				URL:         ct.URL,
				Headers:     ct.Headers,
				Body:        ct.Body,
				InputSchema: ct.InputSchema,
				Steps:       steps,
			})
		}
	}
	return agent.RemoteMCPConn{
		ID:          srv.ID,
		Name:        srv.Name,
		Account:     srv.Account,
		Default:     srv.IsDefault,
		URL:         srv.URL,
		Transport:   srv.Transport,
		Owner:       owner,
		AuthHeader:  srv.APIKeyHeader,
		AuthQuery:   srv.APIKeyQuery,
		AuthPrefix:  srv.APIKeyPrefix,
		CustomTools: customTools,
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

// MarkRemoteMCPUnauthorized records that a server refused the stored
// credential at mount time (HTTP 401): the OWNER's row goes to needs_reauth
// with the detail, so Settings → Connections shows "Sign-in needed" now
// rather than after the token's natural expiry (#1006). Implements
// agent.RemoteMCPStatusMarker. Runs in the credential-owning child like the
// rest of the resolver; nothing about the credential crosses the wire.
func (s *Service) MarkRemoteMCPUnauthorized(ctx context.Context, ownerEmail, serverID, detail string) error {
	return s.store.SetRemoteMCPStatus(ctx, ownerEmail, serverID, store.RemoteMCPStatusNeedsReauth, detail)
}

// SafeHTTPClient exposes the SSRF-safe client used to dial user-supplied servers
// (also reused as the data-plane transport for the overlay MCP client).
func (s *Service) SafeHTTPClient() *http.Client { return s.httpClient }

// AllowInsecureHTTP reports whether http:// URLs are permitted (dev/test escape hatch).
func (s *Service) AllowInsecureHTTP() bool { return s != nil && s.cfg.AllowInsecureHTTP }

// Ensure *Service satisfies the agent-side resolver contract at compile time.
var _ agent.RemoteMCPResolver = (*Service)(nil)
