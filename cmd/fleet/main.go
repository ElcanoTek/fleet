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
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/runner"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/sched/scheduler"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/ElcanoTek/fleet/internal/store"
)

func main() {
	// Subcommand dispatch. With no args (or any non-subcommand arg) fleet boots
	// THE fleet server (run). `fleet acp` instead runs fleet AS an ACP AGENT
	// over stdio (P-ACP-3 ingress): an external editor launches it and drives
	// fleet's OWN governed pipeline. The ingress path deliberately does NOT boot
	// the HTTP servers / scheduler / worker pool — it is a single-process stdio
	// adapter on the same governed turn.
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		if err := runACP(); err != nil {
			log.Fatalf("fleet acp: %v", err)
		}
		return
	}
	// `fleet mcp-broker` runs the out-of-process MCP credential broker over stdio
	// (issue #167): it holds the connector secrets + MCP subprocesses and serves
	// delegated MCP calls back to a parent fleet process. Like `acp`, it boots no
	// HTTP servers / scheduler — it is a single-purpose stdio adapter.
	if len(os.Args) > 1 && os.Args[1] == "mcp-broker" {
		if err := runMCPBroker(); err != nil {
			log.Fatalf("fleet mcp-broker: %v", err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatalf("fleet: %v", err)
	}
}

// preflightDefaultRuntimeImage fails closed (#159) when the bundle's DEFAULT
// runtime flavor is container-backed (native-acp / external acp) but its agent
// image is not present in the service user's container store. Without this, the
// first turn would die at `podman run` with an opaque error; here we surface an
// actionable startup error instead and refuse to boot — deliberately NOT
// degrading to the in-process loop, which would run uncontained on a deploy that
// believes it is containerized. Native-inprocess defaults (operator opted in via
// FLEET_ENABLE_INPROCESS_LOOP) need no image and pass through.
func preflightDefaultRuntimeImage(cfg *config.Config, bundle *clientconfig.Bundle, defaultRuntime string) error {
	def, ok := bundle.Runtime(defaultRuntime)
	if !ok || strings.TrimSpace(def.Image) == "" {
		return nil // in-process default (no image) — nothing to verify
	}
	runtimeBin := strings.TrimSpace(cfg.SandboxRuntime)
	if runtimeBin == "" {
		runtimeBin = "podman"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	//nolint:gosec // G204: runtimeBin is the operator-configured sandbox runtime and def.Image is a manifest ref (not request input); this is a read-only `image exists` probe.
	if err := exec.CommandContext(ctx, runtimeBin, "image", "exists", def.Image).Run(); err != nil {
		return fmt.Errorf(
			"default runtime flavor %q requires agent image %q, which is not in the %s store for this user; "+
				"build it (scripts/build-native-agent-image.sh, or re-run scripts/bootstrap.sh) or set FLEET_NATIVE_AGENT_IMAGE to a prebuilt ref. "+
				"Refusing to start; the in-process loop is NOT used as a silent fallback",
			def.Name, def.Image, runtimeBin)
	}
	log.Printf("runtimes: preflight ok — default flavor %q image %q present", def.Name, def.Image)
	return nil
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
	skillsDir := bundle.SkillsDir

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
	chatStore, err := store.Open(chatDB)
	if err != nil {
		return fmt.Errorf("open chat DB: %w", err)
	}
	defer chatStore.Close()
	log.Printf("chat DB connected + migrated")

	// Full-text search (#308): honor FLEET_SEARCH_ENABLED, then backfill the
	// message-content index for any pre-FTS messages in the background so startup
	// isn't blocked on a large walk. Idempotent + batched (see BackfillSearchContent).
	chatStore.SetSearchEnabled(cfg.SearchEnabled)
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
	if err := schedStorage.Initialize(schedDB); err != nil {
		return fmt.Errorf("open sched DB: %w", err)
	}
	defer schedStorage.Close()
	schedStorage.SetTimezone(timezone())
	log.Printf("sched DB connected + migrated")

	// Notes store + the live provider/proposer wired into BOTH drivers.
	notesStore := sched.NewStore(schedStorage.DB())
	notesProvider := &notesAdapter{store: notesStore}

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
	})
	if err != nil {
		return fmt.Errorf("build interactive engine: %w", err)
	}
	defer mgr.Close()

	// Wire the bundle's runtime-flavor catalog so a conversation can select a
	// flavor (native-acp sandboxed default; native-inprocess gated behind
	// FLEET_ENABLE_INPROCESS_LOOP). The native-agent image for native-acp turns is
	// the manifest's runtimes.native-acp.image.
	var nativeAgentImage string
	if acpRT, ok := bundle.Runtime(clientconfig.RuntimeNativeACP); ok {
		nativeAgentImage = acpRT.Image
	}
	// The interactive default is the bundle's declared default (native-acp
	// post-#159); FLEET_DEFAULT_RUNTIME overrides it against the gated catalog
	// (an unknown/gated value is ignored). Mirrors the scheduled resolution.
	defaultRuntime := bundle.DefaultRuntime()
	if want := strings.TrimSpace(cfg.DefaultRuntime); want != "" {
		if _, ok := bundle.Runtime(want); ok {
			defaultRuntime = want
		} else {
			// want/defaultRuntime are operator-set env / bundle flavor names, not request input.
			log.Printf("FLEET_DEFAULT_RUNTIME %q not found/selectable in bundle; using default %s", want, defaultRuntime)
		}
	}
	mgr.SetRuntimes(bundle.Runtimes(), defaultRuntime, nativeAgentImage)
	// defaultRuntime is a bundle/operator-env flavor name, not request input.
	log.Printf("runtimes: default=%s flavors=%d native_agent_image=%q",
		defaultRuntime, len(bundle.Runtimes()), nativeAgentImage)

	// Fail-closed preflight (#159): when the DEFAULT flavor is container-backed
	// (native-acp / external acp), its agent image must already exist in the
	// service user's container store — otherwise the very first turn dies at
	// `podman run` with an opaque error, in both chat and scheduled paths. Refuse
	// to start with an actionable message instead. We do NOT silently fall back to
	// the in-process loop: that would run uncontained on a deploy that believes it
	// is containerized. Skipped in MockMode (tests have no container runtime).
	if !cfg.MockMode {
		if err := preflightDefaultRuntimeImage(cfg, bundle, defaultRuntime); err != nil {
			return err
		}
	}

	// Health summary (#301): uptime + an injected scheduler worker/task provider
	// (adapts the sched store's dashboard stats) so the chat-side endpoint can
	// report a single-pane view without httpapi importing the sched packages.
	workerStats := func(context.Context) (*httpapi.WorkerStats, error) {
		ds, err := schedStorage.GetDashboardStats()
		if err != nil {
			return nil, err
		}
		return &httpapi.WorkerStats{
			TotalNodes:     ds.TotalNodes,
			ActiveNodes:    ds.ActiveNodes,
			IdleNodes:      ds.IdleNodes,
			QueuedTasks:    ds.PendingTasks,
			RunningTasks:   ds.RunningTasks,
			CompletedToday: ds.CompletedTasksToday,
			FailedToday:    ds.FailedTasksToday,
		}, nil
	}
	chatSrv := httpapi.New(cfg, mgr, chatStore,
		httpapi.WithClientConfig(bundle),
		httpapi.WithStartTime(startTime),
		httpapi.WithWorkerStats(workerStats),
	)

	// ── orchestrator HTTP (sched/handlers) ──
	keyMgr, err := apikeys.NewManager(filepath.Join(cfg.DataDir, "api_keys.json"), "")
	if err != nil {
		return fmt.Errorf("apikeys manager: %w", err)
	}
	hcfg := handlers.Config{
		AdminAPIKey:         os.Getenv("ADMIN_API_KEY"),
		RegistrationToken:   os.Getenv("REGISTRATION_TOKEN"),
		Version:             "fleet",
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
	// Let task creation reject a runtime flavor this deployment doesn't offer
	// (the bundle's gated catalog — e.g. native-inprocess when the in-process loop
	// is off, #159) instead of silently falling back at dispatch.
	h.SetRuntimeValidator(func(name string) bool {
		_, ok := bundle.Runtime(name)
		return ok
	})
	notesHandlers := handlers.NewNotesHandlers(notesStore, h)
	orchHandler := buildOrchestratorMux(h, notesHandlers)

	// ── scheduler ticker (promote scheduled→pending + recover leases) ──
	sch := scheduler.New(schedStorage, timezone())
	sch.Start()
	defer sch.Stop()

	// Resolve the scheduled execution flavor from FLEET_SCHEDULED_RUNTIME against
	// the bundle's runtimes catalog (process-wide; the scheduled Task model carries
	// no per-task runtime). An unknown / empty value falls back to native-inprocess.
	// native-acp scheduled tasks reuse the SAME native-agent image as interactive.
	// An EXTERNAL (type: acp / delegated_policy) flavor is admitted on the scheduler
	// ONLY behind the per-client opt-in below — otherwise the scheduled-external gate
	// fails it closed at dispatch (internal/agent/scheduled_external.go).
	// Default to the bundle's default flavor (native-acp post-#159, since
	// native-inprocess is gated out unless FLEET_ENABLE_INPROCESS_LOOP is set), so
	// scheduled and interactive share one default. FLEET_SCHEDULED_RUNTIME overrides
	// it; an unknown/gated value falls back to the bundle default.
	scheduledRuntime := bundle.DefaultRuntime()
	var scheduledFlavor clientconfig.Runtime
	if rt, ok := bundle.Runtime(scheduledRuntime); ok {
		scheduledFlavor = rt
	}
	if want := strings.TrimSpace(cfg.ScheduledRuntime); want != "" {
		if rt, ok := bundle.Runtime(want); ok {
			scheduledRuntime = rt.Name
			scheduledFlavor = rt
		} else {
			// want/scheduledRuntime are operator-set env / bundle flavor names, not request input.
			log.Printf("FLEET_SCHEDULED_RUNTIME %q not found/selectable in bundle; using default %s", want, scheduledRuntime)
		}
	}
	if scheduledRuntime == clientconfig.RuntimeNativeACP && nativeAgentImage == "" {
		log.Printf("warn: scheduled runtime native-acp selected but no native-agent image is configured; scheduled tasks will fall back to in-process")
	}
	// The per-client opt-in for ungoverned external agents on the scheduler. Default
	// false (the generic bundle leaves it unset): a scheduled-external task without
	// this is a LOUD ERROR at dispatch (fail-closed; no fallback to a native flavor).
	allowUngovernedScheduled := bundlePolicy.AllowUngovernedScheduledAgents
	externalScheduled := scheduledFlavor.Type == clientconfig.RuntimeTypeACP || scheduledFlavor.DelegatedPolicy
	if externalScheduled {
		log.Printf("scheduled runtime %q is EXTERNAL (containment tier, governance: delegated); allow_ungoverned_scheduled_agents=%v",
			scheduledRuntime, allowUngovernedScheduled)
		if !allowUngovernedScheduled {
			log.Printf("warn: scheduled-external runtime %q selected but allow_ungoverned_scheduled_agents is OFF; scheduled tasks will FAIL at dispatch (fail-closed, no fallback)", scheduledRuntime)
		}
	}
	log.Printf("scheduled runtime: flavor=%s native_agent_image=%q", scheduledRuntime, nativeAgentImage)

	// ── capped worker pool: TaskRunner = the scheduled agent over the SHARED sandbox pool ──
	taskRunner := scheduledrun.New(scheduledrun.Options{
		Config:                   cfg,
		Manager:                  mgr,
		NotesProvider:            notesProvider,
		NoteProposer:             notesProvider,
		PersonasDir:              personasDir,
		SystemPromptsDir:         systemPromptsDir,
		ProtocolsDir:             protocolsDir,
		Runtime:                  scheduledRuntime,
		NativeAgentImage:         nativeAgentImage,
		RuntimeFlavor:            scheduledFlavor,
		AllowUngovernedScheduled: allowUngovernedScheduled,
		// Resolve a task's per-task runtime-flavor override (Operations Center
		// picker) against the same bundle catalog the chat picker uses.
		ResolveRuntime: bundle.Runtime,
	})
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
	pool := runner.NewPool(schedStorage, taskRunner, runner.Config{Limiter: agentLimiter, DrainGrace: poolGrace})
	log.Printf("worker pool: scheduled cap=%d (shared box-wide limiter)", pool.Cap())

	// ── boot listeners ──
	chatAddr := addrOr(cfg.Addr, ":8080")
	orchAddr := orchestratorAddr()

	chatServer := &http.Server{Addr: chatAddr, Handler: securityHeadersMiddleware(chatSrv.Routes(), tlsActive(cfg)), ReadHeaderTimeout: 30 * time.Second}
	orchServer := &http.Server{Addr: orchAddr, Handler: orchHandler, ReadHeaderTimeout: 30 * time.Second}

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
		r.Post("/tasks/model", h.BulkSetTaskModel) // fleet-wide model re-assignment (admin-gated)
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
		r.Get("/nodes", h.ListNodes)
		r.Get("/nodes/{node_id}", h.GetNode)
		r.Get("/tasks", h.ListTasks)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Delete("/tasks/{task_id}", h.CancelTask)
		r.Get("/logs/{task_id}", h.GetLogs)
		r.Get("/stats", h.GetDashboardStats)
		r.Get("/api/me", h.GetCurrentUser)

		// Optional-MCP catalog + credential-account seats for the task-form
		// picker + admin table (read-only; never secret values). The web app
		// proxies /api/orchestrator/mcp-servers + /mcp-accounts to these.
		r.Get("/mcp-servers", h.GetMCPServers)
		r.Get("/mcp-accounts", h.GetMCPAccounts)

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

	// The two high-cost endpoints carry the sliding-window rate limiter
	// (per-API-key + global), so a runaway key can't flood the task queue or
	// drain the LLM budget. The admin key bypasses it (see SchedRateLimitMiddleware).
	r.With(h.SchedRateLimitMiddleware).Post("/tasks", h.CreateTask)
	r.With(h.SchedRateLimitMiddleware).Post("/upload", h.HandleUpload)
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
