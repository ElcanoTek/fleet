package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

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
	nativeTools   []fantasy.AgentTool
	systemPrompt  string
	persona       string
	maxIterations int
	logSession    *LogSession
	sb            *sandbox.Sandbox
	notesProvider agentcore.NotesProvider
	noteProposer  agentcore.NoteProposer

	// phoneAFriendEnabled gates the one-time "phone a friend" super-LLM review
	// (part of #175). OFF by default — config/default behaviour is unchanged.
	// reviewerModel is the (typically stronger) reviewer used by that review; it
	// is host-side, like every other model handle, and never enters the sandbox
	// or the agent's model context. When the flag is on but reviewerModel is nil,
	// the review degrades to a no-op (it requires a reviewer model to run).
	phoneAFriendEnabled bool
	reviewerModel       fantasy.LanguageModel

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

	// CredentialAllowlist scopes which (server, account) MCP pairs this task may
	// call (Gate-3, #184). nil = inherit global. Threaded into RunConfig so the
	// run loop denies any pair not on the list before the call is dispatched.
	CredentialAllowlist agentcore.CredentialAllowlist

	// PhoneAFriendEnabled turns on the one-time "phone a friend" super-LLM review
	// (part of #175). OFF by default so config/default behaviour is unchanged.
	// ReviewerModel is the resolved (typically stronger) reviewer model that
	// review uses; like every model handle it is host-side. The review is skipped
	// when the flag is off OR no reviewer model is supplied.
	PhoneAFriendEnabled bool
	ReviewerModel       fantasy.LanguageModel
}

// NewAgent builds a scheduled driver from options. The session log is fresh.
func NewAgent(opts Options) *Agent {
	maxIter := opts.MaxIterations
	if maxIter <= 0 && opts.Config != nil {
		maxIter = opts.Config.MaxIterations
	}
	if maxIter <= 0 {
		maxIter = 500
	}
	return &Agent{
		config:              opts.Config,
		model:               opts.Model,
		fallbackModel:       opts.FallbackModel,
		mcpClient:           opts.MCPClient,
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
		credentialAllowlist: opts.CredentialAllowlist,
		phoneAFriendEnabled: opts.PhoneAFriendEnabled,
		reviewerModel:       opts.ReviewerModel,
	}
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

	// Scheduled policy: audit gating + finish enforcement (agentcore) + verifier.
	var maxCostUSD float64
	var maxTotalTokens int
	if a.config != nil {
		maxCostUSD = a.config.MaxCostUSD
		maxTotalTokens = a.config.MaxTotalTokens
	}
	inner := agentcore.NewScheduledPolicy(a.logSession, a.maxIterations, maxCostUSD, maxTotalTokens)
	if a.noteProposer != nil {
		inner.SetNoteProposer(a.noteProposer)
	}
	policy := &scheduledPolicy{inner: inner, agent: a, task: task, runCtx: ctx}

	// propose_note tool registration in lockstep with wiring + the prompt
	// advertisement: the scheduled prompt advertises propose_note and the policy
	// wires the proposer above, so the tool must actually be in the roster when a
	// proposer is present (the base NewTurnTools set does not include it).
	nativeTools := a.nativeTools
	if a.noteProposer != nil {
		nativeTools = append(append([]fantasy.AgentTool{}, nativeTools...), tools.NewProposeNoteTool())
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
		temp = a.config.LLMTemperature
	}

	allow, optional := a.mcpGates()

	deps := agentcore.Deps{
		Input:           scheduledInput{systemPrompt: systemPrompt, task: task, label: a.logSession.Title},
		Observer:        &scheduledObserver{session: a.logSession},
		Policy:          inner, // inner policy exposes orchestration() for confirm_audit + usage
		Executor:        NewSandboxExecutor(a.sb),
		Model:           a.model,
		FallbackModel:   a.fallbackModel,
		MCPClient:       a.mcpClient,
		LogSession:      a.logSession,
		MCPServersDirty: a.mcpDirty,
		ClearMCPDirty:   a.clearMCPDirty,
	}
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
		LoaderTools:         loaderTools,
		NativeTools:         nativeTools,
		ProviderHeaders:     agentcore.DefaultProviderHeaders,
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

// markServerLoaded records a server as loaded and flags a rebuild.
func (a *Agent) markServerLoaded(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loadedServers[name] {
		a.loadedServers[name] = true
		a.mcpServersDirty = true
	}
}
