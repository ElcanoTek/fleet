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

	"github.com/ElcanoTek/fleet/internal/acpruntime"
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

	// OptionalMCPServersEnabled is the conversation's opt-in list for Optional
	// MCP servers (e.g. gamma). nil/empty means "no optional servers".
	OptionalMCPServersEnabled []string

	// Memories are user-scoped long-term facts injected into the system prompt.
	Memories []string

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

	// Runtime selects the execution flavor for this turn (clientconfig flavor
	// name). "" / "native-inprocess" run the in-process loop; "native-acp" routes
	// through the sandboxed ACP agent; an "acp" (external) flavor drives an
	// external provider's agent (Claude Code / Goose) at the containment tier.
	// Unknown values fall back to the default.
	Runtime string

	// PermissionBroker, when set, routes an EXTERNAL acp agent's
	// session/request_permission to a human (default-deny on timeout, no
	// approve-all). Required for the external flavor; ignored by the native
	// flavors (which are fully governed in-loop). Nil with an external flavor
	// fails the turn's permission requests closed (deny).
	PermissionBroker PermissionBroker
}

// PermissionBroker routes an external ACP agent's permission request to a human
// and blocks for the decision (default-deny on timeout / cancel; no
// approve-all). It is re-exported from acpruntime so the httpapi layer can wire
// an implementation without importing acpruntime directly.
type PermissionBroker = acpruntime.PermissionBroker

// PermissionRequest / PermissionDecision are the broker's request/response
// shapes, re-exported from acpruntime for the same reason.
type (
	PermissionRequest  = acpruntime.PermissionRequest
	PermissionDecision = acpruntime.PermissionDecision
)

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
	SystemPromptsDir string

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
}

// New constructs a Manager: it dials OpenRouter (via the model resolver),
// connects every enabled MCP server in ServerSpecs (credentialed host-side),
// registers the native tool set, and builds the per-turn sandbox warm pool.
// No language model is preloaded — each turn's model is resolved lazily from the
// slug the frontend sends.
func New(opts ManagerOptions) (*Manager, error) {
	cfg := opts.Config
	if cfg == nil {
		return nil, fmt.Errorf("config required")
	}

	resolver, err := agentcore.NewModelResolver(cfg.OpenRouterAPIKey, agentcore.DefaultProviderHeaders)
	if err != nil {
		return nil, err
	}

	client := mcp.NewClient()
	allow := mcpAllowlist{}
	optional := mcpOptionalSet{}
	for name, spec := range opts.ServerSpecs {
		if !spec.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var addErr error
		switch {
		case spec.URL != "":
			addErr = client.AddHTTPServerWithHeaders(ctx, name, spec.URL, spec.Headers)
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
		if len(spec.ToolAllowlist) > 0 {
			allow[name] = spec.ToolAllowlist
		}
		if spec.Optional {
			optional[name] = true
		}
		log.Printf("MCP %s connected (%d tools available, optional=%v)", name, len(client.GetAllTools()), spec.Optional)
	}

	pool, err := buildSandboxPool(cfg, opts.PersonasDir, opts.ProtocolsDir, opts.SystemPromptsDir)
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
		systemPromptsDir:     opts.SystemPromptsDir,
		chatSystemPromptFile: chatPromptFile,
	}
	m.mcpToolRoster = m.computeMCPToolRoster()
	m.optionalServerMetadata = m.buildOptionalServerMetadata(opts.ServerSpecs)
	return m, nil
}

// buildSandboxPool constructs the per-turn container warm pool from config,
// mirroring chat's New() wiring: container mode in production (an image is
// mandatory — bash/run_python only run inside per-turn containers), a no-op
// host-mode stub in mock mode.
func buildSandboxPool(cfg *config.Config, personasDir, protocolsDir, systemPromptsDir string) (*sandbox.Pool, error) {
	poolCfg := sandbox.PoolConfig{
		Size:         2,
		Mode:         sandbox.ModeContainer,
		BridgeScript: tools.PythonBridgeScript(),
	}
	if cfg.MockMode {
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

	poolCfg.Container = sandbox.ContainerConfig{
		Image:            cfg.SandboxImage,
		WorkspaceHostDir: workspaceRoot,
		Runtime:          cfg.SandboxRuntime,
		BridgeDir:        filepath.Join(filepath.Dir(workspaceRoot), "data", "sandbox-bridge"),
		ReadOnlyMounts:   absSupportingDocs(personasDir, protocolsDir, systemPromptsDir, uploadsRoot),
	}
	log.Printf("sandbox: container mode, image=%s, pool=%d, workspace=%s, runtime=%s",
		poolCfg.Container.Image, poolCfg.Size, poolCfg.Container.WorkspaceHostDir, defaultIfEmpty(poolCfg.Container.Runtime, "podman default"))
	return sandbox.NewPool(poolCfg), nil
}

// absSupportingDocs absolutizes the persona/protocol/system-prompt dirs (plus
// the uploads root) and drops empties so they can be passed as
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
// list of the tool names that survive the per-server allowlist filter.
func (m *Manager) computeMCPToolRoster() []string {
	if m.mcpClient == nil {
		return nil
	}
	all := m.mcpClient.GetAllTools()
	names := make([]string, 0, len(all))
	for _, st := range all {
		if list, ok := m.allowlist[st.ServerName]; ok && len(list) > 0 {
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

// takeTurnSandbox pulls a per-turn sandbox: a no-network locked-down container
// for lockdown chats, else a warm-pool container.
func (m *Manager) takeTurnSandbox(ctx context.Context, lockdown bool) (*sandbox.Sandbox, func(), error) {
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
	sb, cleanup, err := m.sandboxPool.Take()
	if err != nil {
		return nil, nil, fmt.Errorf("take sandbox: %w", err)
	}
	return sb, cleanup, nil
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

	systemPrompt, err := m.buildSystemPrompt(persona, in.ConversationID, in.Memories, notes, in.OptionalMCPServersEnabled)
	if err != nil {
		return nil, fmt.Errorf("compose system prompt: %w", err)
	}

	sb, sbCleanup, err := m.takeTurnSandbox(ctx, in.Lockdown)
	if err != nil {
		return nil, err
	}
	defer sbCleanup()
	turnTools := tools.NewTurnTools(sb)

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

	flavor := m.resolveRuntime(in.Runtime)

	tc := TurnConfig{
		SystemPrompt:     systemPrompt,
		Messages:         messages,
		Label:            in.ConversationID,
		Model:            model,
		Temperature:      m.config.Temperature,
		MaxTokens:        maxTokens,
		MaxIterations:    m.config.MaxIterations,
		PriorHistory:     in.History,
		NativeTools:      turnTools.Tools,
		Sandbox:          sb,
		MCPClient:        m.mcpClient,
		Allowlist:        agentcore.MCPAllowlist(m.allowlist),
		OptionalServers:  agentcore.MCPOptionalSet(m.optionalServers),
		Selection:        selection,
		MaxCostUSD:       m.config.MaxCostUSD,
		MaxTotalTokens:   m.config.MaxTotalTokens,
		ApprovalStager:   in.ApprovalStager,
		MemoryProposer:   in.MemoryProposer,
		NoteProposer:     m.noteProposer,
		Runtime:          flavor.Name,
		RuntimeFlavor:    flavor,
		NativeAgentImage: m.nativeAgentImage,
		Lockdown:         in.Lockdown,
		PermissionBroker: in.PermissionBroker,
	}

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
	if ctxErr != nil {
		reason = ctxErr.Error()
	}
	sink.Emit("turn.cancelled", map[string]any{
		"reason":                  reason,
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
