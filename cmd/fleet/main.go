// Command fleet is THE Mega Box binary. It boots, in ONE process:
//
//   - the chat HTTP/SSE server (httpapi) on :8080, driven by the concrete
//     interactive turnEngine (agent.Manager over agentcore.Run);
//   - the orchestrator HTTP server (sched/handlers) on :8000;
//   - the scheduler ticker (sched/scheduler) — promotes scheduled→pending +
//     recovers expired leases every 30s;
//   - the capped in-process worker pool (internal/runner) whose TaskRunner is
//     the scheduled agent.Agent.Execute over the SAME shared sandbox pool.
//
// It opens BOTH Postgres pools (store for chat, sched/db for sched), each
// self-migrating on start, and wires the live sched-backed notes provider into
// BOTH the interactive engine's Deps and the runner's scheduled-agent Deps.
// Graceful shutdown drains the worker pool and closes the servers + pools.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/runner"
	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/scheduler"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fleet: %v", err)
	}
}

func run() error {
	// Load the client bundle first: it supplies the MCP catalog (built into
	// cfg.MCPServers), the supporting-doc dirs, and branding/empty-state. Its
	// manifest also tells us which connector env-var names to admit from the
	// .env file, so register them BEFORE config.Load reads the env.
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)
	log.Printf("client config: bundle=%s app=%q mcp_catalog=%d", bundle.Dir, bundle.Branding.AppName, len(bundle.MCPCatalog))

	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// The MCP catalog comes from the bundle manifest, gated on the now-loaded
	// process env.
	cfg.MCPServers = bundle.MCPServerConfigs()

	// The sandbox image is a per-client bundle artifact: resolve it from the
	// bundle manifest (sandbox.image when set — the opt-in prebuilt/registry
	// path — else sandbox.tag, the build-on-box default). An explicit
	// FLEET_SANDBOX_IMAGE / CHAT_SANDBOX_IMAGE in the process env still wins
	// (config.Load already populated cfg.SandboxImage from it). fleet does NOT
	// build the image here — bootstrap / scripts/build-sandbox-image.sh does;
	// this only feeds the resolved ref to the consuming sandbox pool.
	if strings.TrimSpace(cfg.SandboxImage) == "" {
		if ref := bundle.Sandbox().ResolvedImageRef(); ref != "" {
			cfg.SandboxImage = ref
			log.Printf("sandbox: image resolved from bundle = %s", ref)
		}
	}

	// Install the bundle's agent tool-behavior policy (parallel-safe tools,
	// critical-tool suffixes, substitute map). The generic bundle ships none, so
	// agentcore stays on its base generic critical suffixes. Must run before any
	// turn starts.
	bundlePolicy := bundle.AgentPolicy()
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		ParallelSafeTools:       bundlePolicy.ParallelSafeTools,
		CriticalToolSuffixes:    bundlePolicy.CriticalToolSuffixes,
		CriticalToolSubstitutes: bundlePolicy.CriticalToolSubstitutes,
	})

	personasDir := bundle.PersonasDir
	protocolsDir := bundle.ProtocolsDir
	systemPromptsDir := bundle.SystemPromptsDir

	// ── DB pools (both self-migrate on open) ──
	chatStore, err := store.Open(chatDSN(cfg))
	if err != nil {
		return fmt.Errorf("open chat DB: %w", err)
	}
	defer chatStore.Close()
	log.Printf("chat DB connected + migrated")

	schedStorage := storage.New()
	if err := schedStorage.Initialize(schedDSN()); err != nil {
		return fmt.Errorf("open sched DB: %w", err)
	}
	defer schedStorage.Close()
	schedStorage.SetTimezone(timezone(cfg))
	log.Printf("sched DB connected + migrated")

	// Notes store + the live provider/proposer wired into BOTH drivers.
	notesStore := sched.NewStore(schedStorage.DB())
	notesProvider := &notesAdapter{store: notesStore}

	// ── interactive engine (the concrete turnEngine) ──
	serverSpecs := buildMCPSpecs(cfg)
	mgr, err := agent.New(agent.ManagerOptions{
		Config:               cfg,
		ServerSpecs:          serverSpecs,
		PersonasDir:          personasDir,
		ProtocolsDir:         protocolsDir,
		SystemPromptsDir:     systemPromptsDir,
		ChatSystemPromptFile: "chat.md",
		NotesProvider:        notesProvider,
	})
	if err != nil {
		return fmt.Errorf("build interactive engine: %w", err)
	}
	defer mgr.Close()

	// Wire the bundle's runtime-flavor catalog so a conversation can select a
	// flavor (native-inprocess default, native-acp sandboxed). The native-agent
	// image for native-acp turns is the manifest's runtimes.native-acp.image.
	var nativeAgentImage string
	if acpRT, ok := bundle.Runtime(clientconfig.RuntimeNativeACP); ok {
		nativeAgentImage = acpRT.Image
	}
	mgr.SetRuntimes(bundle.Runtimes(), bundle.DefaultRuntime(), nativeAgentImage)
	log.Printf("runtimes: default=%s flavors=%d native_agent_image=%q",
		bundle.DefaultRuntime(), len(bundle.Runtimes()), nativeAgentImage)

	chatSrv := httpapi.New(cfg, mgr, chatStore, httpapi.WithClientConfig(bundle))

	// ── orchestrator HTTP (sched/handlers) ──
	keyMgr, err := apikeys.NewManager(filepath.Join(cfg.DataDir, "api_keys.json"), "")
	if err != nil {
		return fmt.Errorf("apikeys manager: %w", err)
	}
	hcfg := handlers.Config{
		AdminAPIKey:       os.Getenv("ADMIN_API_KEY"),
		RegistrationToken: os.Getenv("REGISTRATION_TOKEN"),
		Version:           "fleet",
		DataDir:           cfg.DataDir,
		Timezone:          timezone(cfg),
		ElcanoCookieName:  "elcano_auth",
	}
	h := handlers.New(hcfg, schedStorage, keyMgr)
	notesHandlers := handlers.NewNotesHandlers(notesStore, h)
	orchHandler := buildOrchestratorMux(h, notesHandlers)

	// ── scheduler ticker (promote scheduled→pending + recover leases) ──
	sch := scheduler.New(schedStorage, timezone(cfg))
	sch.Start()
	defer sch.Stop()

	// ── capped worker pool: TaskRunner = the scheduled agent over the SHARED sandbox pool ──
	taskRunner := newScheduledRunner(cfg, mgr, schedStorage, notesProvider, personasDir, systemPromptsDir, protocolsDir)
	pool := runner.NewPool(schedStorage, taskRunner, runner.Config{})
	log.Printf("worker pool: cap=%d", pool.Cap()) //nolint:gosec // G706 false positive: pool.Cap() is an int formatted with %d; it cannot carry CR/LF.

	// ── boot listeners ──
	chatAddr := addrOr(cfg.Addr, ":8080")
	orchAddr := orchestratorAddr()

	chatServer := &http.Server{Addr: chatAddr, Handler: chatSrv.Routes(), ReadHeaderTimeout: 30 * time.Second}
	orchServer := &http.Server{Addr: orchAddr, Handler: orchHandler, ReadHeaderTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Worker pool runs until ctx is cancelled, then drains.
	poolDone := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(poolDone)
	}()

	errCh := make(chan error, 2)
	go func() {
		log.Printf("chat-server listening on %s", chatAddr) //nolint:gosec // G706 false positive: chatAddr is an operator-configured bind address (env/flag), not request input.
		if err := chatServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("chat-server: %w", err)
		}
	}()
	go func() {
		log.Printf("orchestrator listening on %s", orchAddr)
		if err := orchServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("orchestrator: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("fleet: shutdown signal received; draining...")
	case err := <-errCh:
		log.Printf("fleet: listener error: %v; shutting down", err)
		stop()
	}

	// Graceful shutdown: stop accepting, drain the pool, close servers + pools.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = chatServer.Shutdown(shutdownCtx)
	_ = orchServer.Shutdown(shutdownCtx)
	<-poolDone // pool.Run drains taskWG before returning
	log.Printf("fleet: shutdown complete")
	return nil
}

// buildOrchestratorMux registers the orchestrator routes (chi), mirroring moc's
// auth groups, plus the P6b notes CRUD + proposal-decision routes (admin-gated).
func buildOrchestratorMux(h *handlers.Handlers, notes *handlers.NotesHandlers) http.Handler {
	r := chi.NewRouter()
	// ClientIPFromXFF replaces the deprecated, spoofable middleware.RealIP
	// (GHSA-3fxj-6jh8-hvhx et al.): with no trusted prefixes it reads the
	// rightmost (closest-hop) X-Forwarded-For entry and never trusts the
	// client-supplied leftmost values, storing the result for GetClientIP —
	// exactly what getClientIP() already expects.
	r.Use(middleware.ClientIPFromXFF())
	r.Use(middleware.Recoverer)
	r.Use(h.SecurityHeadersMiddleware)
	r.Use(h.BodySizeLimitMiddleware)
	r.Use(h.CSRFMiddleware)

	r.Get("/health", h.HealthCheck)
	r.Get("/api/config", h.GetDashboardConfig)

	// Registration (rate-limited).
	r.Group(func(r chi.Router) {
		r.Use(h.RateLimitMiddleware)
		r.Use(h.RegistrationAuthMiddleware)
		r.Post("/register", h.RegisterNode)
	})

	// Admin-gated mutations.
	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		r.Delete("/nodes/{node_id}", h.UnregisterNode)
		r.Post("/tasks/cleanup", h.CleanupHistory)
		r.Post("/users", h.CreateUser)
		r.Post("/keys", h.CreateAPIKey)
		r.Get("/keys", h.ListAPIKeys)
		r.Get("/keys/audit", h.GetAuditLog)
		r.Get("/keys/{key_id}", h.GetAPIKey)
		r.Post("/keys/{key_id}/rotate", h.RotateAPIKey)
		r.Post("/keys/{key_id}/revoke", h.RevokeAPIKey)
		r.Delete("/keys/{key_id}", h.DeleteAPIKey)

		// Notes admin: CRUD + proposal decisions (NOTES_WIKI_SPEC §6).
		r.Post("/notes", notes.CreateNote)
		r.Put("/notes/{slug}", notes.UpdateNote)
		r.Delete("/notes/{slug}", notes.ArchiveNote)
		r.Post("/notes/proposals/{id}/publish", notes.PublishProposal)
		r.Post("/notes/proposals/{id}/reject", notes.RejectProposal)
	})

	// Admin-or-user reads.
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/nodes", h.ListNodes)
		r.Get("/nodes/{node_id}", h.GetNode)
		r.Get("/tasks", h.ListTasks)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Delete("/tasks/{task_id}", h.CancelTask)
		r.Get("/logs/{task_id}", h.GetLogs)
		r.Get("/stats", h.GetDashboardStats)
		r.Get("/api/me", h.GetCurrentUser)

		// Notes reads (admin + scoped user).
		r.Get("/notes", notes.ListNotes)
		r.Get("/notes/{slug}", notes.GetNote)
		r.Get("/notes/proposals", notes.ListProposals)
		r.Get("/notes/proposals/{id}", notes.GetProposal)
	})

	// Node lease/report endpoints (kept for protocol compatibility; the
	// in-process pool uses storage directly).
	r.Group(func(r chi.Router) {
		r.Use(h.NodeAuthMiddleware)
		r.Post("/nodes/heartbeat", h.NodeHeartbeat)
		r.Get("/tasks/pending", h.GetPendingTask)
		r.Post("/status", h.ReportStatus)
		r.Post("/logs", h.SubmitLogs)
	})

	r.Post("/tasks", h.CreateTask)
	r.Post("/upload", h.HandleUpload)
	r.Get("/files/{filename}", h.HandleDownload)
	r.Post("/auth/login", h.Login)
	r.Get("/auth/elcano-login", h.ElcanoLogin)
	r.Post("/auth/logout", h.ElcanoLogout)

	return r
}

// ── DSN / addr resolution ──

// chatDSN resolves the interactive chat Postgres DSN.
func chatDSN(cfg *config.Config) string {
	if v := strings.TrimSpace(os.Getenv("FLEET_CHAT_DATABASE_URL")); v != "" {
		return v
	}
	return cfg.DatabaseURL
}

// schedDSN resolves the orchestrator (sched) Postgres DSN. The sched db layer
// also reads DATABASE_URL itself, so an explicit override wins; otherwise the
// sched layer falls back to DATABASE_URL / DB_* parts.
func schedDSN() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_SCHED_DATABASE_URL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("SCHED_DATABASE_URL")); v != "" {
		return v
	}
	// Empty → db.Init reads DATABASE_URL / DB_* parts.
	return ""
}

func timezone(_ *config.Config) string {
	if v := strings.TrimSpace(os.Getenv("FLEET_TIMEZONE")); v != "" {
		return v
	}
	return "UTC"
}

func addrOr(addr, def string) string {
	if strings.TrimSpace(addr) == "" {
		return def
	}
	return addr
}

func orchestratorAddr() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_ORCHESTRATOR_ADDR")); v != "" {
		return v
	}
	return ":8000"
}

// buildMCPSpecs converts config.MCPServers into the agent.MCPServerSpec map the
// interactive Manager connects at startup. Credentials live in the env the
// config builder populated; they reach MCP subprocesses host-side only.
func buildMCPSpecs(cfg *config.Config) map[string]agent.MCPServerSpec {
	out := make(map[string]agent.MCPServerSpec, len(cfg.MCPServers))
	for name, sc := range cfg.MCPServers {
		out[name] = agent.MCPServerSpec{
			Enabled:       sc.Enabled,
			Command:       sc.Command,
			Args:          sc.Args,
			Env:           sc.Env,
			URL:           sc.URL,
			Headers:       sc.Headers,
			ToolAllowlist: sc.ToolAllowlist,
		}
	}
	return out
}

// ── notes adapter: the sched-backed NotesProvider + NoteProposer ──

// notesAdapter implements agentcore.NotesProvider + agentcore.NoteProposer over
// the sched notes store. Wired into BOTH the interactive engine and the
// scheduled runner so both modes inject the SAME admin-curated knowledge base
// and stage propose_note edits into the same pending queue.
type notesAdapter struct {
	store *sched.Store
}

func (a *notesAdapter) PublishedNotes(ctx context.Context) ([]agentcore.Note, error) {
	notes, err := a.store.ListPublishedNotes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.Note, len(notes))
	for i, n := range notes {
		out[i] = agentcore.Note{Slug: n.Slug, Title: n.Title, Body: n.Body}
	}
	return out, nil
}

func (a *notesAdapter) Propose(slug, title, body, reason string) (string, error) {
	p, err := a.store.CreateProposal(context.Background(), slug, title, body, reason, "agent")
	if err != nil {
		return "", err
	}
	return p.ID.String(), nil
}
