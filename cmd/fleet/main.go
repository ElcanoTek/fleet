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
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/admincli"
	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/apiversion"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/datasets"
	"github.com/ElcanoTek/fleet/internal/diskguard"
	"github.com/ElcanoTek/fleet/internal/guardrail"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/observability"
	"github.com/ElcanoTek/fleet/internal/otelsetup"
	"github.com/ElcanoTek/fleet/internal/ownerdigest"
	"github.com/ElcanoTek/fleet/internal/piiredact"
	"github.com/ElcanoTek/fleet/internal/runner"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/budget"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	schedmodels "github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/push"
	"github.com/ElcanoTek/fleet/internal/sched/scheduler"
	"github.com/ElcanoTek/fleet/internal/sched/slamonitor"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/ElcanoTek/fleet/internal/secretbox"
	"github.com/ElcanoTek/fleet/internal/sharedfiles"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/taskrun"
	"github.com/ElcanoTek/fleet/internal/tools"
	"github.com/ElcanoTek/fleet/internal/version"
	"github.com/ElcanoTek/fleet/internal/webpush"
	"github.com/ElcanoTek/fleet/internal/worktree"
)

// approvalExpirySweepInterval is how often the background sweep auto-denies
// pending approvals past their default-deny deadline (#225). Bounded by the
// approval card's expectation that a stale request closes within a minute.
const approvalExpirySweepInterval = 30 * time.Second

// maintenanceInterval is how often the background maintenance loop runs the
// box's reclamation pass — the chat plane's database retention sweeps plus the
// attachment, temp-upload, workspace and git-worktree reclaimers. Hourly is
// plenty: every TTL involved is measured in days, and the loop exists so an
// idle server (no chat turns, which used to be the only trigger for most of
// this) still frees disk and database rows.
const maintenanceInterval = httpapi.DefaultMaintenanceInterval

// maintenancePassTimeout bounds ONE maintenance iteration. The pass walks
// filesystem trees and runs host git, either of which can be slow on a large,
// slow-storage box; without a ceiling one pathological iteration would wedge
// the loop and silently stop every later one. Generously longer than a healthy
// pass, which is seconds. An overrun aborts the remaining steps and the next
// tick retries them.
const maintenancePassTimeout = 15 * time.Minute

func main() {
	// `fleet` is the ONE unified binary (#461). It dispatches three families:
	//
	//   1. Server-family verbs handled HERE — they boot or inspect the server
	//      process itself and must not route through the operator CLI.
	//   2. The daemon — bare `fleet` (back-compat with the historical systemd
	//      ExecStart=/usr/local/bin/fleet) AND the explicit `fleet serve`. Keeping
	//      bare `fleet` start the server means a unit file can migrate to
	//      `fleet serve` on its own schedule WITHOUT a restart-mid-upgrade ever
	//      bricking the box (the binary understands both forms). See ADR-0012.
	//   3. Everything else → the operator/admin CLI (internal/admincli): bootstrap,
	//      update, status, diagnose, restart/stop/logs, chat (the TUI + chat-user
	//      admin), sched, task, mcp, notes, worktree, backup/restore.
	argv := os.Args[1:]
	switch classifyInvocation(argv) {
	case invokeVersion:
		// Build identity (release stamp + VCS revision; docs/VERSIONING.md). Boots nothing, so it
		// works on a box where the DBs or sandbox are down.
		if len(argv) > 1 && argv[1] == "--web" {
			// The embedded web bundle's build id (ADR-0091): the check `make
			// check-web-bundle`, scripts/update.sh and CI run on a built binary.
			// Exit 1 when the binary carries no bundle.
			os.Exit(printWebBundle(os.Stdout))
		}
		if len(argv) > 1 && argv[1] == "--build-info" {
			// JSON build facts, including the host-executor compile-time
			// marker release.yml and `fleet release fetch` refuse on.
			os.Exit(printBuildInfo(os.Stdout))
		}
		fmt.Println("fleet " + version.String())
	case invokeMCPBroker:
		// The out-of-process MCP credential broker over stdio (issue #167): it holds
		// the connector secrets + MCP subprocesses and serves delegated MCP calls
		// back to a parent fleet process. No HTTP servers / scheduler.
		if err := runMCPBroker(); err != nil {
			log.Fatalf("fleet mcp-broker: %v", err)
		}
	case invokeValidateConfig:
		// Preflight checks (#248) against the SAME loaders the server boots through;
		// exits 0/1. Starts no servers, runs no migrations.
		os.Exit(runValidateConfig(argv[1:]))
	case invokeMCPTest:
		// Per-server MCP smoke: handshake + tools/list against the bundle
		// catalog, spawned exactly as the broker would. Boots nothing else.
		os.Exit(runMCPTest(argv[2:]))
	case invokeEval:
		// Eval & regression harness (#502): replay bundle goldens through the
		// governed loop and gate on the set threshold; exits 0/1 (2 = usage).
		os.Exit(runEvalCmd(argv[1:]))
	case invokeGenerateVAPIDKeys:
		// Browser Web Push setup (#292): print a fresh VAPID key pair as env
		// lines for the operator's env-file. Boots nothing. Errors go to
		// stderr so `>> fleet.env` never captures one as an env line.
		os.Exit(runGenerateVAPIDKeys(os.Stdout, os.Stderr))
	case invokeTaskRun:
		// Local one-shot harness: run a single task YAML to completion through
		// the governed scheduled runtime (no server, no DB). The old cutlass
		// binary, folded into the unified CLI.
		os.Exit(taskrun.Run(argv[2:], "fleet task run"))
	case invokeServe:
		// Daemon: bare `fleet` (legacy) or `fleet serve`. The server is env/config
		// driven — there are no serve flags — so `-h`/`--help` prints that and
		// exits 0, and anything else after `serve` is refused rather than
		// silently ignored (a typo'd `fleet serve --config x` used to boot the
		// daemon with the operator none the wiser).
		if code, done := serveArgsExit(serveArgs(argv), os.Stdout, os.Stderr); done {
			os.Exit(code)
		}
		if err := run(); err != nil {
			log.Fatalf("fleet: %v", err)
		}
	default: // invokeAdmin
		// The admin CLI builds tasks through models.NewTask in a process that
		// never runs handlers.New, so install the deployment retry default
		// here too (#1538); otherwise an imported task would get 0 retries
		// where the same request through the API gets the configured default.
		schedmodels.SetDefaultMaxRetries(config.TaskDefaultMaxRetriesFromEnv())
		os.Exit(admincli.Run(argv))
	}
}

type invocation int

const (
	invokeServe invocation = iota // bare `fleet` (legacy ExecStart) OR `fleet serve`
	invokeVersion
	invokeMCPBroker
	invokeValidateConfig
	invokeMCPTest
	invokeEval
	invokeGenerateVAPIDKeys
	invokeTaskRun
	invokeAdmin // every operator/admin verb → internal/admincli
)

// classifyInvocation decides which family the argv belongs to. The load-bearing
// invariant (ADR-0012, #461): bare `fleet` (no args) AND `fleet serve` BOTH map
// to invokeServe, so a historical systemd unit running ExecStart=/usr/local/bin/fleet
// keeps starting the daemon even as units migrate to `fleet serve` — no restart
// mid-upgrade can ever brick the box. Server-family verbs (version, mcp-broker,
// validate-config) are handled in-binary; everything else is an admin verb.
func classifyInvocation(argv []string) invocation {
	if len(argv) == 0 {
		return invokeServe
	}
	switch argv[0] {
	case "serve":
		return invokeServe
	case "version", "--version", "-v":
		return invokeVersion
	case "mcp-broker":
		return invokeMCPBroker
	case "validate-config":
		return invokeValidateConfig
	case "eval":
		return invokeEval
	case "generate-vapid-keys":
		return invokeGenerateVAPIDKeys
	case "task":
		// `fleet task run` is the local one-shot harness (the old cutlass binary,
		// folded in) and needs the full agent runtime, so it dispatches here;
		// every other `task` subverb (export/import/memories) is admincli's.
		if len(argv) > 1 && argv[1] == "run" {
			return invokeTaskRun
		}
		return invokeAdmin
	case "mcp":
		// `fleet mcp test` probes the bundle catalog locally (spawns the
		// servers host-side, like the broker) so it dispatches here; every
		// other `mcp` subverb (reload) talks to the running server via
		// admincli.
		if len(argv) > 1 && argv[1] == "test" {
			return invokeMCPTest
		}
		return invokeAdmin
	default:
		return invokeAdmin
	}
}

// serveArgs returns the tokens after the `serve` verb (none for bare `fleet`).
func serveArgs(argv []string) []string {
	if len(argv) > 0 && argv[0] == "serve" {
		return argv[1:]
	}
	return nil
}

// serveUsage is what `fleet serve --help` prints. The daemon takes no flags:
// everything is env/config driven (docs/OPERATORS.md), so the usage says where
// the configuration actually lives instead of listing options that don't exist.
const serveUsage = `usage: fleet serve

Run the fleet daemon in the foreground (chat + scheduler, one process). It
takes no flags: configuration is the process environment plus the env file
($FLEET_ENV_FILE) and the client bundle ($FLEET_CLIENT_CONFIG_DIR).
Preflight with: fleet validate-config    Manage the unit with: fleet start|restart|stop|logs
Bare "fleet" (no verb) also serves — legacy ExecStart compatibility (ADR-0012).
`

// serveArgsExit decides whether `serve`'s trailing args short-circuit the boot:
// -h/--help prints serveUsage on out and exits 0; any other arg is a usage
// error on errW and exits 1 — the daemon must never start on a command line the
// operator got wrong. Empty args (the normal case) return done=false. Split
// from main so route_test can pin it without booting anything.
func serveArgsExit(args []string, out, errW io.Writer) (code int, done bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(out, serveUsage)
		return 0, true
	default:
		fmt.Fprintf(errW, "fleet serve: unexpected argument %q — serve takes no flags (see fleet serve --help)\n", args[0])
		return 1, true
	}
}

func run() error {
	startTime := time.Now() // process start, for the admin health summary uptime (#301)
	// Load the client bundle first: it supplies the MCP catalog (built into
	// cfg.MCPServers), the supporting-doc dirs, and branding/empty-state. Its
	// manifest also tells us which connector env-var names to admit from the
	// .env file, so register them BEFORE config.Load reads the env. (Load
	// itself already folded the FLEET_ENV_FILE file into the process env for
	// its manifest interpolation, #1123 — config.Load's application below is
	// an idempotent re-read that additionally admits the literal-named keys
	// registered here.)
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)
	// Webhook/Slack signing secrets are per-request auth secrets (read via
	// os.Getenv at verification time). Pin them to their boot values across
	// config hot-reload (#584): auth secrets are non-reloadable, and a drifted
	// .env copy must be reported as Skipped, never silently rotated mid-run.
	config.RegisterNonReloadableSecretEnvVars(bundle.WebhookSecretEnvNames()...)

	cfg, err := config.Load(envreg.Get("FLEET_ENV_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.PinWorkspaceRoot()
	// config.Load also exported the outbound attachment staging dir
	// (agentcore.AdmitOutboundAttachmentDir); the MCP broker started below
	// inherits it.

	// OpenTelemetry distributed tracing (#186): OPT-IN via FLEET_OTEL_ENDPOINT.
	// With the endpoint unset (the default) this is a complete no-op — no
	// exporter, the global no-op TracerProvider stays in place, zero per-span
	// overhead — while the W3C propagator is still installed so an inbound
	// traceparent threads request context for free. The stop func (bounded flush)
	// is deferred BEFORE any goroutine starts, like the Sentry flush.
	defer initTracing(version.String())()

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
	defer flushObservability(sentryActive)
	// safe owns recovery and remains SDK-independent; this structured hook is the
	// single adapter into Sentry. It carries only opaque IDs and boundary names —
	// never tool arguments/output, recovered values, stacks, or credentials.
	safe.PanicEventHook = func(event safe.PanicEvent, _ any) {
		observability.CapturePanicClassWithTags(context.Background(), event.Class, map[string]string{
			"incident_id":     event.IncidentID,
			"panic_location":  event.Location,
			"panic_boundary":  event.Boundary,
			"tool_name":       event.ToolName,
			"tool_call_id":    event.ToolCallID,
			"run_mode":        event.RunMode,
			"task_id":         event.TaskID,
			"conversation_id": event.ConversationID,
		})
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
		Format:     cfg.Log.Format,
		Level:      cfg.Log.Level,
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
	warnUncategorizedConnectors(bundle)
	// Inline http_tools (issue #261): resolved host-side (auth headers expanded from
	// the process env) and registered onto the credentialed MCP client alongside the
	// MCP catalog. Empty in the generic bundle.
	cfg.HTTPTools = bundle.HTTPToolConfigs()
	// Outbound A2A peers (#1368): same resolution and the same seam. Empty in
	// the generic bundle.
	cfg.A2APeers = bundle.A2APeerConfigs()

	// Resolve the sandbox image, OCI runtime, backend (and its kubernetes
	// settings) and egress allowlist from env + bundle (env wins). Fail-closed
	// on an unrecognized backend value (#989).
	if err := resolveSandboxInto(cfg, bundle); err != nil {
		return err
	}

	// Install the bundle's agent tool-behavior policy (parallel-safe tools,
	// critical-tool suffixes, substitute map). The generic bundle ships none, so
	// agentcore stays on its base generic critical suffixes. Must run before any
	// turn starts.
	agentcore.ConfigureAgentPolicy(bundle.AgentcorePolicy())

	// Connector credentials cross exactly one process boundary at boot: the child
	// inherits the current environment, proves liveness + public discovery, then
	// the parent erases connector env keys and every resolved connector config
	// copy. All subsequent interactive/scheduled MCP work uses broker scopes.
	mcpRuntime, serverSpecs, err := startDefaultProductionMCPRuntime(bundle, cfg)
	if err != nil {
		return fmt.Errorf("start production MCP broker: %w", err)
	}
	defer func() { _ = mcpRuntime.Close() }()

	// Governed lifecycle hooks (#788): translate the bundle's declared hooks into
	// the agentcore form and install them process-wide. The generic bundle ships
	// none, so this is a no-op by default. Must run before any turn starts.
	agentcore.ConfigureLifecycleHooks(bundle.AgentcoreHooks())

	// Install the bundle's custom model-pricing overrides (#297). The generic
	// bundle ships none, so cost accounting stays on the OpenRouter-returned
	// price (the pre-#297 default). Must run before any turn starts.
	agentcore.ConfigurePricing(bundle.AgentcorePricing())

	// Optional PII redaction (#450): install the process-wide redactor when
	// FLEET_PII_REDACTION_ENABLED is set. Default off = nil redactor = the tool-
	// output pass is a byte-for-byte no-op.
	configurePIIRedaction(cfg)
	configureGuardrail(cfg)

	personasDir := bundle.PersonasDir
	protocolsDir := bundle.ProtocolsDir
	systemPromptsDir := bundle.SystemPromptsDir
	// On the kubernetes backend the skills tree is re-staged INSIDE the
	// workspace claim so sandbox pods can read it (ADR-0055); podman keeps the
	// data-dir tree. Must precede every registration of the skills dir below.
	// Degrades loudly: the pool build then logs why in-sandbox skill reads
	// will not resolve.
	skillsDir, err := agent.StageSkillsForBackend(cfg, bundle)
	if err != nil {
		// Only the kubernetes backend stages, so the message can name it
		// outright (no tainted backend string in the log line).
		log.Printf("warning: stage skills for the kubernetes sandbox backend: %v — skills stay unreadable inside sandbox pods", err)
	}

	// Workspace symlinks must point at the REAL loaded dirs — in particular
	// the merged bundle+builtin skills dir — not the legacy $CWD/<name>
	// convention (see tools.SetSupportingDocDirs).
	tools.SetSupportingDocDirs(map[string]string{
		"personas":       personasDir,
		"protocols":      protocolsDir,
		"system_prompts": systemPromptsDir,
		"skills":         skillsDir,
		// The shared file library's staged tree (docs/SHARED-FILES.md).
		// Registering it here does two jobs at once: EnsureWorkspaceDir plants
		// the per-conversation `shared` symlink so the prompt block's relative
		// paths resolve, and fileOpRoot treats the tree as a read-only
		// supporting-doc root so write_file/edit_file refuse it (the sandbox
		// mounts enforce the same read-only posture for bash/run_python).
		tools.SharedFilesDirName: tools.SharedFilesDir(cfg.WorkspaceRoot),
	})

	// Confine the host-side file tools (view_file/write_file/edit_file) to the
	// workspace root the sandbox bind-mounts (see confineFileToolsToWorkspace).
	confineFileToolsToWorkspace(cfg.WorkspaceRoot)

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
	// cfg's DSNs come from config.ResolveDSNs: one database, the chat store
	// in schema chat and the sched store in schema sched (#2049, ADR-0098).
	chatDB, schedDB := cfg.DatabaseURL, cfg.SchedDatabaseURL
	if err := config.EnsureDistinctStores(chatDB, schedDB); err != nil {
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
	// Stranded-turn recovery (#798): project any turn left 'running' by a crash
	// into canonical history. Runs AFTER SetSearchEnabled (projection writes
	// FTS rows) and BEFORE the HTTP server serves.
	recoverStrandedTurns(chatStore, cfg.InputQueueRetentionDays)
	if cfg.SearchEnabled {
		safe.Go("store.fts-backfill", func() {
			bfCtx, bfCancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer bfCancel()
			n, err := chatStore.BackfillSearchContent(bfCtx)
			switch {
			case err != nil:
				log.Printf("search backfill: %v (after %d rows)", err, n)
			case n > 0:
				log.Printf("search backfill: indexed %d pre-existing message(s)", n)
			}
		})
	} else {
		log.Printf("full-text search: DISABLED (FLEET_SEARCH_ENABLED=false)")
	}

	// Persist every recovered panic (#241, #795) to the chat DB's panic_events
	// table. Only the value-free class and opaque attribution cross this seam.
	safe.StructuredPanicEventWriter = func(event safe.PanicEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		record := store.PanicEventRecord{
			IncidentID:     event.IncidentID,
			Location:       event.Location,
			Boundary:       event.Boundary,
			ToolName:       event.ToolName,
			ToolCallID:     event.ToolCallID,
			RunMode:        event.RunMode,
			TaskID:         event.TaskID,
			ConversationID: event.ConversationID,
			Class:          event.Class,
		}
		if err := chatStore.RecordPanicEvent(ctx, record); err != nil {
			log.Printf("panic event persist failed (location=%s incident=%s): %s", safe.LogSafe(event.Location), safe.LogSafe(event.IncidentID), safe.LogSafe(err.Error()))
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
	// The org default-task timezone (distinct from the server clock above) so
	// EnqueueTask — the create_task tool, chat schedule_task approval, and
	// promote-to-task — localizes recurrence like the HTTP create path does.
	schedStorage.SetDefaultTaskTimezone(defaultTaskTimezone())
	log.Printf("sched DB connected + migrated")

	// Bootstrap operators (#458): provision/promote the configured emails as
	// orchestrator admins so they reach the Operations Center seamlessly via the
	// shared chat session cookie. See seedBootstrapAdmins for the rationale.
	if err := seedBootstrapAdmins(schedStorage, cfg.AccountsManagedByAuth); err != nil {
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

	// The store secret cipher (FLEET_MCP_OAUTH_ENCRYPTION_KEY) protects ALL
	// at-rest secrets — remote-MCP OAuth tokens, LLM provider keys, and now
	// notification secrets — so it installs whenever the key is set, not only
	// when the remote-MCP feature (which additionally needs a public base URL)
	// is enabled.
	installStoreCipher(cfg, chatStore)

	// ── per-user remote (hosted) MCP servers + OAuth (#443) ──
	// The concrete service remains parent-side for explicit OAuth/connectors HTTP
	// control-plane operations. Run-time token acquisition and remote MCP clients
	// live in the credential-owning child and are reached only through this public
	// scope opener. Both are nil when the feature is unconfigured.
	remoteMCPSvc := setupRemoteMCP(cfg, chatStore)
	openRemoteMCPOverlay := productionRemoteMCPOverlayOpener(remoteMCPSvc, mcpRuntime)

	// ── interactive engine (the concrete turnEngine) ──
	bundleProviders := toAgentcoreProviders(bundle)
	mgr, err := buildInteractiveEngine(agent.ManagerOptions{
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
		OpenRemoteMCPOverlay: openRemoteMCPOverlay,
		// The overlay opener above still owns per-turn MCP binding in its
		// credential-owning child; this resolver is the READ side the parent
		// needs to unseal one connector's api_key for a host-side outbound call
		// (browserbase_live_view, #987) — the same thing the parent already does
		// for the add-time probe. openRemoteMCPOverlay takes precedence for the
		// overlay itself, so wiring both changes no MCP behavior.
		RemoteMCP:    remoteMCPResolver(remoteMCPSvc),
		MCPBroker:    mcpRuntime.client,
		MCPCatalog:   mcpRuntime.catalog,
		MCPAccounts:  mcpRuntime.accounts,
		OpenMCPScope: mcpRuntime.openInteractiveScope,
		ReloadMCP:    mcpRuntime.reload,
	}, bundleProviders, chatStore, cfg.OpenRouterAPIKey)
	if err != nil {
		return fmt.Errorf("build interactive engine: %w", err)
	}
	defer mgr.Close()
	// The Tools picker's connector metadata (categories, needs-setup rows),
	// owned by the Manager so an MCP reload refreshes it with the catalog.
	mgr.SetConnectorMeta(bundle.ConnectorMeta())

	// Host disk headroom guard: measures the filesystem holding the data dir and
	// decides whether the box should shed BACKGROUND load. One guard, three
	// consumers — the Prometheus gauges, /healthz + the admin health summary,
	// and the scheduler's claim gate — so what is charted, what is reported and
	// what is enforced can never disagree.
	diskGuard := diskguard.New(cfg.DataDir, cfg.DiskMinFreePercent)
	logDiskGuard(diskGuard, cfg)

	// Health summary (#301): uptime + an injected scheduler worker/task provider
	// (adapts the sched store's dashboard stats) so the chat-side endpoint can
	// report a single-pane view without httpapi importing the sched packages.
	chatOpts := []httpapi.Option{
		httpapi.WithClientConfig(bundle),
		httpapi.WithDiskGuard(diskGuard),
		httpapi.WithStartTime(startTime),
		httpapi.WithVersion(version.String()),
		httpapi.WithWorkerStats(workerStatsProvider(schedStorage)),
		// Admin Users tab two-plane writes (grant/revoke Operations Center
		// admin alongside chat-role changes) — same composition `fleet admin
		// add` does, triggered over HTTP.
		httpapi.WithOpsAdmins(opsAdminsService{st: schedStorage}),
		// Knowledge-graph extraction (#523): the seam is always wired; whether
		// anything fires is gated by FLEET_MEMORY_GRAPH_ENABLED (default off).
		httpapi.WithMemoryGraphExtractor(mgr.ExtractMemoryGraph),
	}
	if remoteMCPSvc != nil {
		chatOpts = append(chatOpts, httpapi.WithRemoteMCP(remoteMCPSvc))
	}
	// Browser Web Push (#292): nil unless the operator generated VAPID keys
	// (`fleet generate-vapid-keys`) and set the three FLEET_VAPID_* vars. When
	// off, the /push/* endpoints answer 501 and no trigger fires — the default,
	// behavior-unchanged posture. The private key stays host-side in the env.
	pushSvc := webpush.New(webpush.Load(), chatStore)
	if pushSvc.Enabled() {
		log.Printf("web push: enabled")
		chatOpts = append(chatOpts, httpapi.WithPush(pushSvc))
	}

	// Task notifier (email/webhook/Web Push) — built here, before the runner
	// pool, because the budget gate below reuses the SAME delivery pipeline for
	// its soft-limit alerts (#601 part 2). See buildTaskNotifier.
	taskNotifier := buildTaskNotifier(pushSvc)

	// Per-principal rolling budgets (#601 part 2): ONE enforcer shared by every
	// task-create path — POST /tasks + /tasks/batch (via h.SetBudgetGate below)
	// and the chat schedule_task seam (threaded into taskSchedulerProvider) —
	// so the paths cannot drift. Spend is recomputed from the part-1 usage read
	// model (task_iterations + the chat turn_metrics seam); the Ceilings func
	// reads the LIVE #286 hot-reload accessors so a budget can never be more
	// permissive than the current global FLEET_MAX_COST_USD /
	// FLEET_MAX_TOTAL_TOKENS.
	chatUsage := chatUsageProvider(chatStore)
	budgetEnforcer := budget.New(budget.Config{
		Store:     schedStorage,
		ChatUsage: budget.ChatUsage(chatUsage),
		Notifier:  taskNotifier,
		Ceilings:  func() (float64, int) { return cfg.LiveMaxCostUSD(), cfg.LiveMaxTotalTokens() },
	})

	// schedule_task (#239): let an approved interactive call create an
	// orchestrator task in-process, reusing the SAME validated storage create
	// path POST /tasks uses — no second governance/create path is forked. The
	// seam carries the budget gate so scheduling from chat cannot bypass a
	// budget that would refuse the same user on POST /tasks (#601 part 2).
	chatOpts = append(chatOpts, httpapi.WithTaskScheduler(taskSchedulerProvider(schedStorage, budgetEnforcer)))
	chatOpts = append(chatOpts, httpapi.WithTaskManager(taskManagerProvider(schedStorage)))
	chatOpts = append(chatOpts, httpapi.WithSchedPing(schedStorage.DB().Ping))

	// Admin-managed LLM providers: after a persisted edit the handler invokes
	// this to re-read the store, re-merge with the bundle table, and swap the
	// manager's resolver — the edit takes effect for the NEXT turn/run with no
	// restart. A failed swap leaves the current table serving.
	chatOpts = append(chatOpts, httpapi.WithLLMProvidersChanged(
		llmProvidersReloader(mgr, chatStore, bundleProviders, cfg.OpenRouterAPIKey)))

	// Admin-managed workspace feature settings (internal/settings): the admin
	// Features panel. Defaults come from the env-derived Config; overrides load
	// from the workspace_settings table and apply LIVE (PII redactor swap,
	// config live setters, agentcore holders) — both now, at boot, and after
	// every admin edit.
	chatOpts = appendWorkspaceSettingsOption(chatOpts, cfg, chatStore)

	// Admin-managed task notification settings (internal/notifyadmin): the
	// admin Notifications panel. A persisted admin row hot-swaps the shared
	// notifier's config at boot and after every edit; no row = the env-derived
	// config already in taskNotifier keeps serving.
	chatOpts = appendNotifySettingsOption(chatOpts, chatStore, taskNotifier)

	chatSrv := httpapi.New(cfg, mgr, chatStore, chatOpts...)

	// Materialize the shared file library's staged tree (docs/SHARED-FILES.md)
	// before the listeners come up, so the very first turn already reads a
	// reconciled <workspace>/shared and a wiped workspace volume self-heals at
	// boot instead of waiting an hour for the maintenance pass. Best-effort:
	// a degraded library must not brick boot — Sync runs again every pass.
	if err := chatSrv.SyncSharedFiles(context.Background()); err != nil {
		log.Printf("shared files: boot sync: %v (the hourly maintenance pass will retry)", err)
	}

	// Auto-approve-in-test (#225) bypasses the human-in-the-loop approval gate.
	// It is off by default and intended only for CI/test pipelines with a mocked
	// backend; log loudly so it can never be on in production unnoticed.
	logCentralAuth(cfg)
	if cfg.AutoApproveInTest {
		log.Printf("WARNING: FLEET_AUTO_APPROVE_IN_TEST is ON — executable critical tools are auto-approved without human review; handler-only task, preview and model-suggestion cards still require explicit decisions. Do NOT use this in production.")
	}

	// ── orchestrator HTTP (sched/handlers) ──
	keyMgr, err := apikeys.NewManager(filepath.Join(cfg.DataDir, "api_keys.json"), "")
	if err != nil {
		return fmt.Errorf("apikeys manager: %w", err)
	}
	rl, err := schedRateLimits()
	if err != nil {
		return err
	}
	hcfg := handlers.Config{
		AdminAPIKey:                   envreg.Get("ADMIN_API_KEY"),
		Version:                       version.String(),
		DataDir:                       cfg.DataDir,
		Timezone:                      timezone(),
		DefaultTaskTimezone:           defaultTaskTimezone(),
		UploadMaxBytes:                cfg.UploadMaxBytes,
		SchedRateLimitPerMinute:       rl.perMinute,
		SchedRateLimitPerDay:          rl.perDay,
		SchedGlobalRateLimitPerMinute: rl.globalPerMinute,
		ElcanoCookieName:              "elcano_auth",
		// Reuse the chat shared token so the Next proxy's X-User-Email path is
		// trusted by the orchestrator too (#157). cfg.SharedToken is guaranteed
		// non-empty by config.Validate.
		SharedToken: cfg.SharedToken,
		// Cost-forecast inputs (#233): mirror the runtime selection so POST
		// /tasks/estimate projects against the same model + iteration cap + cost
		// ceiling a real dispatch uses. cfg.MaxIterations is the per-task default
		// the runner applies when a task omits one. DefaultTaskModel stays
		// unset: the handlers read the live task tier, as the runner does.
		MaxCostUSD:           cfg.MaxCostUSD,
		LiveMaxCostUSD:       cfg.LiveMaxCostUSD,
		LiveMaxTotalTokens:   cfg.LiveMaxTotalTokens,
		DefaultMaxIterations: cfg.MaxIterations,
		DefaultMaxRetries:    cfg.TaskDefaultMaxRetries,
		// Per-task sandbox-limit ceilings (#205): validateSandboxLimits rejects an
		// override above these. 0 = no ceiling.
		SandboxMemoryMaxMB: cfg.SandboxMemoryMaxMB,
		SandboxCPUsMax:     cfg.SandboxCPUsMax,
		SandboxPidsMax:     cfg.SandboxPidsMax,
		// Central Auth only: the Operations Center refuses its own password
		// login and the bearer it minted, like the other two planes.
		PasswordLoginDisabled: cfg.PasswordLoginDisabled,
		AccountsManagedByAuth: cfg.AccountsManagedByAuth,
	}
	warnIfNoAdminKey(hcfg.AdminAPIKey)
	h := handlers.New(hcfg, schedStorage, keyMgr)
	// Wire the orchestrator's read-only MCP catalog + credential-account seats
	// from the SAME in-process source the chat side uses: the Manager's optional
	// catalog plus its live always-on status rows (descriptions, tool counts, and
	// the per-server credential-account seat names it derives from the bundle's
	// AccountVars via creds.AccountsFor). Never exposes secret values — only
	// server + account names. This is what makes the scheduled-task MCP picker +
	// credential admin table work.
	h.SetMCPCatalogProvider(func() []handlers.MCPServerCatalogEntry { return mcpCatalogEntries(mgr) })
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
	// The hybrid prompt library reads bundle prompts/ live (Git-backed,
	// read-only) and merges it with sched-DB private/workspace entries.
	h.SetPromptCatalogProvider(bundle.Prompts)
	// Signed-in Chat users whose independent Operations Center role (ops_role) is
	// "none" still use the shared Prompt Library in Chat and Settings → Prompts.
	h.SetChatMemberRoleProvider(chatMemberRoleProvider(chatStore))
	// Wire the chat side of the usage report (#601 part 1) — see wireChatUsage.
	h.SetChatUsageProvider(chatUsage)
	// Wire the chat side of the adoption report (the exec Adoption view):
	// per-user-per-day chat metering + the provisioned-account roster, both
	// read-only aggregations over what the chat store already records.
	h.SetChatUserDayUsageProvider(chatUserDayUsageProvider(chatStore))
	h.SetChatAccountsProvider(chatAccountsProvider(chatStore))
	// Both planes are reached with the SAME session cookie, so the Operations
	// Center checks the same revocation claim chat does. The epoch lives in the
	// chat users table, which this plane does not read (separate stores, ADR-0098) — hand it the
	// lookup, not the schema. Wired unconditionally: the chat store is open by
	// the time these handlers exist, and a nil seam would silently stop checking.
	h.SetChatSessionEpochProvider(chatStore.SessionEpoch)
	// Read-only on the request path; rows are created by the web tier's mint via
	// httpapi's /auth/external-session-epoch.
	h.SetExternalSessionEpochProvider(func(ctx context.Context, issuer, subject, _ string) (string, error) {
		return chatStore.LookupExternalSessionEpoch(ctx, issuer, subject)
	})
	// Budget gate for POST /tasks + /tasks/batch and the /admin/budgets CRUD
	// surface (#601 part 2) — the SAME enforcer the chat schedule_task seam
	// carries, so no create path can drift.
	h.SetBudgetGate(budgetEnforcer)
	notesHandlers := handlers.NewNotesHandlers(notesStore, h)
	orchHandler := buildOrchestratorMux(h, notesHandlers, reloadConfigHandler(cfg), mcpReloadHandler(mgr), cfg.TrustedProxies)

	// ── scheduler ticker (promote scheduled→pending + recover leases) ──
	sch := scheduler.New(schedStorage, timezone())
	sch.SetTickInterval(cfg.SchedTickSeconds)
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
	// Paused-task expiry (#510): fail tasks stuck awaiting input past the
	// window. Off (wait forever) when TaskPausedExpiryMinutes<=0.
	sch.SetPausedExpiry(cfg.TaskPausedExpiryMinutes)
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
		// Self-improving memory (#516): the active distilled learned instruction
		// is injected at run-start. Gated on FLEET_SELF_IMPROVE_ENABLED — nil
		// provider leaves scheduled runs byte-for-byte unchanged.
		LearnedInstructions: learnedInstructionProvider(cfg, schedStorage),
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
		// Per-user remote (hosted) MCP + OAuth (#443): the child-owned scope
		// opener plus a creator-UUID → email resolver (the sched username IS the
		// chat email for the elcano-auth tier). A nil opener leaves the feature off.
		OpenRemoteMCPOverlay: openRemoteMCPOverlay,
		OwnerEmail:           ownerEmailResolver(schedStorage),
		// The owner's hosted connections as a list (no token, no dial), for
		// the create-time mcp_selection check wired below.
		HostedConnections: hostedConnectionsLister(remoteMCPSvc),
		// The owner's builder skills + propose_skill staging (docs/SKILLS.md):
		// scheduled runs inline the owner's ACTIVE skills into the prompt and
		// stage agent-drafted proposals against the same chat-store rows the
		// Skills page reviews.
		UserSkills:       userSkillDocsProvider(chatStore),
		SkillProposerFor: skillProposerFactory(chatStore),
		// Shared file library announcement (#1301): scheduled workspaces already
		// carry the readable shared/ tree (#1290 seeds the symlink; the staged
		// copy is mounted on both backends) — announce it in the run's system
		// prompt through the SAME renderer chat turns use, over the same chat
		// store manifest. Best-effort: a list error logs and the run proceeds
		// unannounced, mirroring the chat path.
		SharedFilesPrompt: func(ctx context.Context) string {
			files, err := chatStore.ListSharedFiles(ctx)
			if err != nil {
				log.Printf("shared files: list for scheduled prompt block: %v", err)
				return ""
			}
			return sharedfiles.PromptBlock(files)
		},
		OpenTaskMCPScope:   mcpRuntime.openTaskScope,
		MCPServerInventory: mcpRuntime.inventory.snapshot,
	})
	// Wire the cost-forecast's system-prompt resolver (#233) from the SAME runner
	// that assembles the prompt at dispatch, so POST /tasks/estimate counts the
	// exact system prompt a real run would send. Read-only; never dispatches.
	h.SetSystemPromptProvider(taskRunner.SystemPromptForPersona)
	// Refuse at create/clone/rerun/import time a task whose mcp_selection its
	// runs would refuse (a server neither in the bundle nor among the owner's
	// hosted connections), through the runner's own resolution.
	h.SetMCPSelectionCheck(taskRunner.CheckTaskMCPSelection)
	h.SetModelCatalogResolver(mgr.CatalogModelSlug)
	// Wire the persona catalog (#720) so the task-create paths can reject an
	// unknown persona with a 400 listing the valid names, instead of silently
	// dispatching on the global default. Reads the bundle's personas dir live
	// per call, so a bundle hot-reload is reflected without a restart.
	h.SetPersonaCatalog(func() []string { return listBundlePersonas(personasDir) })
	// Reclaim sandbox containers orphaned by a PRIOR crash: they run
	// `--detach --rm` under conmon, so a non-graceful exit leaves them holding
	// host RAM/PIDs across systemd restarts. Best-effort — log and continue if
	// podman is absent (e.g. mock/dev) or the sweep fails.
	//
	// NOTE this runs AFTER buildInteractiveEngine above, which constructs the
	// sandbox pool and starts it filling. The sweep is safe here only because it
	// skips containers carrying this process's own instance label in every
	// state — otherwise a warm container caught in "created" state would be
	// force-removed by its own process. See sandbox.PruneOrphanedContainers.
	pruneOrphanedSandboxes(mgr.SandboxPool())

	// Share the process shutdown grace with the pool so its in-flight-task drain
	// uses the same budget as the chat-turn drain (#278).
	poolGrace := poolDrainGrace(cfg)
	// The pool reuses the task-completion notifier (#208) built above (it now
	// also carries the budget gate's soft-limit alerts, #601 part 2).
	pool := runner.NewPool(schedStorage, taskRunner, runner.Config{
		Limiter:    agentLimiter,
		DrainGrace: poolGrace,
		Notifier:   taskNotifier,
		// FLEET_SCHED_TICK_SECONDS drives both scheduling cadences: the
		// scheduler's promote/recover tick (SetTickInterval above) and this
		// pool's idle claim poll — one dial for "how fast the engine notices
		// due work". Default 30s each.
		PollInterval:  time.Duration(cfg.SchedTickSeconds) * time.Second,
		PublicURLBase: envreg.Get("FLEET_PUBLIC_URL"),
		// Post-failure error-recovery diagnosis (#317): the interactive Manager
		// doubles as the runner's ErrorAnalyzer (it has the host-side resolver +
		// cheap-model seam, exactly like SuggestTitle). Gated on
		// FLEET_ERROR_ANALYSIS_ENABLED; nil = off (the analyze path is a no-op).
		ErrorAnalyzer: errorAnalyzerFor(cfg, mgr),
		// Email reply-back (#511): nil unless SMTP is configured for sending.
		EmailReplier: emailReplierFor(taskNotifier),
		// Disk backpressure: stop CLAIMING scheduled work while the data dir's
		// filesystem is below the free-space floor. Running tasks are untouched
		// and interactive chat is never gated — a full disk is usually made by
		// unattended runs, and chat is how an operator fixes it.
		AdmitScheduled: diskAdmissionGate(diskGuard),
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
	// Live-run take-over (#508): DELETE /tasks/{id} on a task executing in this
	// process now interrupts the governed run at its next checkpoint, with
	// who-stopped-it attribution on the terminal record.
	h.SetTaskStopper(pool.StopTask)
	// Immediate dispatch for synchronous creates (#1279): a create/resume/wake
	// that lands a task in pending wakes the pool's claim loop instead of
	// waiting out the poll tick — an A2A blocking send holds its caller on the
	// line for that latency.
	h.SetTaskKicker(pool.Kick)
	// Dashboard agent cards: live pool occupancy (active scheduled agents /
	// schedulable slots), stamped onto GET /stats at response time.
	h.SetAgentPoolStats(pool.ActiveTasks, pool.Cap)
	// Self-improving memory (#516): the feedback→learned-instruction distiller,
	// gated on FLEET_SELF_IMPROVE_ENABLED (default off; nil leaves feedback
	// recorded but never auto-distilled).
	h.SetLearnedInstructionDistiller(selfImproveDistiller(cfg, mgr))

	// A2A protocol server (#1279): opt-in, fail-closed. Unwired, the routes
	// answer 501; wired, the card is rendered ONCE (it is static per boot) and
	// the dispatcher translates onto the same task seams wired above.
	a2aPush, err := wireA2A(cfg, bundle, h, schedStorage)
	if err != nil {
		return err
	}

	// Dataset / table agent (#514): rows run through the SAME governed
	// interactive entrypoint (Manager.RunTurn → agentcore.Run). Extracted
	// wiring: boot sweep + runner construction + handler seam.
	datasetRunner := setupDatasetRunner(schedStorage, mgr, h)
	defer datasetRunner.Shutdown()

	// Metrics gauges (#176): live in-flight turn counts + warm sandbox depth,
	// evaluated at each /metrics scrape. Extracted to keep run() within the
	// cyclomatic budget.
	registerRuntimeMetrics(chatSrv.ActiveTurns, pool.ActiveTasks, mgr.SandboxPool(), diskGuard)

	// ── boot listeners ──
	// Last-resort fallback only — config.Load already defaults Addr to
	// 127.0.0.1:8080; keep the fallback loopback too so an explicitly empty
	// FLEET_SERVER_ADDR can never widen the bind to every interface.
	chatAddr := defaultString(cfg.Addr, "127.0.0.1:8080")
	orchAddr := orchestratorAddr()

	// Liveness + readiness probes (#215) on BOTH ports, sharing one check set
	// and one drain signal (chatSrv.BeginShutdown is the single graceful-drain
	// trigger, so both ports report not_ready while draining).
	readinessChecks := buildReadinessChecks(cfg, chatStore, schedStorage.DB(), diskGuard, mgr.SandboxPool())
	// apiversion.Router (#321) makes both servers reachable under a /v1 prefix
	// (X-Fleet-API-Version on those responses) while the legacy bare paths keep
	// working with a Deprecation signal. It wraps INSIDE the health-probe layer so
	// /healthz stays unversioned + untagged. Route registration is untouched.
	chatHandler := securityHeadersMiddleware(withHealthProbes(apiversion.Router(chatSrv.Routes()), startTime, chatSrv.IsDraining, readinessChecks), tlsActive(cfg))
	orchHandlerWithProbes := withHealthProbes(apiversion.Router(orchHandler), startTime, chatSrv.IsDraining, readinessChecks)

	chatServer := &http.Server{Addr: chatAddr, Handler: chatHandler, ReadHeaderTimeout: 30 * time.Second}
	orchServer := &http.Server{Addr: orchAddr, Handler: orchHandlerWithProbes, ReadHeaderTimeout: 30 * time.Second}
	// The browser tier (ADR-0091): the SPA and every /api/* route, calling the
	// two backends in-process. nil when FLEET_WEB_ADDR=off.
	webServer, err := buildWebServer(cfg, chatSrv.Routes(), orchHandler)
	if err != nil {
		return err
	}

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

	startA2APush(ctx, a2aPush)
	defer slaMon.Stop()

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	defer signal.Stop(sigCh)

	// Config hot-reload on SIGUSR2 (#286): re-read the reloadable settings without
	// a restart. Extracted to a helper so run() stays within the cyclomatic budget.
	defer onSignal(ctx, syscall.SIGUSR2, "cmd.config-reload", func() {
		logConfigReload(cfg.Reload(envreg.Get("FLEET_ENV_FILE")))
	})()

	// MCP hot-reload on SIGHUP (#218): re-read the bundle's MCP catalog and apply
	// server add/remove/restart to the live Manager without a restart.
	defer onSignal(ctx, syscall.SIGHUP, "cmd.mcp-reload", func() {
		logMCPReload(reloadMCPServers(ctx, mgr))
	})()

	// Worker pool runs until ctx is cancelled, then drains. Guarded so a panic in
	// the pool loop is a contained, logged event — and poolDone still closes
	// (deferred) so shutdown's <-poolDone can't hang.
	poolDone := make(chan struct{})
	go func() {
		defer safe.Recover("cmd.pool.run", nil)
		defer close(poolDone)
		pool.Run(ctx)
	}()

	errCh := make(chan error, 3)
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
	serveInBackground("orchestrator", orchServer, errCh)
	serveInBackground("web", webServer, errCh) // nil (skipped) when FLEET_WEB_ADDR=off
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
					log.Printf("approval expiry sweep: auto-denied %d timed-out approval(s)", n)
				}
				cancel()
			}
		}
	}()
	// In-chat patrol and timer sweep (migration 086): claims due chat_schedules
	// rows every 10 seconds and enqueues their turns into chat_input_queue.
	startChatScheduleSweeper(ctx, chatSrv)
	// One maintenance loop drives every periodic reclaimer on the box (chat
	// retention sweeps, attachment + temp-upload files, orphan workspaces,
	// stale git worktrees). See startMaintenanceLoop.
	startMaintenanceLoop(ctx, cfg, h, chatSrv, chatStore)
	// The daily owner digest (docs/OWNER-DIGEST.md): off until an admin turns
	// it on in Settings → Admin → Notifications; the loop reads that switch
	// live from the shared notifier.
	ownerdigest.New(schedStorage, chatStore, taskNotifier).Start(ctx)

	// Listeners are bound; tell a systemd-aware supervisor we are ready (no-op
	// when NOTIFY_SOCKET is unset, i.e. non-systemd / dev / tests).
	grace := shutdownGrace(cfg)
	log.Printf("fleet: shutdown grace = %s (FLEET_SHUTDOWN_GRACE_SECONDS)", grace)
	sdNotify("READY=1")

	// Block until a terminal signal / listener error decides graceful vs immediate,
	// then drain. Extracted so run() stays within the cyclomatic budget.
	graceful := awaitShutdown(sigCh, errCh, chatSrv, pool, agentLimiter)
	performShutdown(graceful, grace, cancel, chatSrv, pool, poolDone, chatServer, orchServer, webServer)
	return nil
}

func startChatScheduleSweeper(ctx context.Context, chatSrv *httpapi.Server) {
	go func() {
		defer safe.Recover("cmd.chat-schedule-sweep", nil)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				if _, err := chatSrv.SweepDueChatSchedules(sweepCtx); err != nil {
					log.Printf("chat schedule sweep: %v", err)
				}
				cancel()
			}
		}
	}()
}

// initTracing wires OpenTelemetry tracing (#186) and returns a stop func meant
// to be deferred. A misconfigured exporter is NON-FATAL: it logs and returns a
// no-op stop, so a bad FLEET_OTEL_ENDPOINT degrades to disabled tracing rather
// than preventing the agent platform from starting — tracing is optional
// observability, not a hard dependency. The stop func flushes buffered spans
// under a bounded timeout so a stuck collector can't hang shutdown.
func initTracing(serviceVersion string) func() {
	shutdown, err := otelsetup.Init(context.Background(), serviceVersion)
	if err != nil {
		log.Printf("otel: init failed, tracing disabled: %v", err)
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			log.Printf("otel: shutdown error: %v", err)
		}
	}
}

func flushObservability(active bool) {
	if active {
		observability.Flush(2 * time.Second)
	}
}

// startA2APush runs the A2A push-notification dispatcher (#1279 Phase 2) on
// the serve-lifetime ctx — cancelling stops new scans; an in-flight POST
// finishes within its own 10s bound. nil (A2A or push disabled) is a no-op.
func startA2APush(ctx context.Context, d *push.Dispatcher) {
	if d == nil {
		return
	}
	go func() {
		defer safe.Recover("cmd.a2a-push", nil)
		d.Run(ctx)
	}()
}

// wireA2A applies the FLEET_A2A_ENABLED gate (#1279): off logs the remedy and
// leaves the routes on their 501 guard; on, a card that cannot be rendered is
// a misconfiguration, not a degraded mode — refuse to boot rather than serve
// a discovery document we advertise wrongly.
func wireA2A(cfg *config.Config, bundle *clientconfig.Bundle, h *handlers.Handlers, store *storage.Storage) (*push.Dispatcher, error) {
	if !cfg.A2AEnabled {
		log.Printf("a2a: disabled (set FLEET_A2A_ENABLED=1 to serve the A2A protocol)")
		return nil, nil
	}
	// Push notifications (#1279 Phase 2) exist only when the store cipher is
	// configured: caller-supplied webhook secrets are sealed at rest, and
	// plaintext storage is not an option. The card advertises exactly what the
	// dispatcher will accept (spec §3.3.4), so a cipher-less deployment keeps
	// answering -32003 and conformance suites skip rather than fail.
	pushEnabled := false
	if len(cfg.MCPOAuthEncryptionKey) > 0 {
		cipher, cerr := secretbox.NewCipher(cfg.MCPOAuthEncryptionKey)
		if cerr != nil {
			return nil, fmt.Errorf("a2a: push cipher: %w", cerr)
		}
		store.SetA2APushCipher(cipher)
		pushEnabled = true
	}
	a2aCfg, err := buildA2AConfig(cfg, bundle, pushEnabled)
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	h.SetA2A(a2aCfg)
	log.Printf("a2a: enabled (card at /.well-known/agent-card.json, JSON-RPC at /v1/a2a, persona=%q model=%q, push=%v)",
		cfg.A2APersona, cfg.A2AModel, pushEnabled)
	if !pushEnabled {
		log.Printf("a2a: push notifications disabled (set FLEET_MCP_OAUTH_ENCRYPTION_KEY to store webhook configs)")
		return nil, nil
	}
	if cfg.A2APushAllowPrivate {
		log.Printf("a2a: push SSRF guard RELAXED (FLEET_A2A_PUSH_ALLOW_PRIVATE=1) — loopback/private webhook targets accepted; do not run production this way")
	}
	return push.New(store, cfg.A2APushAllowPrivate), nil
}

// buildA2AConfig assembles the A2A surface's static inputs (#1279): the Agent
// Card — identity from the branding bundle, version from the binary, endpoint
// from FLEET_PUBLIC_BASE_URL (server-relative when unset, which still
// satisfies clients that resolved the card from the same origin) — plus the
// operator-pinned persona/model every A2A task runs with.
func buildA2AConfig(cfg *config.Config, bundle *clientconfig.Bundle, pushEnabled bool) (*handlers.A2AConfig, error) {
	base := strings.TrimRight(cfg.PublicBaseURL, "/")
	spec := a2abridge.CardSpec{
		Name:              bundle.Branding.AppName,
		Description:       strings.TrimSpace(bundle.Branding.ShareDescription),
		Version:           version.Version(),
		RPCURL:            base + "/v1/a2a",
		PushNotifications: pushEnabled,
		Persona:           cfg.A2APersona,
		Model:             cfg.A2AModel,
	}
	body, etag, err := a2abridge.MarshalCard(a2abridge.BuildCard(spec))
	if err != nil {
		return nil, fmt.Errorf("render agent card: %w", err)
	}
	// The authenticated variant (spec §13.3): rendered through the same
	// MarshalCard shadow so its securityRequirements stay schema-valid.
	extBody, _, err := a2abridge.MarshalCard(a2abridge.BuildExtendedCard(spec))
	if err != nil {
		return nil, fmt.Errorf("render extended agent card: %w", err)
	}
	// The inbound delegation-depth ceiling (#1368) shares its knob with the
	// outbound peer tools (agent.RegisterA2APeers), so a fleet-to-fleet chain
	// is refused at the same hop count on both ends.
	maxDepth, err := config.EnvKnobInt("FLEET_A2A_MAX_DELEGATION_DEPTH", config.DefaultA2AMaxDelegationDepth)
	if err != nil {
		return nil, err
	}
	return &handlers.A2AConfig{
		CardJSON:           body,
		CardETag:           etag,
		Persona:            cfg.A2APersona,
		Model:              cfg.A2AModel,
		PublicBaseURL:      base,
		PushEnabled:        pushEnabled,
		ExtendedCardJSON:   extBody,
		UnaryWaitBudget:    time.Duration(cfg.A2AUnaryWaitSeconds) * time.Second,
		MaxDelegationDepth: maxDepth,
	}, nil
}

// ── DSN / addr resolution ──

// logCentralAuth says at boot what central Auth controls on this Fleet
// (ADR-0095), so an operator reading the journal knows where accounts and
// passwords are managed.
func logCentralAuth(cfg *config.Config) {
	if cfg.AccountsManagedByAuth {
		log.Printf("auth: accounts, roles and teams are managed in central Auth; Fleet's account pages are view-only (the root CLI still works).")
		if len(cfg.AdminEmails) > 0 {
			log.Printf("auth: ADMIN_EMAILS is ignored: Fleet Admin comes from central Auth")
		}
	}
	if cfg.PasswordLoginDisabled {
		log.Printf("auth: Fleet passwords are off; people sign in through central Auth only. Root can turn them back on with FLEET_OIDC_ALLOW_PASSWORD=1 and a restart.")
	}
	if config.OIDCConfigured() && !cfg.AccountsManagedByAuth {
		log.Printf("auth: FLEET_OIDC_GENERIC=1: the SSO provider is not central Auth, so Fleet keeps its own passwords and account management.")
	}
}

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

// setupDatasetRunner wires the dataset / table agent (#514): the boot sweep
// resets crash-orphaned running datasets/rows so work is resumable, the
// runner drives each row through Manager.RunTurn, and the handler seam keeps
// sched handlers decoupled from the agent graph. Drain (the caller's defer)
// cancels active runs; in-flight rows persist their outcome best-effort.
func setupDatasetRunner(schedStorage *storage.Storage, mgr *agent.Manager, h *handlers.Handlers) *datasets.Runner {
	if err := schedStorage.ResetStaleRunningDatasets(context.Background()); err != nil {
		log.Printf("dataset boot sweep: %v", err)
	}
	datasetRunner := datasets.New(schedStorage, mgr)
	datasetRunner.SetDrainCheck(httpapi.UpdateDrainActive)
	h.SetDatasetRunner(datasetRunner)
	return datasetRunner
}

func timezone() string {
	if v := strings.TrimSpace(envreg.Get("FLEET_TIMEZONE")); v != "" {
		return v
	}
	return "UTC"
}

// defaultTaskTimezone is the IANA timezone applied to a scheduled task created
// without an explicit one. Distinct from FLEET_TIMEZONE (the server clock) so an
// operator can set an org default for task scheduling without moving the system
// clock. Empty defaults to "UTC", matching prior behaviour.
func defaultTaskTimezone() string {
	if v := strings.TrimSpace(envreg.Get("FLEET_DEFAULT_TIMEZONE")); v != "" {
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
// chat/orchestrator membership separation (separate stores, ADR-0098). Extracted from run() to
// keep it within the cyclomatic budget.
func seedBootstrapAdmins(schedStorage *storage.Storage, managedByAuth bool) error {
	admins := bootstrapAdmins()
	if len(admins) == 0 {
		return nil
	}
	// Where central Auth manages accounts it alone grants the Ops role; seeding
	// at every boot would restore access Auth took away (ADR-0095).
	if managedByAuth {
		log.Printf("orchestrator bootstrap admins ignored: accounts are managed in central Auth (%d configured; grant Fleet Admin there instead)", len(admins))
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
	raw := strings.TrimSpace(envreg.Get("FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS"))
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

// schedRateLimitWindows are the orchestrator's sliding-window rate limits for
// POST /tasks + /upload. 0 disables a window.
type schedRateLimitWindows struct {
	perMinute       int
	perDay          int
	globalPerMinute int
}

// schedRateLimits resolves the three FLEET_SCHED_RATE_LIMIT_* windows at serve
// start, OUTSIDE the config loader's struct literal (they are handed straight to
// handlers.Config, which holds no *Config). Since #1273 they are scopeExternal
// rows in the SAME envKnobs registry, so each read goes through
// config.EnvKnobInt — one parser, one set of bounds, shared with the boot gate
// in config.Load and with the `fleet validate-config` preflight. Before that
// they were parsed here with a warn-and-default helper, so a typo'd window
// silently ran at the default.
//
// A malformed value therefore refuses to boot: config.Load already rejected it
// before this runs, so the error path is belt-and-braces (it also surfaces a
// registry-row mistake rather than swallowing it).
func schedRateLimits() (schedRateLimitWindows, error) {
	var rl schedRateLimitWindows
	for _, w := range []struct {
		key string
		def int
		dst *int
	}{
		{"FLEET_SCHED_RATE_LIMIT_PER_MINUTE", 60, &rl.perMinute},
		{"FLEET_SCHED_RATE_LIMIT_PER_DAY", 500, &rl.perDay},
		{"FLEET_SCHED_RATE_LIMIT_GLOBAL_PER_MINUTE", 200, &rl.globalPerMinute},
	} {
		v, err := config.EnvKnobInt(w.key, w.def)
		if err != nil {
			return schedRateLimitWindows{}, fmt.Errorf("invalid environment configuration: %w", err)
		}
		*w.dst = v
	}
	return rl, nil
}

// listBundlePersonas returns the persona names (basenames of personas/*.yaml,
// without the extension) currently on disk in the bundle's personas dir (#720).
// Read live per call — personas hot-reload with the bundle, so caching here
// would let the create-time existence check drift from what dispatch can load.
// Errors (e.g. no personas dir in a bare deployment) return nil, which the
// handlers treat as "the bundle declares no personas".
func listBundlePersonas(personasDir string) []string {
	entries, err := os.ReadDir(personasDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
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

// toAgentcoreProviders translates the bundle manifest's providers: block (#289)
// into the agentcore resolver's provider table, resolving each API-key env var
// HOST-SIDE (os.Getenv) so the secret is handed to the resolver in memory and
// never lives in the manifest. Returns nil when the bundle declares no providers,
// which routes the Manager to the historical single-OpenRouter resolver.
func toAgentcoreProviders(bundle *clientconfig.Bundle) []agentcore.ProviderConfig {
	if bundle == nil || len(bundle.Providers) == 0 {
		return nil
	}
	out := make([]agentcore.ProviderConfig, 0, len(bundle.Providers))
	for _, p := range bundle.Providers {
		key := ""
		if env := strings.TrimSpace(p.APIKeyEnv); env != "" {
			key = envreg.Unregistered(env)
			if strings.TrimSpace(key) == "" {
				// A keyed type then fails resolver construction; a type whose
				// key is optional (ollama, openaicompat) would otherwise run
				// keyless without a word, and a typo'd name would surface only
				// as a 401 on the first turn. Names the provider only: nothing
				// read through APIKeyEnv reaches a log line, so the variable's
				// name (in the manifest entry) is not printed either.
				log.Printf("WARNING: bundle provider %q: its api_key_env variable is unset or empty, so it has no API key", strings.TrimSpace(p.Name))
			}
		}
		warnUndeclaredContextWindow("bundle", strings.TrimSpace(p.Name), strings.TrimSpace(p.Type), p.ContextWindowTokens,
			"declare context_window_tokens on its providers: entry")
		out = append(out, agentcore.ProviderConfig{
			Name:                strings.TrimSpace(p.Name),
			Type:                agentcore.ProviderType(strings.TrimSpace(p.Type)),
			APIKey:              key,
			BaseURL:             strings.TrimSpace(p.BaseURL),
			Models:              p.Models,
			Region:              strings.TrimSpace(p.Region),
			ContextWindowTokens: p.ContextWindowTokens,
			FallbackProviders:   append([]string(nil), bundle.FallbackProviders...),
		})
	}
	return out
}

// buildInteractiveEngine constructs the agent manager over the boot provider
// table: the bundle's providers: block (#289) overlaid with the enabled
// admin-managed DB rows. When BOTH are empty, opts.LLMProviders stays nil and
// the manager falls back to the historical single-OpenRouter resolver
// (including its "OPENROUTER_API_KEY required" boot error). A DB overlay that
// is unreadable or fails eager construction (e.g. the encryption key changed
// under a stored key) must never brick boot: it degrades to the bundle/env
// table with a loud log, and the admin fixes the row in the UI.
func buildInteractiveEngine(
	opts agent.ManagerOptions,
	bundleProviders []agentcore.ProviderConfig,
	chatStore *store.Store,
	openRouterKey string,
) (*agent.Manager, error) {
	opts.LLMProviders = bundleProviders
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbProviders, err := loadDBLLMProviders(ctx, chatStore)
	switch {
	case err != nil:
		log.Printf("WARNING: admin LLM providers unavailable (%v) — booting with the bundle/env provider table only", err)
	case len(dbProviders) > 0:
		merged := opts
		merged.LLMProviders = agentcore.MergeLLMProviders(bundleProviders, dbProviders, openRouterKey)
		mgr, mergeErr := agent.New(merged)
		if mergeErr == nil {
			return mgr, nil
		}
		log.Printf("WARNING: provider table with admin rows failed to build (%v) — retrying with the bundle/env table only", mergeErr)
	}
	return agent.New(opts)
}

// llmProvidersReloader builds the WithLLMProvidersChanged callback: re-read
// the admin rows, re-merge with the bundle/env table, swap the manager's
// resolver. Kept as a named constructor (not an inline closure in run) so the
// boot path and the hot-reload path visibly share loadDBLLMProviders +
// MergeLLMProviders and cannot drift.
//
// The whole read→merge→swap is serialized under one mutex (the mcpReloadMu
// pattern): the handler invokes this only after its write committed, so with
// concurrent admin edits the LAST commit's read always runs last and the final
// resolver state matches the final DB state — two overlapping reloads can
// never swap in stale order.
func llmProvidersReloader(
	mgr *agent.Manager,
	chatStore *store.Store,
	bundleProviders []agentcore.ProviderConfig,
	openRouterKey string,
) func(context.Context) error {
	var reloadMu sync.Mutex
	return func(ctx context.Context) error {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		dbProviders, err := loadDBLLMProviders(ctx, chatStore)
		if err != nil {
			return err
		}
		return mgr.SetLLMProviders(agentcore.MergeLLMProviders(bundleProviders, dbProviders, openRouterKey))
	}
}

// loadDBLLMProviders reads the enabled admin-managed provider rows (decrypted
// keys — resolver building only, never serialized) and converts them to the
// agentcore routing-table shape. Both the boot merge and the hot-swap callback
// go through this one read so they can never disagree.
func loadDBLLMProviders(ctx context.Context, chatStore *store.Store) ([]agentcore.ProviderConfig, error) {
	rows, err := chatStore.LLMProviderConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.ProviderConfig, 0, len(rows))
	for _, r := range rows {
		warnUndeclaredContextWindow("admin", r.Name, r.Type, r.ContextWindowTokens,
			"set its context window under Settings → Admin → Model providers")
		out = append(out, agentcore.ProviderConfig{
			Name:                r.Name,
			Type:                agentcore.ProviderType(r.Type),
			APIKey:              r.APIKey,
			BaseURL:             r.BaseURL,
			Models:              r.Models,
			Region:              r.Region,
			ContextWindowTokens: r.ContextWindowTokens,
		})
	}
	return out, nil
}

// warnUndeclaredContextWindow logs, at boot and on every admin-provider
// reload, an active non-OpenRouter provider with no declared context window.
// It still loads: refusing it would fail boot on existing manifests and rows
// saved before migration 082. But it runs on the resolver's conservative
// fallback (32,000 tokens, 4,096 for Ollama), which is below what a turn
// reserves with the default tool set, so its chat turns fail before sending
// and the user only sees a generic "could not complete this turn".
func warnUndeclaredContextWindow(source, name, typ string, window int, fix string) {
	if window > 0 || agentcore.ProviderType(typ) == agentcore.ProviderTypeOpenRouter {
		return
	}
	log.Printf("WARNING: %s provider %q (type %s) declares no context window, so it runs on the "+
		"conservative fallback (32,000 tokens; 4,096 for Ollama), which is below what a turn reserves "+
		"with the default tool set: its chat turns will fail before sending — %s", source, name, typ, fix)
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

// warnIfNoAdminKey logs a startup warning when ADMIN_API_KEY is unset. Since
// verifyAdminKey now fails closed on an empty configured key (an empty key used
// to match an empty header and silently authenticate admin routes), the admin
// surface (/keys, /users, /tasks/cleanup, config/MCP reload) is unreachable
// without it — warn so an operator on a bare deploy isn't left guessing why
// admin calls 403. bootstrap.sh always sets it. Extracted from run() to keep it
// within the cyclomatic budget.
func warnIfNoAdminKey(adminKey string) {
	if strings.TrimSpace(adminKey) == "" {
		log.Printf("WARNING: ADMIN_API_KEY is not set — the orchestrator admin API (/keys, /users, /tasks/cleanup, config/MCP reload) will reject all requests. Set it to enable admin access.")
	}
}

// confineFileToolsToWorkspace registers the workspace root as the base for the
// host-side file tools: the absolute cfg.WorkspaceRoot that PinWorkspaceRoot
// resolved, the same value the agent manager reads. Without this their
// AllowedBaseDirs falls back to the process cwd, which under systemd is the
// whole StateDirectory (/var/lib/fleet) and would let an absolute path reach
// DataDir attachments/uploads + api_keys.json that the sandbox never mounts.
// The root is relative only when PinWorkspaceRoot could not read the cwd, where
// filepath.Abs would fail too, so the IsAbs guard skips that case as before.
// Extracted from run() to keep it within the cyclomatic budget.
func confineFileToolsToWorkspace(workspaceRoot string) {
	if filepath.IsAbs(workspaceRoot) {
		tools.SetWorkspaceRoot(workspaceRoot)
	}
}

// orchestratorAddr resolves the orchestrator listener address. The default is
// loopback-only, matching the chat listener (config.Load defaults Addr to
// 127.0.0.1:8080) and the deployment contract: the deploy docs (Caddyfile,
// fleet.service, DEPLOYMENT.md, grafana/README.md) all promise that the Go
// backends bind loopback and are only reached through the on-box web tier /
// reverse proxy (which forwards the public /v1 API here with the web-tier
// header-trust headers stripped — ADR-0053). A bare ":8000" would bind every
// interface and expose the orchestrator admin surface AND the header-trust
// channel directly on hosts without a firewall. Multi-host topologies opt in
// explicitly via FLEET_ORCHESTRATOR_ADDR; the Caddyfile renderer follows the
// env-file value (scripts/lib/caddyfile.sh) when they do.
func orchestratorAddr() string {
	if v := strings.TrimSpace(envreg.Get("FLEET_ORCHESTRATOR_ADDR")); v != "" {
		return v
	}
	return "127.0.0.1:8000"
}

// pruneOrphanedSandboxes reclaims sandboxes orphaned by a PRIOR crash,
// routed by the active backend: pods under the kubernetes backend (#989),
// podman containers otherwise. Best-effort — log and continue. Extracted from
// run() to keep it within the cyclomatic budget; see the call site's comment
// for why this must run only AFTER the manager (and thus the pool) exists.
func pruneOrphanedSandboxes(pool *sandbox.Pool) {
	pruneCtx, pruneCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pruneCancel()
	if kb := pool.KubernetesBackend(); kb != nil {
		// Kubernetes backend (#989): orphans are pods, not podman containers.
		if n, err := kb.PruneOrphanedPods(pruneCtx); err != nil {
			log.Printf("startup: prune orphaned sandbox pods: %v", err)
		} else if n > 0 {
			log.Printf("startup: pruned %d orphaned sandbox pod(s) from a prior run", n)
		}
		return
	}
	if n, err := sandbox.PruneOrphanedContainers(pruneCtx, "podman"); err != nil {
		log.Printf("startup: prune orphaned sandbox containers: %v", err)
	} else if n > 0 {
		log.Printf("startup: pruned %d orphaned sandbox container(s) from a prior run", n)
	}
}

// resolveSandboxInto applies the bundle's sandbox block to cfg through the one
// shared resolver (agent.ApplyBundleSandbox: env wins, field by field;
// fail-closed on an unrecognized backend) and logs what the bundle decided.
// Extracted from run() to keep it within the cyclomatic budget.
func resolveSandboxInto(cfg *config.Config, bundle *clientconfig.Bundle) error {
	envImage := strings.TrimSpace(cfg.SandboxImage)
	envRuntime := strings.TrimSpace(cfg.SandboxRuntime)
	if err := agent.ApplyBundleSandbox(cfg, bundle); err != nil {
		return err
	}
	if envImage == "" && strings.TrimSpace(cfg.SandboxImage) != "" {
		log.Printf("sandbox: image resolved from bundle = %s", cfg.SandboxImage)
	}
	if cfg.SandboxRuntime != envRuntime && cfg.SandboxRuntime != "" {
		log.Printf("sandbox: runtime resolved to %q (podman OCI runtime name)", cfg.SandboxRuntime)
	}
	if cfg.SandboxBackend == sandbox.BackendKubernetes {
		log.Printf("sandbox: backend resolved to %q (control plane and runners split — pods in namespace %q)",
			cfg.SandboxBackend, defaultString(cfg.SandboxK8sNamespace, "fleet-sandboxes"))
	}
	return nil
}

// defaultString returns v, or fallback when v is empty or whitespace.
func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// registerRuntimeMetrics wires the pull-at-scrape gauges (#176): in-flight turn
// counts (interactive/scheduled), warm sandbox depth, host disk headroom, and
// the Go runtime's goroutine/heap counters. Extracted from run() to keep it
// within the cyclomatic budget.
func registerRuntimeMetrics(activeTurns, activeTasks func() int, sandboxPool *sandbox.Pool, guard *diskguard.Guard) {
	metrics.RegisterActiveAgents(activeTurns, activeTasks)
	if sandboxPool != nil {
		// Parked-and-ready containers (operationally interesting "how warm now"),
		// not the configured target size.
		metrics.RegisterSandboxPoolSize(func() int { _, avail := sandboxPool.Stats(); return avail })
	}
	// Disk headroom + the shed decision derived from it. The guard caches its
	// statfs, so a scrape does not mean a syscall.
	metrics.RegisterDiskGauges(func() metrics.DiskSample {
		st := guard.Status()
		return metrics.DiskSample{
			Available:  st.Available,
			TotalBytes: st.TotalBytes,
			FreeBytes:  st.FreeBytes,
			Shedding:   st.Shedding,
		}
	})
	// Goroutines + heap. Previously visible only as a point-in-time number in
	// the admin health JSON, where the shape that actually matters — a count
	// climbing steadily across a day — is invisible.
	metrics.RegisterRuntimeGauges()
}

// diskAdmissionGate adapts the disk guard to the runner's AdmitScheduled seam.
// Returns nil when the guard is disabled so the claim path keeps its original
// zero-overhead shape on a box that has opted out.
func diskAdmissionGate(guard *diskguard.Guard) func() (bool, string) {
	return func() (bool, string) {
		if httpapi.UpdateDrainActive() {
			return false, "update drain active"
		}
		if !guard.Enabled() {
			return true, ""
		}
		st := guard.Status()
		if !st.Shedding {
			return true, ""
		}
		return false, fmt.Sprintf("%s (%.1f%% free on %s, floor %d%%)",
			st.Reason(), st.FreePercent, st.Path, st.MinFreePercent)
	}
}

// logDiskGuard states the disk-backpressure posture at boot, and warns straight
// away if the box is ALREADY below the floor — an operator restarting a wedged
// box needs to know that before wondering why nothing is being claimed.
func logDiskGuard(guard *diskguard.Guard, cfg *config.Config) {
	if !guard.Enabled() {
		log.Printf("disk backpressure: OFF (FLEET_DISK_MIN_FREE_PERCENT=0)")
		return
	}
	st := guard.Sample()
	log.Printf("disk backpressure: ON (floor %d%% free on %s; scheduled work pauses below it, chat is never gated)",
		cfg.DiskMinFreePercent, cfg.DataDir)
	switch {
	case !st.Available:
		log.Printf("⚠ disk backpressure: cannot measure %s (%s) — failing OPEN, scheduled work will not be held back", cfg.DataDir, st.Err)
	case st.Shedding:
		log.Printf("⚠ disk backpressure: only %.1f%% free on %s at boot — scheduled tasks are held back until space is reclaimed", st.FreePercent, cfg.DataDir)
	}
}

// workerStatsProvider adapts the sched store's dashboard stats into the
// httpapi.WorkerStats the admin health summary embeds (#301), keeping httpapi
// free of any sched-package import. Extracted from run() to keep it within the
// cyclomatic budget.
// taskSchedulerProvider adapts the chat server's sched-agnostic TaskScheduleRequest
// (#239) to the orchestrator's task-create path. It maps the request to a
// models.TaskCreate and calls schedStorage.EnqueueTask — the SAME constructor +
// persistence (models.NewTask + AddTask, cron validation, DB name-uniqueness) the
// create_task tool uses and that underlies POST /tasks. It does NOT re-run the
// HTTP handler's field-limit checks (max_iterations bounds, prompt-min-length),
// matching the create_task seam; the human approval plus the box-wide ceilings
// bound a misconfigured task. The human approval IS the authorization; no task is
// created until the user clicks Approve. NOTE: chat-created tasks are left
// unattributed (CreatedBy nil) — like create_task-spawned tasks — so they run
// against the shared global MCP catalog, NOT the requesting user's personal
// remote-MCP connections (#443/#449). That is the safe (less-access) direction.
// selfImproveDistiller returns the feedback→learned-instruction distiller
// (#516), or nil when self-improvement is disabled.
func selfImproveDistiller(cfg *config.Config, mgr *agent.Manager) handlers.LearnedInstructionDistiller {
	if !cfg.SelfImproveEnabled {
		return nil
	}
	return mgr
}

// learnedInstructionProvider returns the run-start learned-instruction source
// (#516), or nil when self-improvement is disabled — in which case scheduled
// runs never inject a learned instruction (byte-for-byte unchanged).
func learnedInstructionProvider(cfg *config.Config, st *storage.Storage) scheduledrun.LearnedInstructionProvider {
	if !cfg.SelfImproveEnabled {
		return nil
	}
	return st
}

// configurePIIRedaction installs the process-wide PII redactor (#450) from the
// env config, before the store is up. The env→mode semantics live in ONE
// place — the piiRedactorState in the workspace-settings wiring — so what
// boots here is byte-identical to what the admin panel reports as the default
// and what Reset re-applies. Any DB override is layered on later by the
// settings boot apply (appendWorkspaceSettingsOption).
func configurePIIRedaction(cfg *config.Config) {
	st := newPIIRedactorState(cfg)
	if err := st.rebuild(); err != nil {
		// defaultPIIRedactionMode only ever yields a registry value, so this is
		// belt-and-suspenders; a failure here means no redactor installed.
		log.Printf("PII redaction: failed to install redactor from env config: %v", err)
		return
	}
	if st.mode != string(piiredact.ModeOff) {
		// The mode is a validated constant, never raw env input.
		log.Printf("PII redaction: enabled (mode=%s, engine=pattern)", st.mode)
	}
}

func configureGuardrail(cfg *config.Config) {
	g := newGuardrailState(cfg)
	g.mu.Lock()
	err := g.rebuildLocked()
	g.mu.Unlock()
	if err != nil {
		log.Printf("content guardrail: not installed: %v", err)
		mode, _ := guardrail.ParseMode(g.mode)
		// Preserve the configured fail semantics even when the URL is missing:
		// block mode rejects ingress, while observe mode records the detector
		// outage on each boundary. The admin panel remains available to repair it.
		agentcore.SetGuardrail(true, mode == guardrail.ModeBlock, string(mode), g.profile,
			guardrail.UnavailableDetector{Err: err})
		return
	}
	if g.mode != string(guardrail.ModeOff) {
		log.Printf("content guardrail: enabled")
	}
}

// emailReplierFor wires email reply-back (#511) only when SMTP is configured for
// sending, so a deployment without SMTP does no per-run reply lookups. Returns a
// nil interface (reply-back off) otherwise.
// buildTaskNotifier constructs the task-completion notifier (#208). Config
// comes from the host env-file (FLEET_SMTP_*/FLEET_WEBHOOK_*/FLEET_NOTIFY_*);
// secrets stay host-side and never enter the sandbox or the log. Default OFF —
// with none of those vars set, Enabled() is false and the fire path is a
// no-op. When Web Push is configured (#292) it is attached as a per-user
// notify backend, so task terminal + progress/ask events fan out to the
// owner's browsers through the SAME Notify path as email/webhook — one
// delivery pipeline, no parallel trigger plumbing.
func buildTaskNotifier(pushSvc *webpush.Service) *notify.Notifier {
	n := notify.New(notify.Load())
	if n.Enabled() {
		log.Printf("task notifications: enabled")
	}
	if pushSvc.Enabled() {
		n.SetPush(pushSvc)
	}
	return n
}

// emailReplierFor always wires the notifier as the reply-back seam (#511):
// ReplyToEmailEvent no-ops internally when SMTP isn't configured, and the
// check is LIVE — an admin enabling SMTP from the Notifications panel turns
// reply-back on with no restart (a boot-time nil would freeze it off).
func emailReplierFor(n *notify.Notifier) runner.EmailReplier {
	if n.ReplyEnabled() {
		log.Printf("email trigger reply-back: enabled")
	}
	return n
}

// startMaintenanceLoop is the box's single reclamation driver: on a timer it
// runs the chat plane's full pass (database retention sweeps + attachment files
// + orphaned per-conversation workspaces), the orchestrator's temp-upload
// cleanup, and the git-worktree reclaimer.
//
// The loop exists because ALL of that used to be a side effect of chat traffic.
// The database sweeps and the workspace sweep ran only at the tail of a
// completed chat turn, so a scheduler-only box — or any box whose chat went
// quiet — grew its turn ledgers, expired conversations, terminal input-queue
// rows and orphan workspace dirs without bound, while a busy box ran the same
// global sweeps once per turn. The worktree reclaimer had no automatic caller
// at all: it was reachable only by an operator typing `fleet worktree prune`.
// Now the timer is the guarantee and the post-turn path (rate-gated, see
// internal/httpapi/maintenance.go) is the promptness optimization on top.
//
// chatSrv may be nil in configurations that run no chat plane; the loop then
// still drives the orchestrator-side reclaimers. Bound to ctx so it stops on
// shutdown; a panic is contained so maintenance can't crash the process.
func startMaintenanceLoop(ctx context.Context, cfg *config.Config, h *handlers.Handlers, chatSrv *httpapi.Server, chatStore *store.Store) {
	safe.Go("cmd.maintenance-loop", func() {
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runMaintenancePass(ctx, cfg, h, chatSrv, chatStore)
			}
		}
	})
}

// pruneStaleWorktrees reclaims per-run git worktrees that no run will clean up:
// a process crash between `git worktree add` and its deferred removal, a task
// with auto_cleanup off, or a `git worktree remove` that failed. Each is a full
// checkout of the workspace repository, so on a box that uses worktree
// isolation this is usually the largest single reclaimer in the pass.
//
// Disabled by FLEET_WORKTREE_PRUNE_AGE=0. The sweep is a no-op (not an error)
// on the majority of boxes, which never enable worktree isolation and so have
// no .fleet-worktrees directory at all.
func pruneStaleWorktrees(ctx context.Context, cfg *config.Config) {
	if cfg.WorktreePruneAge <= 0 {
		return
	}
	root := worktree.ResolveWorkspaceRoot(cfg.WorkspaceRoot)
	res, err := worktree.PruneStale(ctx, root, cfg.WorktreePruneAge, false)
	for _, w := range res.Warnings {
		log.Printf("maintenance: worktree prune: %s", w)
	}
	if err != nil {
		log.Printf("maintenance: worktree prune: %v", err)
		return
	}
	if n := res.Count(); n > 0 {
		log.Printf("maintenance: reclaimed %d stale worktree dir(s) older than %s", n, cfg.WorktreePruneAge)
	}
}

// runMaintenancePass is one iteration of the maintenance loop, factored out so
// it is directly testable and so each step's failure is visibly independent of
// the others. Every step is best-effort: one erroring must never skip the rest.
func runMaintenancePass(ctx context.Context, cfg *config.Config, h *handlers.Handlers, chatSrv *httpapi.Server, chatStore *store.Store) {
	// The whole pass is bounded so a pathological filesystem (a huge attachment
	// tree on slow storage) cannot wedge the loop and silently stop every later
	// iteration. Exceeding it aborts the remaining steps, which the next tick
	// retries.
	passCtx, cancel := context.WithTimeout(ctx, maintenancePassTimeout)
	defer cancel()

	ttl := time.Duration(cfg.ConversationTTL) * 24 * time.Hour

	if chatSrv != nil {
		chatSrv.RunMaintenance(passCtx)
		// Tell the post-turn gate this pass happened so the two drivers don't
		// each keep their own idea of "recently swept" and double the work.
		chatSrv.NoteMaintenanceRun(time.Now())
	} else if n, err := store.SweepAttachments(cfg.EmailAttachmentDir, ttl); err != nil {
		// No chat plane to run the full pass: the attachment tree is still
		// shared with the orchestrator upload path, so sweep it directly.
		log.Printf("maintenance: attachments: %v", err)
	} else if n > 0 {
		log.Printf("maintenance: removed %d expired attachment file(s)", n)
	}

	// Orchestrator temp_uploads — the sched plane's own upload staging area,
	// which the chat plane's sweeps do not cover.
	h.CleanupTempFiles(ttl)

	// Abandoned per-user remote-MCP OAuth flow rows. Correctness is already
	// guaranteed by their single-use + expiry checks; this only reclaims the
	// rows so the table does not grow forever on a box with many connectors.
	if chatStore != nil {
		if _, err := chatStore.SweepExpiredOAuthFlows(passCtx); err != nil {
			log.Printf("maintenance: remote-mcp oauth flow sweep: %v", err)
		}
	}

	// Git worktrees orphaned by a crash, by a task with auto_cleanup off, or by
	// a `git worktree remove` that failed after the run. The age bound is
	// generous on purpose: a worktree younger than it may belong to a task
	// still running.
	pruneStaleWorktrees(passCtx, cfg)
}

// mcpCatalogEntries renders the orchestrator MCP picker's catalog rows: the
// Manager's live always-on status rows (locked in the picker, Enabled =
// discovery-backed availability) ahead of the Optional-server catalog
// (ADR-0052). Named — not a closure in run() — so its loops don't count
// against run's gocyclo budget; it reads the Manager fresh on every call, so
// hot-reload keeps the picker current.
func mcpCatalogEntries(mgr *agent.Manager) []handlers.MCPServerCatalogEntry {
	catalog := mgr.MCPServerCatalog()
	alwaysOn := mgr.AlwaysOnMCPServerCatalog()
	out := make([]handlers.MCPServerCatalogEntry, 0, len(catalog)+len(alwaysOn))
	for _, info := range alwaysOn {
		out = append(out, handlers.MCPServerCatalogEntry{
			Name:        info.Name,
			DisplayName: info.DisplayName,
			Description: info.Description,
			ToolCount:   info.ToolCount,
			Enabled:     info.Available,
			Accounts:    info.Accounts,
			AlwaysOn:    true,
		})
	}
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
}

// chatUsageProvider adapts the chat store's turn_metrics roll-up into the
// sched-side usage bucket shape (#601 part 1): GET /admin/usage merges the
// sched DB's task_iterations roll-up with the chat store's, and the two live
// in separate databases, so the orchestrator handler — and, in part 2, the
// budget enforcer — reach the chat store through this ONE seam. Read-only
// aggregation of already-persisted metering — no new accounting path. Kept
// separate from run() so the adapter stays out of run()'s cyclomatic budget.
func chatUsageProvider(chatStore *store.Store) handlers.ChatUsageProvider {
	return func(ctx context.Context, from, to time.Time, groupBy string) ([]schedmodels.UsageBucket, error) {
		rows, err := chatStore.UsageSummary(ctx, from, to, groupBy)
		if err != nil {
			return nil, err
		}
		out := make([]schedmodels.UsageBucket, 0, len(rows))
		for _, r := range rows {
			out = append(out, schedmodels.UsageBucket{
				Key:              r.Key,
				Label:            r.Label,
				ChatCostUSD:      r.CostUSD,
				PromptTokens:     r.PromptTokens,
				CompletionTokens: r.CompletionTokens,
				CachedTokens:     r.CachedTokens,
				ChatTurns:        r.Turns,
			})
		}
		return out, nil
	}
}

// chatUserDayUsageProvider adapts the chat store's per-user-per-day
// turn_metrics roll-up into the sched-side shape for the adoption report —
// the same one-seam pattern (and rationale) as chatUsageProvider above.
func chatUserDayUsageProvider(chatStore *store.Store) handlers.ChatUserDayUsageProvider {
	return func(ctx context.Context, from, to time.Time) ([]schedmodels.UserDayUsage, error) {
		rows, err := chatStore.UsageByUserDay(ctx, from, to)
		if err != nil {
			return nil, err
		}
		out := make([]schedmodels.UserDayUsage, 0, len(rows))
		for _, r := range rows {
			out = append(out, schedmodels.UserDayUsage{
				User:             r.User,
				Day:              r.Day,
				CostUSD:          r.CostUSD,
				PromptTokens:     r.PromptTokens,
				CompletionTokens: r.CompletionTokens,
				CachedTokens:     r.CachedTokens,
				Units:            r.Turns,
			})
		}
		return out, nil
	}
}

// chatMemberRoleProvider resolves a chat-plane user's RBAC role ("admin",
// "member", or "viewer") by email for /prompts authorization when the caller's
// independent Operations Center role (ops_role) is "none".
func chatMemberRoleProvider(chatStore *store.Store) handlers.ChatMemberRoleProvider {
	return func(ctx context.Context, email string) (string, error) {
		u, err := chatStore.GetUser(ctx, email)
		if errors.Is(err, store.ErrUserNotFound) || u == nil {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return u.Role, nil
	}
}

// chatAccountsProvider adapts the chat store's provisioned-user roster into
// the adoption report's seat list — the denominator for the adoption rate and
// the "not yet active" roster. Read-only; emails only, never credentials.
func chatAccountsProvider(chatStore *store.Store) handlers.ChatAccountsProvider {
	return func(ctx context.Context) ([]schedmodels.AdoptionSeat, error) {
		users, err := chatStore.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]schedmodels.AdoptionSeat, 0, len(users))
		for _, u := range users {
			out = append(out, schedmodels.AdoptionSeat{
				Email:     u.Email,
				CreatedAt: time.Unix(u.CreatedAt, 0).UTC(),
			})
		}
		return out, nil
	}
}

// userSkillDocsProvider adapts the chat store's active builder skills into the
// runner's inline-prompt shape.
func userSkillDocsProvider(chatStore *store.Store) func(ctx context.Context, email string) ([]scheduledrun.UserSkillDoc, error) {
	return func(ctx context.Context, email string) ([]scheduledrun.UserSkillDoc, error) {
		skills, err := chatStore.ListActiveUserSkills(ctx, email)
		if err != nil {
			return nil, err
		}
		docs := make([]scheduledrun.UserSkillDoc, 0, len(skills))
		for _, sk := range skills {
			docs = append(docs, scheduledrun.UserSkillDoc{Name: sk.Name, Description: sk.Description, Body: sk.Body})
		}
		return docs, nil
	}
}

// skillProposerFactory binds propose_skill staging to a resolved task owner.
func skillProposerFactory(chatStore *store.Store) func(ownerEmail string) agentcore.SkillProposer {
	return func(ownerEmail string) agentcore.SkillProposer {
		return scheduledSkillProposer{store: chatStore, owner: ownerEmail}
	}
}

// scheduledSkillProposer stages a propose_skill call from a scheduled run as a
// PROPOSED builder skill for the task owner (docs/SKILLS.md phase 3).
type scheduledSkillProposer struct {
	store *store.Store
	owner string
}

func (p scheduledSkillProposer) Propose(name, description, body, _ string) (string, error) {
	sk, err := p.store.CreateUserSkillProposal(context.Background(), p.owner, name, description, body)
	if err != nil {
		return "", err
	}
	return sk.ID, nil
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

// recoverStrandedTurns projects turns left 'running' by a crash into canonical
// history at boot (#798): journal-authoritative tool calls paired with results
// (unknown-outcome synthesized when missing), best-effort assistant text from
// turn_events, one explicit interrupted turn per crash. A failure logs loudly
// but does not abort boot: the DB may be degraded, and the affected turns stay
// 'running' for the next boot to retry. See docs/TURN-JOURNAL.md + ADR-0039.
func recoverStrandedTurns(chatStore *store.Store, inputQueueRetentionDays int) {
	recCtx, recCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer recCancel()
	recovered, err := chatStore.RecoverStrandedTurns(recCtx)
	if err != nil {
		log.Printf("stranded-turn recovery: %v (recovered %d before failing)", err, len(recovered))
	}
	for _, r := range recovered {
		log.Printf("stranded-turn recovery: turn %s (conv %s) projected %d entries, %d unknown-outcome tool calls",
			r.TurnID, r.ConversationID, r.Projected, r.Synthesized)
	}
	// Outcomes must follow recovered calls. The store excludes conversations
	// still containing running turns, so one failed recovery cannot strand
	// otherwise-safe approvals in unrelated conversations.
	if count, err := chatStore.RecoverStrandedApprovals(recCtx); err != nil {
		log.Printf("stranded-approval recovery: %v", err)
	} else if count > 0 {
		log.Printf("stranded-approval recovery: %d unknown outcomes", count)
	}
	// Input-queue recovery (#785) resolves rows claimed/injected by the dead
	// process against the #798 durable record: durably-persisted ones complete,
	// injected steers whose turn dispatched tools after the injection cancel
	// (#823: re-running could duplicate side effects), the rest return to
	// 'queued' — visible and addressable, deliberately NOT auto-drained (a
	// restart must not start unattended LLM spend).
	requeued, completed, cancelled, qerr := chatStore.RecoverInputQueue(recCtx)
	if qerr != nil {
		log.Printf("input-queue recovery: %v", qerr)
	} else if requeued+completed+cancelled > 0 {
		log.Printf("input-queue recovery: %d input(s) re-queued, %d completed, %d cancelled (post-injection side effects; not re-run)", requeued, completed, cancelled)
	}
	// Resume after approval (docs/RESUME-AFTER-APPROVAL.md): a resume that was
	// due but had not started when the last process stopped is DROPPED, with a
	// note in its conversation, never started unattended by a restart. It runs
	// after both recoveries above: RecoverStrandedApprovals settles the cards
	// cut off mid-call (as outcome unknown), and RecoverInputQueue returns an
	// uncommitted resume row to the queue, which this then cancels.
	// On its own bounded context (the shared one may be spent by the
	// recoveries above), retried once. A failure is not fatal: the chat
	// server also refuses to launch a 'resume' row from an earlier process.
	noted, err := dropApprovalResumesAtBoot(chatStore)
	if err != nil {
		log.Printf("approval-resume recovery: %v (any stale resume row is still refused at launch)", err)
	} else if noted > 0 {
		log.Printf("approval-resume recovery: dropped the pending automatic resume in %d conversation(s), with a note in each", noted)
	}
	// Reclaim terminal idempotency rows at boot as well as after turns. Recovery
	// runs first so a stranded non-terminal row is resolved before retention is
	// considered; non-terminal rows are never eligible for deletion.
	if purged, err := chatStore.PurgeTerminalInputs(recCtx,
		time.Duration(inputQueueRetentionDays)*24*time.Hour); err != nil {
		log.Printf("input-queue startup purge: %v", err)
	} else if purged > 0 {
		log.Printf("input-queue startup purge: removed %d terminal row(s)", purged)
	}
}

// dropApprovalResumesAtBoot runs the boot sweep for resume after approval on
// its own deadline, with one retry.
func dropApprovalResumesAtBoot(chatStore *store.Store) (int, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var noted int
		noted, err = chatStore.DropApprovalResumesAtBoot(ctx)
		cancel()
		if err == nil {
			return noted, nil
		}
	}
	return 0, err
}

// warnUncategorizedConnectors logs the bundle connectors with no Tools-picker
// category, so they get tagged; they show under Misc Tools meanwhile.
func warnUncategorizedConnectors(bundle *clientconfig.Bundle) {
	if untagged := bundle.UncategorizedConnectors(); len(untagged) > 0 {
		log.Printf("warning: client config: %d connector(s) have no category and show under Misc Tools in the Tools picker; add category: to their mcp_servers entry: %s",
			len(untagged), strings.Join(untagged, ", "))
	}
}
