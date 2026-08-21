package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The concrete interactive engine. Manager owns the per-process state reused
// across every chat turn (MCP client, model resolver, sandbox warm pool, native
// tools, persona/protocol/system-prompt source dirs) and drives one live turn
// through agent.RunInteractiveTurn → agentcore.Run, forwarding the run's
// streamed events into the caller's EventSink and mapping the accumulated
// agentcore.RunEntry transcript back to agent.HistoryEntry for persistence.
//
// This is the concrete implementation of httpapi.turnEngine: RunTurn /
// Summarize / SuggestTitle / MCPBroker / SandboxPool / MCPServerCatalog /
// ListPersonas. cmd/fleet constructs it once at boot and hands it to
// httpapi.New.

// TurnInput carries per-turn inputs from the HTTP layer to the engine.
type TurnInput struct {
	UserMessage string
	Persona     string // persona name, e.g. "assistant"
	// Model is the OpenRouter slug to drive this turn. Required: the server
	// holds no default. A blank or unresolvable slug fails the turn up-front.
	Model   string
	History []HistoryEntry

	// ImageAttachments are user-attached image files for THIS turn only.
	ImageAttachments []ImageAttachment

	// ConversationID scopes per-turn filesystem state to this chat.
	ConversationID string

	// UserEmail is the authenticated user driving this turn. Used to resolve the
	// user's connected remote (hosted) MCP servers + mint their OAuth bearers for
	// the per-turn overlay (#443). Empty disables the overlay for the turn.
	UserEmail string

	// OptionalMCPServersEnabled is the conversation's opt-in list for Optional
	// MCP servers (e.g. gamma). nil/empty means "no optional servers".
	OptionalMCPServersEnabled []string

	// UserSkills is the caller's user-authored skill roster for this turn
	// (docs/SKILLS.md phase 2): the HTTP layer materializes each ACTIVE skill
	// into the conversation workspace (user-skills/<name>/SKILL.md) before the
	// run and passes the Level-1 metadata here for the prompt roster. Only the
	// author's own runs ever see them.
	UserSkills []UserSkillPromptEntry

	// MCPAccountDefaults maps an opted-in server name to the credential-account
	// seat the user chose as their default on the connections page (unified
	// connector UX). Absent/empty entries use the server's default seat. The
	// HTTP layer validates a seat against the catalog before passing it, so a
	// stale pref (seat's env vars removed) degrades to the default seat instead
	// of failing the turn.
	MCPAccountDefaults map[string]string

	// Memories are user-scoped long-term facts injected into the system prompt.
	// Project-scoped shared memories (#509) ride the same slice, prefixed
	// "[project] " by the HTTP layer.
	Memories []string

	// ProjectInstructions are the standing instructions of the project/space
	// this conversation belongs to (#509); injected as a dedicated system-prompt
	// section. Empty = no project.
	ProjectInstructions string

	// ApprovalStager, when set, intercepts critical tool calls (send_email /
	// risky bash / preview_email / suggest_advanced_model) and routes them
	// through the approvals table instead of running directly.
	ApprovalStager ApprovalStager

	// MemoryProposer, when set, intercepts propose_memory tool calls and creates
	// pending memory proposals for user confirmation.
	MemoryProposer MemoryProposer

	// SkillProposer, when set, intercepts propose_skill tool calls and stages
	// an agent-drafted personal skill for THIS turn's user to review in the
	// builder (docs/SKILLS.md phase 3). Per-turn like MemoryProposer so the
	// proposal is attributed to the right owner.
	SkillProposer agentcore.SkillProposer

	// Lockdown is set when the conversation row has lockdown=true. Forces a
	// per-turn container sandbox and constrains the resolved model slug to the
	// operator's lockdown allow-list.
	Lockdown bool

	// ThinkingConfig, when set and Enabled, activates Claude extended thinking
	// (#220) for this turn. The caller (httpapi) resolves it from the
	// per-conversation override or the global default before the call; nil = off.
	// A non-Claude model silently ignores it (see agentcore.supportsExtendedThinking).
	ThinkingConfig *agentcore.ThinkingConfig

	// Durable turn journal + gated terminal commit (#798). All three are
	// optional (evals/tests leave them nil, preserving legacy behavior).
	//
	// TurnJournal persists tool intents/results from inside the run loop.
	// CommitUser durably persists the user entry BEFORE the first provider
	// call; an error fails the turn before any side effect.
	// CommitTerminal transactionally projects the turn's transcript (everything
	// AFTER the already-committed user entry) into canonical history. RunTurn
	// calls it BEFORE emitting turn.completed / turn.cancelled — a failure
	// yields ErrHistoryCommitFailed and a visible turn.error instead of a
	// completed answer that disappears on reload.
	TurnJournal    agentcore.TurnJournal
	CommitUser     func(ctx context.Context, entry HistoryEntry) error
	CommitTerminal func(entries []HistoryEntry, cancelled bool) error

	// SteerSource feeds mid-turn steer inputs (#785) into the run's
	// PrepareStep boundary. nil = no steering.
	SteerSource agentcore.SteerSource
}

// TurnResult is returned after a turn completes.
type TurnResult struct {
	FinalText           string
	NewHistory          []HistoryEntry // the user msg + any assistant/tool events this turn
	PromptTokens        int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	CostUSD             float64
	// Model is the resolved OpenRouter slug this turn actually ran against.
	Model string
	// Cancelled is true when the turn ended because the caller's ctx was
	// cancelled. Partial history and cost are still returned.
	Cancelled bool
}

// SummarizeInput carries the inputs the summarize endpoint needs.
type SummarizeInput struct {
	// History is the full conversation history up to (and not including) any new
	// user message.
	History []HistoryEntry
	// Model is the OpenRouter slug to drive the summarize call.
	Model string
	// Lockdown mirrors TurnInput.Lockdown.
	Lockdown bool
	// OnTextDelta, if non-nil, is invoked for each chunk of summary text the
	// model produces (wired to the SSE stream). Optional.
	OnTextDelta func(text string)
}

// SummarizeResult is what the summarize endpoint returns.
type SummarizeResult struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}

// ManagerOptions configure a Manager.
type ManagerOptions struct {
	Config           *config.Config
	ServerSpecs      map[string]MCPServerSpec
	PersonasDir      string
	ProtocolsDir     string
	SkillsDir        string
	SystemPromptsDir string

	// Limiter is the SHARED process-wide admission governor. When set, RunTurn
	// admits each interactive turn through it (with a short bounded wait, then a
	// graceful ErrAtCapacity) so chat counts against the box-wide concurrency cap.
	// Nil = no interactive admission control (cutlass one-shot, tests).
	Limiter *admission.Limiter

	// ChatSystemPromptFile is the bundle-relative filename (inside
	// SystemPromptsDir) of the INTERACTIVE base prompt. Empty defaults to
	// "chat.md". The scheduled path reads its own base (default.md) separately.
	ChatSystemPromptFile string

	// NotesProvider supplies the admin-curated knowledge base injected into the
	// system prompt every turn. Nil = no notes section.
	NotesProvider agentcore.NotesProvider

	// NoteProposer stages agent-proposed admin-notes edits (propose_note). Wired
	// onto the Manager so every interactive turn inherits propose_note as a single
	// agentcore-boundary guarantee. Typically the SAME notesAdapter as NotesProvider.
	// Nil = propose_note unavailable. Note: note proposals are intentionally GLOBAL
	// (author "agent", un-scoped) — unlike per-conversation/user memory proposals.
	NoteProposer agentcore.NoteProposer

	// PersonaPolicies is the per-persona tool allowlist (Gate-4, #294), keyed by
	// persona basename, translated from the bundle manifest's personas: block.
	// nil/empty = no narrowing for any persona (defaults unchanged). cmd/fleet
	// builds it once from the bundle and hands it to BOTH drivers.
	PersonaPolicies map[string]agentcore.PersonaToolPermissions

	// RemoteMCP resolves a user's OAuth-connected remote (hosted) MCP servers and
	// mints their bearer tokens for the per-turn overlay (#443). nil = the feature
	// is off unless OpenRemoteMCPOverlay is set; retained as the in-process
	// compatibility path.
	RemoteMCP RemoteMCPResolver
	// OpenRemoteMCPOverlay creates a per-user remote-server overlay without
	// exposing its credentialed client. When set it takes precedence over
	// RemoteMCP, allowing production to bind the overlay in a broker subprocess.
	OpenRemoteMCPOverlay RemoteMCPOverlayOpener

	// LLMProviders is the resolved multi-provider routing table (#289), translated
	// by cmd/fleet from the bundle manifest's providers: block (API-key env vars
	// already resolved host-side). Empty = the historical single-OpenRouter path
	// (NewModelResolver with cfg.OpenRouterAPIKey), so existing deployments are
	// unchanged.
	LLMProviders []agentcore.ProviderConfig

	// MCPBroker/MCPCatalog replace the credentialed in-process MCP client with a
	// call seam and public discovery data. OpenMCPScope, when set, creates an
	// isolated broker-owned client per interactive turn. The scope input carries
	// public server/account names and a workspace path, never credential values.
	MCPBroker    agentcore.MCPBroker
	MCPCatalog   []mcp.ServerTool
	OpenMCPScope MCPScopeOpener
	// ReloadMCP asks an injected credential owner to reload its own catalog and
	// return public metadata. Nil is accepted for transitional embedders, but a
	// broker-mode reload then fails explicitly instead of silently succeeding.
	ReloadMCP MCPReloader
	// MCPAccounts contains public credential-seat names keyed by server. It lets
	// the picker avoid scanning connector environment variables in broker mode.
	MCPAccounts map[string][]string
}

// MCPScope is one isolated per-turn MCP call/discovery lease. Close must release
// the broker-owned client; Manager invokes it with a fresh bounded context so a
// cancelled turn cannot suppress cleanup.
type MCPScope struct {
	Broker  agentcore.MCPBroker
	Catalog []mcp.ServerTool
	Close   func(context.Context) error
}

// MCPScopePolicy is the parent's effective, already-decided gate snapshot for
// one scope. It crosses the credential boundary at scope open so the credential
// owner can enforce the run's limits itself (#167 residual 1) instead of
// trusting that the distrusted address space did.
//
// It carries public configuration identifiers only, and it can only NARROW: the
// credential owner re-derives the bundle tool allowlist from its own copy of
// the bundle and intersects this with it, so a parent-side bug (or a scrubbed
// parent that lost its gates entirely, the #960 failure mode) restricts a scope
// rather than widening one.
type MCPScopePolicy struct {
	// ToolAllowlist is the run's effective Gate-2 map (bundle server name →
	// allowed tools). Same convention as in-process: an absent or empty entry
	// adds no narrowing.
	ToolAllowlist agentcore.MCPAllowlist
	// CredentialAllowlist is the run's effective Gate-3 pairs, or nil for
	// "inherit global" — the scheduled driver's per-task allowlist (#184). Only
	// the scheduled driver sets it; interactive turns leave it nil.
	CredentialAllowlist agentcore.CredentialAllowlist
}

// MCPScopeOpener binds the selected public server/account names to a per-turn
// broker scope rooted at workspace, under the run's effective policy.
// Credential resolution remains behind the opener's process boundary.
type MCPScopeOpener func(ctx context.Context, selection agentcore.MCPSelection, policy MCPScopePolicy, workspace string) (*MCPScope, error)

// MCPReloadResult is the public post-reload state returned by an injected
// credential owner. It contains configuration identifiers and tool schemas,
// never resolved connector definitions or credential values.
type MCPReloadResult struct {
	Summary  mcp.ReloadSummary
	Catalog  []mcp.ServerTool
	Accounts map[string][]string
	Specs    map[string]MCPServerSpec
}

// MCPReloader reloads credential-bearing MCP configuration behind the injected
// broker boundary and returns one coherent public snapshot.
type MCPReloader func(ctx context.Context) (*MCPReloadResult, error)

// New constructs a Manager: it dials OpenRouter (via the model resolver), uses
// an injected MCP broker/catalog or connects enabled MCP servers locally,
// registers the native tool set, and builds the per-turn sandbox warm pool.
// No language model is preloaded — each turn's model is resolved lazily from the
// slug the frontend sends.
// BuildMCPClient registers every enabled server in specs onto a fresh MCP client,
// credentialed host-side via each spec's Env (stdio) or Headers (HTTP), then
// registers any inline http_tools (issue #261) onto the same client, and returns
// it. A server that fails to connect is logged and skipped so the rest still
// register. It is shared by the interactive Manager and the out-of-process broker
// (fleet mcp-broker) so both connect the catalog identically — one credential path,
// not two (issue #167). httpTools may be nil/empty (the generic default), in which
// case no inline HTTP tools are registered and behavior is unchanged.
func BuildMCPClient(specs map[string]MCPServerSpec, httpTools []config.HTTPToolConfig) *mcp.Client {
	client := mcp.NewClient()
	for name, spec := range specs {
		if !spec.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var addErr error
		switch {
		case spec.URL != "":
			addErr = client.AddHTTPServerWithOptions(ctx, name, spec.URL, mcp.HTTPServerOptions{Headers: spec.Headers, TLS: spec.TLS})
		case spec.Command != "":
			// Substitute the reserved ${FLEET_WORKSPACE} manifest-env token with
			// the stable per-deployment MCP workspace dir: this is a SHARED
			// (process-lifetime) spawn, so every run sees the same directory
			// (managed-run detection + a cross-run ledger window). Resolved
			// lazily so token-free catalogs create nothing on disk.
			env := spec.Env
			if agentcore.EnvReferencesWorkspace(env) {
				env = agentcore.ExpandWorkspaceEnv(env, agentcore.SharedMCPWorkspaceDir())
			}
			// Shared spawn ⇒ no task identity: drop ${FLEET_TASK_ID}-bearing keys
			// rather than hand the connector a literal placeholder. Only the
			// scheduled per-run path resolves the token to a real ID.
			env = agentcore.ExpandTaskIDEnv(env, "")
			addErr = client.AddStdioServer(ctx, name, spec.Command, spec.Args, env, spec.Dir)
		default:
			addErr = fmt.Errorf("spec has neither Command nor URL")
		}
		cancel()
		if addErr != nil {
			log.Printf("warn: MCP %s failed to connect: %v", name, addErr)
			continue
		}
		log.Printf("MCP %s connected (%d tools available, optional=%v)", name, len(client.GetAllTools()), spec.Optional)
	}
	// Inline HTTP tools (issue #261): registered host-side on the SAME credentialed
	// client so they route through the same broker/governance seam as MCP tools. The
	// resolved auth headers live only in this process and are applied to the outbound
	// request at call time — never shipped to the sandbox or the model.
	RegisterHTTPTools(client, httpTools)
	return client
}

// RegisterHTTPTools translates the resolved config.HTTPToolConfig catalog into the
// mcp package's spec shape and registers it onto client. Exported so the scheduled
// per-task binder (which builds a fresh per-run client for a task with an explicit
// MCP selection) carries the SAME inline-HTTP-tool catalog as the interactive
// Manager and the broker — one registration path, host-side credentials only.
func RegisterHTTPTools(client *mcp.Client, httpTools []config.HTTPToolConfig) {
	if len(httpTools) == 0 {
		return
	}
	specs := make([]mcp.HTTPToolSpec, 0, len(httpTools))
	for _, t := range httpTools {
		specs = append(specs, mcp.HTTPToolSpec{
			Name:         t.Name,
			Description:  t.Description,
			Method:       t.Method,
			URL:          t.URL,
			Headers:      t.Headers,
			BodyTemplate: t.BodyTemplate,
			InputSchema:  t.InputSchema,
			ResponseJQ:   t.ResponseJQ,
		})
	}
	client.AddHTTPTools(specs)
	log.Printf("inline HTTP tools registered: %d", len(specs))
}

func New(opts ManagerOptions) (*Manager, error) {
	cfg := opts.Config
	if cfg == nil {
		return nil, fmt.Errorf("config required")
	}
	if opts.OpenMCPScope != nil && opts.MCPBroker == nil {
		return nil, fmt.Errorf("open MCP scope requires MCP broker")
	}
	if opts.MCPBroker != nil && opts.MCPCatalog == nil {
		return nil, fmt.Errorf("MCP broker requires an explicit MCP catalog")
	}
	if opts.ReloadMCP != nil && opts.MCPBroker == nil {
		return nil, fmt.Errorf("MCP reload seam requires MCP broker")
	}

	// Multi-provider LLM routing (#289): when the bundle declares a providers:
	// block, resolve models across the configured providers; otherwise fall back
	// to the historical single-OpenRouter resolver (byte-identical behavior).
	var (
		resolver *agentcore.ModelResolver
		err      error
	)
	if len(opts.LLMProviders) > 0 {
		resolver, err = agentcore.NewModelResolverWithProviders(opts.LLMProviders, agentcore.DefaultProviderHeaders)
	} else {
		resolver, err = agentcore.NewModelResolver(cfg.OpenRouterAPIKey, agentcore.DefaultProviderHeaders)
	}
	if err != nil {
		return nil, err
	}

	// An injected broker owns credentialed execution and supplies only public
	// discovery/account data. The compatibility path builds the local client with
	// the SAME builder used by fleet mcp-broker, so registration does not diverge.
	// Inline http_tools (#261) join that local client only on the compatibility path.
	client := (*mcp.Client)(nil)
	broker := opts.MCPBroker
	catalog := cloneMCPCatalog(opts.MCPCatalog)
	accounts := cloneMCPAccounts(opts.MCPAccounts)
	if broker == nil {
		client = BuildMCPClient(opts.ServerSpecs, cfg.HTTPTools)
		broker = agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints)
		catalog = client.GetAllTools()
		for name, spec := range opts.ServerSpecs {
			accounts[name] = creds.AccountsFor(spec.AccountVars)
		}
	}
	// Gating metadata (per-server allowlist + Optional flag) is pure spec data,
	// independent of the live connection.
	allow := mcpAllowlist{}
	optional := mcpOptionalSet{}
	enabledServers := make(map[string]bool)
	for name, spec := range opts.ServerSpecs {
		if !spec.Enabled {
			continue
		}
		if len(spec.ToolAllowlist) > 0 {
			allow[name] = spec.ToolAllowlist
		}
		if spec.Optional {
			optional[name] = true
		}
		enabledServers[name] = true
	}

	pool, err := buildSandboxPool(cfg, opts.PersonasDir, opts.ProtocolsDir, opts.SystemPromptsDir, opts.SkillsDir)
	if err != nil {
		return nil, err
	}

	chatPromptFile := strings.TrimSpace(opts.ChatSystemPromptFile)
	if chatPromptFile == "" {
		chatPromptFile = "chat.md"
	}

	m := &Manager{
		config:               cfg,
		mcpClient:            client,
		mcpBroker:            broker,
		mcpCatalog:           catalog,
		openMCPScope:         opts.OpenMCPScope,
		reloadMCP:            opts.ReloadMCP,
		mcpAccounts:          accounts,
		allowlist:            allow,
		resolver:             resolver,
		native:               tools.DefaultTools(),
		sandboxPool:          pool,
		notesProvider:        opts.NotesProvider,
		noteProposer:         opts.NoteProposer,
		optionalServers:      optional,
		enabledMCPServers:    enabledServers,
		personasDir:          opts.PersonasDir,
		protocolsDir:         opts.ProtocolsDir,
		skillsDir:            opts.SkillsDir,
		systemPromptsDir:     opts.SystemPromptsDir,
		chatSystemPromptFile: chatPromptFile,
		limiter:              opts.Limiter,
		health:               agentcore.NewProviderHealthRegistry(),
		personaPolicies:      opts.PersonaPolicies,
		remoteMCP:            opts.RemoteMCP,
		openRemoteMCPOverlay: opts.OpenRemoteMCPOverlay,
	}
	m.mcpToolRoster = m.computeMCPToolRoster(allow)
	m.optionalServerMetadata = m.buildOptionalServerMetadata(opts.ServerSpecs)
	return m, nil
}

// buildSandboxPool constructs the per-turn container warm pool from config,
// mirroring chat's New() wiring: container mode in production (an image is
// mandatory — bash/run_python only run inside per-turn containers), a no-op
// host-mode stub in mock mode.
// warmPoolSize derives how many sandboxes to keep pre-warmed from the configured
// concurrency cap, clamped to [2, 8]. Warm sandboxes are cheap to park — each is
// an idle `sleep infinity` container until a turn claims it — so scaling with the
// cap cuts cold-start latency on a busy box at negligible idle cost, while the
// ceiling bounds how many containers the pool spawns (in a background goroutine)
// at boot. This is NOT a concurrency limit: the pool's Take() cold-starts a fresh
// sandbox whenever the warm slots are empty, so real concurrency is bounded by
// host resources (and, for scheduled tasks, the worker-pool semaphore), not this.
func warmPoolSize(maxConcurrent int) int {
	const floor, ceiling = 2, 8
	switch {
	case maxConcurrent < floor:
		return floor
	case maxConcurrent > ceiling:
		return ceiling
	default:
		return maxConcurrent
	}
}

// resolveWarmSize picks the warm-pool depth: an explicit FLEET_SANDBOX_WARM_SIZE
// (>0) pins it; otherwise it is derived from MaxConcurrentAgents (clamped 2..8),
// preserving the prior default (#181).
func resolveWarmSize(cfg *config.Config) int {
	if cfg.SandboxWarmSize > 0 {
		return cfg.SandboxWarmSize
	}
	return warmPoolSize(cfg.MaxConcurrentAgents)
}

func buildSandboxPool(cfg *config.Config, personasDir, protocolsDir, systemPromptsDir, skillsDir string) (*sandbox.Pool, error) {
	poolCfg := sandbox.PoolConfig{
		Size:         resolveWarmSize(cfg),
		Mode:         sandbox.ModeContainer,
		BridgeScript: tools.PythonBridgeScript(),
		WarmTTL:      time.Duration(cfg.SandboxWarmTTLSeconds) * time.Second,
		// Python REPL knobs (#213). PythonCellTimeout is the per-cell ceiling;
		// the persistent-* knobs only bite when PersistentREPL is on.
		PythonCellTimeout:     time.Duration(cfg.PythonCellTimeoutSeconds) * time.Second,
		PersistentREPL:        cfg.PersistentPythonREPL(),
		PersistentIdleTTL:     time.Duration(cfg.PythonREPLIdleTTLSeconds) * time.Second,
		PersistentMaxSessions: cfg.PythonREPLMaxSessions,
	}
	if cfg.MockMode {
		// MockMode runs ModeHost (unsandboxed, os/exec). That executor is only
		// compiled in with -tags fleet_host_executor (#159); fail closed at boot in
		// a release binary so a stray FLEET_MOCK_MODE in production can never run
		// agent tool calls unsandboxed on the host.
		if !sandbox.HostExecutorCompiledIn() {
			return nil, fmt.Errorf(
				"FLEET_MOCK_MODE is set but the host executor is not compiled into this binary; " +
					"it is gated behind -tags fleet_host_executor (tests/dev only) and must not run in production")
		}
		poolCfg.Size = 0
		poolCfg.Mode = sandbox.ModeHost
		log.Printf("sandbox: mock mode — tool calls are stubbed by e2e harness")
		return sandbox.NewPool(poolCfg), nil
	}

	if cfg.SandboxImage == "" {
		return nil, fmt.Errorf(
			"FLEET_SANDBOX_IMAGE is required: bash and run_python only execute inside per-turn containers. " +
				"Run scripts/build-sandbox-image.sh and set FLEET_SANDBOX_IMAGE to enable container mode")
	}
	workspaceRoot := cfg.WorkspaceRoot
	if workspaceRoot == "" {
		abs, err := filepath.Abs("workspace")
		if err != nil {
			return nil, fmt.Errorf("resolve workspace root: %w", err)
		}
		workspaceRoot = abs
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil { //nolint:gosec // bind-mount source must be readable by the rootless container user
		return nil, fmt.Errorf("ensure workspace root %s: %w", workspaceRoot, err)
	}
	uploadsRoot := filepath.Join(cfg.EmailAttachmentDir, "uploads")
	if abs, err := filepath.Abs(uploadsRoot); err == nil {
		uploadsRoot = abs
	}
	if err := os.MkdirAll(uploadsRoot, 0o755); err != nil { //nolint:gosec // same — readable by the rootless container user via bind mount
		return nil, fmt.Errorf("ensure uploads root %s: %w", uploadsRoot, err)
	}

	// Normalize the OCI runtime name to what podman understands ("libkrun" →
	// "krun") so the --runtime flag, the preflight, and the probe binary mapping
	// all key off the same value. main.go normalizes cfg.SandboxRuntime up front;
	// re-normalizing here is idempotent and keeps a direct buildSandboxPool caller
	// (tests) on the same path.
	sandboxRuntime, _ := sandbox.NormalizeRuntime(cfg.SandboxRuntime)
	poolCfg.Container = sandbox.ContainerConfig{
		Image:            cfg.SandboxImage,
		WorkspaceHostDir: workspaceRoot,
		Runtime:          sandboxRuntime,
		MemoryLimit:      cfg.SandboxMemory, // empty → sandbox default (512m)
		CPULimit:         cfg.SandboxCPUs,   // empty → sandbox default (1.0)
		PidsLimit:        cfg.SandboxPids,   // 0 → sandbox default (128)
		DiskLimitGB:      cfg.SandboxDiskGB, // 0 → sandbox default (5); negative disables
		BridgeDir:        filepath.Join(filepath.Dir(workspaceRoot), "data", "sandbox-bridge"),
		ReadOnlyMounts:   absSupportingDocs(personasDir, protocolsDir, systemPromptsDir, skillsDir, uploadsRoot),
	}
	// Reclaim bridge-script/seccomp temp files orphaned by a PRIOR crash: only
	// the graceful close path removes them, so without this sweep every
	// non-graceful exit leaks them into BridgeDir permanently. Age-bounded and
	// best-effort — log and continue, like the container orphan prune.
	if n, err := sandbox.PruneOrphanedBridgeFiles(poolCfg.Container.BridgeDir); err != nil {
		log.Printf("sandbox: prune orphaned bridge files: %v", err)
	} else if n > 0 {
		log.Printf("sandbox: pruned %d orphaned bridge temp file(s) from a prior run", n)
	}
	// Fail closed BEFORE the warm pool spawns its first container: a kata/krun
	// runtime whose KVM or runtime binary is missing must abort boot, never
	// silently degrade to a shared-kernel container (the no-degrade invariant,
	// ADR-0010). A named shared-kernel runtime (runc/crun/runsc) is checked too,
	// but only for podman resolvability; the empty default preflights as a no-op.
	if err := sandbox.PreflightRuntime(context.Background(), poolCfg.Container.PodmanBinary, sandboxRuntime); err != nil {
		return nil, fmt.Errorf("sandbox runtime preflight failed (fail-closed): %w", err)
	}
	log.Printf("sandbox: container mode, image=%s, pool=%d, workspace=%s, runtime=%s",
		poolCfg.Container.Image, poolCfg.Size, poolCfg.Container.WorkspaceHostDir, defaultIfEmpty(poolCfg.Container.Runtime, "podman default"))
	if poolCfg.PersistentREPL {
		log.Printf("sandbox: run_python REPL mode=persistent — one kernel per conversation survives across turns (idle TTL %s, max %d sessions)",
			poolCfg.PersistentIdleTTL, cfg.PythonREPLMaxSessions)
		warnPersistentMemoryBudget(poolCfg.PersistentMaxSessions, poolCfg.Container.MemoryLimit)
	} else {
		log.Printf("sandbox: run_python REPL mode=per-turn — kernel is fresh each turn (the default)")
	}
	if poolCfg.PythonCellTimeout > 0 {
		log.Printf("sandbox: run_python per-cell timeout ceiling=%s (FLEET_PYTHON_CELL_TIMEOUT)", poolCfg.PythonCellTimeout)
	}

	// Sandbox egress mode (#211). The mode + allowlist are carried on the pool so
	// the scheduled run path can pick a take method; for allowlisted mode we also
	// stand up the host-side egress proxy here and fail CLOSED at boot if it can't
	// bind (never silently downgrade to open egress). Best-effort control over
	// proxy-honoring clients, NOT a hard jail — lockdown remains the hard seal.
	// See docs/adr/0012-sandbox-egress-allowlist.md.
	poolCfg.DefaultNetworkMode = cfg.DefaultNetworkMode
	poolCfg.DefaultEgressAllowlist = cfg.SandboxNetworkAllowlist
	switch cfg.DefaultNetworkMode {
	case sandbox.NetworkModeAllowlisted:
		// Allowlisted is the one posture that needs a specific rootless network
		// helper (slirp4netns, for the host-loopback route to the proxy). Probe
		// it BEFORE the warm pool spawns anything: on a host without that helper
		// every container start fails, and without this check boot would succeed,
		// log "egress filtered to […]", and then error on every single tool call.
		// Fails closed — never downgraded to open egress.
		if err := sandbox.PreflightAllowlistedNetwork(context.Background(), poolCfg.Container.PodmanBinary); err != nil {
			return nil, fmt.Errorf("sandbox egress preflight failed (fail-closed): %w", err)
		}
		proxy := sandbox.NewEgressProxy()
		if err := proxy.Start(); err != nil {
			return nil, fmt.Errorf("start sandbox egress proxy (#211): %w", err)
		}
		poolCfg.EgressProxy = proxy
		if len(cfg.SandboxNetworkAllowlist) == 0 {
			log.Printf("sandbox: WARNING network mode=allowlisted but the allowlist is EMPTY — networked scheduled-task, interactive chat, AND approved-bash sandboxes can reach NO domains (set sandbox.network_allowlist in the bundle manifest)")
		} else {
			log.Printf("sandbox: network mode=allowlisted — networked scheduled-task, interactive chat, AND approved-bash egress filtered to %v via the host proxy (best-effort; ADR-0012).", cfg.SandboxNetworkAllowlist)
		}
	case sandbox.NetworkModeLockdown:
		log.Printf("sandbox: network mode=lockdown — scheduled-task, interactive chat, AND approved-bash egress sealed regardless of per-task AllowNetwork.")
	}
	return sandbox.NewPool(poolCfg), nil
}

// absSupportingDocs absolutizes the persona/protocol/skill/system-prompt dirs
// (plus the uploads root) and drops empties so they can be passed as
// ContainerConfig.ReadOnlyMounts. The container backend bind-mounts each at the
// SAME absolute path inside the container.
func absSupportingDocs(dirs ...string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			log.Printf("warn: cannot absolutize supporting-doc dir %q: %v (skipping bind mount)", d, err)
			continue
		}
		out = append(out, abs)
	}
	return out
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Resolve loads + caches the model for a slug. Exposed so the scheduled
// runner (cmd/fleet) resolves its task model through the SAME cached resolver
// the interactive turns use.
func (m *Manager) Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error) {
	return m.modelResolver().Resolve(ctx, slug)
}

func (m *Manager) ResolveWithFallback(ctx context.Context, slug string) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
	return m.modelResolver().ResolveWithFallback(ctx, slug)
}

func (m *Manager) ResolveWithFallbacks(ctx context.Context, slug string) (fantasy.LanguageModel, []fantasy.LanguageModel, error) {
	return m.modelResolver().ResolveWithFallbacks(ctx, slug)
}

// modelResolver returns the current resolver snapshot. The resolver is
// hot-swappable (SetLLMProviders), so runtime reads take the RLock; the
// returned pointer is safe to use lock-free (a swap installs a fresh resolver,
// never mutates a published one) — the same discipline as the MCP gating maps.
func (m *Manager) modelResolver() *agentcore.ModelResolver {
	m.resolverMu.RLock()
	defer m.resolverMu.RUnlock()
	return m.resolver
}

// SetLLMProviders rebuilds the model resolver over the given routing table and
// swaps it in atomically — the runtime half of admin-managed LLM providers.
// An empty table falls back to the single catch-all OpenRouter resolver
// (exactly the boot default), and a table that fails eager construction
// (bad type, missing key) leaves the CURRENT resolver serving — the swap is
// all-or-nothing, so a bad admin edit can never take chat down.
func (m *Manager) SetLLMProviders(providers []agentcore.ProviderConfig) error {
	var (
		resolver *agentcore.ModelResolver
		err      error
	)
	if len(providers) > 0 {
		resolver, err = agentcore.NewModelResolverWithProviders(providers, agentcore.DefaultProviderHeaders)
	} else {
		resolver, err = agentcore.NewModelResolver(m.config.OpenRouterAPIKey, agentcore.DefaultProviderHeaders)
	}
	if err != nil {
		return err
	}
	m.resolverMu.Lock()
	m.resolver = resolver
	m.resolverMu.Unlock()
	return nil
}

// MCPClient exposes the shared MCP client to the scheduled-run compatibility
// path. Interactive and out-of-band approval calls use MCPBroker instead so
// production can move connector execution across the broker process boundary.
func (m *Manager) MCPClient() *mcp.Client { return m.mcpClient }

// MCPBroker returns the interactive manager's MCP call seam. The local-client
// default is intentionally adapted here rather than in the HTTP layer, so the
// transport can remain agnostic to where credentialed execution runs.
func (m *Manager) MCPBroker() agentcore.MCPBroker {
	if m.mcpBroker == nil && m.mcpClient != nil {
		return agentcore.NewLocalMCPBroker(m.mcpClient, agentcore.DefaultRemediationHints)
	}
	return m.mcpBroker
}

// MCPCatalog returns the public server-qualified tool catalog used to resolve
// staged approval calls without reaching into a concrete MCP client.
func (m *Manager) MCPCatalog() []mcp.ServerTool {
	return m.mcpCatalogSnapshot()
}

// OpenApprovalMCPScope reopens the credential seat a staged approval recorded,
// so an approved call executes on the {server, account} its turn was running on
// instead of the broker's default bundle seat (#167 residual 2). An approval
// card can outlive its turn scope, so this is a NEW short-lived scope carrying
// the same public selection — never a rehydration of the closed one.
//
// It returns (nil, nil) when this Manager has no scope opener (the transitional
// local-client mode), which tells the caller to fall back to MCPBroker(). A
// seat the credential owner no longer provisions fails the open, and the
// approval fails closed rather than silently running as a different account.
func (m *Manager) OpenApprovalMCPScope(ctx context.Context, selection agentcore.MCPSelection, workspace string) (*MCPScope, error) {
	if m.openMCPScope == nil {
		return nil, nil
	}
	// The card's own turn is gone, so the live gates are the right snapshot:
	// a tool the operator has since removed from a server's allowlist must not
	// execute just because it was staged while it was still permitted.
	allow, _ := m.mcpGates()
	scope, err := m.openMCPScope(ctx, selection, MCPScopePolicy{ToolAllowlist: agentcore.MCPAllowlist(allow)}, workspace)
	if err != nil {
		return nil, err
	}
	if scope == nil || scope.Broker == nil || scope.Close == nil {
		if scope != nil && scope.Close != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), mcpScopeCloseTimeout)
			defer cancel()
			_ = scope.Close(closeCtx)
		}
		return nil, errors.New("open MCP approval scope: opener returned an incomplete scope")
	}
	return scope, nil
}

// SandboxPool exposes the per-turn sandbox warm pool for the out-of-band
// approved-bash execution path (runStagedBash).
func (m *Manager) SandboxPool() *sandbox.Pool { return m.sandboxPool }

// Close releases MCP subprocesses and reaps any pooled sandboxes.
func (m *Manager) Close() error {
	if m.sandboxPool != nil {
		m.sandboxPool.Close()
	}
	if m.mcpClient != nil {
		return m.mcpClient.Close()
	}
	return nil
}

// computeMCPToolRoster walks the live MCP registry once and returns a sorted
// list of the tool names that survive the per-server allowlist filter. The
// allowlist is passed in (rather than read from m.allowlist) so this stays
// pure over the gating snapshot — the boot path and a hot reload (#218) each
// call it with their own allowlist without a lock.
func (m *Manager) computeMCPToolRoster(allow mcpAllowlist) []string {
	return computeMCPToolRosterFromCatalog(m.mcpCatalogSnapshot(), allow)
}

func computeMCPToolRosterFromCatalog(all []mcp.ServerTool, allow mcpAllowlist) []string {
	names := make([]string, 0, len(all))
	for _, st := range all {
		if list, ok := allow[st.ServerName]; ok && len(list) > 0 {
			allowed := false
			for _, n := range list {
				if n == st.Tool.Name {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}
		names = append(names, fmt.Sprintf("mcp_%s_%s", st.ServerName, st.Tool.Name))
	}
	sort.Strings(names)
	return names
}

// takeTurnSandbox pulls the sandbox for one interactive turn.
//
//   - Lockdown chats ALWAYS get a fresh no-network locked-down container (never
//     persistent): isolation is the whole point of lockdown.
//   - The fleet-wide egress posture (#211, FLEET_DEFAULT_NETWORK_MODE) then
//     applies to ordinary chat turns exactly as it does to scheduled tasks
//     (see takeTaskSandbox): `lockdown` seals every turn, `allowlisted` routes
//     the turn's egress through the host proxy scoped to the bundle allowlist.
//     Before this, chat turns silently got OPEN egress even when the operator
//     set a containing mode — a real gap ADR-0012 called out as deferred.
//   - When persistent REPL mode is on (#213) and this is an open-egress,
//     non-lockdown chat with a conversation ID, the turn borrows the
//     conversation's long-lived sandbox so the python kernel survives across
//     turns. A containing network mode takes PRECEDENCE over persistence: an
//     allowlisted turn needs a fresh per-turn proxy token (so it must
//     cold-start) and a sealed turn must have no network at all, neither of
//     which a shared long-lived container can provide. Containment is a
//     security boundary; persistence is a convenience — the boundary wins.
//   - Otherwise (the default) it's a fresh warm-pool container, closed at turn
//     end via the returned cleanup.
//
// Scheduled runs never reach here — they drive agentcore.Run through the
// scheduled runner, which owns its own per-run sandbox + worktree.
func (m *Manager) takeTurnSandbox(ctx context.Context, lockdown bool, convID string) (*sandbox.Sandbox, func(), error) {
	persistent := m.config.PersistentPythonREPL() && convID != ""
	return takeTurnSandboxFrom(ctx, m.sandboxPool, turnSandboxPosture{
		lockdown:          lockdown,
		lockdownAvailable: m.config.LockdownAvailable(),
		persistent:        persistent,
		convID:            convID,
	})
}

// turnSandboxTaker is the slice of *sandbox.Pool that takeTurnSandboxFrom uses.
// An interface so the egress routing is unit-testable with a fake (mirroring
// scheduledrun.sandboxTaker), rather than requiring a live podman pool.
type turnSandboxTaker interface {
	EgressDefault() (mode string, allowlist []string)
	TakeContainer(ctx context.Context) (*sandbox.Sandbox, func(), error)
	TakeContainerWithEgress(ctx context.Context, ov sandbox.ResourceOverride, allowlist []string) (*sandbox.Sandbox, func(), error)
	TakePersistent(ctx context.Context, convID string) (*sandbox.Sandbox, func(), error)
	Take(ctx context.Context) (*sandbox.Sandbox, func(), error)
}

// turnSandboxPosture is the per-turn decision inputs for the interactive
// sandbox take.
type turnSandboxPosture struct {
	lockdown          bool   // this conversation is in lockdown mode
	lockdownAvailable bool   // a sandbox image is configured (lockdown can run)
	persistent        bool   // persistent REPL is on AND we have a conversation
	convID            string // conversation id for the persistent borrow
}

// takeTurnSandboxFrom applies the interactive sandbox posture over taker (see
// takeTurnSandbox for the full contract). Extracted as a free function over an
// interface so the #211 egress routing is testable without a real pool.
func takeTurnSandboxFrom(ctx context.Context, taker turnSandboxTaker, p turnSandboxPosture) (*sandbox.Sandbox, func(), error) {
	mode, allowlist := taker.EgressDefault()

	// A per-conversation lockdown OR the fleet-wide lockdown mode seals the turn.
	if p.lockdown || mode == sandbox.NetworkModeLockdown {
		if p.lockdown && !p.lockdownAvailable {
			return nil, nil, fmt.Errorf("conversation is in lockdown mode but the server has no sandbox image configured")
		}
		sb, cleanup, err := taker.TakeContainer(ctx)
		if errors.Is(err, sandbox.ErrContainerUnavailable) {
			// Host/mock pool (no container backend): nothing to seal — fall back
			// to the host take, matching takeTaskSandbox's degrade.
			return taker.Take(ctx)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("take lockdown sandbox: %w", err)
		}
		return sb, cleanup, nil
	}

	// Fleet-wide allowlisted mode: filter the turn's egress through the host
	// proxy (fresh per-turn token). Precedes persistence (a per-turn token
	// can't be shared by a long-lived container). Fails closed in the pool.
	if mode == sandbox.NetworkModeAllowlisted {
		sb, cleanup, err := taker.TakeContainerWithEgress(ctx, sandbox.ResourceOverride{}, allowlist)
		if errors.Is(err, sandbox.ErrContainerUnavailable) {
			return taker.Take(ctx)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("take allowlisted sandbox: %w", err)
		}
		return sb, cleanup, nil
	}

	if p.persistent {
		// TakePersistent reuses the conversation's sandbox (or creates one,
		// pulling a warm container for the first turn). It degrades to a per-turn
		// Take internally if persistence is disabled in the pool, so this is
		// always safe to call.
		sb, cleanup, err := taker.TakePersistent(ctx, p.convID)
		if err != nil {
			return nil, nil, fmt.Errorf("take persistent sandbox: %w", err)
		}
		return sb, cleanup, nil
	}
	sb, cleanup, err := taker.Take(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("take sandbox: %w", err)
	}
	return sb, cleanup, nil
}

// ReleaseChatSession tears down the persistent per-conversation sandbox (#213)
// for convID, if any. Called when a conversation is deleted so its kernel +
// container are reclaimed promptly rather than waiting for the idle reaper. A
// no-op when persistent mode is off or the conversation has no live sandbox.
func (m *Manager) ReleaseChatSession(convID string) {
	if m.sandboxPool == nil {
		return
	}
	m.sandboxPool.ReleaseChatSession(convID)
}

// ── RunTurn ──

func (m *Manager) configureTurnWorkspace(ctx context.Context, sb *sandbox.Sandbox, conversationID string) (context.Context, string, func(), error) {
	fileOpRoot := m.config.WorkspaceRoot
	var err error
	if conversationID != "" {
		fileOpRoot, err = tools.EnsureWorkspaceDir(conversationID)
		if err != nil {
			return ctx, "", func() {}, fmt.Errorf("prepare conversation workspace: %w", err)
		}
	}
	if fileOpRoot == "" {
		fileOpRoot = tools.WorkspaceDirForConversation(conversationID)
	}
	if err := sb.BindFileOpRoot(ctx, fileOpRoot); err != nil {
		return ctx, "", func() {}, fmt.Errorf("bind conversation file capability: %w", err)
	}
	// Retained model-output artifacts are an optional recovery aid, never a
	// reason to weaken the hard output cap or fall back to host I/O. Install the
	// writer only after this turn owns its live sandbox and private conversation
	// workspace; agentcore then uses it after governance for native and MCP tools.
	if conversationID == "" {
		return ctx, fileOpRoot, func() {}, nil
	}
	artifactCtx, releaseArtifacts, artifactErr := tools.WithSandboxModelOutputArtifacts(ctx, sb, fileOpRoot)
	if artifactErr != nil {
		log.Printf("conversation %s: governed tool-output artifact recovery unavailable: %v", conversationID, artifactErr)
		return ctx, fileOpRoot, func() {}, nil
	}
	return artifactCtx, fileOpRoot, releaseArtifacts, nil
}

func (m *Manager) openTurnMCPScope(ctx context.Context, selection agentcore.MCPSelection, policy MCPScopePolicy, workspace string) (agentcore.MCPBroker, []mcp.ServerTool, func(), error) {
	if m.openMCPScope == nil {
		return m.mcpBroker, m.mcpCatalogSnapshot(), func() {}, nil
	}
	scope, err := m.openMCPScope(ctx, selection, policy, workspace)
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("open MCP turn scope: %w", err)
	}
	if scope == nil || scope.Close == nil {
		return nil, nil, func() {}, errors.New("open MCP turn scope: opener returned an incomplete scope")
	}
	cleanup := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), mcpScopeCloseTimeout)
		defer cancel()
		if closeErr := scope.Close(closeCtx); closeErr != nil {
			log.Printf("RunTurn: close MCP turn scope: %v", closeErr)
		}
	}
	if scope.Broker == nil {
		cleanup()
		return nil, nil, func() {}, errors.New("open MCP turn scope: opener returned an incomplete scope")
	}
	return scope.Broker, cloneMCPCatalog(scope.Catalog), cleanup, nil
}

func (m *Manager) openRemoteOverlay(ctx context.Context, email string, baseCatalog []mcp.ServerTool, enabledNames []string) (*RemoteMCPOverlay, error) {
	shadowed := make(map[string]bool, len(baseCatalog))
	for _, st := range baseCatalog {
		shadowed[st.ServerName] = true
	}
	// A remote server participates in an interactive turn only when the
	// conversation opted in. New conversations seed this list from discovery,
	// so newly connected servers remain on by default but toggleable.
	enabled := make(map[string]bool, len(enabledNames))
	for _, name := range enabledNames {
		if n := strings.TrimSpace(name); n != "" {
			enabled[n] = true
		}
	}
	if m.openRemoteMCPOverlay != nil {
		overlay, err := m.openRemoteMCPOverlay(ctx, email, shadowed, enabled)
		if err != nil {
			return nil, err
		}
		if err := overlay.Validate(); err != nil {
			overlay.Close()
			return nil, err
		}
		return overlay, nil
	}
	return BuildRemoteMCPOverlay(ctx, m.remoteMCP, email, shadowed, enabled)
}

// turnSink adapts the httpapi EventSink to an agentcore.Observer, forwarding the
// run's streamed events as SSE frames. Run-loop events arrive as
// (eventType, payload) and pass straight through to Emit — the agentcore stream
// bridge already names them with the SSE event names the frontend reads
// (text.delta / reasoning.* / tool.call / tool.result).
type turnSink struct {
	sink EventSink
}

func (o turnSink) Observe(eventType string, payload map[string]any) {
	if o.sink == nil {
		return
	}
	// Drop the run loop's internal "enforcement" event — interactive runs never
	// emit one (CanFinish is always true at round 0) but guard anyway so an
	// internal marker never leaks to the browser as an unknown SSE event.
	if eventType == "enforcement" {
		return
	}
	o.sink.Emit(eventType, payload)
}

// RunTurn executes one interactive turn: it builds the per-turn system prompt +
// sandbox + tools, resolves the model, drives RunInteractiveTurn (which streams
// through the sink), then maps the accumulated transcript to history + usage.
// Mirrors chat's session.go::RunTurn over the unified loop.
func (m *Manager) RunTurn(ctx context.Context, in TurnInput, sink EventSink) (*TurnResult, error) {
	startedAt := time.Now()
	persona := defaultIfEmpty(strings.TrimSpace(in.Persona), m.config.PersonaDefault)

	if in.ConversationID != "" {
		ctx = tools.WithConversationID(ctx, in.ConversationID)
	}

	if in.Lockdown && in.Model != "" && !m.config.LockdownAllows(in.Model) {
		return nil, fmt.Errorf("model %q not allowed in lockdown mode", in.Model)
	}

	// Admission control: an interactive turn holds one slot in the shared box-wide
	// concurrency limiter for its whole duration, so chat counts against the same
	// cap as scheduled tasks (and draws on the reserve that keeps chat ahead of
	// background work). Wait only briefly — a human is watching — then surface
	// ErrAtCapacity so the UI shows a clean "at capacity, retry" instead of a hung
	// turn or an over-subscribed box. The slot is released when the turn returns.
	if m.limiter != nil {
		admitCtx, cancel := context.WithTimeout(ctx, interactiveAdmitWait)
		release, admitted := m.limiter.AcquireInteractive(admitCtx.Done())
		cancel()
		if !admitted {
			if ctx.Err() != nil {
				return nil, ctx.Err() // the caller (user) abandoned the turn while waiting
			}
			return nil, ErrAtCapacity
		}
		defer release()
	}

	// Admin-curated knowledge base (best-effort: a notes failure runs the turn
	// without the section rather than failing it).
	var notes []agentcore.Note
	if m.notesProvider != nil {
		if got, err := m.notesProvider.PublishedNotes(ctx); err != nil {
			log.Printf("agent notes unavailable; running without notes section: %v", err)
		} else {
			notes = got
		}
	}

	systemPrompt, err := m.buildSystemPrompt(persona, in.ConversationID, in.Memories, in.ProjectInstructions, notes, in.OptionalMCPServersEnabled, in.UserSkills)
	if err != nil {
		return nil, fmt.Errorf("compose system prompt: %w", err)
	}

	sb, sbCleanup, err := m.takeTurnSandbox(ctx, in.Lockdown, in.ConversationID)
	if err != nil {
		return nil, err
	}
	defer sbCleanup()
	ctx, workspace, releaseArtifacts, err := m.configureTurnWorkspace(ctx, sb, in.ConversationID)
	if err != nil {
		return nil, err
	}
	defer releaseArtifacts()

	// Snapshot the MCP gating ONCE under one RLock (#218 can swap it mid-turn)
	// and use that single snapshot for both halves of enforcement: the scope
	// policy the credential owner enforces, and the tool registration below.
	// Two reads could disagree, which would show up as a tool the loop
	// advertises and the broker then refuses.
	turnAllowlist, turnOptional := m.mcpGates()
	turnSelection := m.scopeSelection(in.OptionalMCPServersEnabled, in.MCPAccountDefaults)
	turnBroker, turnCatalog, releaseMCPScope, err := m.openTurnMCPScope(ctx, turnSelection,
		MCPScopePolicy{ToolAllowlist: agentcore.MCPAllowlist(turnAllowlist)}, workspace)
	if err != nil {
		return nil, err
	}
	defer releaseMCPScope()
	// Hand the approval stager the seat this turn actually runs on, before any
	// tool can stage a card (#167 residual 2). Without it, staging resolves and
	// pre-validates against the process-wide default-seat catalog, and the
	// approval it persists carries no seat for execution to reopen.
	if binder, ok := in.ApprovalStager.(MCPScopeBinder); ok && binder != nil {
		binder.BindTurnMCPScope(TurnMCPScope{
			Broker:    turnBroker,
			Catalog:   turnCatalog,
			Selection: turnSelection,
		})
	}
	turnTools := tools.NewTurnTools(sb)
	turnTools.Tools = filterNativeToolsByOptIn(turnTools.Tools, in.OptionalMCPServersEnabled)

	model, providerFallbacks, err := m.modelResolver().ResolveWithFallbacks(ctx, in.Model)
	if err != nil {
		return nil, fmt.Errorf("resolve model: %w", err)
	}
	modelSlug := model.Model()

	history, err := replayHistory(in.History)
	if err != nil {
		return nil, fmt.Errorf("replay history: %w", err)
	}
	imageParts, imageRefs := loadImageAttachments(in.ImageAttachments)
	messages := make([]fantasy.Message, 0, len(history)+1)
	messages = append(messages, history...)
	messages = append(messages, fantasy.NewUserMessage(in.UserMessage, imageParts...))

	// The new user message + its image refs are persisted as the first entry of
	// the turn; the run loop's accumulated entries follow.
	userEntry := mustEntry("user", "text", TextContent{Text: in.UserMessage, Images: imageRefs})

	// Durable-before-execution (#798): the user message commits to canonical
	// history before the first provider or tool call. A store failure fails
	// the turn HERE — with no side effects yet — instead of surfacing later
	// as a completed answer with no question above it.
	if in.CommitUser != nil {
		if err := in.CommitUser(ctx, userEntry); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrHistoryCommitFailed, err)
		}
	}

	sink.Emit("turn.started", map[string]any{"persona": persona})

	maxTokens := m.config.LLMMaxTokens
	if maxTokens <= 0 {
		maxTokens = 16384
	}

	// The conversation's opt-in list becomes the per-run MCP selection
	// (default account). agentcore.buildFantasyTools registers the opted-in
	// servers' tools through the InteractivePolicy gate.
	selection := make(agentcore.MCPSelection, 0, len(in.OptionalMCPServersEnabled))
	for _, name := range in.OptionalMCPServersEnabled {
		if n := strings.TrimSpace(name); n != "" {
			// The user's default seat (connections page) rides along; a chat
			// turn is supervised, so the per-user default is the right seat —
			// scheduled tasks pin their own {server, account} explicitly.
			selection = append(selection, agentcore.MCPChoice{Server: n, Account: in.MCPAccountDefaults[n]})
		}
	}

	// Per-user remote (hosted) MCP overlay (#443). Builds a short-lived client of
	// the user's OAuth-connected servers (fresh bearer, SSRF-safe transport) that
	// composes with the shared catalog. Best-effort: a server that needs re-auth
	// is skipped, never failing the turn. The overlay is closed when the turn ends.
	var overlay *RemoteMCPOverlay
	if in.UserEmail != "" && (m.openRemoteMCPOverlay != nil || m.remoteMCP != nil) {
		ov, oerr := m.openRemoteOverlay(ctx, in.UserEmail, turnCatalog, in.OptionalMCPServersEnabled)
		if oerr != nil {
			log.Printf("RunTurn: remote-mcp overlay unavailable for %s: %v", in.UserEmail, oerr)
		} else if ov != nil {
			overlay = ov
			defer overlay.Close()
			if len(ov.Skipped) > 0 {
				// Interactive: the user can see+fix these on the Connections page.
				log.Printf("RunTurn: remote MCP server(s) need re-auth for %s: %v", in.UserEmail, ov.Skipped)
			}
		}
	}

	tc := TurnConfig{
		SystemPrompt:    systemPrompt,
		Messages:        messages,
		Label:           in.ConversationID,
		ConversationID:  in.ConversationID,
		Model:           model,
		FallbackModels:  providerFallbacks,
		Temperature:     m.config.LiveTemperature(),
		MaxTokens:       maxTokens,
		MaxIterations:   m.config.LiveMaxIterations(),
		NativeTools:     turnTools.Tools,
		Sandbox:         sb,
		MCPClient:       m.mcpClient,
		MCPBroker:       turnBroker,
		MCPCatalog:      turnCatalog,
		Allowlist:       agentcore.MCPAllowlist(turnAllowlist),
		OptionalServers: agentcore.MCPOptionalSet(turnOptional),
		Selection:       selection,
		Persona:         persona,
		PersonaPolicy:   m.personaPolicy(persona),
		MaxCostUSD:      m.config.LiveMaxCostUSD(),
		MaxTotalTokens:  m.config.LiveMaxTotalTokens(),
		ApprovalStager:  in.ApprovalStager,
		MemoryProposer:  in.MemoryProposer,
		NoteProposer:    m.noteProposer,
		SkillProposer:   in.SkillProposer,
		HealthRegistry:  m.health,
		ThinkingConfig:  in.ThinkingConfig,
		TurnJournal:     in.TurnJournal,
		SteerSource:     in.SteerSource,
		// Governed sub-agents in interactive chat (#1043): the fleet-wide flag
		// (default true; Admin → Features / FLEET_SUBAGENTS_ENABLED is the kill
		// switch) is the only chat gate — there is no per-conversation column.
		// Mirrors the scheduledrun wiring; the child model resolves HOST-SIDE
		// through the same Manager resolver, so credentials stay host-side.
		Config: m.config,
		Subagent: SubagentOptions{
			Enabled:        m.config.LiveSubagentsEnabled(),
			MaxDepth:       m.config.SubagentsMaxDepth,
			MaxChildren:    m.config.SubagentsMaxChildren,
			BudgetFraction: m.config.SubagentsBudgetFraction,
			ModelSlug:      m.config.SubagentsModel,
			Resolver:       m,
		},
	}
	tc.Overlay = overlay

	res, runErr := RunInteractiveTurn(ctx, tc, turnSink{sink: sink})

	if runErr != nil {
		// Distinguish caller-cancelled (handled below via res.Cancelled when the
		// loop returns a partial result) from a genuine stream failure that the
		// user can fix by choosing another model.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return m.cancelledTurnResult(res, userEntry, modelSlug, startedAt, ctxErr, sink, in.CommitTerminal)
		}
		if errors.Is(runErr, agentcore.ErrGuardrailBlocked) {
			sink.Emit("turn.policy_blocked", map[string]any{"policy": "prompt-injection"})
			return nil, runErr
		}
		commitPartialSideEffects(runErr, res, in.CommitTerminal)
		reason, status, _ := agentcore.ClassifyStreamErrorReason(runErr)
		log.Printf("RunTurn stream failed (reason=%s model=%s status=%d): %v", reason, modelSlug, status, runErr)
		emitModelSelectionRequired(sink, reason, modelSlug, status, runErr)
		return nil, fmt.Errorf("%w: %w", ErrModelSelectionRequired, runErr)
	}

	if res.Cancelled {
		return m.cancelledTurnResult(res, userEntry, modelSlug, startedAt, ctx.Err(), sink, in.CommitTerminal)
	}

	newHistory := make([]HistoryEntry, 0, len(res.Entries)+2)
	newHistory = append(newHistory, userEntry)
	newHistory = append(newHistory, mapRunEntries(res.Entries)...)

	finalText := res.FinalText
	usage := res.Usage
	summary := TurnSummaryContent{
		CostUSD:              usage.CostUSD,
		PromptTokens:         usage.PromptTokens,
		PromptTokensLastStep: usage.LastStepInputTokens,
		CompletionTokens:     usage.CompletionTokens,
		CachedTokens:         usage.CachedTokens,
		CacheCreationTokens:  usage.CacheCreationTokens,
		DurationMs:           int(time.Since(startedAt).Milliseconds()),
		Model:                modelSlug,
	}
	newHistory = append(newHistory, mustEntry("assistant", "turn_summary", summary))

	// Terminal gate (#798): canonical history commits BEFORE turn.completed is
	// advertised. The user entry (newHistory[0]) was committed up-front and is
	// excluded. On failure the turn errors visibly — the SSE ledger and the
	// journal keep the evidence, and startup recovery does not re-project a
	// turn that ended alive (the driver records a terminal error state).
	if in.CommitTerminal != nil {
		if err := in.CommitTerminal(newHistory[1:], false); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrHistoryCommitFailed, err)
		}
	}

	sink.Emit("turn.completed", map[string]any{
		"cost_usd":                usage.CostUSD,
		"prompt_tokens":           usage.PromptTokens,
		"prompt_tokens_last_step": usage.LastStepInputTokens,
		"completion_tokens":       usage.CompletionTokens,
		"cached_tokens":           usage.CachedTokens,
		"cache_creation_tokens":   usage.CacheCreationTokens,
		"duration_ms":             summary.DurationMs,
		"model":                   modelSlug,
	})

	return &TurnResult{
		FinalText:           finalText,
		NewHistory:          newHistory,
		PromptTokens:        usage.PromptTokens,
		CompletionTokens:    usage.CompletionTokens,
		CachedTokens:        usage.CachedTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		CostUSD:             usage.CostUSD,
		Model:               modelSlug,
	}, nil
}

// cancelledTurnResult builds the partial TurnResult for a cancelled turn,
// persisting whatever transcript accumulated and emitting turn.cancelled.
// commitPartialSideEffects terminally commits a failed turn's executed tool
// records. A mid-stream failure AFTER a committed tool side effect (ADR-0035)
// carries the partial transcript back with the error; dropping it left the
// executed call visible only in the turn journal — never projected into
// canonical history (the turn seals non-`running`, so RecoverStrandedTurns
// skips it) — and the retried turn re-issued the side effect blind, the exact
// hazard the #798 journal closes. Best-effort: a commit failure is logged,
// the original stream error still surfaces.
func commitPartialSideEffects(runErr error, res agentcore.Result, commit func([]HistoryEntry, bool) error) {
	if !errors.Is(runErr, agentcore.ErrCommittedSideEffects) || len(res.Entries) == 0 || commit == nil {
		return
	}
	if cerr := commit(mapRunEntries(res.Entries), false); cerr != nil {
		log.Printf("RunTurn: committing partial side-effect transcript failed: %v", cerr)
	}
}

func (m *Manager) cancelledTurnResult(res agentcore.Result, userEntry HistoryEntry, modelSlug string, startedAt time.Time, ctxErr error, sink EventSink, commitTerminal func([]HistoryEntry, bool) error) (*TurnResult, error) {
	newHistory := make([]HistoryEntry, 0, len(res.Entries)+2)
	newHistory = append(newHistory, userEntry)
	newHistory = append(newHistory, mapRunEntries(res.Entries)...)
	usage := res.Usage
	summary := TurnSummaryContent{
		CostUSD:              usage.CostUSD,
		PromptTokens:         usage.PromptTokens,
		PromptTokensLastStep: usage.LastStepInputTokens,
		CompletionTokens:     usage.CompletionTokens,
		CachedTokens:         usage.CachedTokens,
		CacheCreationTokens:  usage.CacheCreationTokens,
		DurationMs:           int(time.Since(startedAt).Milliseconds()),
		Cancelled:            true,
		Model:                modelSlug,
	}
	newHistory = append(newHistory, mustEntry("assistant", "turn_summary", summary))
	// Same terminal gate as the completed path (#798): partial work commits
	// before turn.cancelled is advertised, so a Stop never loses transcript.
	if commitTerminal != nil {
		if err := commitTerminal(newHistory[1:], true); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrHistoryCommitFailed, err)
		}
	}
	reason := "cancelled"
	switch {
	case res.StoppedByBudget:
		// A per-turn cost/token ceiling fired — not a user Stop. Surface it
		// distinctly so the UI can say "budget reached" instead of "cancelled".
		reason = "cost_ceiling_reached"
	case ctxErr != nil:
		reason = ctxErr.Error()
	}
	sink.Emit("turn.cancelled", map[string]any{
		"reason":                  reason,
		"budget_reached":          res.StoppedByBudget,
		"cost_usd":                usage.CostUSD,
		"prompt_tokens":           usage.PromptTokens,
		"prompt_tokens_last_step": usage.LastStepInputTokens,
		"completion_tokens":       usage.CompletionTokens,
		"cached_tokens":           usage.CachedTokens,
		"cache_creation_tokens":   usage.CacheCreationTokens,
		"duration_ms":             summary.DurationMs,
		"model":                   modelSlug,
	})
	return &TurnResult{
		FinalText:           res.FinalText,
		NewHistory:          newHistory,
		PromptTokens:        usage.PromptTokens,
		CompletionTokens:    usage.CompletionTokens,
		CachedTokens:        usage.CachedTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		CostUSD:             usage.CostUSD,
		Model:               modelSlug,
		Cancelled:           true,
	}, nil
}

// mapRunEntries converts the agentcore run transcript into agent.HistoryEntry
// records for persistence + replay. Mirrors the entry shapes session.go's
// RunTurn produced (reasoning / tool_call / tool_result / assistant text).
func mapRunEntries(entries []agentcore.RunEntry) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		switch e.Type {
		case "reasoning":
			out = append(out, mustEntry("assistant", "reasoning", ReasoningContent{Text: e.Text}))
		case "text":
			out = append(out, mustEntry("assistant", "text", TextContent{Text: e.Text}))
		case "user_text":
			// A steered mid-turn user message (#785): persisted in stream order
			// like every other entry, exactly once (the sink dedupes by SteerID).
			out = append(out, mustEntry("user", "text", TextContent{Text: e.Text}))
		case "tool_call":
			out = append(out, mustEntry("assistant", entryTypeToolCall, ToolCallContent{
				ID: e.ToolCallID, Name: e.ToolName, Input: e.ToolInput,
			}))
		case "tool_result":
			out = append(out, mustEntry("tool", "tool_result", ToolResultContent{
				ID: e.ToolCallID, Name: e.ToolName, Text: e.Text, IsErr: e.IsErr,
			}))
		}
	}
	return out
}

// emitModelSelectionRequired tells the frontend to reopen its model picker
// because the current model can't complete the turn. Mirrors chat's helper of
// the same name; the HTTP layer detects ErrModelSelectionRequired and suppresses
// its own generic turn.error so only this structured event is sent.
func emitModelSelectionRequired(sink EventSink, reason agentcore.StreamErrorReason, failedModel string, status int, streamErr error) {
	if sink == nil {
		return
	}
	raw := ""
	if streamErr != nil {
		raw = streamErr.Error()
	}
	sink.Emit("turn.model_required", map[string]any{
		"reason":       string(reason),
		"failed_model": failedModel,
		"status_code":  status,
		"message":      humanMessageForReason(reason, status),
		"raw":          truncate(raw, 1000),
	})
}

func humanMessageForReason(reason agentcore.StreamErrorReason, status int) string {
	switch reason {
	case agentcore.ReasonContextTooLarge:
		return "This conversation exceeds the selected model's context window. Pick a model with a larger window or start a new chat."
	case agentcore.ReasonRetryExhausted:
		if status == 429 {
			return "The selected model is rate-limiting this request. Retrying did not help — pick a different model to continue."
		}
		return "The selected model's provider is failing repeatedly. Pick a different model to continue."
	default:
		if status > 0 {
			return fmt.Sprintf("The selected model returned an error (HTTP %d). Pick a different model to continue.", status)
		}
		return "The selected model could not complete this turn. Pick a different model to continue."
	}
}

// ErrModelSelectionRequired is the sentinel RunTurn returns when the chosen
// model failed in a way the user can fix by picking a different model. The HTTP
// layer detects it with errors.Is and does NOT emit a generic turn.error
// (turn.model_required was already emitted). Mirrors chat's sentinel.
var ErrModelSelectionRequired = fmt.Errorf("model selection required")

// ErrHistoryCommitFailed marks a turn that failed because its canonical
// history could not be durably committed (#798) — before the first provider
// call (user commit) or at terminal projection. The turn surfaces a visible
// error; it never advertises success for work that would vanish on reload.
var ErrHistoryCommitFailed = fmt.Errorf("conversation history could not be saved")

// interactiveAdmitWait bounds how long an interactive turn waits for a free
// concurrency slot before giving up. Short, because a human is watching: it
// smooths a momentary spike (a slot usually frees in well under this) but on a
// genuinely saturated box yields a fast, honest "at capacity" instead of a hung
// turn. A var (not const) so tests can shorten it.
var interactiveAdmitWait = 5 * time.Second

const mcpScopeCloseTimeout = 5 * time.Second

// ErrAtCapacity is the sentinel RunTurn returns when the box is at its concurrency
// cap and no interactive slot freed within interactiveAdmitWait. The HTTP layer
// surfaces its message as a turn.error so the user sees a clean "try again in a
// moment" rather than a hung spinner. The user-facing text is the error itself.
var ErrAtCapacity = fmt.Errorf("the workspace is at capacity right now — please resend your message in a moment")

// warnPersistentMemoryBudget flags a persistent-REPL configuration whose worst
// case oversubscribes host RAM.
//
// The ceiling is arithmetic, not a guess: PersistentMaxSessions conversations
// may each hold a container capped at MemoryLimit, so the pool's worst case is
// their product — and the DEFAULTS multiply out to 32 x 512 MiB = 16 GiB before
// the fleet process, Postgres, or the warm pool have taken a byte. On a smaller
// box that is an OOM waiting for a busy afternoon, and nothing said so: the cap
// was reported as a session count, which reads as harmless.
//
// Advisory only. It never clamps the operator's configuration — a box may
// legitimately be sized for it, and containers rarely reach their cap — it just
// makes the number visible at boot instead of at 3am.
func warnPersistentMemoryBudget(maxSessions int, memoryLimit string) {
	if maxSessions <= 0 || memoryLimit == "" {
		return
	}
	perSession, err := sandbox.ParseMemoryLimitBytes(memoryLimit)
	if err != nil || perSession <= 0 {
		return // unparseable limits are podman's problem to report, not ours
	}
	worstCase := uint64(maxSessions) * perSession
	total, err := hostMemoryBytes()
	if err != nil || total == 0 {
		return // no /proc/meminfo (non-Linux, restricted container): stay quiet
	}
	// Two thirds: the fleet process, Postgres, the warm sandbox pool and the OS
	// all need headroom, so "the persistent pool alone could claim most of RAM"
	// is already the warning-worthy case — waiting for it to exceed 100% would
	// warn only after the box was certain to OOM.
	if worstCase*3 <= total*2 {
		return
	}
	log.Printf("⚠ sandbox: persistent REPL worst case is %d sessions x %s = %.1f GiB, against %.1f GiB of host RAM — "+
		"lower FLEET_PYTHON_REPL_MAX or FLEET_SANDBOX_MEMORY, or expect OOM kills under load",
		maxSessions, memoryLimit, float64(worstCase)/(1<<30), float64(total)/(1<<30))
}

// hostMemoryBytes reads MemTotal from /proc/meminfo. Returns an error on any
// platform or sandbox where that is unavailable; callers treat that as "do not
// warn" rather than as a fault.
func hostMemoryBytes() (uint64, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			return 0, fmt.Errorf("malformed MemTotal line")
		}
		kb, perr := strconv.ParseUint(fields[0], 10, 64)
		if perr != nil {
			return 0, perr
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
