// `fleet acp` — fleet AS an ACP AGENT over stdio (Plan v4, P-ACP-3 ingress).
//
// An external host (Zed / Neovim / any ACP editor) launches `fleet acp` and
// drives fleet's OWN governed, sandboxed pipeline: each prompt runs the SAME
// interactive turn the web path runs (agent.Manager.RunTurn → agentcore.Run),
// streaming replies back as ACP session/update and asking the human over ACP
// session/request_permission when fleet's policy gates a critical action.
//
// This entrypoint constructs the SAME real dependencies the server path does
// (client-config bundle, model resolver from OPENROUTER_API_KEY, store, sandbox
// pool, MCP catalog, runtime-flavor catalog, notes provider) so the ingress turn
// inherits full governance — then serves ACP over stdin/stdout (stderr → logs,
// no PTY). It does NOT boot the HTTP servers, scheduler, or worker pool.
//
// Trust model (documented honestly in docs/USING-AGENTS.md): launching
// `fleet acp` runs as the box user = the same trust as running fleet. This is
// LOCAL-PROCESS trust, distinct from the web path's signed-key auth. Remote
// ingress is out of scope.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/acpingress"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/ElcanoTek/fleet/internal/store"
)

func runACP() error {
	// All diagnostics go to stderr — stdout is the JSON-RPC protocol channel.
	log.SetOutput(os.Stderr)
	log.SetPrefix("[fleet-acp] ")

	// Same bundle → config → MCP-catalog boot as the server path, so the ingress
	// turn sees the identical persona/notes/MCP/model defaults + sandbox image +
	// agent-policy the web path sees.
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)

	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.MCPServers = bundle.MCPServerConfigs()
	if strings.TrimSpace(cfg.SandboxImage) == "" {
		if ref := bundle.Sandbox().ResolvedImageRef(); ref != "" {
			cfg.SandboxImage = ref
		}
	}

	// Install the bundle's agent tool-behavior policy (critical-tool suffixes
	// etc.) BEFORE any turn — identical to the server path, so ingress governs
	// the same critical-tool surface.
	bundlePolicy := bundle.AgentPolicy()
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		ParallelSafeTools:       bundlePolicy.ParallelSafeTools,
		CriticalToolSuffixes:    bundlePolicy.CriticalToolSuffixes,
		CriticalToolSubstitutes: bundlePolicy.CriticalToolSubstitutes,
	})

	// ── stores ── chat store holds the ingress conversation + approvals + history;
	// the sched store backs the notes provider (the SAME admin-curated knowledge
	// base the server injects every turn).
	chatStore, err := store.Open(chatDSN(cfg))
	if err != nil {
		return fmt.Errorf("open chat DB: %w", err)
	}
	defer chatStore.Close()

	schedStorage := storage.New()
	if err := schedStorage.Initialize(schedDSN()); err != nil {
		return fmt.Errorf("open sched DB: %w", err)
	}
	defer schedStorage.Close()
	schedStorage.SetTimezone(timezone(cfg))
	notesProvider := &notesAdapter{store: sched.NewStore(schedStorage.DB())}

	// ── interactive engine (the SAME concrete turnEngine the server drives) ──
	mgr, err := agent.New(agent.ManagerOptions{
		Config:               cfg,
		ServerSpecs:          scheduledrun.BuildMCPSpecs(cfg),
		PersonasDir:          bundle.PersonasDir,
		ProtocolsDir:         bundle.ProtocolsDir,
		SkillsDir:            bundle.SkillsDir,
		SystemPromptsDir:     bundle.SystemPromptsDir,
		ChatSystemPromptFile: "chat.md",
		NotesProvider:        notesProvider,
		NoteProposer:         notesProvider, // same adapter; wires propose_note over ingress too
	})
	if err != nil {
		return fmt.Errorf("build interactive engine: %w", err)
	}
	defer mgr.Close()

	// Wire the runtime-flavor catalog so the ingress turn runs the configured
	// flavor (native-inprocess dev / native-acp prod sandbox), inheriting "our
	// agent runs in a sandbox" for free.
	var nativeAgentImage string
	if acpRT, ok := bundle.Runtime(clientconfig.RuntimeNativeACP); ok {
		nativeAgentImage = acpRT.Image
	}
	mgr.SetRuntimes(bundle.Runtimes(), bundle.DefaultRuntime(), nativeAgentImage)

	// Resolve the ingress turn config from the environment + bundle defaults.
	model := acpModel(cfg)
	if model == "" {
		return fmt.Errorf("no ingress model: set FLEET_ACP_MODEL (or LLM_DEFAULT_MODEL) to the OpenRouter slug `fleet acp` should drive")
	}
	runtime := acpRuntime(bundle)
	principal := acpPrincipal()

	// Lockdown for ingress mirrors the web path's req.Lockdown||LockdownOnly OR:
	// a LockdownOnly server seals EVERY ingress turn (so an editor connecting can
	// never get a network-enabled turn), and FLEET_ACP_LOCKDOWN opts a session in
	// otherwise. Validate availability + the model allow-list up-front so the
	// operator gets an immediate, honest failure rather than a per-turn refusal.
	lockdown := acpLockdown() || cfg.LockdownOnly
	if lockdown {
		if !cfg.LockdownAvailable() {
			return fmt.Errorf("lockdown requested (FLEET_ACP_LOCKDOWN/CHAT_LOCKDOWN_ONLY) but no sandbox image is configured")
		}
		if !cfg.LockdownAllows(model) {
			return fmt.Errorf("model %q is not on the lockdown allow-list (CHAT_LOCKDOWN_ALLOWED_MODELS); ingress lockdown turns would fail", model)
		}
	}

	ingress := acpingress.New(
		mgr,                                 // TurnEngine
		chatStore,                           // ConversationStore
		chatStore,                           // ApprovalStore
		acpingress.NewStagedToolRunner(mgr), // StagedToolRunner (MCP + sandbox via the Manager)
		acpingress.Config{
			Principal: acpingress.Principal{Email: principal},
			Persona:   strings.TrimSpace(os.Getenv("FLEET_ACP_PERSONA")),
			Model:     model,
			Runtime:   runtime,
			Lockdown:  lockdown,
			Version:   "fleet",
		},
	)

	// ACP over stdio: stdout is the protocol channel, stderr is diagnostics.
	conn := acp.NewAgentSideConnection(ingress, os.Stdout, os.Stdin)
	conn.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ingress.SetAgentConnection(conn)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("ready on stdio (model=%s runtime=%q lockdown=%t principal=%s sandbox_image=%q)",
		model, runtime, lockdown, principal, cfg.SandboxImage)
	select {
	case <-conn.Done():
		log.Printf("editor disconnected")
	case <-ctx.Done():
		log.Printf("shutdown signal received")
	}
	return nil
}

// acpModel resolves the OpenRouter slug ingress turns drive. FLEET_ACP_MODEL
// wins; else LLM_DEFAULT_MODEL (the same env the server reads for its default).
func acpModel(_ *config.Config) string {
	if v := strings.TrimSpace(os.Getenv("FLEET_ACP_MODEL")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("LLM_DEFAULT_MODEL"))
}

// acpRuntime resolves the execution flavor for ingress turns. FLEET_ACP_RUNTIME
// wins (validated against the bundle catalog); else the bundle default.
func acpRuntime(bundle *clientconfig.Bundle) string {
	if want := strings.TrimSpace(os.Getenv("FLEET_ACP_RUNTIME")); want != "" {
		if rt, ok := bundle.Runtime(want); ok {
			return rt.Name
		}
		log.Printf("FLEET_ACP_RUNTIME %q not in bundle; using default %s", want, bundle.DefaultRuntime()) //nolint:gosec // G706 false positive: want is an operator-configured env var, not request input; %q quotes it.
	}
	return bundle.DefaultRuntime()
}

// acpLockdown reports whether the operator opted this `fleet acp` process into
// lockdown via FLEET_ACP_LOCKDOWN. It is ORed with the server's LockdownOnly, so
// a LockdownOnly server always wins regardless of this value (see runACP).
func acpLockdown() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLEET_ACP_LOCKDOWN"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// acpPrincipal resolves the audit identity ingress sessions bind to.
// FLEET_ACP_PRINCIPAL wins; else the package default placeholder.
func acpPrincipal() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_ACP_PRINCIPAL")); v != "" {
		return v
	}
	return acpingress.DefaultPrincipalEmail
}
