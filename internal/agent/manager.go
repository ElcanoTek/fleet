package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
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
	// A2ADepth is the run's inbound A2A delegation depth (#1368): the task
	// row's a2a_delegation_depth, 0 for anything a human started. The broker
	// child stamps it on the scope's a2a peer tools so a task that itself
	// arrived over A2A cannot re-delegate past FLEET_A2A_MAX_DELEGATION_DEPTH.
	// Public configuration state, not a credential; like every other field
	// here it can only NARROW what the child allows.
	A2ADepth int
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
func BuildMCPClient(specs map[string]MCPServerSpec, httpTools []config.HTTPToolConfig, a2aPeers []config.A2APeerConfig) *mcp.Client {
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
			// One shared dir serves both jobs here: the ${FLEET_WORKSPACE}
			// substitution AND the subprocess cwd. It is resolved
			// unconditionally now (not only for token-bearing catalogs),
			// because a server that never mentions the token still writes its
			// relative output paths somewhere — and "somewhere" must not be
			// the operator's bundle checkout.
			shared := agentcore.SharedMCPWorkspaceDir()
			env := spec.Env
			if agentcore.EnvReferencesWorkspace(env) {
				env = agentcore.ExpandWorkspaceEnv(env, shared)
			}
			// Shared spawn ⇒ no task identity: drop ${FLEET_TASK_ID}-bearing keys
			// rather than hand the connector a literal placeholder. Only the
			// scheduled per-run path resolves the token to a real ID.
			env = agentcore.ExpandTaskIDEnv(env, "")
			addErr = client.AddStdioServer(ctx, name, spec.Command, spec.Args, env,
				agentcore.StdioCwd(spec.Dir, spec.DirPinned, shared))
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
	// Outbound A2A peers (#1368): the same synthetic-server seam. This is a
	// shared, process-lifetime client, so the calling run's depth is unknown
	// here and defaults to 0 (human-initiated); scheduled runs stamp their real
	// depth on the call context (mcp.WithA2ADepth), which wins.
	RegisterA2APeers(client, a2aPeers, 0)
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

// RegisterA2APeers translates the resolved config.A2APeerConfig catalog into
// the mcp package's spec shape and registers it onto client (#1368). Exported
// for the same reason as RegisterHTTPTools: the interactive Manager, the
// broker (boot client and per-scope clients), and the scheduled per-task
// binder all register the SAME peer catalog through ONE path, host-side
// credentials only. defaultDepth is the calling run's delegation depth when
// the call context carries none — 0 for human-initiated work.
//
// The two knobs are resolved HERE, at the point of use, because the broker
// child registers peers without ever running config.Load: a malformed value
// was already refused at boot by the parent's external-knob gate, so the
// child keeping its safe default on a parse error is defense in depth.
func RegisterA2APeers(client *mcp.Client, peers []config.A2APeerConfig, defaultDepth int) {
	if len(peers) == 0 {
		return
	}
	maxDepth, err := config.EnvKnobInt("FLEET_A2A_MAX_DELEGATION_DEPTH", config.DefaultA2AMaxDelegationDepth)
	if err != nil {
		log.Printf("warn: %v; a2a peers keep the default delegation ceiling %d", err, config.DefaultA2AMaxDelegationDepth)
		maxDepth = config.DefaultA2AMaxDelegationDepth
	}
	allowPrivate, err := config.EnvKnobBool("FLEET_A2A_CLIENT_ALLOW_PRIVATE", false)
	if err != nil {
		log.Printf("warn: %v; a2a peers keep the SSRF guard on", err)
		allowPrivate = false
	}
	if defaultDepth < 0 {
		defaultDepth = 0
	}
	specs := make([]mcp.A2APeerSpec, 0, len(peers))
	for _, p := range peers {
		specs = append(specs, mcp.A2APeerSpec{
			Name:         p.Name,
			Description:  p.Description,
			RPCURL:       p.RPCURL,
			Headers:      p.Headers,
			MaxDepth:     maxDepth,
			DefaultDepth: defaultDepth,
			AllowPrivate: allowPrivate,
		})
	}
	client.AddA2APeers(specs)
	log.Printf("a2a peers registered: %d (delegation depth %d of max %d, private peers allowed=%v)", len(specs), defaultDepth, maxDepth, allowPrivate)
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
		client = BuildMCPClient(opts.ServerSpecs, cfg.HTTPTools, cfg.A2APeers)
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
	m.alwaysOnServerMetadata = m.buildAlwaysOnServerMetadata(opts.ServerSpecs)
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
// pins it — including 0, which disables warming entirely so every take pays a
// cold start (#1264; the config default is a -1 "unset" sentinel precisely so
// 0 stays expressible) — otherwise it is derived from MaxConcurrentAgents
// (clamped 2..8), preserving the prior default (#181).
func resolveWarmSize(cfg *config.Config) int {
	if cfg.SandboxWarmSize >= 0 {
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
	// The shared file library's staged tree (docs/SHARED-FILES.md). It lives
	// UNDER the workspace root because that is the one tree both backends make
	// visible inside sandboxes; the read-only mount below then overlays the
	// read-write workspace mount so no turn can tamper with what every other
	// chat reads. Created here, before the pool spawns anything, because on
	// kubernetes the pod spec mounts this directory as a subPath of the
	// workspace claim — if the kubelet creates it first it is root-owned and
	// the control plane can no longer stage files into it.
	sharedFilesDir := tools.SharedFilesDir(workspaceRoot)
	if err := os.MkdirAll(sharedFilesDir, 0o755); err != nil { //nolint:gosec // same — readable by the rootless container user via bind mount
		return nil, fmt.Errorf("ensure shared files dir %s: %w", sharedFilesDir, err)
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
		// 0 → sandbox default (30s). The #1358 escape hatch; the boot pre-warm
		// below is what keeps the default sufficient after an image update.
		StartTimeout:   time.Duration(cfg.SandboxStartTimeoutSeconds) * time.Second,
		BridgeDir:      filepath.Join(filepath.Dir(workspaceRoot), "data", "sandbox-bridge"),
		ReadOnlyMounts: absSupportingDocs(personasDir, protocolsDir, systemPromptsDir, skillsDir, uploadsRoot, sharedFilesDir),
	}
	// Kubernetes backend (#989): sandboxes are ephemeral pods in a cluster
	// instead of co-located podman containers. All podman-specific boot work
	// below (bridge-file prune, OCI-runtime preflight, egress proxy) is
	// replaced by the backend's own fail-closed cluster preflight.
	if cfg.SandboxBackend == sandbox.BackendKubernetes {
		return buildKubernetesSandboxPool(cfg, poolCfg, sandboxRuntime,
			absSupportingDocs(personasDir, protocolsDir, systemPromptsDir, skillsDir))
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
	// Pay the one-time keep-id id-remapped layer copy of a NEW sandbox image
	// up front (#1358), BEFORE the warm pool's first cold start: without this,
	// every start after an image update died at the start timeout mid-copy
	// ("podman run: signal: killed") and each retry restarted the copy, so the
	// box stayed wedged. Marker-gated: an unchanged image costs one
	// `podman image inspect` per boot. Best-effort — the tunable start
	// timeout plus its named error remain the backstop.
	sandbox.PrewarmKeepIDImage(context.Background(), poolCfg.Container.PodmanBinary,
		poolCfg.Container.Image,
		filepath.Join(filepath.Dir(workspaceRoot), "data", "sandbox-keepid-prewarm"))
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
		log.Printf("sandbox: network mode=lockdown — scheduled-task, interactive chat, AND approved-bash egress sealed regardless of per-task AllowNetwork; the warm pool spawns sealed (--network=none) containers, and sealed turns claim them (#1291).")
	}
	return sandbox.NewPool(poolCfg), nil
}

// buildKubernetesSandboxPool finishes pool construction for the kubernetes
// backend (#989): it refuses podman-only knobs that would otherwise be
// silently ignored (a configured-but-inert security knob is the failure mode
// ADR-0010's no-degrade rule exists for), builds the backend handle, and runs
// the fail-closed cluster preflight before the warm pool spawns its first pod.
//
// bundleDocDirs are the bundle's own supporting-doc roots (personas,
// protocols, system_prompts, skills) — the subset of
// poolCfg.Container.ReadOnlyMounts a sandbox IMAGE could plausibly carry, and
// therefore the only ones bundle_docs_in_image can vouch for.
func buildKubernetesSandboxPool(cfg *config.Config, poolCfg sandbox.PoolConfig, sandboxRuntime string, bundleDocDirs []string) (*sandbox.Pool, error) {
	if sandboxRuntime != "" {
		return nil, fmt.Errorf(
			"FLEET_SANDBOX_RUNTIME=%q is a podman OCI-runtime knob and has no effect under FLEET_SANDBOX_BACKEND=kubernetes; "+
				"select hypervisor isolation with FLEET_SANDBOX_K8S_RUNTIME_CLASS (a cluster RuntimeClass, e.g. kata) instead (fail-closed)", sandboxRuntime)
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_SANDBOX_SECCOMP_PROFILE")); v != "" {
		return nil, fmt.Errorf(
			"FLEET_SANDBOX_SECCOMP_PROFILE=%q is a podman knob and has no effect under FLEET_SANDBOX_BACKEND=kubernetes; "+
				"install the profile on the sandbox nodes and set FLEET_SANDBOX_K8S_SECCOMP_PROFILE (a kubelet-relative Localhost profile) instead (fail-closed)", v)
	}
	if cfg.DefaultNetworkMode == sandbox.NetworkModeAllowlisted {
		return nil, fmt.Errorf(
			"FLEET_DEFAULT_NETWORK_MODE=allowlisted is not supported under FLEET_SANDBOX_BACKEND=kubernetes: the host-side egress proxy " +
				"is unreachable from sandbox pods. Use lockdown (sealed by the deny-all NetworkPolicy) or open, and shape egress with cluster NetworkPolicies (fail-closed)")
	}
	// A pids ceiling is a containment control — it is what bounds a fork bomb —
	// and a Pod spec cannot express one: the cap lives on the kubelet
	// (podPidsLimit), outside anything this process writes. Podman applies
	// --pids-limit on every container, so the same knob means "bounded" there
	// and "unbounded" here. That is exactly the configured-but-inert shape the
	// three refusals above exist for, and it stayed silent only because a pids
	// limit reads like a resource knob rather than a security one.
	if poolCfg.Container.PidsLimit > 0 {
		return nil, fmt.Errorf(
			"FLEET_SANDBOX_PIDS=%d is a podman cgroup knob and has no effect under FLEET_SANDBOX_BACKEND=kubernetes: a Pod spec has no per-pod pids limit, "+
				"so this would read as a process ceiling while imposing none. Set the kubelet's podPidsLimit on the sandbox nodes instead, and unset this knob "+
				"to acknowledge that process counts there are bounded only by the pod's memory and CPU limits (fail-closed)", poolCfg.Container.PidsLimit)
	}
	// Supporting-doc mounts are same-path HOST bind mounts, and a pod has no
	// host filesystem to bind them from — so by default they are dropped, and
	// the fileop anchor then refuses those roots rather than trusting paths
	// nothing mounted. A sandbox IMAGE can still carry the bundle's doc dirs
	// at the same absolute paths (that is how bash/run_python keep resolving
	// `protocols/…` through the workspace symlinks); an operator who built
	// such an image declares it with bundle_docs_in_image, and the anchors for
	// those roots stay valid so the FILE TOOLS work too.
	//
	// The declaration cannot be probed — fleet does not inspect image
	// contents — but it cannot widen anything either: it only re-admits
	// read-only anchors for operator-configured bundle paths, and the reads
	// still execute inside the sandbox. A wrong declaration surfaces as a
	// not-found read, which is the podman missing-dir behavior.
	docsInImage, err := sandbox.ParseK8sBundleDocsInImage(cfg.SandboxK8sBundleDocsInImage)
	if err != nil {
		return nil, fmt.Errorf("FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE / sandbox.kubernetes.bundle_docs_in_image: %w", err)
	}
	// Read-only roots nested INSIDE the workspace claim (the shared file
	// library's staged tree; the skills tree StageSkillsForBackend staged at
	// boot) are not host paths at all — every pod sees them by construction,
	// mounted read-only as a subPath of the claim by the pod spec builder — so
	// they bypass the host-mount drop below entirely.
	wsNested, hostMounts := splitWorkspaceNestedMounts(poolCfg.Container.ReadOnlyMounts, poolCfg.Container.WorkspaceHostDir)
	if len(wsNested) > 0 {
		log.Printf("sandbox: kubernetes backend — %d read-only root(s) inside the workspace claim %v are mounted read-only (subPath) in every sandbox pod", len(wsNested), wsNested)
	}
	kept, dropped := k8sDocMounts(hostMounts, bundleDocDirs, docsInImage)
	poolCfg.Container.ReadOnlyMounts = slices.Concat(wsNested, kept)
	if len(kept) > 0 {
		log.Printf("sandbox: kubernetes backend — bundle_docs_in_image declared: keeping fileop read anchors for %d bundle doc root(s) %v; the SANDBOX IMAGE must carry them at these exact paths or reads fail not-found", len(kept), kept)
	}
	for _, d := range dropped {
		switch {
		case clientconfig.IsMaterializedSkillsDir(d):
			// Only reachable when StageSkillsForBackend failed at boot: the
			// staged copy inside the claim would have been split off as a
			// nested mount above.
			log.Printf("sandbox: kubernetes backend — skills dir %q is still the merged tree under the control plane's data dir because staging it into the workspace claim failed at boot (see the earlier 'stage skills' warning); no pod can see it, so in-sandbox skill reads will not resolve", d)
		case docsInImage:
			log.Printf("sandbox: kubernetes backend — %q is not a bundle doc dir, so bundle_docs_in_image does not vouch for it; in-sandbox reads there will not resolve", d)
		}
	}
	if !docsInImage && len(dropped) > 0 {
		log.Printf("sandbox: kubernetes backend — supporting-doc bind mounts do not apply (pods mount only the workspace claim); in-sandbox reads of %d host dir(s) will not resolve. If your sandbox image carries the bundle's doc dirs at the same paths, set sandbox.kubernetes.bundle_docs_in_image (FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE=true)", len(dropped))
	}
	poolCfg.Container.Runtime = ""

	// Scheduling knobs fail closed on a malformed value: a typo'd selector
	// must not silently schedule sandboxes onto the wrong (untainted,
	// unlabeled) nodes.
	nodeSelector, err := sandbox.ParseK8sNodeSelector(cfg.SandboxK8sNodeSelector)
	if err != nil {
		return nil, fmt.Errorf("FLEET_SANDBOX_K8S_NODE_SELECTOR / sandbox.kubernetes.node_selector: %w", err)
	}
	tolerations, err := sandbox.ParseK8sTolerations(cfg.SandboxK8sTolerations)
	if err != nil {
		return nil, fmt.Errorf("FLEET_SANDBOX_K8S_TOLERATIONS / sandbox.kubernetes.tolerations: %w", err)
	}
	backend, err := sandbox.NewKubernetesBackend(sandbox.KubernetesConfig{
		Namespace:                      cfg.SandboxK8sNamespace,
		WorkspaceClaim:                 cfg.SandboxK8sWorkspaceClaim,
		ServiceAccount:                 cfg.SandboxK8sServiceAccount,
		ImagePullSecret:                cfg.SandboxK8sImagePullSecret,
		RuntimeClassName:               cfg.SandboxK8sRuntimeClass,
		SeccompLocalhostProfile:        cfg.SandboxK8sSeccompProfile,
		KubeconfigPath:                 cfg.SandboxK8sKubeconfig,
		NetworkPolicyName:              cfg.SandboxK8sNetworkPolicy,
		OpenEgressPolicyName:           cfg.SandboxK8sOpenEgressPolicy,
		DefaultNetworkMode:             cfg.DefaultNetworkMode,
		UnrestrictedEgressAcknowledged: cfg.SandboxK8sOpenEgressAcknowledged,
		NodeSelector:                   nodeSelector,
		Tolerations:                    tolerations,
		// The same knob as the podman backend's container start bound (#1358):
		// here it caps one pod's schedule+pull+start. 0 → the backend's own
		// 2-minute default.
		StartTimeout: time.Duration(cfg.SandboxStartTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	// Fail closed BEFORE the warm pool spawns its first pod: a cluster that
	// cannot run sandboxes (unreachable apiserver, missing RBAC, absent
	// workspace claim or sealed-egress policy) must abort boot, never
	// silently fall back to podman or host execution.
	if err := backend.Preflight(context.Background()); err != nil {
		return nil, fmt.Errorf("kubernetes sandbox preflight failed (fail-closed): %w", err)
	}
	poolCfg.Mode = sandbox.ModeKubernetes
	poolCfg.KubernetesBackend = backend

	poolCfg.DefaultNetworkMode = cfg.DefaultNetworkMode
	poolCfg.DefaultEgressAllowlist = nil
	log.Printf("sandbox: kubernetes backend — image=%s, pool=%d, workspace=%s, namespace=%s, runtime_class=%s",
		poolCfg.Container.Image, poolCfg.Size, poolCfg.Container.WorkspaceHostDir, backend.Namespace(), defaultIfEmpty(cfg.SandboxK8sRuntimeClass, "cluster default"))
	if poolCfg.PersistentREPL {
		log.Printf("sandbox: run_python REPL mode=persistent — one kernel per conversation survives across turns (idle TTL %s, max %d sessions)",
			poolCfg.PersistentIdleTTL, cfg.PythonREPLMaxSessions)
	} else {
		log.Printf("sandbox: run_python REPL mode=per-turn — kernel is fresh each turn (the default)")
	}
	log.Print(k8sNetworkModeLine(cfg.DefaultNetworkMode))
	return sandbox.NewPool(poolCfg), nil
}

// k8sNetworkModeLine renders the boot line describing the egress posture every
// sandbox pod will be created with.
//
// Both postures speak, which they did not before (#1264): lockdown printed a
// full paragraph and open printed nothing at all, so the riskier configuration
// was the quieter one and an operator reading the log of an open deployment
// saw no sign that model-authored code could reach the cluster.
//
// Neither line claims enforcement. Under lockdown that is the CNI's job, and
// fleet verifies only that the policy OBJECT exists. Under open there is
// nothing to verify: the protecting policy may carry any name and come from
// any tooling, so fleet states the posture it is creating pods with and leaves
// the conclusion to the reader rather than inventing a verdict.
//
// Only these two modes reach here — allowlisted is refused at the top of
// buildKubernetesSandboxPool.
func k8sNetworkModeLine(mode string) string {
	if mode == sandbox.NetworkModeLockdown {
		// "Every pod" includes the warm pool's parked spawns: the pool seals
		// warm spawns under fleet-wide lockdown (#1291), so this claim is true
		// by construction. Before that fix, warm pods were labeled egress=open
		// right beside this line.
		return fmt.Sprintf("sandbox: network mode=lockdown — every sandbox pod, warm-pool spawns included, is labeled %s=none for the deny-all NetworkPolicy (enforcement is the cluster CNI's job — see docs/DEPLOYMENT-KUBERNETES.md)", k8sEgressLabel)
	}
	return fmt.Sprintf("sandbox: WARNING network mode=open — every sandbox pod is labeled %s=open and has UNRESTRICTED pod-network reach unless a NetworkPolicy selects that label. Model-authored code can then reach the fleet Service, the in-cluster database, the apiserver, and the node's metadata endpoint. The chart ships that policy but does NOT enable it by default: set networkPolicies.openEgress.create=true with blockedCIDRs, or use mode=lockdown (see docs/DEPLOYMENT-KUBERNETES.md)", k8sEgressLabel)
}

// k8sEgressLabel is the pod label both the chart's NetworkPolicies and the
// backend's pod builder key on.
const k8sEgressLabel = "fleet.elcanotek.com/egress"

// splitWorkspaceNestedMounts partitions the read-only mount list into roots
// nested inside the workspace root and everything else. Under the kubernetes
// backend the two have opposite fates: a workspace-nested root lives in the
// shared claim (reachable in every pod — the pod spec re-mounts it read-only
// via subPath), while any other root is a HOST path a pod cannot bind and goes
// through the k8sDocMounts drop rules. Pure so the policy is pinned by tests.
func splitWorkspaceNestedMounts(mounts []string, workspaceRoot string) (nested, others []string) {
	root := filepath.Clean(workspaceRoot)
	for _, m := range mounts {
		if m == "" {
			continue
		}
		rel, err := filepath.Rel(root, filepath.Clean(m))
		if workspaceRoot != "" && err == nil && rel != "." && filepath.IsLocal(rel) {
			nested = append(nested, m)
			continue
		}
		others = append(others, m)
	}
	return nested, others
}

// StageSkillsForBackend makes the bundle's skills tree reachable from inside
// a sandbox on the selected backend and returns the skills dir every downstream
// consumer (SetSupportingDocDirs, ManagerOptions.SkillsDir, the pool's
// read-only mounts) must use from then on.
//
// Under podman the merged tree under the data dir is bind-mounted same-path,
// so nothing changes and bundle.SkillsDir is returned as is. Under kubernetes a
// pod mounts only the workspace claim, so the complete tree — built-in pack,
// Agent Plugin skills, the bundle's own skills/ — is re-staged at
// <workspace root>/skills (tools.StagedSkillsDir) inside that claim, where
// buildKubernetesSandboxPool then treats it like the shared file library: a
// read-only subPath mount in every pod, no image bake, no declaration
// (ADR-0055). Staging always happens on that backend, whether or not the
// built-in pack is inherited — the point is that skills reach a pod at all.
//
// It must run BEFORE the pool is built (the pod spec mounts the directory as a
// subPath, so the control plane has to create it first — the same ordering
// the shared library needs) and before the skills dir is registered anywhere.
// A staging failure is returned with bundle.SkillsDir unchanged so the caller
// can degrade loudly rather than fail boot: skills are a capability, not a
// boot invariant, and buildKubernetesSandboxPool's IsMaterializedSkillsDir
// branch then names the consequence in the log.
func StageSkillsForBackend(cfg *config.Config, bundle *clientconfig.Bundle) (string, error) {
	if bundle == nil {
		return "", nil
	}
	if cfg == nil || cfg.SandboxBackend != sandbox.BackendKubernetes {
		return bundle.SkillsDir, nil
	}
	root := strings.TrimSpace(cfg.WorkspaceRoot)
	if root == "" {
		root = "workspace"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return bundle.SkillsDir, fmt.Errorf("stage skills: resolve workspace root: %w", err)
	}
	staged := tools.StagedSkillsDir(abs)
	if err := bundle.StageSkillsAt(staged); err != nil {
		return bundle.SkillsDir, err
	}
	log.Printf("sandbox: kubernetes backend — skills tree (built-in pack + plugin skills + bundle skills/) staged at %s inside the workspace claim; every sandbox pod mounts it read-only, no image bake needed", staged)
	return bundle.SkillsDir, nil
}

// k8sDocMounts splits the supporting-doc mount list into the roots whose
// fileop anchors survive under the kubernetes backend and the roots that are
// dropped, given the operator's bundle_docs_in_image declaration.
//
// Pure and total so the policy is pinned by tests rather than read out of the
// boot log. Three rules, in order:
//
//   - Without the declaration, nothing survives: a pod mounts only the
//     workspace claim, so every host path is a path the anchor must not trust.
//   - With it, only the BUNDLE's own doc dirs survive. Everything else in the
//     list (the uploads root) lives in control-plane state a sandbox image
//     cannot contain, and the declaration says nothing about it. Chat
//     attachments stay reachable anyway: the chat server stages them into the
//     conversation workspace under this backend (httpapi
//     stageAttachmentsIntoWorkspace) instead of relying on this mount.
//   - A materialized (merged built-in + bundle) skills tree never survives:
//     it lives under the data dir with a path derived from the bundle path, so
//     no image can carry it. See clientconfig.IsMaterializedSkillsDir.
func k8sDocMounts(mounts, bundleDocDirs []string, docsInImage bool) (kept, dropped []string) {
	bundle := make(map[string]bool, len(bundleDocDirs))
	for _, d := range bundleDocDirs {
		if d != "" {
			bundle[filepath.Clean(d)] = true
		}
	}
	for _, m := range mounts {
		if m == "" {
			continue
		}
		clean := filepath.Clean(m)
		if docsInImage && bundle[clean] && !clientconfig.IsMaterializedSkillsDir(clean) {
			kept = append(kept, m)
			continue
		}
		dropped = append(dropped, m)
	}
	return kept, dropped
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

// logSafeAgent strips CR/LF from a user-influenced value before it is
// interpolated into a log line, so a hostile value cannot forge a log entry —
// the same guard as httpapi's logSafeSlug / handlers' logSafe / the runner's
// logSafeRunner, declared per package because none of those is importable
// without a cycle or a widened API.
//
// strings.ReplaceAll rather than the NewReplacer the siblings use: CodeQL's
// go/log-injection query models ReplaceAll of "\n"/"\r" as the sanitizer, so
// this spelling both is the guard and is provable as one — a NewReplacer
// version kept the alert open as a false positive.
func logSafeAgent(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
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

func (m *Manager) openRemoteOverlay(ctx context.Context, email string, baseCatalog []mcp.ServerTool, sel RemoteMCPSelection) (*RemoteMCPOverlay, error) {
	shadowed := make(map[string]bool, len(baseCatalog))
	for _, st := range baseCatalog {
		shadowed[st.ServerName] = true
	}
	if m.openRemoteMCPOverlay != nil {
		overlay, err := m.openRemoteMCPOverlay(ctx, email, shadowed, sel)
		if err != nil {
			return nil, err
		}
		if err := overlay.Validate(); err != nil {
			overlay.Close()
			return nil, err
		}
		return overlay, nil
	}
	return BuildRemoteMCPOverlay(ctx, m.remoteMCP, email, shadowed, sel)
}

// OpenApprovalRemoteMCPScope reopens the hosted-connection seat a staged
// approval recorded (#988, the remote half of #167 residual 2): a short-lived
// overlay mounting exactly {server, account} — Exact, so "" means the
// unlabeled seat and a default that has since moved is never substituted. It
// returns (nil, nil) when remote MCP is not wired into this Manager. A seat
// that no longer resolves (disconnected, needs re-auth, removed) yields an
// error naming it, so the approval fails closed rather than running as a
// different account. The caller owns the returned overlay and MUST Close it.
func (m *Manager) OpenApprovalRemoteMCPScope(ctx context.Context, email, server, account string) (*RemoteMCPOverlay, error) {
	if m.openRemoteMCPOverlay == nil && m.remoteMCP == nil {
		return nil, nil
	}
	sel := RemoteMCPSelection{
		Filter:   true,
		Enabled:  map[string]bool{server: true},
		Accounts: map[string]string{server: account},
		Exact:    true,
	}
	overlay, err := m.openRemoteOverlay(ctx, email, m.mcpCatalogSnapshot(), sel)
	if err != nil {
		return nil, err
	}
	want := agentcore.RegisteredMCPName(server, account)
	if overlay == nil || !overlay.Servers[want] {
		overlay.Close()
		return nil, fmt.Errorf("hosted connection %q is not connected for %s (skipped: %v)", want, email, overlay.skippedNames())
	}
	return overlay, nil
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

// admitInteractiveTurn is RunTurn's admission control: an interactive turn
// holds one slot in the shared box-wide concurrency limiter for its whole
// duration, so chat counts against the same cap as scheduled tasks (and draws
// on the reserve that keeps chat ahead of background work). Wait only briefly —
// a human is watching — then surface ErrAtCapacity so the UI shows a clean "at
// capacity, retry" instead of a hung turn or an over-subscribed box. The caller
// defers the returned release so the slot is held for the whole turn; without a
// limiter the release is a no-op.
func (m *Manager) admitInteractiveTurn(ctx context.Context) (func(), error) {
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
		return release, nil
	}
	return func() {}, nil
}

// composeTurnSystemPrompt builds the per-turn system prompt, first fetching the
// admin-curated knowledge base (best-effort: a notes failure runs the turn
// without the section rather than failing it).
func (m *Manager) composeTurnSystemPrompt(ctx context.Context, in TurnInput, persona string) (string, error) {
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
		return "", fmt.Errorf("compose system prompt: %w", err)
	}
	return systemPrompt, nil
}

// assembleTurnMessages replays the persisted history and appends this turn's
// user message (with any image attachments) as the outgoing model messages. It
// also returns the user HistoryEntry: the new user message + its image refs
// are persisted as the first entry of the turn; the run loop's accumulated
// entries follow.
func assembleTurnMessages(in TurnInput) ([]fantasy.Message, HistoryEntry, error) {
	history, err := replayHistory(in.History)
	if err != nil {
		return nil, HistoryEntry{}, fmt.Errorf("replay history: %w", err)
	}
	imageParts, imageRefs := loadImageAttachments(in.ImageAttachments)
	messages := make([]fantasy.Message, 0, len(history)+1)
	messages = append(messages, history...)
	messages = append(messages, fantasy.NewUserMessage(in.UserMessage, imageParts...))
	userEntry := mustEntry("user", "text", TextContent{Text: in.UserMessage, Images: imageRefs})
	return messages, userEntry, nil
}

// interactiveRunSelection maps the conversation's opt-in list to the per-run
// MCP selection (default account). agentcore.buildFantasyTools registers the
// opted-in servers' tools through the InteractivePolicy gate.
func interactiveRunSelection(enabled []string, accountDefaults map[string]string) agentcore.MCPSelection {
	selection := make(agentcore.MCPSelection, 0, len(enabled))
	for _, name := range enabled {
		if n := strings.TrimSpace(name); n != "" {
			// The user's default seat (connections page) rides along; a chat
			// turn is supervised, so the per-user default is the right seat —
			// scheduled tasks pin their own {server, account} explicitly.
			selection = append(selection, agentcore.MCPChoice{Server: n, Account: accountDefaults[n]})
		}
	}
	return selection
}

// openTurnRemoteOverlay opens the per-user remote (hosted) MCP overlay (#443):
// a short-lived client of the user's OAuth-connected servers (fresh bearer,
// SSRF-safe transport) that composes with the shared catalog. Best-effort: a
// server that needs re-auth is skipped, and an overlay failure logs and returns
// nil, never failing the turn. The caller closes a non-nil overlay when the
// turn ends.
func (m *Manager) openTurnRemoteOverlay(ctx context.Context, in TurnInput, turnCatalog []mcp.ServerTool) *RemoteMCPOverlay {
	var overlay *RemoteMCPOverlay
	if in.UserEmail != "" && (m.openRemoteMCPOverlay != nil || m.remoteMCP != nil) {
		// The conversation's seat overrides (and, for bundled names, the user's
		// connections-page default) ride in MCPAccountDefaults; a remote name
		// without an entry mounts its default seat (#988).
		ov, oerr := m.openRemoteOverlay(ctx, in.UserEmail, turnCatalog, RemoteMCPEnabledOnly(in.OptionalMCPServersEnabled, in.MCPAccountDefaults))
		if oerr != nil {
			log.Printf("RunTurn: remote-mcp overlay unavailable for %s: %v", logSafeAgent(in.UserEmail), oerr)
		} else if ov != nil {
			overlay = ov
			if len(ov.Skipped) > 0 {
				// Interactive: the user can see+fix these on the Connections page.
				// Skipped carries USER-NAMED servers — CR/LF-strip them so a
				// hostile name cannot forge a log entry (log-injection guard).
				log.Printf("RunTurn: remote MCP server(s) need re-auth for %s: %s",
					logSafeAgent(in.UserEmail), logSafeAgent(strings.Join(ov.Skipped, ", ")))
			}
		}
	}
	return overlay
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

	release, err := m.admitInteractiveTurn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	systemPrompt, err := m.composeTurnSystemPrompt(ctx, in, persona)
	if err != nil {
		return nil, err
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
	turnTools := tools.NewTurnTools(sb, m.browserbaseKeyFunc(ctx, in.UserEmail, in.OptionalMCPServersEnabled))
	turnTools.Tools = filterNativeToolsByOptIn(turnTools.Tools, in.OptionalMCPServersEnabled)

	model, providerFallbacks, err := m.modelResolver().ResolveWithFallbacks(ctx, in.Model)
	if err != nil {
		return nil, fmt.Errorf("resolve model: %w", err)
	}
	modelSlug := model.Model()

	messages, userEntry, err := assembleTurnMessages(in)
	if err != nil {
		return nil, err
	}

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

	selection := interactiveRunSelection(in.OptionalMCPServersEnabled, in.MCPAccountDefaults)

	overlay := m.openTurnRemoteOverlay(ctx, in, turnCatalog)
	if overlay != nil {
		defer overlay.Close()
	}
	// Rebind the stager once the hosted overlay is up (#988): a remote tool
	// must resolve against the composite the loop dispatches on, and its
	// staged card must record the {connection, account} seat that was
	// actually mounted — the seat approval execution reopens verbatim.
	if overlay.Active() {
		if binder, ok := in.ApprovalStager.(MCPScopeBinder); ok && binder != nil {
			broker, catalog := overlay.ComposeWith(turnBroker, turnCatalog)
			binder.BindTurnMCPScope(TurnMCPScope{
				Broker:    broker,
				Catalog:   catalog,
				Selection: append(append(agentcore.MCPSelection(nil), turnSelection...), overlay.SeatSelection()...),
			})
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
		return m.failedTurnResult(ctx, runErr, res, userEntry, modelSlug, startedAt, sink, in.CommitTerminal)
	}
	if res.Cancelled {
		return m.cancelledTurnResult(res, userEntry, modelSlug, startedAt, ctx.Err(), sink, in.CommitTerminal)
	}
	return m.completedTurnResult(res, userEntry, modelSlug, startedAt, sink, in.CommitTerminal)
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

// failedTurnResult classifies a RunInteractiveTurn error. Caller-cancelled
// (distinguished from a genuine stream failure via ctx.Err(); the loop returned
// a partial result) finalizes as a cancelled turn. A guardrail block surfaces
// as-is after emitting turn.policy_blocked. Everything else is a stream failure
// the user can fix by choosing another model: committed side effects persist
// first (ADR-0035), then ErrModelSelectionRequired wraps the cause so the HTTP
// layer suppresses its generic turn.error in favor of turn.model_required.
func (m *Manager) failedTurnResult(ctx context.Context, runErr error, res agentcore.Result, userEntry HistoryEntry, modelSlug string, startedAt time.Time, sink EventSink, commitTerminal func([]HistoryEntry, bool) error) (*TurnResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return m.cancelledTurnResult(res, userEntry, modelSlug, startedAt, ctxErr, sink, commitTerminal)
	}
	if errors.Is(runErr, agentcore.ErrGuardrailBlocked) {
		sink.Emit("turn.policy_blocked", map[string]any{"policy": "prompt-injection"})
		return nil, runErr
	}
	commitPartialSideEffects(runErr, res, commitTerminal)
	reason, status, _ := agentcore.ClassifyStreamErrorReason(runErr)
	// modelSlug is a user-selected string and runErr can embed provider
	// response text — CR/LF-strip both so neither can forge a log entry
	// (log-injection guard); %q on the slug keeps a hostile value visibly
	// quoted rather than blending into the line. reason is a fixed enum
	// the classifier returns, but it is DERIVED from runErr, so it gets the
	// same guard to keep the whole line provably clean.
	log.Printf("RunTurn stream failed (reason=%s model=%q status=%d): %s",
		logSafeAgent(string(reason)), logSafeAgent(modelSlug), status, logSafeAgent(runErr.Error()))
	emitModelSelectionRequired(sink, reason, modelSlug, status, runErr)
	return nil, fmt.Errorf("%w: %w", ErrModelSelectionRequired, runErr)
}

// completedTurnResult finalizes a turn that ran to completion: it maps the
// accumulated transcript to history entries, appends the turn_summary, commits
// canonical history behind the terminal gate, emits turn.completed, and builds
// the TurnResult. Mirrors cancelledTurnResult for the completed path.
func (m *Manager) completedTurnResult(res agentcore.Result, userEntry HistoryEntry, modelSlug string, startedAt time.Time, sink EventSink, commitTerminal func([]HistoryEntry, bool) error) (*TurnResult, error) {
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
	if commitTerminal != nil {
		if err := commitTerminal(newHistory[1:], false); err != nil {
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
