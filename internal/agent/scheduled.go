package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
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

	// loadedServers is the set of MCP servers whose tools are currently
	// registered. mcpServersDirty signals the loop to rebuild the tool list
	// after a mcp_load_servers call registered new servers.
	mu              sync.Mutex
	loadedServers   map[string]bool
	mcpServersDirty bool

	// logFile is where the session log is persisted at run end ("" = default).
	logFile string

	// runtime selects the scheduled execution flavor. "" / "native-inprocess"
	// run the in-process loop (default + parity oracle); "native-acp" routes the
	// SAME loop through a sandboxed ACP agent (acpruntime.ClientRuntime) over
	// podman-stdio, with every governed effect (tool exec, MCP, staging, usage)
	// delegated back to the host — governed identically to in-process.
	// nativeAgentImage names the agent image for the native-acp flavor.
	runtime          string
	nativeAgentImage string

	// runtimeFlavor is the resolved clientconfig descriptor for runtime. For an
	// EXTERNAL (type: acp / delegated_policy) flavor it carries the provider
	// image, the model_only egress posture, the delegated-policy bit, and the
	// model-cred env var names the scheduled-external path needs to spawn the
	// provider's agent at the CONTAINMENT tier. Empty Type for the native
	// flavors (which never consult it).
	runtimeFlavor clientconfig.Runtime

	// allowUngovernedScheduled is the per-client opt-in (manifest
	// agent_policy.allow_ungoverned_scheduled_agents, default false) that admits
	// an EXTERNAL flavor as a SCHEDULED task. fleet is FAIL-CLOSED here: when this
	// is false and a scheduled task selects an external flavor, Execute returns a
	// LOUD ERROR at dispatch — NEVER a silent fallback to a native flavor (the
	// INVERSE of the native-acp fallback). See Execute's external gate.
	allowUngovernedScheduled bool

	// newExternalRuntime builds the runtime that drives a scheduled-external turn.
	// Production wires acpruntime.NewExternalRuntime (spawns the provider's agent
	// via podman); tests inject a fake that drives an in-process fake external ACP
	// agent over io.Pipe so the scheduled-external path is exercised with NO
	// podman, NO live key. Nil falls back to the real runtime.
	newExternalRuntime func(acpruntime.ExternalConfig) externalRuntime

	// mcpSelection is the task's declared MCP selection (the {server, account}
	// choices the scheduled runner bound onto a DEDICATED per-task client). It is
	// the authoritative MCP surface for the native-acp path: only these servers are
	// advertised, so a no-selection task gets NO MCP surface (native-acp has no
	// in-loop load-on-demand) rather than inheriting whatever the shared client
	// happens to hold — avoiding cross-task scope creep. Empty/nil = no MCP surface.
	mcpSelection agentcore.MCPSelection

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

	// Runtime selects the scheduled execution flavor ("" / "native-inprocess" =
	// in-process loop; "native-acp" = sandboxed ACP agent, fully governed; an
	// external "acp" flavor = the CONTAINMENT tier, gated by
	// AllowUngovernedScheduled). NativeAgentImage names the agent image for the
	// native-acp flavor; required when Runtime is native-acp.
	Runtime          string
	NativeAgentImage string

	// RuntimeFlavor is the resolved clientconfig descriptor for Runtime. For an
	// EXTERNAL (type: acp) flavor it carries the provider image, args, model_env,
	// and the delegated-policy bit the scheduled-external path needs. The native
	// flavors leave it zero (they never read it).
	RuntimeFlavor clientconfig.Runtime

	// AllowUngovernedScheduled is the per-client opt-in
	// (agent_policy.allow_ungoverned_scheduled_agents, default false) that admits
	// an EXTERNAL flavor as a SCHEDULED task. Off → a scheduled-external task is a
	// LOUD ERROR at dispatch (fail-closed, no fallback). On → the scheduled turn
	// runs at the containment tier (sandbox REQUIRED, governance: delegated,
	// permissions default-DENY).
	AllowUngovernedScheduled bool

	// MCPSelection is the task's declared {server, account} MCP selection. For the
	// native-acp flavor it is the authoritative advertised MCP surface (only these
	// servers); empty = no MCP surface. The in-process flavor ignores it (it uses
	// load-on-demand via the loader tools).
	MCPSelection agentcore.MCPSelection

	// CredentialAllowlist scopes which (server, account) MCP pairs this task may
	// call (Gate-3, #184). nil = inherit global. Threaded into RunConfig so the
	// run loop denies any pair not on the list before the call is dispatched.
	CredentialAllowlist agentcore.CredentialAllowlist
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
		config:                   opts.Config,
		model:                    opts.Model,
		fallbackModel:            opts.FallbackModel,
		mcpClient:                opts.MCPClient,
		nativeTools:              opts.NativeTools,
		systemPrompt:             opts.SystemPrompt,
		persona:                  opts.Persona,
		maxIterations:            maxIter,
		logSession:               NewLogSession(),
		sb:                       opts.Sandbox,
		loadedServers:            make(map[string]bool),
		logFile:                  opts.LogFile,
		notesProvider:            opts.NotesProvider,
		noteProposer:             opts.NoteProposer,
		runtime:                  opts.Runtime,
		nativeAgentImage:         opts.NativeAgentImage,
		runtimeFlavor:            opts.RuntimeFlavor,
		allowUngovernedScheduled: opts.AllowUngovernedScheduled,
		mcpSelection:             opts.MCPSelection,
		credentialAllowlist:      opts.CredentialAllowlist,
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

// scheduledPolicy layers the end-of-run verifier onto agentcore.ScheduledPolicy.
// agentcore's audit/finish enforcement gates finishing first; once those clear,
// the verifier runs ONCE and any missing deliverables it reports are injected as
// one more enforcement round (CanFinish returns false with the missing-action
// nudges). After the verifier has run once it stops re-gating so the loop
// terminates.
type scheduledPolicy struct {
	inner    *agentcore.ScheduledPolicy
	agent    *Agent
	task     string
	verified bool
	// runCtx is the run's context, captured at build time so the end-of-run
	// verifier's model call honors the run's deadline/cancellation (CanFinish
	// itself takes no ctx). Falls back to context.Background() if unset.
	runCtx context.Context
}

func (p *scheduledPolicy) BeforeToolCall(toolName, toolCallID, rawInput string) (bool, string) {
	return p.inner.BeforeToolCall(toolName, toolCallID, rawInput)
}

func (p *scheduledPolicy) RecordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	p.inner.RecordToolResult(toolName, rawInput, resultText, succeeded)
}

// CanFinish first defers to the audit/finish enforcement. When that clears, it
// runs the end-of-run verifier once; missing actions become a final enforcement
// round. The verifier requires a fallback model — without one it is skipped.
func (p *scheduledPolicy) CanFinish(round int) (bool, []string) {
	if ok, msgs := p.inner.CanFinish(round); !ok {
		return false, msgs
	}
	if p.verified || p.agent == nil || p.agent.fallbackModel == nil {
		return true, nil
	}
	p.verified = true
	records := buildToolExecSummary(p.agent.logSession)
	vctx := p.runCtx
	if vctx == nil {
		vctx = context.Background()
	}
	missing, err := p.agent.runEndOfRunVerifier(vctx, p.task, records)
	if err != nil {
		log.Printf("verifier skipped: %v", err)
		return true, nil
	}
	if len(missing) == 0 {
		return true, nil
	}
	return false, []string{fmt.Sprintf(
		"End-of-run verification found unfinished required actions: %v. "+
			"Complete each one now, or call confirm_audit(success=false, user_visible_summary=...) to abort explicitly.",
		missing)}
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

	// EXTERNAL (type: acp / delegated_policy) flavor as a SCHEDULED task — the
	// FAIL-CLOSED gate (P-ACP-4). This is the deliberate INVERSE of the native-acp
	// fallback below: native-acp may SILENTLY fall back to the fully-governed
	// in-process loop when it cannot faithfully govern; an external scheduled task
	// must NEVER fall back to a native flavor, because doing so would run a
	// DIFFERENT agent than the operator selected. Instead, when the per-client
	// opt-in is OFF, dispatch is a LOUD ERROR recorded in the run/session log; the
	// run is failed, not degraded. When the opt-in is ON, the scheduled turn runs
	// at the CONTAINMENT tier (governance: delegated), the sandbox is REQUIRED, and
	// permissions default-DENY (no human on the scheduled loop). We check this
	// FIRST — before the OpenRouter model guard and the native-acp branch — because
	// an external agent drives its OWN model endpoint with its own provider key
	// (a.model / OPENROUTER_API_KEY is irrelevant to it), and an external flavor
	// must never reach a native path.
	if a.isExternalFlavor() {
		return a.runScheduledExternal(ctx, task)
	}

	// The native paths drive the LLM loop through fleet's resolved OpenRouter
	// model; without one there is nothing to run. (External flavors returned
	// above and never reach this guard.)
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

	// native-acp routes the SAME scheduled loop (Mode=Scheduled, confirm_audit
	// gating, finish enforcement) through a sandboxed ACP agent. Every governed
	// effect stays host-side over `_fleet/*`: bash/python → the host sandbox; MCP
	// calls → the host's per-task credentialed client (creds NEVER enter the
	// container); propose_note staging → the real NoteProposer; usage/cost →
	// reported per step and accounted host-side. It governs identically to the
	// in-process scheduled path (the parity gate). A future surface added before
	// its delegation seam would fall back here — acpScheduledFallback is the single
	// place to re-introduce that, so the flavor never silently under-governs.
	if a.runtime == clientconfig.RuntimeNativeACP {
		if reason := acpScheduledFallback(a); reason != "" {
			log.Printf("native-acp: falling back to in-process for this scheduled task (%s)", reason)
		} else {
			return a.runScheduledACP(ctx, task, systemPrompt)
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

// acpScheduledFallback returns a non-empty reason when a scheduled task uses a
// governed feature the native-acp path cannot faithfully reproduce, so the caller
// falls back to the fully-governed in-process loop instead of silently
// under-governing. Empty = native-acp may run.
//
// P-ACP-2c closed the scheduled native-acp governance gap the same way P-ACP-2b
// closed it for interactive: the SAME ScheduledPolicy (confirm_audit gating +
// finish enforcement) runs inside the agent loop; MCP tool calls delegate over
// `_fleet/mcp` to the host's per-task credentialed client (creds never enter the
// container); propose_note staging delegates over `_fleet/stage`; usage/cost is
// reported over `_fleet/event` and accounted host-side.
//
// The END-OF-RUN VERIFIER (the in-process scheduledPolicy.CanFinish →
// runEndOfRunVerifier re-check layered on top of agentcore's audit/finish
// enforcement) now runs for native-acp too: the agent's in-loop scheduled policy
// reaches the host verifier over the `_fleet/verify` seam, which runs the SAME
// runEndOfRunVerifier on the host fallback model (host-side creds; only the
// tool-exec summary crosses the seam). So a verifier-reliant task is verified, not
// silently finished unverified — closing the P0 #35 governance gap.
//
// MCP LOAD-ON-DEMAND is also handled here rather than left as a silent residual:
// native-acp advertises a FIXED MCP surface up-front (the task's mcp_selection)
// and ships no in-loop mcp_load_servers loader, whereas the in-process path can
// load servers mid-run. A task that declares NO mcp_selection but runs on a box
// that HAS loadable (enabled) MCP servers would, under native-acp, run with NO
// MCP tools and be unable to load any — a silent capability divergence. So that
// case falls back to the fully-governed in-process loop (which ships the loader)
// instead of running tool-less. A task that declared a selection needs no loader
// (native-acp advertises it up-front) and does NOT fall back.
//
// The CORE governance — per-tool policy, audit, finish enforcement, MCP credential
// brokering, note staging, usage/cost, end-of-run verifier — is at FULL parity.
// This switch is the single auditable place to fall back when a scheduled surface
// cannot be faithfully reproduced, so the flavor never silently under-governs.
func acpScheduledFallback(a *Agent) string {
	if a.nativeAgentImage == "" {
		return "native-acp runtime selected but no agent image configured"
	}
	if a.sb == nil {
		// bash/python delegate to the HOST sandbox over `_fleet/tool`; without one
		// the delegated executor would fail at the first tool call. Fall back to the
		// in-process loop (which makes the same requirement explicit) rather than
		// spawning an agent that cannot execute tools.
		return "native-acp runtime selected but no host sandbox is configured"
	}
	if len(a.mcpSelection) == 0 && a.hasLoadableMCPServers() {
		// No declared selection + loadable servers exist = the task relies on the
		// in-loop loader (mcp_load_servers), which native-acp does not ship. Fall
		// back to the in-process loop rather than run silently tool-less.
		return "native-acp has no in-loop MCP loader; task declares no mcp_selection but loadable servers exist"
	}
	return ""
}

// hasLoadableMCPServers reports whether the runtime MCP catalog holds at least
// one ENABLED server the in-loop mcp_load_servers tool could load. Nil-safe: a
// nil config (some tests / a minimally-built agent) has nothing to load.
func (a *Agent) hasLoadableMCPServers() bool {
	if a.config == nil {
		return false
	}
	for _, sc := range a.config.MCPServers {
		if sc.Enabled {
			return true
		}
	}
	return false
}

// runScheduledACP drives one scheduled task through the native-acp flavor: a
// sandboxed ACP agent (acpruntime.ClientRuntime) runs the SAME agentcore.Run loop
// in Mode=Scheduled with the SAME ScheduledPolicy (confirm_audit gating + finish
// enforcement), but EVERY governed effect stays host-side via the `_fleet/*`
// delegation seam, so the task governs identically to the in-process scheduled
// path (the parity gate):
//
//   - bash/python execute in the HOST sandbox (a.sb) via the real Executor;
//   - MCP tool calls run against the HOST's per-task credentialed mcp.Client
//     (mcpBroker) — MCP credentials NEVER enter the agent container;
//   - propose_note staging hits the real NoteProposer (stageBroker);
//   - usage/cost is reported per step over `_fleet/event` and accounted host-side
//     (written into the captain's-log session for the SAME accounting the
//     in-process path produces).
//
// The host owns the sandbox + credentials; the agent container holds no executor
// and no secrets beyond the model-endpoint key.
func (a *Agent) runScheduledACP(ctx context.Context, task, systemPrompt string) error {
	messages := []fantasy.Message{fantasy.NewUserMessage(task)}
	messagesJSON, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshal scheduled task message: %w", err)
	}

	allow, optional := a.mcpGates()

	// The MCP surface is the task's DECLARED selection only. The scheduled runner
	// binds a DEDICATED per-task client for a non-empty selection (so its
	// GetAllTools() is exactly those servers) but reuses the SHARED process-wide
	// client for an empty selection (which may hold other servers). To avoid
	// advertising an unselected, cross-task surface, we hand buildACPHostGovernance
	// a client ONLY when the task declared a selection; an empty selection yields no
	// MCP surface — matching the in-process load-on-demand start (native-acp has no
	// in-loop loader to add servers mid-run).
	mcpClient := a.acpMCPClient()

	// Host-side governance seam, built by the SAME shared helper the interactive
	// driver uses. Scheduled wires the note proposer only (approval/memory are
	// interactive-only) — matching the in-process scheduled policy. MCP descriptors
	// carry NO credentials; the broker runs each call host-side against the
	// per-task credentialed client.
	gov := buildACPHostGovernance(mcpClient, allow, optional, a.mcpSelection, a.credentialAllowlist, acpStagers{
		note: a.noteProposer,
	})

	maxTokens := agentcore.DefaultMaxCompletionTokens
	if a.config != nil && a.config.LLMMaxTokens > 0 {
		maxTokens = a.config.LLMMaxTokens
	}
	temp := 0.3
	if a.config != nil {
		temp = a.config.LLMTemperature
	}

	rt := acpruntime.NewClientRuntime(acpruntime.ClientConfig{
		Image: a.nativeAgentImage,
		// ONLY the model-endpoint credentials enter the agent container — its one
		// allowed egress. MCP creds are never shipped (host-brokered).
		ModelEnv: modelEndpointEnv(),
	})

	res, err := rt.Run(ctx,
		acpruntime.RunSpec{
			Mode:                agentcore.ModeScheduled.String(),
			ModelSlug:           slugOf(a.model),
			FallbackSlug:        slugOf(a.fallbackModel),
			SystemPrompt:        systemPrompt,
			Temperature:         temp,
			MaxTokens:           maxTokens,
			Label:               a.logSession.Title,
			ProviderXTitle:      agentcore.DefaultProviderHeaders.XTitle,
			ProviderHTTPReferer: agentcore.DefaultProviderHeaders.HTTPReferer,
			MCPTools:            gov.MCPDescriptors,
			StagingWired:        gov.StagingWired,
			NoteProposerWired:   gov.NoteProposerWired,
			// The end-of-run verifier runs only when a fallback model exists — the
			// EXACT condition the in-process scheduledPolicy.CanFinish gates on. When
			// set, the agent's scheduled policy reaches the host verifier over
			// `_fleet/verify` instead of silently finishing unverified (#35).
			VerifierWired: a.fallbackModel != nil,
		},
		task,
		acpruntime.PromptMeta{MessagesJSON: string(messagesJSON)},
		acpruntime.Deps{
			Executor:    NewSandboxExecutor(a.sb),
			Observer:    &scheduledObserver{session: a.logSession},
			MCPBroker:   gov.MCPBroker,
			StageBroker: gov.StageBroker,
			// The verifier EFFECT (an LLM re-check on the host fallback model, with
			// host-side creds) runs through the SAME runEndOfRunVerifier the in-process
			// path uses; only the agent-shipped tool-exec summary crosses the seam.
			Verifier: &acpVerifyBroker{agent: a, task: task},
		},
	)

	// Reconcile usage/cost from the agent's per-step `_fleet/event` reports into
	// the captain's-log session BEFORE handling any error. The in-process path
	// accumulates usage onto the LogSession LIVE (every LLM step writes through
	// orchestration.updateUsage), so an errored in-process scheduled run still
	// persists its partial token/cost. acpruntime.Run mirrors that by returning the
	// usage that accrued so far even on error, so we record it on EVERY exit path —
	// a native-acp run that exhausts the enforcement round cap (the common
	// scheduled error) accounts for what it consumed, identically to in-process. The
	// run is complete (single goroutine here), so the direct field writes are
	// race-free.
	a.recordACPUsage(res.Usage)
	if res.FinalText != "" {
		a.logSession.AddMessage(roleAssistant, res.FinalText, nil, nil)
	}
	if err != nil {
		return err
	}
	return nil
}

// acpVerifyBroker adapts the host-side end-of-run verifier to the acpruntime
// VerifyBroker seam: it maps the agent-shipped tool-exec summary onto the
// agent package's records and runs the SAME runEndOfRunVerifier the in-process
// scheduled path runs (host fallback model, host-side creds). The verifier model
// call therefore never enters the agent container — only the records + round
// cross the seam, and the missing-actions verdict comes back.
type acpVerifyBroker struct {
	agent *Agent
	task  string
}

func (b *acpVerifyBroker) Verify(ctx context.Context, _ int, records []acpruntime.ToolExecRecord) ([]string, error) {
	recs := make([]toolExecRecord, len(records))
	for i, r := range records {
		recs[i] = toolExecRecord{Name: r.Name, Succeeded: r.Succeeded}
	}
	return b.agent.runEndOfRunVerifier(ctx, b.task, recs)
}

// acpMCPClient returns the client whose tools the native-acp path advertises, or
// nil to advertise NO MCP surface. A task that declared an mcp_selection ran on a
// DEDICATED per-task client (bound by the scheduled runner to exactly those
// servers), so its full tool set is the task's selection — safe to advertise. A
// task with no selection reuses the SHARED process-wide client, whose contents are
// not task-scoped; advertising it would leak an unselected, cross-task MCP surface,
// so we return nil. A no-selection task that COULD load servers never reaches here
// — acpScheduledFallback routes it to the in-process loop (which ships the loader)
// rather than run tool-less. So when this returns nil, the catalog has no loadable
// servers and a tool-less native-acp run is genuinely correct.
func (a *Agent) acpMCPClient() *mcp.Client {
	if len(a.mcpSelection) == 0 {
		return nil
	}
	return a.mcpClient
}

// recordACPUsage folds the ACP-reported run usage into the captain's-log session
// counters, the SAME fields the in-process orchestration writes via updateUsage.
func (a *Agent) recordACPUsage(u agentcore.RunUsage) {
	if a.logSession == nil {
		return
	}
	a.logSession.PromptTokens = u.PromptTokens
	a.logSession.CompletionTokens = u.CompletionTokens
	a.logSession.CachedTokens = u.CachedTokens
	a.logSession.CacheCreationTokens = u.CacheCreationTokens
	// LastStepPromptTokens: the in-process path sets this to the last step's
	// (input + cache-read) tokens. agentcore.RunUsage reports the last step's input
	// (LastStepInputTokens) but only the CUMULATIVE cached total, so the last step's
	// cache-read split is not recoverable here — we record the input portion. The
	// gross fields (PromptTokens, CachedTokens, Cost) are exact; only this single
	// last-step counter omits cache reads when prompt caching is active. It is not
	// persisted by convertLogSession today (it feeds chat's per-turn context
	// estimate, not the scheduled captain's-log), so this is a benign approximation.
	a.logSession.LastStepPromptTokens = u.LastStepInputTokens
	a.logSession.Cost = u.CostUSD
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
