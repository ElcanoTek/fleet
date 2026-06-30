// Command fleet is THE single fleet process. It boots, in ONE process:
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
//
// Graceful shutdown (#278) distinguishes SIGTERM (deployment restart) from SIGINT
// (dev Ctrl-C): on SIGTERM it stops admitting new work (/healthz → 503 so a load
// balancer drains it), then drains in-flight chat turns AND scheduled tasks within
// FLEET_SHUTDOWN_GRACE_SECONDS before force-cancelling the stragglers and closing
// the listeners; SIGINT is the immediate path. It emits sd_notify READY=1 /
// STOPPING=1 (a no-op off systemd) and answers SIGUSR1 with an in-flight snapshot.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/observability"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/runner"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/scheduler"
	"github.com/ElcanoTek/fleet/internal/sched/slamonitor"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/ElcanoTek/fleet/internal/secretbox"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
	"github.com/ElcanoTek/fleet/internal/version"
)

// approvalExpirySweepInterval is how often the background sweep auto-denies
// pending approvals past their default-deny deadline (#225). Bounded by the
// approval card's expectation that a stale request closes within a minute.
const approvalExpirySweepInterval = 30 * time.Second

func main() {
	// Subcommand dispatch. With no args (or any non-subcommand arg) fleet boots
	// THE fleet server (run).
	//
	// `fleet version` (also `--version` / `-v`) prints the build identity — the
	// release version stamped from the top-level VERSION file plus the VCS
	// revision — and exits. It boots nothing, so it works on a box where the DBs
	// or sandbox are down.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("fleet " + version.String())
		return
	}
	// `fleet mcp-broker` instead runs the out-of-process MCP credential broker
	// over stdio (issue #167): it holds the connector secrets + MCP subprocesses
	// and serves delegated MCP calls back to a parent fleet process. It boots no
	// HTTP servers / scheduler — it is a single-purpose stdio adapter.
	if len(os.Args) > 1 && os.Args[1] == "mcp-broker" {
		if err := runMCPBroker(); err != nil {
			log.Fatalf("fleet mcp-broker: %v", err)
		}
		return
	}
	// `fleet validate-config` runs the preflight checks (#248) — env vars, the
	// manifest bundle, MCP servers, the databases, credentials, the sandbox, and
	// the model API — against the SAME loaders the server boots through, then exits
	// 0 (all blocking checks passed) or 1. It starts no servers and runs no
	// migrations; it is a read-only diagnostic for CI and pre-`systemctl start`.
	if len(os.Args) > 1 && os.Args[1] == "validate-config" {
		os.Exit(runValidateConfig(os.Args[2:]))
	}
	if err := run(); err != nil {
		log.Fatalf("fleet: %v", err)
	}
}

func run() error {
	startTime := time.Now() // process start, for the admin health summary uptime (#301)
	// Load the client bundle first: it supplies the MCP catalog (built into
	// cfg.MCPServers), the supporting-doc dirs, and branding/empty-state. Its
	// manifest also tells us which connector env-var names to admit from the
	// .env file, so register them BEFORE config.Load reads the env.
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)

	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Sentry error tracking (#193): OPT-IN via FLEET_SENTRY_DSN. With the DSN
	// unset (the default) this is a complete no-op — no SDK init, no transport,
	// zero per-call overhead — and the startup log says "sentry: disabled".
	// When set, Init wires RedactEvent as the BeforeSend hook so no secret
	// (MCP/connector credentials, auth headers) ever leaves the host via the
	// Sentry transport. The deferred Flush is registered BEFORE any goroutine
	// starts so a panic anywhere downstream is captured and drained at shutdown.
	sentryActive := observability.Init(observability.Options{
		DSN:         cfg.SentryDSN,
		Environment: cfg.Environment,
		Redact:      agentcore.RedactSecrets,
	})
	if sentryActive {
		defer observability.Flush(2 * time.Second)
	}

	// Process log file sink (#298): OPT-IN. With FLEET_LOG_FILE unset (the
	// default) this is a no-op and the process logs only to stderr — which
	// journald rotates under the shipped systemd unit (ADR-0004), so the default
	// behaviour is unchanged. When set (typically a container/non-systemd box),
	// it ALSO tees the standard log lines to a size/age/backup-rotated file. It
	// rotates the existing lines as-is; it does NOT restructure them to JSON
	// (slog migration #178 is separate). Configured before the first diagnostic
	// log line so the file sink captures startup too.
	logCloser, err := logging.Configure(logging.Config{
		File:       cfg.Log.File,
		MaxSizeMB:  cfg.Log.MaxSizeMB,
		MaxAgeDays: cfg.Log.MaxAgeDays,
		MaxBackups: cfg.Log.MaxBackups,
		Compress:   cfg.Log.Compress,
	})
	if err != nil {
		return fmt.Errorf("configure log file sink (FLEET_LOG_FILE=%q): %w", cfg.Log.File, err)
	}
	if logCloser != nil {
		defer logCloser.Close()
		log.Printf("logging: file sink enabled at %s (max_size=%dMB max_age=%dd max_backups=%d compress=%t)",
			cfg.Log.File, cfg.Log.MaxSizeMB, cfg.Log.MaxAgeDays, cfg.Log.MaxBackups, cfg.Log.Compress)
	}

	log.Printf("client config: bundle=%s app=%q mcp_catalog=%d", bundle.Dir, bundle.Branding.AppName, len(bundle.MCPCatalog))
	// The MCP catalog comes from the bundle manifest, gated on the now-loaded
	// process env.
	cfg.MCPServers = bundle.MCPServerConfigs()
	// Inline http_tools (issue #261): resolved host-side (auth headers expanded from
	// the process env) and registered onto the credentialed MCP client alongside the
	// MCP catalog. Empty in the generic bundle.
	cfg.HTTPTools = bundle.HTTPToolConfigs()

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

	resolveSandboxRuntimeInto(cfg, bundle)

	// Install the bundle's agent tool-behavior policy (parallel-safe tools,
	// critical-tool suffixes, substitute map). The generic bundle ships none, so
	// agentcore stays on its base generic critical suffixes. Must run before any
	// turn starts.
	bundlePolicy := bundle.AgentPolicy()
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		ParallelSafeTools:       bundlePolicy.ParallelSafeTools,
		CriticalToolSuffixes:    bundlePolicy.CriticalToolSuffixes,
		CriticalToolSubstitutes: bundlePolicy.CriticalToolSubstitutes,
		CriticalToolTimeouts:    bundlePolicy.CriticalToolTimeouts,
	})

	// Install the bundle's custom model-pricing overrides (#297). The generic
	// bundle ships none, so cost accounting stays on the OpenRouter-returned
	// price (the pre-#297 default). Must run before any turn starts.
	agentcore.ConfigurePricing(toAgentcorePricing(bundle.Pricing()))

	personasDir := bundle.PersonasDir
	protocolsDir := bundle.ProtocolsDir
	systemPromptsDir := bundle.SystemPromptsDir
	skillsDir := bundle.SkillsDir

	// Per-persona tool allowlists (Gate-4, #294): translate the bundle manifest's
	// personas: block into the agentcore form once and hand the SAME map to both
	// drivers. The generic bundle declares no personas: block, so this is empty
	// and behaviour is unchanged (every persona sees all permitted tools). The gate
	// only NARROWS — a persona's allowlist can subtract from, never widen beyond,
	// the server/credential gates.
	personaPolicies := toAgentcorePersonaPolicies(bundle)

	// Shared box-wide admission limiter: bounds TOTAL in-flight agent turns
	// (interactive chat + scheduled tasks) to FLEET_MAX_CONCURRENT_AGENTS, with a
	// slice reserved for interactive chat so background tasks can never starve it.
	// Handed to BOTH the interactive Manager and the scheduled worker pool so the
	// cap is genuinely box-wide.
	agentLimiter := admission.New(cfg.MaxConcurrentAgents, admission.DefaultReserved(cfg.MaxConcurrentAgents))
	// Args are ints (%d) from operator-set FLEET_MAX_CONCURRENT_AGENTS — no CR/LF, no log-injection vector.
	log.Printf("admission: total=%d scheduled_max=%d interactive_reserved=%d",
		agentLimiter.Total(), agentLimiter.SchedulableSlots(), agentLimiter.Total()-agentLimiter.SchedulableSlots())

	// ── DB pools (both self-migrate on open) ──
	chatDB := chatDSN(cfg)
	schedDB := schedDSN()
	if err := ensureDistinctDatabases(chatDB, schedDB); err != nil {
		return err
	}
	chatStore, err := store.Open(chatDB, store.PoolConfig{
		MaxOpenConns:    cfg.ChatDBPool.MaxOpenConns,
		MaxIdleConns:    cfg.ChatDBPool.MaxIdleConns,
		ConnMaxIdleTime: cfg.ChatDBPool.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.ChatDBPool.ConnMaxLifetime,
		ConnectTimeout:  cfg.ChatDBPool.ConnectTimeout,
	})
	if err != nil {
		return fmt.Errorf("open chat DB: %w", err)
	}
	defer chatStore.Close()
	log.Printf("chat DB connected + migrated")

	// Full-text search (#308): honor FLEET_SEARCH_ENABLED, then backfill the
	// message-content index for any pre-FTS messages in the background so startup
	// isn't blocked on a large walk. Idempotent + batched (see BackfillSearchContent).
	chatStore.SetSearchEnabled(cfg.SearchEnabled)
	// Conversation soft-delete (#279): honor FLEET_CONVERSATION_SOFT_DELETE. When
	// enabled, delete operations tombstone rows (deleted_at = NOW()) instead of
	// hard-deleting; reads hide tombstoned rows and SweepExpired permanently
	// purges rows older than 30 days. Default off = unchanged hard-delete behavior.
	chatStore.SetSoftDelete(cfg.ConversationSoftDelete)
	if cfg.ConversationSoftDelete {
		log.Printf("conversation soft-delete: ENABLED (deleted rows tombstoned, purged after 30 days)")
	}
	if cfg.SearchEnabled {
		safe.Go("store.fts-backfill", func() {
			bfCtx, bfCancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer bfCancel()
			n, err := chatStore.BackfillSearchContent(bfCtx)
			switch {
			case err != nil:
				//nolint:gosec // G706: err wraps internal DB/migration errors (not request input) and n is an int — neither can forge a log line.
				log.Printf("search backfill: %v (after %d rows)", err, n)
			case n > 0:
				//nolint:gosec // G706: only an int count is formatted — no request-input string is logged.
				log.Printf("search backfill: indexed %d pre-existing message(s)", n)
			}
		})
	} else {
		log.Printf("full-text search: DISABLED (FLEET_SEARCH_ENABLED=false)")
	}

	// Persist every recovered panic (#241) to the chat DB's panic_events table so
	// operators can query crashes even if stdout/journald lost the line. The hook
	// is best-effort and bounded so it never stalls or re-panics inside recovery.
	safe.PanicEventWriter = func(location, message string, stack []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := chatStore.RecordPanic(ctx, location, message, string(stack)); err != nil {
			log.Printf("panic event persist failed (location=%s): %v", location, err)
		}
	}

	schedStorage := storage.New()
	if err := schedStorage.Initialize(schedDB, scheddb.PoolConfig{
		MaxOpenConns:    cfg.SchedDBPool.MaxOpenConns,
		MaxIdleConns:    cfg.SchedDBPool.MaxIdleConns,
		ConnMaxIdleTime: cfg.SchedDBPool.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.SchedDBPool.ConnMaxLifetime,
		ConnectTimeout:  cfg.SchedDBPool.ConnectTimeout,
	}); err != nil {
		return fmt.Errorf("open sched DB: %w", err)
	}
	defer schedStorage.Close()
	schedStorage.SetTimezone(timezone())
	log.Printf("sched DB connected + migrated")

	// Bootstrap operators (#458): provision/promote the configured emails as
	// orchestrator admins so they reach the Operations Center seamlessly via the
	// shared chat session cookie. See seedBootstrapAdmins for the rationale.
	if err := seedBootstrapAdmins(schedStorage); err != nil {
		return err
	}

	// Pool health metrics (#276): expose both pools' live db.Stats() at scrape.
	metrics.RegisterDBPool(map[string]func() metrics.DBPoolStats{
		"chat":  func() metrics.DBPoolStats { return toDBPoolStats(chatStore.PoolStats()) },
		"sched": func() metrics.DBPoolStats { return toDBPoolStats(schedStorage.DB().Stats()) },
	})

	// Notes store + the live provider/proposer wired into BOTH drivers.
	notesStore := sched.NewStore(schedStorage.DB())
	notesProvider := &notesAdapter{store: notesStore}

	// Self-improvement (#285): the task-memory store over the SAME sched store,
	// handed only to the scheduled runner — interactive chat has no task to scope
	// memory to. Prompt/knowledge self-improvement stays on the existing,
	// DB-backed propose_note path (notesProvider, wired into both drivers below).
	taskMemoryStore := &taskMemoryAdapter{store: notesStore}

	// ── per-user remote (hosted) MCP servers + OAuth (#443) ──
	// remoteMCPSvc is the concrete service (for the HTTP endpoints); remoteMCPResolver
	// is the same value as the agent-side interface (for the chat + scheduled overlay).
	// Both nil when the feature is unconfigured, leaving every path unchanged.
	remoteMCPSvc, remoteMCPResolver := setupRemoteMCP(cfg, chatStore)

	// ── interactive engine (the concrete turnEngine) ──
	serverSpecs := scheduledrun.BuildMCPSpecs(cfg)
	mgr, err := agent.New(agent.ManagerOptions{
		Config:               cfg,
		ServerSpecs:          serverSpecs,
		PersonasDir:          personasDir,
		ProtocolsDir:         protocolsDir,
		SkillsDir:            skillsDir,
		SystemPromptsDir:     systemPromptsDir,
		ChatSystemPromptFile: "chat.md",
		Limiter:              agentLimiter, // shared box-wide cap; interactive turns admitted through it
		NotesProvider:        notesProvider,
		NoteProposer:         notesProvider, // same adapter; wires propose_note for every interactive turn
		PersonaPolicies:      personaPolicies,
		RemoteMCP:            remoteMCPResolver,
	})
	if err != nil {
		return fmt.Errorf("build interactive engine: %w", err)
	}
	defer mgr.Close()

	// Health summary (#301): uptime + an injected scheduler worker/task provider
	// (adapts the sched store's dashboard stats) so the chat-side endpoint can
	// report a single-pane view without httpapi importing the sched packages.
	chatOpts := []httpapi.Option{
		httpapi.WithClientConfig(bundle),
		httpapi.WithStartTime(startTime),
		httpapi.WithVersion(version.String()),
		httpapi.WithWorkerStats(workerStatsProvider(schedStorage)),
	}
	if remoteMCPSvc != nil {
		chatOpts = append(chatOpts, httpapi.WithRemoteMCP(remoteMCPSvc))
	}
	chatSrv := httpapi.New(cfg, mgr, chatStore, chatOpts...)

	// Auto-approve-in-test (#225) bypasses the human-in-the-loop approval gate.
	// It is off by default and intended only for CI/test pipelines with a mocked
	// backend; log loudly so it can never be on in production unnoticed.
	if cfg.AutoApproveInTest {
		log.Printf("WARNING: FLEET_AUTO_APPROVE_IN_TEST is ON — every staged critical tool is auto-approved without human review. Do NOT use this in production.")
	}

	// ── orchestrator HTTP (sched/handlers) ──
	keyMgr, err := apikeys.NewManager(filepath.Join(cfg.DataDir, "api_keys.json"), "")
	if err != nil {
		return fmt.Errorf("apikeys manager: %w", err)
	}
	hcfg := handlers.Config{
		AdminAPIKey:         os.Getenv("ADMIN_API_KEY"),
		Version:             version.String(),
		DataDir:             cfg.DataDir,
		Timezone:            timezone(),
		DefaultTaskTimezone: defaultTaskTimezone(),
		// Sliding-window rate limits for POST /tasks + /upload (0 disables a window).
		SchedRateLimitPerMinute:       envIntDefault("FLEET_SCHED_RATE_LIMIT_PER_MINUTE", 60),
		SchedRateLimitPerDay:          envIntDefault("FLEET_SCHED_RATE_LIMIT_PER_DAY", 500),
		SchedGlobalRateLimitPerMinute: envIntDefault("FLEET_SCHED_RATE_LIMIT_GLOBAL_PER_MINUTE", 200),
		ElcanoCookieName:              "elcano_auth",
		// Reuse the chat shared token so the Next proxy's X-User-Email path is
		// trusted by the orchestrator too (#157). cfg.SharedToken is guaranteed
		// non-empty by config.Validate.
		SharedToken: cfg.SharedToken,
		// Cost-forecast inputs (#233): mirror the runtime selection so POST
		// /tasks/estimate projects against the same model + iteration cap + cost
		// ceiling a real dispatch uses. cfg.MaxIterations is the per-task default
		// the runner applies when a task omits one.
		DefaultTaskModel:     cfg.TaskModel,
		MaxCostUSD:           cfg.MaxCostUSD,
		DefaultMaxIterations: cfg.MaxIterations,
		// Per-task sandbox-limit ceilings (#205): validateSandboxLimits rejects an
		// override above these. 0 = no ceiling.
		SandboxMemoryMaxMB: cfg.SandboxMemoryMaxMB,
		SandboxCPUsMax:     cfg.SandboxCPUsMax,
		SandboxPidsMax:     cfg.SandboxPidsMax,
	}
	h := handlers.New(hcfg, schedStorage, keyMgr)
	// Wire the orchestrator's read-only Optional-MCP catalog + credential-account
	// seats from the SAME in-process source the chat side uses: the Manager's
	// Optional-server catalog (descriptions, tool counts, and the per-server
	// credential-account seat names it derives from the bundle's AccountVars via
	// creds.AccountsFor). Never exposes secret values — only server + account
	// names. This is what makes the scheduled-task MCP picker + credential admin
	// table work.
	h.SetMCPCatalogProvider(func() []handlers.MCPServerCatalogEntry {
		catalog := mgr.MCPServerCatalog()
		out := make([]handlers.MCPServerCatalogEntry, 0, len(catalog))
		for _, info := range catalog {
			out = append(out, handlers.MCPServerCatalogEntry{
				Name:        info.Name,
				DisplayName: info.DisplayName,
				Description: info.Description,
				ToolCount:   info.ToolCount,
				Enabled:     info.EnabledByDefault,
				// Seats are derived from the bundle's AccountVars by the Manager's
				// catalog (creds.AccountsFor) — names only, never secret values.
				Accounts: info.Accounts,
			})
		}
		return out
	})
	// Surface each caller's per-user remote (hosted) MCP servers (#443) in the
	// orchestrator picker too (#466) — see wireRemoteMCPCatalog. No-op when the
	// feature is disabled (remoteMCPSvc == nil).
	wireRemoteMCPCatalog(h, remoteMCPSvc)
	// Wire the orchestrator's read-only task-template catalog from the loaded
	// client bundle (#262). Templates are pre-filled scheduled-task shapes the
	// task-create UI offers as a starting point; the task itself is still created
	// through POST /tasks, so a template grants no capability the create path
	// doesn't already validate. Pure config read-through — never persisted.
	h.SetTaskTemplateProvider(func() []clientconfig.TaskTemplate {
		return bundle.TaskTemplates
	})
	notesHandlers := handlers.NewNotesHandlers(notesStore, h)
	orchHandler := buildOrchestratorMux(h, notesHandlers)

	// ── scheduler ticker (promote scheduled→pending + recover leases) ──
	sch := scheduler.New(schedStorage, timezone())
	// Automatic run-history retention (#252): a daily sweep prunes old terminal
	// runs, always keeping the most recent KeepRunsPerTask per task. Off when
	// RunLogRetentionDays<=0.
	sch.SetRetention(cfg.RunLogRetentionDays, cfg.KeepRunsPerTask, cfg.CleanupHour)
	// Log archival (#272): a daily sweep compresses (optionally encrypts) old
	// terminal-task log payloads in place to shrink the sched DB. Off by default
	// (LogArchiveAfterDays<=0). The optional encryption key is wired host-side onto
	// the storage layer and never logged.
	schedStorage.SetLogArchiveKey(cfg.LogArchiveEncryptionKey)
	sch.SetLogArchival(cfg.LogArchiveAfterDays)
	// Anti-starvation promotion (#230): each tick promotes pending tasks that have
	// waited past the window up to the High floor so a stream of higher-priority
	// work can't starve them. Off when TaskStarvationWindowMinutes<=0.
	sch.SetStarvationWindow(cfg.TaskStarvationWindowMinutes)
	sch.Start()
	defer sch.Stop()

	// ── capped worker pool: TaskRunner = the scheduled agent over the SHARED sandbox pool ──
	taskRunner := scheduledrun.New(scheduledrun.Options{
		Config:           cfg,
		Manager:          mgr,
		NotesProvider:    notesProvider,
		NoteProposer:     notesProvider,
		PersonasDir:      personasDir,
		SystemPromptsDir: systemPromptsDir,
		ProtocolsDir:     protocolsDir,
		PersonaPolicies:  personaPolicies,
		// Captain's Log persistent memory (#198, #285): handed to a run only when its
		// task set instruction_self_improve. Caps bound per-task memory growth
		// (FLEET_TASK_MEMORY_MAX_*).
		TaskMemory: taskMemoryStore,
		TaskMemoryConfig: tools.TaskMemoryConfig{
			MaxKeys:       cfg.TaskMemoryMaxKeys,
			MaxValueBytes: cfg.TaskMemoryMaxValueBytes,
		},
		// Record per-iteration telemetry for looped tasks (#179).
		IterationStore: schedStorage,
		// Back the built-in create_task tool (#277) so a scheduled task that opted
		// in (allow_task_creation) can enqueue follow-up tasks through the shared
		// sched storage. Tasks without the flag never see the tool.
		TaskEnqueuer: schedStorage,
		// Per-user remote (hosted) MCP + OAuth (#443): the same service the chat
		// path uses, plus a creator-UUID → email resolver (the sched username IS
		// the chat email for the elcano-auth tier). Both nil when the feature is
		// off, leaving scheduled runs unchanged.
		RemoteMCP:  remoteMCPResolver,
		OwnerEmail: ownerEmailResolver(schedStorage),
	})
	// Wire the cost-forecast's system-prompt resolver (#233) from the SAME runner
	// that assembles the prompt at dispatch, so POST /tasks/estimate counts the
	// exact system prompt a real run would send. Read-only; never dispatches.
	h.SetSystemPromptProvider(taskRunner.SystemPromptForPersona)
	// Reclaim sandbox containers orphaned by a PRIOR crash before building the
	// pool: they run `--detach --rm` under conmon, so a non-graceful exit leaves
	// them holding host RAM/PIDs across systemd restarts. Best-effort — log and
	// continue if podman is absent (e.g. mock/dev) or the sweep fails.
	pruneCtx, pruneCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if n, err := sandbox.PruneOrphanedContainers(pruneCtx, "podman"); err != nil {
		log.Printf("startup: prune orphaned sandbox containers: %v", err)
	} else if n > 0 {
		log.Printf("startup: pruned %d orphaned sandbox container(s) from a prior run", n)
	}
	pruneCancel()

	// Share the process shutdown grace with the pool so its in-flight-task drain
	// uses the same budget as the chat-turn drain (#278). A non-positive grace
	// (operator set FLEET_SHUTDOWN_GRACE_SECONDS=0) maps to a negative DrainGrace
	// so the pool force-cancels immediately instead of substituting its default.
	poolGrace := shutdownGrace(cfg)
	if poolGrace <= 0 {
		poolGrace = -1
	}
	// Task-completion notifier (#208): host-side outbound email/webhook on a
	// scheduled task reaching a terminal status. Config comes from the host
	// env-file (FLEET_SMTP_*/FLEET_WEBHOOK_*/FLEET_NOTIFY_*); secrets stay
	// host-side and never enter the sandbox or the log. Default OFF — with none of
	// those vars set, taskNotifier.Enabled() is false and the fire path is a no-op.
	taskNotifier := notify.New(notify.Load())
	if taskNotifier.Enabled() {
		log.Printf("task notifications: enabled")
	}
	pool := runner.NewPool(schedStorage, taskRunner, runner.Config{
		Limiter:       agentLimiter,
		DrainGrace:    poolGrace,
		Notifier:      taskNotifier,
		PublicURLBase: os.Getenv("FLEET_PUBLIC_URL"),
	})
	log.Printf("worker pool: scheduled cap=%d (shared box-wide limiter)", pool.Cap())

	// Wire the live SSE run-log stream (#200): GET /tasks/{id}/stream attaches a
	// client to the pool's per-task event buffer for an in-progress task. The two
	// TaskStream interfaces are structurally identical; the closure bridges the
	// runner type to the handler type. Safe to set after buildOrchestratorMux — the
	// handler reads h.taskStreamLookup per request, and the pool isn't running yet.
	registry := pool.StreamRegistry()
	h.SetTaskStreamProvider(func(taskID uuid.UUID) (handlers.TaskStream, bool) {
		return registry.Lookup(taskID)
	})

	// Metrics gauges (#176): live in-flight turn counts + warm sandbox depth,
	// evaluated at each /metrics scrape. Extracted to keep run() within the
	// cyclomatic budget.
	registerRuntimeMetrics(chatSrv.ActiveTurns, pool.ActiveTasks, mgr.SandboxPool())

	// ── boot listeners ──
	chatAddr := addrOr(cfg.Addr, ":8080")
	orchAddr := orchestratorAddr()

	// Liveness + readiness probes (#215) on BOTH ports, sharing one check set
	// and one drain signal (chatSrv.BeginShutdown is the single graceful-drain
	// trigger, so both ports report not_ready while draining).
	readinessChecks := buildReadinessChecks(cfg, chatStore, schedStorage.DB())
	chatHandler := securityHeadersMiddleware(withHealthProbes(chatSrv.Routes(), startTime, chatSrv.IsDraining, readinessChecks), tlsActive(cfg))
	orchHandlerWithProbes := withHealthProbes(orchHandler, startTime, chatSrv.IsDraining, readinessChecks)

	chatServer := &http.Server{Addr: chatAddr, Handler: chatHandler, ReadHeaderTimeout: 30 * time.Second}
	orchServer := &http.Server{Addr: orchAddr, Handler: orchHandlerWithProbes, ReadHeaderTimeout: 30 * time.Second}

	// Signal handling (#278) distinguishes deployment restart from dev Ctrl-C:
	//   - SIGTERM → graceful: stop admitting, drain in-flight chat turns + tasks
	//     within FLEET_SHUTDOWN_GRACE_SECONDS, then force-cancel the stragglers.
	//   - SIGINT  → immediate: cancel everything now (the dev fast-exit path).
	//   - SIGUSR1 → diagnostic: log current in-flight counts WITHOUT shutting down.
	// ctx drives the claim loop / pool; cancelling it stops NEW work but (by
	// design) does NOT touch detached chat turns or in-flight tasks — those drain
	// through their own decoupled contexts so the grace period is meaningful.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SLA monitor (#274): a 60s sweep that warns/fails in-flight tasks running
	// past their expected_duration_minutes threshold. Started alongside the
	// scheduler ticker; stops on ctx cancel OR explicit Stop (whichever the
	// shutdown path reaches first). Like the scheduler, a panic in one tick is
	// contained (safe.Recover) so it never kills the process.
	slaMon := slamonitor.New(schedStorage)
	slaMon.Start(ctx)
	defer slaMon.Stop()

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	defer signal.Stop(sigCh)

	// Worker pool runs until ctx is cancelled, then drains. Guarded so a panic in
	// the pool loop is a contained, logged event — and poolDone still closes
	// (deferred) so shutdown's <-poolDone can't hang.
	poolDone := make(chan struct{})
	go func() {
		defer safe.Recover("cmd.pool.run", nil)
		defer close(poolDone)
		pool.Run(ctx)
	}()

	errCh := make(chan error, 2)
	go func() {
		// A panic in the serve loop triggers graceful shutdown (errCh) rather than
		// crashing the process or hanging; handler panics are caught upstream.
		defer safe.Recover("cmd.chat-server", func(any) { errCh <- fmt.Errorf("chat-server: panicked") })
		// serveChat logs the listening line and terminates TLS when
		// FLEET_TLS_MODE is manual/auto (default off = plain HTTP, unchanged).
		if err := serveChat(chatServer, cfg); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("chat-server: %w", err)
		}
	}()
	go func() {
		defer safe.Recover("cmd.orchestrator", func(any) { errCh <- fmt.Errorf("orchestrator: panicked") })
		log.Printf("orchestrator listening on %s", orchAddr)
		if err := orchServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("orchestrator: %w", err)
		}
	}()
	// Approval expiry sweep (#225): enforces the default-DENY-on-timeout contract
	// for the web approval path. The per-turn SweepExpired only fires when a turn
	// runs, so an idle conversation with a staged card would never auto-deny — this
	// ticker closes that gap. Bound to ctx so it stops on shutdown; a panic is
	// contained so the sweep can't crash the process.
	go func() {
		defer safe.Recover("cmd.approval-expiry-sweep", nil)
		ticker := time.NewTicker(approvalExpirySweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if n, err := chatSrv.SweepExpiredApprovals(sweepCtx); err != nil {
					log.Printf("approval expiry sweep: %v", err)
				} else if n > 0 {
					//nolint:gosec // G706: only an integer count is formatted (%d) — no request-derived string is logged, so no CR/LF log-line forgery is possible.
					log.Printf("approval expiry sweep: auto-denied %d timed-out approval(s)", n)
				}
				cancel()
			}
		}
	}()

	// Listeners are bound; tell a systemd-aware supervisor we are ready (no-op
	// when NOTIFY_SOCKET is unset, i.e. non-systemd / dev / tests).
	grace := shutdownGrace(cfg)
	//nolint:gosec // G706: grace is a time.Duration (digits+unit) derived from operator config, not request input — it cannot forge a log line.
	log.Printf("fleet: shutdown grace = %s (FLEET_SHUTDOWN_GRACE_SECONDS)", grace)
	sdNotify("READY=1")

	// Block until a terminal signal / listener error decides graceful vs immediate,
	// then drain. Extracted so run() stays within the cyclomatic budget.
	graceful := awaitShutdown(sigCh, errCh, chatSrv, pool, agentLimiter)
	performShutdown(graceful, grace, cancel, chatSrv, pool, poolDone, chatServer, orchServer)
	return nil
}

// awaitShutdown blocks until a terminal signal or a fatal listener error,
// returning whether shutdown should be graceful (SIGTERM — deployment restart)
// or immediate (SIGINT — dev Ctrl-C — or a listener error). SIGUSR1 logs a
// diagnostic in-flight snapshot and keeps waiting (no shutdown).
func awaitShutdown(sigCh <-chan os.Signal, errCh <-chan error, chatSrv *httpapi.Server, pool *runner.Pool, lim *admission.Limiter) bool {
	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGTERM:
				log.Printf("fleet: SIGTERM received; beginning graceful shutdown")
				return true
			case syscall.SIGINT:
				log.Printf("fleet: SIGINT received; shutting down immediately")
				return false
			case syscall.SIGUSR1:
				//nolint:gosec // G706: only integer counters are formatted (%d) — no string from request input is logged, so no CR/LF log-line forgery is possible.
				log.Printf("fleet: status — active_chat_turns=%d active_sched_tasks=%d admission_inflight=%d/%d",
					chatSrv.ActiveTurns(), pool.ActiveTasks(), lim.InFlight(), lim.Total())
			}
		case err := <-errCh:
			log.Printf("fleet: listener error: %v; shutting down", err)
			return false
		}
	}
}

// performShutdown runs the drain sequence: stop admitting, drain in-flight chat
// turns + scheduled tasks within the grace budget (force-cancelling stragglers),
// then close the listeners. graceful=false is the fast path (cancel everything
// now). cancel stops the claim loop / scheduler-fed pool.
func performShutdown(graceful bool, grace time.Duration, cancel context.CancelFunc, chatSrv *httpapi.Server, pool *runner.Pool, poolDone <-chan struct{}, chatServer, orchServer *http.Server) {
	// Tell systemd we are stopping so it waits out the drain up to TimeoutStopSec.
	sdNotify("STOPPING=1")
	// Stop admitting new chat turns + flip /healthz to 503 (load balancers stop
	// routing here) and notify attached SSE clients to reconnect elsewhere.
	chatSrv.BeginShutdown()

	if graceful {
		// Stop the claim loop + scheduler-fed pool; in-flight tasks keep their
		// decoupled context and drain within the pool's own grace budget (parallel).
		cancel()
		graceCtx, graceStop := context.WithTimeout(context.Background(), grace)
		if chatSrv.DrainTurns(graceCtx) {
			log.Printf("fleet: all in-flight chat turns drained within grace")
		} else {
			//nolint:gosec // G706: grace is a time.Duration (digits+unit) and n is an int count — neither can forge a log line; values are operator-config-derived, not request input.
			log.Printf("fleet: grace period (%s) expired; force-cancelled %d in-flight chat turn(s)", grace, chatSrv.CancelInflightTurns())
		}
		graceStop()
		<-poolDone // pool ran its own grace-bounded task drain in parallel
	} else {
		// Immediate path (SIGINT / listener error): cancel everything now.
		cancel()
		pool.ForceCancel()
		if n := chatSrv.CancelInflightTurns(); n > 0 {
			//nolint:gosec // G706: only an int count is formatted (%d) — no request-input string is logged, so no log-line forgery.
			log.Printf("fleet: cancelled %d in-flight chat turn(s)", n)
		}
		<-poolDone
	}

	// Close the HTTP listeners last: handlers have returned and detached work has
	// drained or been cancelled, so Shutdown completes promptly. Each server gets
	// the full grace budget and they close in parallel (not one off the other's
	// remainder).
	closeServers(grace, chatServer, orchServer)
	log.Printf("fleet: shutdown complete")
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
	r.Use(orchestratorMetricsMiddleware) // #176: record request count + latency
	r.Use(h.SecurityHeadersMiddleware)
	r.Use(h.BodySizeLimitMiddleware)
	r.Use(h.CSRFMiddleware)

	r.Get("/health", h.HealthCheck)
	r.Get("/api/config", h.GetDashboardConfig)

	// Admin-gated mutations.
	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		// Prometheus scrape endpoint (#176) — admin-API-key gated like other
		// sensitive reads; cost/token data must not be public.
		r.Get("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte(metrics.Render()))
		})
		r.Post("/tasks/cleanup", h.CleanupHistory)
		r.Post("/tasks/model", h.BulkSetTaskModel) // fleet-wide model re-assignment (admin-gated)
		// Pending-queue inspection (#230): per-tier depth + oldest wait, so an
		// operator can see backlog and starvation. Admin-gated like the other
		// sensitive reads.
		r.Get("/admin/queue", h.QueueStats)
		r.Post("/users", h.CreateUser)
		r.Post("/keys", h.CreateAPIKey)
		r.Get("/keys", h.ListAPIKeys)
		r.Get("/keys/audit", h.GetAuditLog)
		r.Get("/keys/{key_id}", h.GetAPIKey)
		r.Get("/keys/{key_id}/spending", h.GetKeySpending)
		r.Post("/keys/{key_id}/reset-spending", h.ResetKeySpending)
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
		// SLA report (#274): per-prompt actual-duration p50/p95 + breach rate over a
		// window. Registered here (not under AdminAuthMiddleware) so the Next proxy's
		// header-trust/bearer path resolves the caller into a principal; the handler
		// then gates on PermissionAdmin (#458). The bare admin-API-key gate made it
		// unreachable from the dashboard — the proxy can never send X-API-Key.
		r.Get("/sla-report", h.GetSLAReport)
		r.Get("/tasks", h.ListTasks)
		// /tasks/tags is registered before /tasks/{task_id} so the static segment
		// wins over the wildcard (#212 tag catalogue). /tasks/export and
		// /tasks/import are likewise static segments that must precede the
		// {task_id} wildcard (#238).
		r.Get("/tasks/tags", h.GetTagCatalogue)
		r.Get("/tasks/export", h.HandleTaskExport)
		r.Post("/tasks/import", h.HandleTaskImport)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Get("/tasks/{task_id}/output", h.GetTaskOutput)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Post("/tasks/{task_id}/tags", h.UpdateTaskTags)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Delete("/tasks/{task_id}", h.CancelTask)
		r.Get("/logs/{task_id}", h.GetLogs)
		// Live SSE run-log stream for an in-progress task, falling back to a one-shot
		// replay of the persisted log once finished (#200). Same auth/ownership gate
		// as /logs/{task_id}.
		r.Get("/tasks/{task_id}/stream", h.StreamTaskLogs)
		// Workspace file browser (#287): list + download artifacts the task's agent
		// wrote into its per-run workspace. Stricter than the generic task gate —
		// the handler restricts access to the admin or the task's creator and
		// enforces the shared path-traversal guard on every file access.
		r.Get("/tasks/{task_id}/workspace", h.TaskWorkspace)
		r.Get("/tasks/{task_id}/workspace/*", h.TaskWorkspaceFile)
		r.Get("/stats", h.GetDashboardStats)
		r.Get("/api/me", h.GetCurrentUser)

		// Optional-MCP catalog + credential-account seats for the task-form
		// picker + admin table (read-only; never secret values). The web app
		// proxies /api/orchestrator/mcp-servers + /mcp-accounts to these.
		r.Get("/mcp-servers", h.GetMCPServers)
		r.Get("/mcp-accounts", h.GetMCPAccounts)

		// Read-only task-template catalog for the task-create UI's "new task from a
		// template" affordance (#262). The web app proxies
		// /api/orchestrator/task-templates to this. Never persists or creates a task.
		r.Get("/task-templates", h.ListTaskTemplates)

		// Notes reads (admin + scoped user).
		r.Get("/notes", notes.ListNotes)
		r.Get("/notes/{slug}", notes.GetNote)
		r.Get("/notes/proposals", notes.ListProposals)
		r.Get("/notes/proposals/{id}", notes.GetProposal)
	})

	// The two high-cost endpoints carry the sliding-window rate limiter
	// (per-API-key + global), so a runaway key can't flood the task queue or
	// drain the LLM budget. The admin key bypasses it (see SchedRateLimitMiddleware).
	r.With(h.SchedRateLimitMiddleware).Post("/tasks", h.CreateTask)
	// Batch task submission (#227): accepts up to MaxBatchSize TaskCreate recipes
	// in one call. Atomic mode wraps the insert in a single transaction; the
	// default (non-atomic) is best-effort with a 207 Multi-Status. Same auth +
	// rate limiter as POST /tasks; the handler additionally charges the scoped
	// key's hourly cap for (N-1) extra tasks so a batch is never a rate-limit
	// bypass.
	r.With(h.SchedRateLimitMiddleware).Post("/tasks/batch", h.CreateTaskBatch)
	// Pre-submission cost forecast (#233): same body, auth, and rate limiter as
	// POST /tasks, but pure local computation — it creates nothing.
	r.With(h.SchedRateLimitMiddleware).Post("/tasks/estimate", h.EstimateTask)
	r.With(h.SchedRateLimitMiddleware).Post("/upload", h.HandleUpload)
	r.Get("/files/{filename}", h.HandleDownload)

	// Webhook triggers (#177): authenticated by per-trigger HMAC-SHA256, NOT the
	// admin API key, so external services (GitHub, Slack, CI) can fire tasks
	// without admin credentials. Deliberately outside every auth group.
	r.With(h.SchedRateLimitMiddleware).Post("/triggers/{slug}", h.HandleWebhookTrigger)
	r.Post("/auth/login", h.Login)
	r.Get("/auth/elcano-login", h.ElcanoLogin)
	r.Post("/auth/logout", h.ElcanoLogout)

	return r
}

// ── DSN / addr resolution ──

// chatDSN resolves the interactive chat Postgres DSN.
// toDBPoolStats adapts a database/sql pool snapshot into the metrics package's
// DB-agnostic shape (#276).
func toDBPoolStats(s sql.DBStats) metrics.DBPoolStats {
	return metrics.DBPoolStats{
		MaxOpenConns:        s.MaxOpenConnections,
		OpenConns:           s.OpenConnections,
		InUse:               s.InUse,
		Idle:                s.Idle,
		WaitCount:           s.WaitCount,
		WaitDurationSeconds: s.WaitDuration.Seconds(),
		MaxIdleClosed:       s.MaxIdleClosed,
		MaxLifetimeClosed:   s.MaxLifetimeClosed,
	}
}

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

func timezone() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_TIMEZONE")); v != "" {
		return v
	}
	return "UTC"
}

// defaultTaskTimezone is the IANA timezone applied to a scheduled task created
// without an explicit one. Distinct from FLEET_TIMEZONE (the server clock) so an
// operator can set an org default for task scheduling without moving the system
// clock. Empty defaults to "UTC", matching prior behaviour.
func defaultTaskTimezone() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_DEFAULT_TIMEZONE")); v != "" {
		return v
	}
	return "UTC"
}

// seedBootstrapAdmins provisions (or promotes) the configured emails as
// orchestrator admins at boot (#458). The list
// (FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS, comma-separated emails) is authoritative
// on every boot and idempotent; empty = no bootstrap. This is the ONLY automatic
// orchestrator grant: a non-bootstrap chat user is still NOT a member (they get a
// clear "ask an admin" page, never silent admin), preserving the deliberate
// chat/orchestrator membership separation (ADR-0005). Extracted from run() to
// keep it within the cyclomatic budget.
func seedBootstrapAdmins(schedStorage *storage.Storage) error {
	admins := bootstrapAdmins()
	if len(admins) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, email := range admins {
		if err := schedStorage.EnsureAdminUser(ctx, email); err != nil {
			return fmt.Errorf("seed orchestrator bootstrap admin: %w", err)
		}
	}
	log.Printf("orchestrator bootstrap admin(s) ensured: %d", len(admins))
	return nil
}

// bootstrapAdmins parses FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS — a comma-separated
// list of emails to provision (or promote) as orchestrator admins at boot (#458)
// — returning the de-duplicated, lowercased, non-empty entries. Empty/unset =
// no bootstrap.
func bootstrapAdmins() []string {
	raw := strings.TrimSpace(os.Getenv("FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS"))
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, part := range strings.Split(raw, ",") {
		email := strings.ToLower(strings.TrimSpace(part))
		if email == "" {
			continue
		}
		if _, dup := seen[email]; dup {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, email)
	}
	return out
}

// envIntDefault reads an integer env var, returning def when unset or
// unparseable. An explicit "0" is honored (e.g. to disable a rate-limit window).
func envIntDefault(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// toAgentcorePricing translates the bundle's pricing config (a low-level
// clientconfig data struct) into the agentcore.PricingConfig the accounting path
// consumes (#297). A blank fallback is left blank so agentcore applies its
// OpenRouter default; the manifest loader already validated the fallback value
// and the override rates.
func toAgentcorePricing(p clientconfig.PricingConfig) agentcore.PricingConfig {
	out := agentcore.PricingConfig{Fallback: agentcore.PricingFallback(p.Fallback)}
	if len(p.Overrides) > 0 {
		out.Overrides = make([]agentcore.PricingOverride, 0, len(p.Overrides))
		for _, o := range p.Overrides {
			out.Overrides = append(out.Overrides, agentcore.PricingOverride{
				Model:                          o.Model,
				InputCostPerMillionTokens:      o.InputCostPerMillionTokens,
				OutputCostPerMillionTokens:     o.OutputCostPerMillionTokens,
				CacheReadCostPerMillionTokens:  o.CacheReadCostPerMillionTokens,
				CacheWriteCostPerMillionTokens: o.CacheWriteCostPerMillionTokens,
			})
		}
	}
	return out
}

// toAgentcorePersonaPolicies translates the bundle manifest's personas: block
// (#294) into the agentcore.PersonaToolPermissions map both drivers consume,
// keyed by persona basename. An entry whose allow+deny lists are both empty is
// SKIPPED (it is the explicit no-narrowing case; carrying it would only add a
// map lookup that resolves to the same passthrough). The generic bundle declares
// no personas: block, so this returns nil and behaviour is unchanged. The
// manifest loader already validated names are non-blank and unique.
func toAgentcorePersonaPolicies(bundle *clientconfig.Bundle) map[string]agentcore.PersonaToolPermissions {
	if bundle == nil || len(bundle.Personas) == 0 {
		return nil
	}
	out := make(map[string]agentcore.PersonaToolPermissions, len(bundle.Personas))
	for _, p := range bundle.Personas {
		policy, ok := bundle.PersonaToolPolicy(p.Name)
		if !ok || (len(policy.Allow) == 0 && len(policy.Deny) == 0) {
			continue
		}
		// Key by the normalized basename so a manifest "name: code-reviewer.yaml"
		// and a run's "code-reviewer" persona resolve to the same entry. The
		// accessor returns defensive copies, so the slices are ours to hand off.
		out[personaKey(p.Name)] = agentcore.PersonaToolPermissions{
			Allow: policy.Allow,
			Deny:  policy.Deny,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// personaKey normalizes a persona reference to its bare basename — stripping any
// directory and trailing .yaml/.yml — so the manifest map keys match the basename
// the drivers look a run's persona up by.
func personaKey(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	if ext := filepath.Ext(base); ext == ".yaml" || ext == ".yml" {
		base = strings.TrimSuffix(base, ext)
	}
	return strings.TrimSpace(base)
}

// ensureDistinctDatabases fails fast when the chat and sched DSNs resolve to the
// SAME database. The two layers use structurally incompatible `users` schemas
// and different migration runners, so sharing one DB corrupts startup — an
// operator who set only DATABASE_URL (a documented fallback for both) would
// otherwise hit a confusing low-level migration error. Best-effort: compares the
// host+dbname of the URL form (the form the docs use); a key=value DSN is left
// to the drivers.
func ensureDistinctDatabases(chatDSN, schedDSN string) error {
	if strings.TrimSpace(schedDSN) == "" {
		schedDSN = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	c := dbIdentity(chatDSN)
	if c != "" && c == dbIdentity(schedDSN) {
		return fmt.Errorf("chat and sched must use SEPARATE databases (incompatible users schemas + migration runners) but both resolve to %q; set distinct FLEET_CHAT_DATABASE_URL and FLEET_SCHED_DATABASE_URL", c)
	}
	return nil
}

// dbIdentity returns host:port/dbname for a postgres URL DSN, or "" when the DSN
// is not a parseable URL.
func dbIdentity(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host + u.Path
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

// ── graceful shutdown helpers (#278) ──

// resolveSandboxRuntimeInto resolves the sandbox OCI runtime (#217) into
// cfg.SandboxRuntime with the SAME precedence as the image: an explicit
// FLEET_SANDBOX_RUNTIME env (already in cfg.SandboxRuntime) wins, else the
// bundle manifest's sandbox.runtime fills it. Both the env and the bundle are
// operator-authored deployment config — the manifest already controls the
// sandbox image and Containerfile — so selecting the runtime is a trusted
// operator choice, not an untrusted downgrade. The friendly name "libkrun" is
// normalized to podman's "krun" once, here, so every downstream consumer (pool,
// preflight, readiness probe, validate-config) keys off the same value.
// Extracted from run() to keep it within the cyclomatic budget.
func resolveSandboxRuntimeInto(cfg *config.Config, bundle *clientconfig.Bundle) {
	// One shared resolver (sandbox.ResolveRuntime) so boot, validate-config, and
	// cutlass all apply the same env-wins-else-bundle precedence + normalization.
	resolved := sandbox.ResolveRuntime(cfg.SandboxRuntime, bundle.Sandbox().Runtime)
	if resolved != strings.TrimSpace(cfg.SandboxRuntime) && resolved != "" {
		//nolint:gosec // G706: runtime name is operator config (FLEET_SANDBOX_RUNTIME / bundle manifest), quoted with %q — not request input.
		log.Printf("sandbox: runtime resolved to %q (podman OCI runtime name)", resolved)
	}
	// Always write back the normalized value so cfg carries the canonical name
	// (also collapses whitespace-padded env values the loader didn't trim).
	cfg.SandboxRuntime = resolved
}

// registerRuntimeMetrics wires the pull-at-scrape gauges (#176): in-flight turn
// counts (interactive/scheduled) and warm sandbox depth. Extracted from run() to
// keep it within the cyclomatic budget.
func registerRuntimeMetrics(activeTurns, activeTasks func() int, sandboxPool *sandbox.Pool) {
	metrics.RegisterActiveAgents(activeTurns, activeTasks)
	if sandboxPool != nil {
		// Parked-and-ready containers (operationally interesting "how warm now"),
		// not the configured target size.
		metrics.RegisterSandboxPoolSize(func() int { _, avail := sandboxPool.Stats(); return avail })
	}
}

// orchestratorMetricsMiddleware records per-request count + latency for the
// Prometheus /metrics endpoint (#176), labeled by chi route pattern (so high-
// cardinality path params don't explode the series), method, and status.
func orchestratorMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		metrics.RecordHTTPRequest(route, r.Method, strconv.Itoa(ww.Status()), time.Since(start).Seconds())
	})
}

// workerStatsProvider adapts the sched store's dashboard stats into the
// httpapi.WorkerStats the admin health summary embeds (#301), keeping httpapi
// free of any sched-package import. Extracted from run() to keep it within the
// cyclomatic budget.
func workerStatsProvider(schedStorage *storage.Storage) func(context.Context) (*httpapi.WorkerStats, error) {
	return func(context.Context) (*httpapi.WorkerStats, error) {
		ds, err := schedStorage.GetDashboardStats()
		if err != nil {
			return nil, err
		}
		return &httpapi.WorkerStats{
			QueuedTasks:    ds.PendingTasks,
			RunningTasks:   ds.RunningTasks,
			CompletedToday: ds.CompletedTasksToday,
			FailedToday:    ds.FailedTasksToday,
		}, nil
	}
}

// setupRemoteMCP wires the per-user remote-MCP + OAuth feature (#443). It is
// enabled only when an encryption key AND a public base URL are configured;
// otherwise it fails closed (returns nil, nil) and the endpoints report the
// feature off. On enable it installs the token cipher on the chat store (secrets
// encrypted at rest) and starts an hourly sweep of abandoned OAuth-flow rows.
// The returned *Service backs the HTTP endpoints; the same value, typed as the
// agent resolver interface, backs the chat + scheduled overlay.
func setupRemoteMCP(cfg *config.Config, chatStore *store.Store) (*remotemcp.Service, agent.RemoteMCPResolver) {
	if len(cfg.MCPOAuthEncryptionKey) == 0 || cfg.PublicBaseURL == "" {
		log.Printf("remote MCP OAuth: disabled (set FLEET_MCP_OAUTH_ENCRYPTION_KEY + FLEET_PUBLIC_BASE_URL to enable)")
		return nil, nil
	}
	cipher, err := secretbox.NewCipher(cfg.MCPOAuthEncryptionKey)
	if err != nil {
		// Config already validates the key length, so this is belt-and-suspenders:
		// disable rather than crash the whole server on a bad key.
		log.Printf("remote MCP OAuth: disabled — invalid encryption key: %v", err)
		return nil, nil
	}
	chatStore.SetTokenCipher(cipher)
	svc := remotemcp.NewService(chatStore, remotemcp.Config{
		PublicBaseURL:     cfg.PublicBaseURL,
		AllowInsecureHTTP: cfg.RemoteMCPAllowInsecureHTTP,
	})
	// Sweep abandoned OAuth flow rows hourly (single-use + expiry already guard
	// correctness; this just reclaims rows). A process-lifetime daemon.
	safe.Go("remote-mcp.flow-sweep", func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			swCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, err := chatStore.SweepExpiredOAuthFlows(swCtx); err != nil {
				log.Printf("remote-mcp: oauth flow sweep: %v", err)
			}
			cancel()
		}
	})
	//nolint:gosec // G706: PublicBaseURL is operator-set config (env var), not request input — it can't forge a log line.
	log.Printf("remote MCP OAuth: ENABLED (per-user hosted servers; redirect %s/api/oauth/mcp/callback)", cfg.PublicBaseURL)
	return svc, svc
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
// servers are returned; they carry no credential seats (auth is the brokered
// per-user token) and are auto-applied to ALL the owner's runs by the run
// overlay, so the UI renders them connected/auto-available rather than as a
// per-task toggle. A lookup error is logged and treated as "no remote servers"
// so it never breaks the bundle catalog.
func remoteMCPCatalogProvider(svc *remotemcp.Service) func(context.Context, string) []handlers.MCPServerCatalogEntry {
	return func(ctx context.Context, email string) []handlers.MCPServerCatalogEntry {
		conns, err := svc.ConnectedServersForUser(ctx, email)
		if err != nil {
			log.Printf("orchestrator mcp catalog: remote server lookup failed: %v", err)
			return nil
		}
		out := make([]handlers.MCPServerCatalogEntry, 0, len(conns))
		for _, c := range conns {
			out = append(out, handlers.MCPServerCatalogEntry{
				Name:        c.Name,
				DisplayName: c.Name,
				Description: "Remote MCP server you connected (" + c.URL + "). Auto-available to your scheduled tasks.",
				Enabled:     true,
				Remote:      true,
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

// shutdownGrace resolves the graceful-shutdown grace period from
// FLEET_SHUTDOWN_GRACE_SECONDS (config default 30). A non-positive value means
// "no wait" (force-cancel immediately) and is returned as 0.
func shutdownGrace(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.ShutdownGraceSeconds <= 0 {
		return 0
	}
	return time.Duration(cfg.ShutdownGraceSeconds) * time.Second
}

// closeServers shuts the HTTP listeners down in parallel, each with the full
// grace budget (a hung one can't eat the other's time), so both drain their
// already-finishing connections without one starving the other. A non-positive
// budget falls back to 30s so Shutdown still has a bounded deadline.
func closeServers(grace time.Duration, servers ...*http.Server) {
	budget := grace
	if budget <= 0 {
		budget = 30 * time.Second
	}
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			defer safe.Recover("cmd.closeServers", nil)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			_ = s.Shutdown(shutdownCtx)
		}(srv)
	}
	wg.Wait()
}

// sdNotify sends one state line to systemd's notify socket (NOTIFY_SOCKET),
// e.g. "READY=1" once listeners are bound and "STOPPING=1" when draining. It is
// a no-op when the socket is unset (non-systemd / dev / tests) and best-effort
// otherwise — no new dependency, errors are intentionally swallowed. Handles the
// abstract-namespace form ('@' → leading NUL) systemd uses.
func sdNotify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	name := sock
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
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

// ── self-improvement adapter (#285): the sched-backed task-memory store, handed
// to the scheduled runner and used only by tasks that opted into Captain's Log. ──

// taskMemoryAdapter implements tools.TaskMemoryStore over the sched store,
// converting the sched row type to the tools-layer type so the tools package
// needs no dependency on sched.
type taskMemoryAdapter struct {
	store *sched.Store
}

func (a *taskMemoryAdapter) UpsertTaskMemory(ctx context.Context, taskID uuid.UUID, key, value string, maxKeys, maxValueBytes int) error {
	return a.store.UpsertTaskMemory(ctx, taskID, key, value, maxKeys, maxValueBytes)
}

func (a *taskMemoryAdapter) GetTaskMemory(ctx context.Context, taskID uuid.UUID, key string) (string, error) {
	return a.store.GetTaskMemory(ctx, taskID, key)
}

func (a *taskMemoryAdapter) ListTaskMemories(ctx context.Context, taskID uuid.UUID) ([]tools.TaskMemory, error) {
	mems, err := a.store.ListTaskMemories(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]tools.TaskMemory, len(mems))
	for i, m := range mems {
		out[i] = tools.TaskMemory{Key: m.Key, Value: m.Value}
	}
	return out, nil
}
