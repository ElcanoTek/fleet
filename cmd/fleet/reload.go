package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/settings"
)

const reloadScopeHeader = "X-Fleet-Reload-Scope"
const reloadScopeModelTiers = "model_tiers"

type reloadConfigOption func(*reloadConfigDeps)

type reloadConfigDeps struct {
	wsSvc      *settings.Service
	inTierLock func(func() error) error
}

func withReloadWorkspaceSettings(wsSvc *settings.Service, inTierLock func(func() error) error) reloadConfigOption {
	return func(d *reloadConfigDeps) {
		d.wsSvc = wsSvc
		d.inTierLock = inTierLock
	}
}

// reloadConfigHandler serves POST /admin/reload-config (#286): it re-reads the
// reloadable settings from the env file + process environment and returns a JSON
// diff of what changed / was skipped (non-reloadable) / errored. When called
// with X-Fleet-Reload-Scope: model_tiers by `fleet model set`/`reset`, it
// instead refreshes only persisted model-tier rows from workspace_settings under
// the server's in-process model-rollout lock. cfg may be nil in route-walking
// tests (the route only needs to exist there); a real request against a nil cfg
// returns 503.
func reloadConfigHandler(cfg *config.Config, opts ...reloadConfigOption) http.HandlerFunc {
	var deps reloadConfigDeps
	for _, opt := range opts {
		if opt != nil {
			opt(&deps)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg == nil {
			http.Error(w, "config reload unavailable", http.StatusServiceUnavailable)
			return
		}
		if scope := strings.TrimSpace(r.Header.Get(reloadScopeHeader)); scope != "" {
			if scope != reloadScopeModelTiers {
				http.Error(w, "unknown reload scope", http.StatusBadRequest)
				return
			}
			reloadModelTiersOnly(w, r, deps)
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

func reloadModelTiersOnly(w http.ResponseWriter, r *http.Request, deps reloadConfigDeps) {
	if deps.wsSvc == nil {
		http.Error(w, "workspace settings unavailable", http.StatusServiceUnavailable)
		return
	}
	runLocked := deps.inTierLock
	if runLocked == nil {
		runLocked = func(fn func() error) error { return fn() }
	}
	result := config.ReloadResult{
		ReloadedAt: time.Now().UTC(),
		Changed:    []config.FieldChange{},
		Skipped:    []config.SkippedField{},
		Errors:     []config.ReloadError{},
	}
	if err := runLocked(func() error {
		oldDef := agentcore.CurrentDefaultModel()
		oldTask := agentcore.CurrentTaskModel()
		oldFB := agentcore.CurrentFallbackModel()
		if err := deps.wsSvc.ApplyKeys(r.Context(), "default_model", "advanced_model", "task_model"); err != nil {
			return err
		}
		if nextDef := agentcore.CurrentDefaultModel(); nextDef != oldDef {
			result.Changed = append(result.Changed, config.FieldChange{Key: "default_model", Old: oldDef, New: nextDef})
		}
		if nextTask := agentcore.CurrentTaskModel(); nextTask != oldTask {
			result.Changed = append(result.Changed, config.FieldChange{Key: "task_model", Old: oldTask, New: nextTask})
		}
		if nextFB := agentcore.CurrentFallbackModel(); nextFB != oldFB {
			result.Changed = append(result.Changed, config.FieldChange{Key: "advanced_model", Old: oldFB, New: nextFB})
		}
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
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
					defer safe.RecoverContext(ctx, label, nil)
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
		slog.Error("mcp reload (SIGHUP): failed", "err", err)
		return
	}
	slog.Info("mcp reload (SIGHUP)", "added", len(summary.Added), "removed", len(summary.Removed), "restarted", len(summary.Restarted), "unchanged", len(summary.Unchanged))
}

// logConfigReload logs the outcome of a SIGUSR2-triggered reload.
func logConfigReload(ctx context.Context, result config.ReloadResult, err error) {
	log := logging.From(ctx)
	if err != nil {
		log.Error("config reload (SIGUSR2): failed", "err", err)
		return
	}
	log.Info("config reload (SIGUSR2): done", "changed", len(result.Changed), "skipped", len(result.Skipped), "errors", len(result.Errors))
	for _, ch := range result.Changed {
		log.Info("config reload: key changed", "key", ch.Key, "old", ch.Old, "new", ch.New)
	}
	for _, e := range result.Errors {
		log.Warn("config reload: setting not applied", "key", e.Key, "err", e.Error)
	}
}
