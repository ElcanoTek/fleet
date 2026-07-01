package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// personaLabel normalizes a scheduled persona reference (which may be a
// personas/<name>.yaml filename) to its bare basename for the
// persona_tool_blocked audit-event label (#294). It is cosmetic; the actual
// policy was already resolved against the manifest by the driver.
func personaLabel(persona string) string {
	base := filepath.Base(strings.TrimSpace(persona))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// The SCHEDULED driver: one-shot run-to-completion over the unified
// agentcore.Run loop. cutlass's Execute is reconstructed here as a Mode=Scheduled
// call into agentcore.Run, supplying:
//
//   - an InputSource over a one-shot task + persona (no live history);
//   - an Observer that writes the JSON session log;
//   - a ScheduledPolicy (confirm_audit gating + finish enforcement + verifier),
//     which loops until the audit clears AND the end-of-run verifier reports no
//     missing deliverables — the verifier gate is layered onto the policy's
//     CanFinish;
//   - the SINGLE sandbox Executor;
//   - load-on-demand MCP loader tools whose dirty-rebuild is driven through
//     agentcore.Run's MCPServersDirty/ClearMCPDirty hooks.
//
// The shared enforcement loop, resilience, cache, orchestration, audit, and MCP
// wrapping all live in agentcore and are REUSED — this file only assembles the
// scheduled seams.

// Agent is the scheduled driver handle. It owns the per-run model handles, the
// MCP client, the native tools, and the session log; Execute drives one task to
// completion through agentcore.Run.
type Agent struct {
	config        *config.Config
	model         fantasy.LanguageModel
	fallbackModel fantasy.LanguageModel
	mcpClient     *mcp.Client
	overlay       *RemoteMCPOverlay
	nativeTools   []fantasy.AgentTool
	systemPrompt  string
	persona       string
	maxIterations int
	logSession    *LogSession
	sb            *sandbox.Sandbox
	notesProvider agentcore.NotesProvider
	noteProposer  agentcore.NoteProposer

	// ── agent self-improvement (#285), gated by the per-task Captain's Log opt-in
	// (instruction_self_improve). The DRIVER (scheduledrun) leaves these nil unless
	// the task opted in, so config/default behaviour is unchanged. ──
	//
	// taskMemory + taskID + taskMemoryConfig back the remember/recall tools and the
	// run-start memory injection (#198).
	learnedInstruction string
	taskMemory         tools.TaskMemoryStore
	taskID             uuid.UUID
	taskMemoryConfig   tools.TaskMemoryConfig

	// phoneAFriendEnabled gates the one-time "phone a friend" super-LLM review
	// (part of #175). OFF by default — config/default behaviour is unchanged.
	// reviewerModel is the (typically stronger) reviewer used by that review; it
	// is host-side, like every other model handle, and never enters the sandbox
	// or the agent's model context. When the flag is on but reviewerModel is nil,
	// the review degrades to a no-op (it requires a reviewer model to run).
	phoneAFriendEnabled bool
	reviewerModel       fantasy.LanguageModel

	// ── sub-agents (#175, part b) ──
	// subagent carries the spawn_subagent feature gate, recursion/fan-out caps,
	// per-child model resolution, depth-in-tree, and the live fan-out counter for
	// THIS run. OFF by default. See subagent.go. The live parent policy
	// (runtimePolicy) is captured in Execute so the spawn tool can read the
	// parent's remaining budget and charge child spend back against it.
	subagent      subagentConfig
	runtimePolicy *agentcore.ScheduledPolicy

	// budgetOverride, when set (>0), forces this run's cost/token ceilings instead
	// of the config defaults. A spawned CHILD sets these to its SLICED budget so
	// its own agentcore.Run enforces the sliced ceiling via the SAME checkCeilings
	// the parent uses (#175). Zero leaves the config-derived ceiling in place.
	costCeilingOverride  float64
	tokenCeilingOverride int

	// loadedServers is the set of MCP servers whose tools are currently
	// registered. mcpServersDirty signals the loop to rebuild the tool list
	// after a mcp_load_servers call registered new servers.
	mu              sync.Mutex
	loadedServers   map[string]bool
	mcpServersDirty bool

	// logFile is where the session log is persisted at run end ("" = default).
	logFile string

	// credentialAllowlist scopes which (server, account) MCP pairs this task may
	// call (Gate-3, #184). nil = inherit global; threaded into RunConfig.
	credentialAllowlist agentcore.CredentialAllowlist

	// personaPolicy is the per-persona tool allowlist (Gate-4, #294). nil = no
	// narrowing (the persona sees every tool the earlier gates permit); threaded
	// into RunConfig so denied tools never enter the model's tool list.
	personaPolicy *agentcore.PersonaToolPermissions
}

// Options configure a scheduled Agent.
type Options struct {
	Config        *config.Config
	Model         fantasy.LanguageModel
	FallbackModel fantasy.LanguageModel
	MCPClient     *mcp.Client
	NativeTools   []fantasy.AgentTool
	SystemPrompt  string
	Persona       string
	MaxIterations int
	Sandbox       *sandbox.Sandbox
	LogFile       string

	// NotesProvider supplies the admin-curated knowledge base appended to the
	// system prompt at run start (both modes inject the same notes). Nil = none.
	NotesProvider agentcore.NotesProvider
	// NoteProposer stages agent-proposed note edits (propose_note). Nil leaves
	// the tool reporting "not wired".
	NoteProposer agentcore.NoteProposer

	// ── agent self-improvement (#285) — set by the driver ONLY when the task
	// opted into Captain's Log (instruction_self_improve); nil otherwise. ──
	//
	// TaskMemory + TaskID enable the remember/recall tools and the run-start
	// memory injection (#198); TaskMemoryConfig caps how much a task accumulates.
	// Wired by the driver only when the task opted into Captain's Log.
	TaskMemory       tools.TaskMemoryStore
	TaskID           uuid.UUID
	TaskMemoryConfig tools.TaskMemoryConfig

	// LearnedInstruction is the task's ACTIVE distilled instruction (#516),
	// resolved by the driver from feedback and injected at run-start. Empty =
	// none (the default; a task with no activated instruction is byte-for-byte
	// unchanged).
	LearnedInstruction string

	// CredentialAllowlist scopes which (server, account) MCP pairs this task may
	// call (Gate-3, #184). nil = inherit global. Threaded into RunConfig so the
	// run loop denies any pair not on the list before the call is dispatched.
	CredentialAllowlist agentcore.CredentialAllowlist

	// PersonaPolicy is the per-persona tool allowlist (Gate-4, #294) for this
	// task's resolved persona. nil = no narrowing (current behavior). The DRIVER
	// (scheduledrun) resolves it from the bundle manifest's personas: block for
	// the task's effective persona and threads it into RunConfig.
	PersonaPolicy *agentcore.PersonaToolPermissions

	// Overlay is the per-user remote-MCP overlay (#443): the task owner's
	// OAuth-connected hosted servers, wired via the same compositeBroker the
	// interactive path uses. nil = no overlay. The DRIVER (scheduledrun) builds
	// and closes it.
	Overlay *RemoteMCPOverlay

	// PhoneAFriendEnabled turns on the one-time "phone a friend" super-LLM review
	// (part of #175). OFF by default so config/default behaviour is unchanged.
	// ReviewerModel is the resolved (typically stronger) reviewer model that
	// review uses; like every model handle it is host-side. The review is skipped
	// when the flag is off OR no reviewer model is supplied.
	PhoneAFriendEnabled bool
	ReviewerModel       fantasy.LanguageModel

	// ── sub-agents (#175, part b) ──
	// Subagent configures the spawn_subagent native tool. OFF by default
	// (Subagent.Enabled=false) so config/default behaviour is unchanged. See
	// subagentConfig / subagent.go for the governance properties (monotonic
	// privilege, budget split, depth/fan-out caps). The DRIVER (scheduledrun)
	// builds this from config + the Manager's model resolver + sandbox.
	Subagent SubagentOptions
}

// SubagentOptions is the spawn_subagent feature configuration the driver supplies
// (#175, part b). It is OFF unless Enabled is set. The child run inherits the
// parent's sandbox, MCP client, and allowlists from the parent Agent itself; this
// struct carries only the policy knobs and the host-side model resolver a child
// needs.
type SubagentOptions struct {
	// Enabled gates the whole feature (FLEET_SUBAGENTS_ENABLED). When false the
	// spawn_subagent tool is not registered at all.
	Enabled bool
	// MaxDepth caps recursion depth (root run = depth 0; a spawn at MaxDepth is
	// refused). MaxChildren caps fan-out per parent. Both <=0 fall back to the
	// package defaults so a misconfiguration can never mean "unbounded". The
	// default MaxDepth is 1 (#264): a child does not get the spawn tool registered
	// at all (see buildChild), so "parent → sub-agent only".
	MaxDepth    int
	MaxChildren int
	// BudgetFraction is each child's default/maximum slice of the parent's
	// REMAINING budget (#264). A child requesting more than this fraction is refused
	// with a tool-result error. <=0 falls back to the package default (0.10); >1 is
	// clamped to 1.0 (the whole remaining budget).
	BudgetFraction float64
	// ModelSlug is the default child model slug (FLEET_SUBAGENTS_MODEL); empty
	// means a child inherits the parent's model. A per-spawn override is resolved
	// through Resolver too.
	ModelSlug string
	// Resolver resolves a child model slug to a host-side LanguageModel — the SAME
	// cached resolver the parent's model came from, so a per-child model choice is
	// resolved host-side exactly like the phone-a-friend reviewer and credentials
	// never enter the sandbox or model context. Nil disables per-child model
	// override (the child always inherits the parent's model handle).
	Resolver ModelResolver
}

// ModelResolver resolves an OpenRouter model slug to a host-side LanguageModel.
// *agent.Manager satisfies it via Resolve. Narrow seam so the spawn tool can
// resolve a per-child model without depending on the whole Manager.
type ModelResolver interface {
	Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error)
}

// NewAgent builds a scheduled driver from options. The session log is fresh.
func NewAgent(opts Options) *Agent {
	maxIter := opts.MaxIterations
	if maxIter <= 0 && opts.Config != nil {
		maxIter = opts.Config.LiveMaxIterations()
	}
	if maxIter <= 0 {
		maxIter = 500
	}
	a := &Agent{
		config:              opts.Config,
		model:               opts.Model,
		fallbackModel:       opts.FallbackModel,
		mcpClient:           opts.MCPClient,
		overlay:             opts.Overlay,
		nativeTools:         opts.NativeTools,
		systemPrompt:        opts.SystemPrompt,
		persona:             opts.Persona,
		maxIterations:       maxIter,
		logSession:          NewLogSession(),
		sb:                  opts.Sandbox,
		loadedServers:       make(map[string]bool),
		logFile:             opts.LogFile,
		notesProvider:       opts.NotesProvider,
		noteProposer:        opts.NoteProposer,
		taskMemory:          opts.TaskMemory,
		learnedInstruction:  opts.LearnedInstruction,
		taskID:              opts.TaskID,
		taskMemoryConfig:    opts.TaskMemoryConfig,
		credentialAllowlist: opts.CredentialAllowlist,
		personaPolicy:       opts.PersonaPolicy,
		phoneAFriendEnabled: opts.PhoneAFriendEnabled,
		reviewerModel:       opts.ReviewerModel,
		subagent:            newSubagentConfig(opts.Subagent),
	}
	// The parent task id labels any sub-agent this run spawns (#264 traceability).
	// A child inherits this same value (buildChild), so every descendant's session
	// log points back to the one owning task.
	if opts.TaskID != uuid.Nil {
		a.subagent.parentTaskID = opts.TaskID.String()
	}
	return a
}

// LogSession exposes the run's session log.
func (a *Agent) LogSession() *LogSession { return a.logSession }

// scheduledInput is the one-shot InputSource: the task text + persona-derived
// system prompt, no live history.
type scheduledInput struct {
	systemPrompt string
	task         string
	label        string
}

func (s scheduledInput) Prompt(_ context.Context) (string, []fantasy.Message, string, error) {
	return s.systemPrompt, []fantasy.Message{fantasy.NewUserMessage(s.task)}, s.label, nil
}

// scheduledObserver writes run events into the JSON session log. Text
// deltas accumulate into the assistant message at round end; tool calls/results
// and enforcement nudges are appended as structured LogMessages.
type scheduledObserver struct {
	session *LogSession
}

func (o *scheduledObserver) Observe(eventType string, payload map[string]any) {
	if o.session == nil {
		return
	}
	switch eventType {
	case "enforcement":
		if msg, ok := payload["message"].(string); ok {
			t := "system_enforcement"
			o.session.AddMessageWithMetadata(roleUser, msg, nil, nil, &t, nil, nil, "")
		}
	case "text":
		if msg, ok := payload["text"].(string); ok && msg != "" {
			o.session.AddMessage(roleAssistant, msg, nil, nil)
		}
	}
}

// multiObserver fans a single Observe to every wrapped Observer in order. It is
// the seam that lets a scheduled run's events reach BOTH the captain's-log writer
// (scheduledObserver) AND an optional live SSE buffer attached by the worker pool
// via agentcore.WithStreamObserver (#200) — the live stream consumes the exact
// same event stream the persisted log does, with no second governance path. A
// nil member is skipped so callers can compose without nil-checking.
type multiObserver []agentcore.Observer

func (m multiObserver) Observe(eventType string, payload map[string]any) {
	for _, o := range m {
		if o != nil {
			o.Observe(eventType, payload)
		}
	}
}

// composeObserver builds the run's Observer: always the captain's-log writer,
// plus the optional context-carried stream sink (#200). When no stream sink is
// present it returns the bare scheduledObserver so the common path is unchanged.
func composeObserver(ctx context.Context, base agentcore.Observer) agentcore.Observer {
	if stream := agentcore.StreamObserverFromContext(ctx); stream != nil {
		return multiObserver{base, stream}
	}
	return base
}

// scheduledPolicy layers two host-side finish gates onto agentcore.ScheduledPolicy,
// in order: the end-of-run verifier, then the "phone a friend" super-LLM review
// (part of #175). agentcore's audit/finish enforcement gates finishing first;
// once those clear, the verifier runs ONCE and any missing deliverables it
// reports are injected as one more enforcement round; once THAT clears, the
// phone-a-friend review (when enabled) runs ONCE and any material issues it
// reports are injected as one more enforcement round. Each gate flips a "done"
// flag after running so it never re-gates — the loop always terminates. Both are
// the same shape: a one-time host-side LLM re-check feeding back through the SAME
// CanFinish enforcement-round channel, so no second governance path is created.
type scheduledPolicy struct {
	inner    *agentcore.ScheduledPolicy
	agent    *Agent
	task     string
	verified bool
	reviewed bool
	// runCtx is the run's context, captured at build time so the end-of-run
	// verifier's and phone-a-friend reviewer's model calls honor the run's
	// deadline/cancellation (CanFinish itself takes no ctx). Falls back to
	// context.Background() if unset.
	runCtx context.Context
}

func (p *scheduledPolicy) BeforeToolCall(toolName, toolCallID, rawInput string) (bool, string) {
	return p.inner.BeforeToolCall(toolName, toolCallID, rawInput)
}

func (p *scheduledPolicy) RecordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	p.inner.RecordToolResult(toolName, rawInput, resultText, succeeded)
}

// CanFinish first defers to the audit/finish enforcement. When that clears, it
// runs the end-of-run verifier once (missing actions become a final enforcement
// round) and then the phone-a-friend review once (material issues become a final
// enforcement round). Each gate requires its model — the verifier the fallback
// model, the review the reviewer model — and is skipped when its model is absent;
// the review gate is additionally skipped unless phoneAFriendEnabled.
func (p *scheduledPolicy) CanFinish(round int) (bool, []string) {
	if ok, msgs := p.inner.CanFinish(round); !ok {
		return false, msgs
	}
	ctx := p.runCtx
	if ctx == nil {
		ctx = context.Background()
	}

	// Gate 1: end-of-run verifier (completeness re-check).
	if !p.verified && p.agent != nil && p.agent.fallbackModel != nil {
		p.verified = true
		records := buildToolExecSummary(p.agent.logSession)
		missing, err := p.agent.runEndOfRunVerifier(ctx, p.task, records)
		if err != nil {
			log.Printf("verifier skipped: %v", err)
		} else if len(missing) > 0 {
			return false, []string{fmt.Sprintf(
				"End-of-run verification found unfinished required actions: %v. "+
					"Complete each one now, or call confirm_audit(success=false, user_visible_summary=...) to abort explicitly.",
				missing)}
		}
	}

	// Gate 2: phone-a-friend super-LLM review (quality re-check, part of #175).
	// Runs only after the verifier gate has cleared, so the reviewer critiques a
	// run that already attempted everything the task required. OFF unless both the
	// feature flag is set and a reviewer model is configured.
	if !p.reviewed && p.agent != nil && p.agent.phoneAFriendEnabled && p.agent.reviewerModel != nil {
		p.reviewed = true
		records := buildToolExecSummary(p.agent.logSession)
		answer := latestAssistantText(p.agent.logSession)
		issues, err := p.agent.runPhoneAFriendReview(ctx, p.agent.reviewerModel, p.task, answer, records)
		if err != nil {
			log.Printf("phone_a_friend review skipped: %v", err)
		} else if len(issues) > 0 {
			return false, []string{fmt.Sprintf(
				"A reviewer model (phone a friend) found problems with the current answer/work that must be "+
					"addressed before finishing: %v. Revise the work to fix each one, or call "+
					"confirm_audit(success=false, user_visible_summary=...) to abort explicitly.",
				issues)}
		}
	}

	return true, nil
}

// Unwrap exposes the inner ScheduledPolicy so agentcore's loop can reach the
// orchestration state (usage accounting) and bind the confirm_audit tool —
// without the wrapper having to expose agentcore's unexported state type.
func (p *scheduledPolicy) Unwrap() agentcore.Policy { return p.inner }

// Execute drives one scheduled task to completion through agentcore.Run. The
// session log is persisted on every exit path.
func (a *Agent) Execute(ctx context.Context, task string) (retErr error) {
	defer writeLogFile(a.logSession, a.logFile)
	defer func() {
		if retErr != nil {
			t := "error"
			a.logSession.AddMessageWithMetadata(roleUser, "[fatal] "+retErr.Error(), nil, nil, &t, nil, nil, "")
			log.Printf("Execute returning error: %v", retErr)
		}
	}()

	a.logSession.AddMessage(roleUser, task, nil, nil)

	// The native loop drives the LLM through fleet's resolved OpenRouter model;
	// without one there is nothing to run.
	if a.model == nil {
		return fmt.Errorf("no language model configured — set OPENROUTER_API_KEY")
	}

	// Inject the admin-curated knowledge base into the system prompt (both modes
	// inject the same notes; here we append to the scheduled base prompt before
	// the run). Failure to read notes is non-fatal — the run proceeds without
	// the notes section rather than aborting.
	systemPrompt := a.systemPrompt
	if a.notesProvider != nil {
		if notes, err := a.notesProvider.PublishedNotes(ctx); err != nil {
			log.Printf("agent notes unavailable; running without notes section: %v", err)
		} else {
			systemPrompt = appendScheduledAgentNotes(systemPrompt, notes)
		}
	}

	// Captain's Log (#285) prompt section, in lockstep with the remember/recall
	// tools wired below and gated by the driver opting the task in. Persistent
	// memory from prior runs is injected here (the section also advertises the
	// remember/recall tools). A read failure is non-fatal — the run proceeds
	// without the section.
	if a.taskMemory != nil {
		mems, err := a.taskMemory.ListTaskMemories(ctx, a.taskID)
		if err != nil {
			log.Printf("task memory unavailable; running without the persistent-memory section: %v", err)
			mems = nil
		}
		systemPrompt = appendTaskMemorySection(systemPrompt, mems)
	}

	// Learned instruction (#516): the human-activated, distilled-from-feedback
	// standing instruction for this task. Injected as its own section so a run
	// visibly follows what prior feedback taught, and revertible (deactivating
	// it removes the section on the next run).
	if strings.TrimSpace(a.learnedInstruction) != "" {
		systemPrompt = appendLearnedInstructionSection(systemPrompt, a.learnedInstruction)
	}

	// Scheduled policy: audit gating + finish enforcement (agentcore) + verifier.
	var maxCostUSD float64
	var maxTotalTokens int
	if a.config != nil {
		maxCostUSD = a.config.LiveMaxCostUSD()
		maxTotalTokens = a.config.LiveMaxTotalTokens()
	}
	// A spawned child carries a SLICED budget (#175): its override REPLACES the
	// config ceiling so the child's own agentcore.Run enforces the slice through
	// the SAME checkCeilings the parent uses — the child cannot outspend its slice,
	// and (because its spend is charged back to the parent) the collective spend of
	// all children cannot breach the parent ceiling.
	if a.costCeilingOverride > 0 {
		maxCostUSD = a.costCeilingOverride
	}
	if a.tokenCeilingOverride > 0 {
		maxTotalTokens = a.tokenCeilingOverride
	}
	inner := agentcore.NewScheduledPolicy(a.logSession, a.maxIterations, maxCostUSD, maxTotalTokens)
	if a.noteProposer != nil {
		inner.SetNoteProposer(a.noteProposer)
	}
	// Capture the live policy so the spawn_subagent tool can read THIS run's
	// remaining budget and charge child spend back against it (#175). It is the
	// SAME ScheduledPolicy agentcore drives, so the budget the tool reads is the
	// budget the loop enforces — there is no separate accounting.
	a.runtimePolicy = inner
	policy := &scheduledPolicy{inner: inner, agent: a, task: task, runCtx: ctx}

	// propose_note tool registration in lockstep with wiring + the prompt
	// advertisement: the scheduled prompt advertises propose_note and the policy
	// wires the proposer above, so the tool must actually be in the roster when a
	// proposer is present (the base NewTurnTools set does not include it).
	nativeTools := a.nativeTools
	if a.noteProposer != nil {
		nativeTools = append(append([]fantasy.AgentTool{}, nativeTools...), tools.NewProposeNoteTool())
	}

	// Captain's Log persistent memory (#198, #285): the remember/recall tools are
	// registered in lockstep with their prompt advertisement and ONLY when the
	// driver opted the task in (it leaves taskMemory nil otherwise, so the default
	// roster is unchanged). propose_note above stays unconditional (gated only on
	// its proposer) to preserve pre-#285 scheduled behaviour.
	if a.taskMemory != nil {
		nativeTools = append(append([]fantasy.AgentTool{}, nativeTools...),
			tools.NewRememberTool(a.taskMemory, a.taskID, a.taskMemoryConfig),
			tools.NewRecallTool(a.taskMemory, a.taskID))
	}

	// spawn_subagent (#175, part b): register the tool ONLY when the feature is
	// enabled, so config/default behaviour is unchanged. The tool body adapts I/O
	// around a CHILD agentcore.Run (a fresh agent.Agent.Execute) — no second
	// governance path. See subagent.go for the monotonic-privilege + budget-split
	// + depth/fan-out enforcement.
	if a.subagent.enabled {
		nativeTools = append(append([]fantasy.AgentTool{}, nativeTools...), a.newSpawnSubagentTool())
	}

	// Loader tools (mcp_list_servers / mcp_load_servers) drive the in-loop tool
	// rebuild via the agentcore MCPServersDirty hook.
	loaderTools := a.buildLoaderTools()

	maxTokens := agentcore.DefaultMaxCompletionTokens
	if a.config != nil && a.config.LLMMaxTokens > 0 {
		maxTokens = a.config.LLMMaxTokens
	}
	temp := 0.3
	if a.config != nil {
		temp = a.config.LiveLLMTemperature()
	}

	allow, optional := a.mcpGates()

	deps := agentcore.Deps{
		Input: scheduledInput{systemPrompt: systemPrompt, task: task, label: a.logSession.Title},
		// Observer is the captain's-log writer, tee'd to a live SSE buffer when the
		// worker pool attached one via agentcore.WithStreamObserver (#200) so an
		// in-progress task's run log can be tailed without forking the event path.
		Observer:        composeObserver(ctx, &scheduledObserver{session: a.logSession}),
		Policy:          inner, // inner policy exposes orchestration() for confirm_audit + usage
		Executor:        NewSandboxExecutor(a.sb),
		Model:           a.model,
		FallbackModel:   a.fallbackModel,
		MCPClient:       a.mcpClient,
		LogSession:      a.logSession,
		MCPServersDirty: a.mcpDirty,
		ClearMCPDirty:   a.clearMCPDirty,
	}
	// Per-user remote-MCP overlay (#443): wire the task owner's OAuth-connected
	// hosted servers via the SAME compositeBroker the interactive path uses, so a
	// headless run reaches them without mutating the shared/per-run bundle client.
	ApplyMCPOverlay(&deps, a.mcpClient, a.overlay)
	// The verifier gate wraps the inner policy. We must drive CanFinish through
	// the wrapper while keeping the inner policy as Deps.Policy so agentcore's
	// confirm_audit tool + usage accounting bind to the same orchestration. We
	// achieve that by giving the wrapper the same orchestration: agentcore reads
	// CanFinish off Deps.Policy, so we instead set Deps.Policy to the wrapper and
	// rely on the wrapper delegating BeforeToolCall/RecordToolResult to inner.
	deps.Policy = policy

	cfg := agentcore.RunConfig{
		EnvPrefix:           agentcore.CanonicalEnvPrefix,
		Temperature:         temp,
		MaxCompletionTokens: maxTokens,
		MaxIterations:       a.maxIterations,
		Allowlist:           allow,
		OptionalServers:     optional,
		Selection:           a.selection(),
		IncludeConfirmAudit: true,
		// Unattended runs gate proactive context compaction behind
		// FLEET_SCHEDULED_AUTO_COMPACT (#209): without it, pressure only warns
		// rather than silently rewriting the transcript.
		RequireCompactionOptIn: true,
		// Per-task credential allowlist (#184): scope which (server, account) MCP
		// pairs this task may call. nil = inherit global.
		CredentialAllowlist: a.credentialAllowlist,
		// Per-persona tool allowlist (#294): NARROW the registered roster to the
		// task persona's permitted tools. nil = no narrowing.
		PersonaName:     personaLabel(a.persona),
		PersonaPolicy:   a.personaPolicy,
		LoaderTools:     loaderTools,
		NativeTools:     nativeTools,
		ProviderHeaders: agentcore.DefaultProviderHeaders,
		// Extended thinking (#220) for scheduled runs is driven by the GLOBAL
		// default only (FLEET_DEFAULT_THINKING_BUDGET_TOKENS). A per-task override
		// column is a documented follow-on; the global default uniformly enables
		// thinking for every scheduled run when an operator turns it on. nil config
		// (some test setups) → no default → thinking off.
		ThinkingConfig: scheduledThinkingConfig(a.config),
	}

	res, err := agentcore.Run(ctx, agentcore.ModeScheduled, cfg, deps)
	if err != nil {
		return err
	}
	if res.FinalText != "" {
		a.logSession.AddMessage(roleAssistant, res.FinalText, nil, nil)
	}
	return nil
}

// scheduledThinkingConfig resolves the extended-thinking config for a scheduled
// run from the global default (#220). nil config or a zero/unset default returns
// nil (thinking off). Per-task overrides are a documented follow-on.
func scheduledThinkingConfig(cfg *config.Config) *agentcore.ThinkingConfig {
	if cfg == nil {
		return nil
	}
	return agentcore.ThinkingConfigForBudget(cfg.DefaultThinkingBudgetTokens)
}

func (a *Agent) mcpDirty() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mcpServersDirty
}

func (a *Agent) clearMCPDirty() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mcpServersDirty = false
}

// mcpGates derives the per-server allowlist + the optional-server set from the
// scheduled config's MCPServers catalog. Servers with a non-empty ToolAllowlist
// constrain which tools register; every credentialed server is optional (the
// load-on-demand model — only loaded servers' tools register).
func (a *Agent) mcpGates() (agentcore.MCPAllowlist, agentcore.MCPOptionalSet) {
	allow := agentcore.MCPAllowlist{}
	optional := agentcore.MCPOptionalSet{}
	if a.config == nil {
		return allow, optional
	}
	for name, sc := range a.config.MCPServers {
		if len(sc.ToolAllowlist) > 0 {
			allow[name] = sc.ToolAllowlist
		}
	}
	return allow, optional
}

// selection returns the per-run MCP selection. For the scheduled driver this is
// derived from the loaded-server set; the caller binds the actual subprocesses
// via agentcore.BindMCPSelection before Execute.
func (a *Agent) selection() agentcore.MCPSelection {
	a.mu.Lock()
	defer a.mu.Unlock()
	sel := make(agentcore.MCPSelection, 0, len(a.loadedServers))
	for name := range a.loadedServers {
		sel = append(sel, agentcore.MCPChoice{Server: name})
	}
	return sel
}

// appendScheduledAgentNotes appends the admin-notes section + the propose_note
// tool block to the scheduled base prompt, reusing the SAME helpers the
// interactive prompt uses (so both modes inject identical text). Notes render
// after the base prompt; scheduled runs have no user-memories block.
func appendScheduledAgentNotes(base string, notes []agentcore.Note) string {
	var sb strings.Builder
	sb.WriteString(base)
	if !strings.HasSuffix(base, "\n\n") {
		sb.WriteString("\n\n")
	}
	appendAgentNotes(&sb, notes)
	appendNoteProposalTool(&sb)
	return sb.String()
}

// appendTaskMemorySection appends the "Your Persistent Memory" block (#198) to
// the scheduled base prompt: the facts saved by prior runs of this task, plus a
// short note about the remember/recall tools. When the task has no memories yet
// (first run) the facts list is omitted but the tool note still renders, so the
// agent knows the capability exists.
func appendTaskMemorySection(base string, mems []tools.TaskMemory) string {
	var sb strings.Builder
	sb.WriteString(base)
	if !strings.HasSuffix(base, "\n\n") {
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Your Persistent Memory\n\n")
	sb.WriteString("This is YOUR memory across runs of this scheduled task. Facts you save with " +
		"`remember` are reloaded here at the start of every future run; use `recall` to re-read them " +
		"mid-run. Use this to track state across time (e.g. the last value you saw, items already " +
		"processed). Do NOT store secrets or credentials.\n\n")
	if len(mems) == 0 {
		sb.WriteString("_No facts saved yet — this is the first run, or memory was cleared._\n\n")
		return sb.String()
	}
	sb.WriteString("Facts saved by previous runs:\n\n")
	for _, m := range mems {
		fmt.Fprintf(&sb, "- **%s**: %s\n", m.Key, strings.TrimSpace(m.Value))
	}
	sb.WriteString("\n")
	return sb.String()
}

// appendLearnedInstructionSection appends the task's active learned instruction
// (#516) — distilled from user feedback and human-activated — to the scheduled
// base prompt. It is deliberately framed as a standing directive the run must
// follow, distinct from persistent memory (facts) and admin notes (knowledge).
func appendLearnedInstructionSection(base, instruction string) string {
	var sb strings.Builder
	sb.WriteString(base)
	if !strings.HasSuffix(base, "\n\n") {
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Learned Instruction\n\n")
	sb.WriteString("Prior runs of this task received user feedback that distilled into the standing " +
		"instruction below. Follow it unless it conflicts with the task's explicit request:\n\n")
	sb.WriteString(strings.TrimSpace(instruction))
	sb.WriteString("\n\n")
	return sb.String()
}

// markServerLoaded records a server as loaded and flags a rebuild.
func (a *Agent) markServerLoaded(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loadedServers[name] {
		a.loadedServers[name] = true
		a.mcpServersDirty = true
	}
}
