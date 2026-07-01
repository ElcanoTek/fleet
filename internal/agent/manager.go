package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
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
// Summarize / SuggestTitle / MCPClient / SandboxPool / MCPServerCatalog /
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

	// Lockdown is set when the conversation row has lockdown=true. Forces a
	// per-turn container sandbox and constrains the resolved model slug to the
	// operator's lockdown allow-list.
	Lockdown bool

	// ThinkingConfig, when set and Enabled, activates Claude extended thinking
	// (#220) for this turn. The caller (httpapi) resolves it from the
	// per-conversation override or the global default before the call; nil = off.
	// A non-Claude model silently ignores it (see agentcore.supportsExtendedThinking).
	ThinkingConfig *agentcore.ThinkingConfig
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
	// is off; turns run exactly as before.
	RemoteMCP RemoteMCPResolver

	// LLMProviders is the resolved multi-provider routing table (#289), translated
	// by cmd/fleet from the bundle manifest's providers: block (API-key env vars
	// already resolved host-side). Empty = the historical single-OpenRouter path
	// (NewModelResolver with cfg.OpenRouterAPIKey), so existing deployments are
	// unchanged.
	LLMProviders []agentcore.ProviderConfig
}

// New constructs a Manager: it dials OpenRouter (via the model resolver),
// connects every enabled MCP server in ServerSpecs (credentialed host-side),
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
			addErr = client.AddStdioServer(ctx, name, spec.Command, spec.Args, spec.Env, spec.Dir)
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

	// Connect the catalog (credentialed host-side). The SAME builder backs the
	// out-of-process broker (fleet mcp-broker), so both register servers
	// identically — there is no second, divergent credential path (issue #167).
	// Inline http_tools (issue #261) are registered onto the same client here too.
	client := BuildMCPClient(opts.ServerSpecs, cfg.HTTPTools)
	// Gating metadata (per-server allowlist + Optional flag) is pure spec data,
	// independent of the live connection.
	allow := mcpAllowlist{}
	optional := mcpOptionalSet{}
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
		allowlist:            allow,
		resolver:             resolver,
		native:               tools.DefaultTools(),
		sandboxPool:          pool,
		notesProvider:        opts.NotesProvider,
		noteProposer:         opts.NoteProposer,
		optionalServers:      optional,
		personasDir:          opts.PersonasDir,
		protocolsDir:         opts.ProtocolsDir,
		skillsDir:            opts.SkillsDir,
		systemPromptsDir:     opts.SystemPromptsDir,
		chatSystemPromptFile: chatPromptFile,
		limiter:              opts.Limiter,
		health:               agentcore.NewProviderHealthRegistry(),
		personaPolicies:      opts.PersonaPolicies,
		remoteMCP:            opts.RemoteMCP,
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
	// Fail closed BEFORE the warm pool spawns its first container: a kata/krun
	// runtime whose KVM or runtime binary is missing must abort boot, never
	// silently degrade to a shared-kernel container (the no-degrade invariant,
	// ADR-0010). A shared-kernel runtime (runc/crun/runsc/empty) preflights as a
	// no-op.
	if err := sandbox.PreflightRuntime(context.Background(), sandboxRuntime); err != nil {
		return nil, fmt.Errorf("sandbox runtime preflight failed (fail-closed): %w", err)
	}
	log.Printf("sandbox: container mode, image=%s, pool=%d, workspace=%s, runtime=%s",
		poolCfg.Container.Image, poolCfg.Size, poolCfg.Container.WorkspaceHostDir, defaultIfEmpty(poolCfg.Container.Runtime, "podman default"))
	if poolCfg.PersistentREPL {
		log.Printf("sandbox: run_python REPL mode=persistent — one kernel per conversation survives across turns (idle TTL %s, max %d sessions)",
			poolCfg.PersistentIdleTTL, cfg.PythonREPLMaxSessions)
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
		proxy := sandbox.NewEgressProxy()
		if err := proxy.Start(); err != nil {
			return nil, fmt.Errorf("start sandbox egress proxy (#211): %w", err)
		}
		poolCfg.EgressProxy = proxy
		if len(cfg.SandboxNetworkAllowlist) == 0 {
			log.Printf("sandbox: WARNING network mode=allowlisted but the allowlist is EMPTY — networked SCHEDULED-task sandboxes can reach NO domains (set sandbox.network_allowlist in the bundle manifest)")
		} else {
			log.Printf("sandbox: network mode=allowlisted — networked SCHEDULED-task egress filtered to %v via the host proxy (best-effort; ADR-0012). Chat turns are unaffected (wiring deferred).", cfg.SandboxNetworkAllowlist)
		}
	case sandbox.NetworkModeLockdown:
		log.Printf("sandbox: network mode=lockdown — SCHEDULED-task egress sealed regardless of per-task AllowNetwork. Chat turns are unaffected (wiring deferred).")
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

// Resolve loads + caches the OpenRouter model for a slug. Exposed so the
// scheduled runner (cmd/fleet) resolves its task model through the SAME cached
// resolver the interactive turns use.
func (m *Manager) Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error) {
	return m.resolver.Resolve(ctx, slug)
}

// MCPClient exposes the shared MCP client for the out-of-band approval-execution
// path (runStagedTool).
func (m *Manager) MCPClient() *mcp.Client { return m.mcpClient }

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
	if m.mcpClient == nil {
		return nil
	}
	all := m.mcpClient.GetAllTools()
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
//   - When persistent REPL mode is on (#213) and this is a non-lockdown chat
//     with a conversation ID, the turn borrows the conversation's long-lived
//     sandbox so the python kernel survives across turns. The returned cleanup
//     releases the borrow rather than closing the sandbox.
//   - Otherwise (the default) it's a fresh warm-pool container, closed at turn
//     end via the returned cleanup.
//
// Scheduled runs never reach here — they drive agentcore.Run through the
// scheduled runner, which owns its own per-run sandbox + worktree.
func (m *Manager) takeTurnSandbox(ctx context.Context, lockdown bool, convID string) (*sandbox.Sandbox, func(), error) {
	if lockdown {
		if !m.config.LockdownAvailable() {
			return nil, nil, fmt.Errorf("conversation is in lockdown mode but the server has no sandbox image configured")
		}
		sb, cleanup, err := m.sandboxPool.TakeContainer(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("take lockdown sandbox: %w", err)
		}
		return sb, cleanup, nil
	}
	if m.config.PersistentPythonREPL() && convID != "" {
		// TakePersistent reuses the conversation's sandbox (or creates one,
		// pulling a warm container for the first turn). It degrades to a per-turn
		// Take internally if persistence is disabled in the pool, so this is
		// always safe to call.
		sb, cleanup, err := m.sandboxPool.TakePersistent(convID)
		if err != nil {
			return nil, nil, fmt.Errorf("take persistent sandbox: %w", err)
		}
		return sb, cleanup, nil
	}
	sb, cleanup, err := m.sandboxPool.Take()
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
	persona := strings.TrimSpace(in.Persona)
	if persona == "" {
		persona = m.config.PersonaDefault
	}

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

	systemPrompt, err := m.buildSystemPrompt(persona, in.ConversationID, in.Memories, in.ProjectInstructions, notes, in.OptionalMCPServersEnabled)
	if err != nil {
		return nil, fmt.Errorf("compose system prompt: %w", err)
	}

	sb, sbCleanup, err := m.takeTurnSandbox(ctx, in.Lockdown, in.ConversationID)
	if err != nil {
		return nil, err
	}
	defer sbCleanup()
	turnTools := tools.NewTurnTools(sb, tools.WithBrowser(tools.BrowserConfig{
		Enabled:  m.config.BrowserEnabled,
		Lockdown: in.Lockdown,
	}))

	model, err := m.resolver.Resolve(ctx, in.Model)
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
			selection = append(selection, agentcore.MCPChoice{Server: n})
		}
	}

	// Per-user remote (hosted) MCP overlay (#443). Builds a short-lived client of
	// the user's OAuth-connected servers (fresh bearer, SSRF-safe transport) that
	// composes with the shared catalog. Best-effort: a server that needs re-auth
	// is skipped, never failing the turn. The overlay is closed when the turn ends.
	var overlay *RemoteMCPOverlay
	if m.remoteMCP != nil && in.UserEmail != "" {
		shadowed := make(map[string]bool)
		for _, st := range m.mcpClient.GetAllTools() {
			shadowed[st.ServerName] = true
		}
		// A remote server participates in a turn only when the conversation opted
		// in (the Tools picker), exactly like a bundle Optional server. New
		// conversations seed the opt-in list from the catalog (remote servers are
		// enabled_by_default), so a freshly-connected server is on by default yet
		// remains toggleable and bounded by the tool ceiling.
		enabled := make(map[string]bool, len(in.OptionalMCPServersEnabled))
		for _, name := range in.OptionalMCPServersEnabled {
			if n := strings.TrimSpace(name); n != "" {
				enabled[n] = true
			}
		}
		ov, oerr := BuildRemoteMCPOverlay(ctx, m.remoteMCP, in.UserEmail, shadowed, enabled)
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

	// Snapshot the MCP gating together under one RLock so this turn sees a
	// consistent (allowlist, optional-set) pair even if a hot reload (#218)
	// swaps them concurrently.
	turnAllowlist, turnOptional := m.mcpGates()

	tc := TurnConfig{
		SystemPrompt:    systemPrompt,
		Messages:        messages,
		Label:           in.ConversationID,
		Model:           model,
		Temperature:     m.config.LiveTemperature(),
		MaxTokens:       maxTokens,
		MaxIterations:   m.config.LiveMaxIterations(),
		PriorHistory:    in.History,
		NativeTools:     turnTools.Tools,
		Sandbox:         sb,
		MCPClient:       m.mcpClient,
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
		HealthRegistry:  m.health,
		ThinkingConfig:  in.ThinkingConfig,
	}
	tc.Overlay = overlay

	res, runErr := RunInteractiveTurn(ctx, tc, turnSink{sink: sink})

	if runErr != nil {
		// Distinguish caller-cancelled (handled below via res.Cancelled when the
		// loop returns a partial result) from a genuine stream failure that the
		// user can fix by choosing another model.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return m.cancelledTurnResult(res, userEntry, modelSlug, startedAt, ctxErr, sink), nil
		}
		reason, status, _ := agentcore.ClassifyStreamErrorReason(runErr)
		log.Printf("RunTurn stream failed (reason=%s model=%s status=%d): %v", reason, modelSlug, status, runErr)
		emitModelSelectionRequired(sink, reason, modelSlug, status, runErr)
		return nil, fmt.Errorf("%w: %w", ErrModelSelectionRequired, runErr)
	}

	if res.Cancelled {
		return m.cancelledTurnResult(res, userEntry, modelSlug, startedAt, ctx.Err(), sink), nil
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
func (m *Manager) cancelledTurnResult(res agentcore.Result, userEntry HistoryEntry, modelSlug string, startedAt time.Time, ctxErr error, sink EventSink) *TurnResult {
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
	}
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

// interactiveAdmitWait bounds how long an interactive turn waits for a free
// concurrency slot before giving up. Short, because a human is watching: it
// smooths a momentary spike (a slot usually frees in well under this) but on a
// genuinely saturated box yields a fast, honest "at capacity" instead of a hung
// turn. A var (not const) so tests can shorten it.
var interactiveAdmitWait = 5 * time.Second

// ErrAtCapacity is the sentinel RunTurn returns when the box is at its concurrency
// cap and no interactive slot freed within interactiveAdmitWait. The HTTP layer
// surfaces its message as a turn.error so the user sees a clean "try again in a
// moment" rather than a hung spinner. The user-facing text is the error itself.
var ErrAtCapacity = fmt.Errorf("the workspace is at capacity right now — please resend your message in a moment")
