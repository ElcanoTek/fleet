package main

import (
	"context"
	"log"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/secretbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

// installStoreCipher installs the at-rest secret cipher on the chat store
// whenever FLEET_MCP_OAUTH_ENCRYPTION_KEY is set. Historically this happened
// only inside setupRemoteMCP (which also requires FLEET_PUBLIC_BASE_URL), but
// the cipher now also protects LLM provider keys and notification secrets —
// features that must not silently require the remote-MCP feature to be on.
// setupRemoteMCP re-installing the same cipher afterwards is harmless.
func installStoreCipher(cfg *config.Config, chatStore *store.Store) {
	if len(cfg.MCPOAuthEncryptionKey) == 0 {
		return
	}
	cipher, err := secretbox.NewCipher(cfg.MCPOAuthEncryptionKey)
	if err != nil {
		// Config already validates the key length; degrade rather than crash.
		log.Printf("store secret cipher: not installed — invalid encryption key: %v", err)
		return
	}
	chatStore.SetTokenCipher(cipher)
}

// remoteMCPResolver adapts the service to the agent-side resolver interface,
// returning a TRUE nil interface when the feature is off. setupRemoteMCP returns
// a typed nil *Service in that case, and assigning that straight into the
// interface field would make `opts.RemoteMCP != nil` true while every method
// call nil-derefs.
func remoteMCPResolver(svc *remotemcp.Service) agent.RemoteMCPResolver {
	if svc == nil {
		return nil
	}
	return svc
}

// setupRemoteMCP wires the per-user remote-MCP + OAuth feature (#443). It is
// enabled only when an encryption key AND a public base URL are configured;
// otherwise it fails closed (returns nil) and the endpoints report the
// feature off. On enable it relies on the token cipher installStoreCipher put
// on the chat store (secrets encrypted at rest); a key that made no cipher
// leaves the feature off.
// The returned Service backs explicit HTTP control-plane operations. Agent runs
// bind their MCP tools through the child-owned broker scope, which is where the
// credentialed MCP client lives; the Service is additionally handed to the agent
// manager as a READ-side resolver so the parent can unseal one connector's
// api_key for a host-side outbound call (browserbase_live_view, #987) — the same
// thing the parent already does for the add-time probe.
func setupRemoteMCP(cfg *config.Config, chatStore *store.Store) *remotemcp.Service {
	if len(cfg.MCPOAuthEncryptionKey) == 0 || cfg.PublicBaseURL == "" {
		log.Printf("remote MCP OAuth: disabled (set FLEET_MCP_OAUTH_ENCRYPTION_KEY + FLEET_PUBLIC_BASE_URL to enable)")
		return nil
	}
	// installStoreCipher (which run() calls first) already put the cipher on
	// the store; it installs nothing for a key that does not make a cipher.
	if !chatStore.RemoteMCPEncryptionEnabled() {
		// Config already validates the key length, so this is belt-and-suspenders:
		// disable rather than crash the whole server on a bad key.
		log.Printf("remote MCP OAuth: disabled — invalid encryption key (see the store secret cipher error)")
		return nil
	}
	// Same wiring the credential-owning child does (see mcp_broker.go, #1124),
	// for this process's own redactor. The parent's control-plane service
	// unseals per-user api_key secrets and mints/refreshes OAuth bearers —
	// e.g. for browserbase_live_view — and those are acquired at RUNTIME, so
	// the boot-time env snapshot cannot know them. Without this the parent's
	// literal set never learned them, and a connector echoing its own bare
	// token back was scrubbed only if it happened to match a shape pattern.
	// The observer is scope-aware (#1274): each server row's rotations
	// supersede that row's previous generation, so a long-running process
	// plateaus instead of scanning for every token it has ever seen.
	svc := newRemoteMCPService(cfg, chatStore, agentcore.RegisterSecretLiterals)
	// Abandoned OAuth flow rows are reclaimed by the maintenance loop (see
	// runMaintenancePass), not by a daemon of their own. This used to be a
	// `for range ticker.C` goroutine with a context.Background() per sweep —
	// unreachable by the shutdown context, so it outlived the drain and could
	// touch a closing store. Folding it into the one loop that already runs
	// hourly fixed that and removed a redundant ticker.
	log.Printf("remote MCP OAuth: ENABLED (per-user hosted servers; redirect %s/api/oauth/mcp/callback)", cfg.PublicBaseURL)
	return svc
}

// newRemoteMCPService builds the remote-MCP control-plane service over st,
// which must already carry the token cipher, and wires observer to receive
// every credential the service acquires at runtime (#1124). Shared by the
// parent (setupRemoteMCP) and the credential-owning broker
// (openMCPBrokerRemoteStore); each keeps its own gating and error policy.
func newRemoteMCPService(cfg *config.Config, st *store.Store, observer remotemcp.SecretObserver) *remotemcp.Service {
	svc := remotemcp.NewService(st, remotemcp.Config{
		PublicBaseURL:     cfg.PublicBaseURL,
		AllowInsecureHTTP: cfg.RemoteMCPAllowInsecureHTTP,
	})
	svc.SetSecretObserver(observer)
	return svc
}

func productionRemoteMCPOverlayOpener(svc *remotemcp.Service, runtime *productionMCPRuntime) agent.RemoteMCPOverlayOpener {
	if svc == nil || runtime == nil {
		return nil
	}
	return func(ctx context.Context, email string, shadowed map[string]bool, sel agent.RemoteMCPSelection) (*agent.RemoteMCPOverlay, error) {
		overlay, err := runtime.openRemoteOverlay(ctx, email, shadowed, sel)
		if err != nil || overlay == nil {
			return overlay, err
		}
		// Seat attribution (#988): the child returns public registration names
		// only. Map each back to its {connection, account} from the parent's own
		// (control-plane) view of the user's seats so approval staging records
		// the seat that actually mounted. Best-effort — an unmapped name keeps
		// the pre-#988 default-seat record.
		if conns, cerr := svc.ConnectedServersForUser(ctx, email); cerr == nil {
			overlay.Seats = make(map[string]agentcore.MCPChoice, len(overlay.Servers))
			for _, c := range conns {
				reg := agentcore.RegisteredMCPName(c.Name, c.Account)
				if overlay.Servers[reg] {
					overlay.Seats[reg] = agentcore.MCPChoice{Server: c.Name, Account: c.Account}
				}
			}
		}
		return overlay, nil
	}
}

// hostedConnectionsLister is the scheduled runner's read-only view of a task
// owner's hosted connections (scheduledrun.Options.HostedConnections): the
// service's ConnectedServersForUser, which lists rows and mints nothing. nil
// when the feature is off — a typed-nil service's method value would
// nil-deref on the first call.
func hostedConnectionsLister(svc *remotemcp.Service) func(context.Context, string) ([]agent.RemoteMCPConn, error) {
	if svc == nil {
		return nil
	}
	return svc.ConnectedServersForUser
}

// wireRemoteMCPCatalog injects the per-user remote-MCP catalog provider (#466)
// into the orchestrator handlers when the feature is on. A nil service (feature
// disabled) is a no-op, so the bundle catalog is served unchanged. Kept separate
// from run() so the nil-guard branch stays out of run()'s cyclomatic budget.
func wireRemoteMCPCatalog(h *handlers.Handlers, svc *remotemcp.Service) {
	if svc == nil {
		return
	}
	h.SetRemoteMCPServersProvider(remoteMCPCatalogProvider(svc))
}

// remoteMCPCatalogProvider adapts the remotemcp Service into the orchestrator's
// per-user remote-MCP catalog provider (#466): given the caller's email (the
// orchestrator username for the elcano-auth tier; see ownerEmailResolver), it
// returns that user's OAuth-connected hosted servers so GetMCPServers can surface
// them in the task form — mirroring chat's GET /mcp-servers. Only CONNECTED
// servers are returned, one entry per connection NAME (#988): its Accounts are
// the labeled seats the task can pin, DefaultAccount the seat a run mounts
// when the task pins none. Remote servers are auto-applied to ALL the owner's
// runs by the run overlay, so the UI renders them connected/auto-available and
// offers the seat pick rather than an on/off toggle. A lookup error is logged
// and treated as "no remote servers" so it never breaks the bundle catalog.
func remoteMCPCatalogProvider(svc *remotemcp.Service) func(context.Context, string) []handlers.MCPServerCatalogEntry {
	return func(ctx context.Context, email string) []handlers.MCPServerCatalogEntry {
		conns, err := svc.ConnectedServersForUser(ctx, email)
		if err != nil {
			log.Printf("orchestrator mcp catalog: remote server lookup failed: %v", err)
			return nil
		}
		out := make([]handlers.MCPServerCatalogEntry, 0, len(conns))
		for _, g := range agent.GroupRemoteMCPSeats(conns) {
			out = append(out, handlers.MCPServerCatalogEntry{
				Name:           g.Name,
				DisplayName:    g.Name,
				Description:    "Remote MCP server you connected (" + g.URL + "). Auto-available to your scheduled tasks on its default seat unless the task pins another.",
				Enabled:        true,
				Accounts:       g.Accounts,
				DefaultAccount: g.DefaultAccount,
				Remote:         true,
			})
		}
		return out
	}
}

// ownerEmailResolver maps a scheduled task's creator UUID to the chat-side email
// its remote-MCP OAuth tokens are keyed by (#443). The orchestrator username IS
// that email for the elcano-auth tier. Returns "" (no error) when the user can't
// be found, so a task created by a since-deleted user simply gets no overlay.
func ownerEmailResolver(s *storage.Storage) func(context.Context, uuid.UUID) (string, error) {
	return func(ctx context.Context, id uuid.UUID) (string, error) {
		m, err := s.GetUsersByIDsWithContext(ctx, []uuid.UUID{id})
		if err != nil {
			return "", err
		}
		return m[id], nil
	}
}
