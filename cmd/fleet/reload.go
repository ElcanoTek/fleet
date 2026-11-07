package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// reloadConfigHandler serves POST /admin/reload-config (#286): it re-reads the
// reloadable settings from the env file + process environment and returns a JSON
// diff of what changed / was skipped (non-reloadable) / errored. cfg may be nil
// in route-walking tests (the route only needs to exist there); a real request
// against a nil cfg returns 503.
func reloadConfigHandler(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if cfg == nil {
			http.Error(w, "config reload unavailable", http.StatusServiceUnavailable)
			return
		}
		result, err := cfg.Reload(envreg.Get("FLEET_ENV_FILE"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

// onSignal runs fn on every sig until ctx is cancelled, and returns a stop func
// that deregisters the signal. A separate channel/goroutine (not the shutdown
// loop) keeps the reload's state in scope and decouples reloads from the
// lifecycle signals. A panic in fn is contained per call (label names it in
// the recovery log), so one bad reload can't permanently disable the signal.
//
// Two users: config hot-reload on SIGUSR2 (#286) — SIGUSR2, not SIGHUP, to
// avoid clashing with log-rotation tooling — and MCP hot-reload on SIGHUP
// (#218), the canonical "reload configuration" signal.
func onSignal(ctx context.Context, sig os.Signal, label string, fn func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sig)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				// Recover PER iteration (not once around the loop).
				func() {
					defer safe.Recover(label, nil)
					fn()
				}()
			}
		}
	}()
	return func() { signal.Stop(ch) }
}

// reloadMCPServers hot-reloads the running Manager's MCP servers (#218). An
// injected credential owner re-reads its own bundle and returns public metadata;
// the local compatibility path re-reads the bundle here and builds throwaway
// specs without mutating the shared cfg.MCPServers.
func reloadMCPServers(ctx context.Context, mgr *agent.Manager) (*mcp.ReloadSummary, error) {
	if mgr == nil {
		return &mcp.ReloadSummary{}, nil
	}
	if mgr.MCPReloadOwnsConfig() {
		return mgr.ReloadMCPServers(ctx, nil)
	}
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return nil, fmt.Errorf("reload client-config bundle: %w", err)
	}
	specs := agent.BuildMCPSpecs(&config.Config{MCPServers: bundle.MCPServerConfigs()})
	summary, err := mgr.ReloadMCPServers(ctx, specs)
	if err == nil {
		// The picker's categories and needs-setup rows follow the same read.
		mgr.SetConnectorMeta(bundle.ConnectorMeta())
	}
	return summary, err
}

// mcpSchemaIssuesResponse is the GET /admin/mcp-servers/schema-issues body.
type mcpSchemaIssuesResponse struct {
	// Since is when this process started recording; a connector no turn has
	// used since then is not reflected.
	Since  time.Time                   `json:"since"`
	Issues []agentcore.ToolSchemaIssue `json:"issues"`
}

// mcpSchemaIssuesHandler serves GET /admin/mcp-servers/schema-issues from the
// runtime record the model boundary keeps (agentcore.ToolSchemaIssues).
func mcpSchemaIssuesHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcpSchemaIssuesResponse{
		Since:  agentcore.ToolSchemaRecordSince(),
		Issues: agentcore.ToolSchemaIssues(),
	})
}

// mcpReloadHandler serves POST /admin/mcp-servers/reload (#218): it hot-reloads
// the MCP catalog and returns a JSON ReloadSummary. mgr may be nil in
// route-walking tests (the route only needs to exist there); a real request
// against a nil mgr returns 503.
func mcpReloadHandler(mgr *agent.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			http.Error(w, "mcp reload unavailable", http.StatusServiceUnavailable)
			return
		}
		summary, err := reloadMCPServers(r.Context(), mgr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(summary)
	}
}

// logMCPReload logs the outcome of a SIGHUP-triggered MCP reload.
func logMCPReload(summary *mcp.ReloadSummary, err error) {
	if err != nil {
		log.Printf("mcp reload (SIGHUP): error: %v", err)
		return
	}
	log.Printf("mcp reload (SIGHUP): added=%d removed=%d restarted=%d unchanged=%d",
		len(summary.Added), len(summary.Removed), len(summary.Restarted), len(summary.Unchanged))
}

// logConfigReload logs the outcome of a SIGUSR2-triggered reload.
func logConfigReload(result config.ReloadResult, err error) {
	if err != nil {
		log.Printf("config reload (SIGUSR2): error: %v", err)
		return
	}
	log.Printf("config reload (SIGUSR2): %d changed, %d skipped, %d error(s)",
		len(result.Changed), len(result.Skipped), len(result.Errors))
	for _, ch := range result.Changed {
		log.Printf("config reload: %s: %q -> %q", ch.Key, ch.Old, ch.New)
	}
}
