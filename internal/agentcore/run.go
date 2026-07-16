package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// The ONE unified run loop. cutlass's outer enforcement loop is the BASE; chat's
// single pass is the 1-round collapse via an interactive Policy whose CanFinish
// returns true at round 1.
//
//	func Run(ctx, mode, cfg, deps) (Result, error)
//
// Per round: rebuild the fantasy tool list + agent when MCP servers went dirty
// (cutlass mcpServersDirty), stream the round through the resilience layer, then
// ask Policy.CanFinish — when finishing is blocked, inject the enforcement
// messages as the next round's nudges and loop. When CanFinish is true at round
// 0 (interactive), the loop runs exactly one pass.

// maxEnforcementRounds bounds the outer loop (cutlass's value).
const maxEnforcementRounds = 20

// RunConfig is the per-run configuration the loop reads. The DRIVERS build it.
type RunConfig struct {
	// TaskID / ConversationID are opaque run identities used only to attribute
	// contained panics in structured operator telemetry. Scheduled drivers set
	// TaskID; interactive drivers set ConversationID. Neither is sent to the
	// model, tool arguments, or sandbox.
	TaskID         string
	ConversationID string
	// EnvPrefix selects the env-var family (kill-switches, retry budget).
	EnvPrefix EnvPrefix
	// Temperature for the model calls.
	Temperature float64
	// MaxCompletionTokens caps a single completion's output (defaults to
	// DefaultMaxCompletionTokens when zero).
	MaxCompletionTokens int
	// MaxIterations caps the number of agent STEPS (tool-call/model round-trips)
	// within a single round's fantasy stream. 0 = no cap (loop until the model
	// stops on its own, bounded only by the per-turn timeout + cost ceiling).
	// Wired into the stream's StopWhen so a model that never stops calling tools
	// is bounded by the configured budget. Per-round (each enforcement round gets
	// a fresh step budget), matching the legacy chat/cutlass per-turn cap.
	MaxIterations int
	// Allowlist is the per-server tool allowlist (Gate-2).
	Allowlist mcpAllowlist
	// OptionalServers is the authoritative catalog of Optional servers.
	OptionalServers mcpOptionalSet
	// Selection is the per-run MCP selection; its server names form the Gate-1
	// opt-in set.
	Selection MCPSelection
	// CredentialAllowlist scopes which (server, account) MCP pairs the run may
	// call (Gate-3, #184). nil = no restriction (inherit global). Only the
	// scheduled driver sets it today; the interactive driver leaves it nil.
	CredentialAllowlist CredentialAllowlist
	// PersonaName is the run's active persona basename (e.g. "code-reviewer"),
	// used only to label persona_tool_blocked audit events. Empty when the run
	// has no persona.
	PersonaName string
	// PersonaPolicy is the per-persona tool allowlist (Gate-4, #294). nil or an
	// empty policy = no narrowing (the persona sees every tool the earlier gates
	// already permitted). When set, it filters the registered tool roster BEFORE
	// the first LLM call so denied tools never appear in the model's tool list,
	// and it equally governs the MCP tools that tool disclosure (#506) defers
	// behind the tool_search/tool_describe/tool_call bridges (#570). It can only
	// SUBTRACT from what the server/credential gates already permitted, never
	// add. The drivers resolve it from the bundle manifest's personas: block.
	PersonaPolicy *PersonaToolPermissions
	// RemediationHints configures the fast.io guard (defaults to
	// DefaultRemediationHints, which exposes both remediation paths).
	RemediationHints RemediationHints
	// IncludeConfirmAudit appends the scheduled confirm_audit tool.
	IncludeConfirmAudit bool
	// RequireCompactionOptIn gates proactive context compaction (#209) behind the
	// FLEET_SCHEDULED_AUTO_COMPACT opt-in. The scheduled driver sets it so an
	// unattended run never silently rewrites its own transcript — without the
	// opt-in it only warns. Interactive leaves it false (compact freely). This is
	// driver-supplied DATA, not a trunk Mode branch (see TestSeamPurity).
	RequireCompactionOptIn bool
	// LoaderTools are extra always-registered tools (scheduled mcp_list/load).
	LoaderTools []fantasy.AgentTool
	// NativeTools are the mode's native tools (bash/python via Executor, etc.).
	NativeTools []fantasy.AgentTool
	// ProviderHeaders identify the run to OpenRouter.
	ProviderHeaders ProviderHeaders

	// OutputSchema enables the fail-closed terminal structured-output contract.
	// The ordinary governed agent loop runs first with its full sandboxed tool
	// roster. Once Policy permits finishing, Run performs a bounded terminal
	// phase over the completed transcript with NO ordinary tools available and
	// returns only schema-validated JSON. Empty preserves free-form behavior.
	OutputSchema json.RawMessage

	// ThinkingConfig, when set and Enabled, activates Claude extended thinking
	// (#220) for this run on a thinking-capable Claude slug. nil = off (the
	// default). The drivers resolve it from the per-conversation setting (chat) or
	// the global FLEET_DEFAULT_THINKING_BUDGET_TOKENS default; a non-Claude model
	// silently ignores it (see supportsExtendedThinking).
	ThinkingConfig *ThinkingConfig
}

// Deps are the run dependencies: the four seams plus the model handles, MCP
// client, and orchestration. The DRIVERS construct these.
type Deps struct {
	// Input supplies the system prompt + seed messages + label.
	Input InputSource
	// Observer receives run events.
	Observer Observer
	// Policy gates tool calls + finishing.
	Policy Policy
	// Executor runs sandboxed code (passed through to NativeTools by the driver;
	// held here so the loop can surface it to the finalize hook).
	Executor Executor

	// Model + FallbackModel are the resolved fantasy language models.
	Model         fantasy.LanguageModel
	FallbackModel fantasy.LanguageModel
	// FallbackModels is an ordered same-model cross-provider chain. An explicit
	// FallbackModel takes precedence and suppresses this chain.
	FallbackModels []fantasy.LanguageModel

	// MCPClient holds the registered (and credential-bound) MCP servers — the
	// merged P1 client. May be nil when a run registers no MCP servers (a fresh
	// empty client is used instead). It is also where the tool CATALOG is
	// discovered (GetAllTools), independent of how calls are run (see MCPBroker).
	MCPClient *mcp.Client

	// MCPBroker, when non-nil, is the seam MCP tool CALLS route through, replacing
	// the default in-process localMCPBroker built over MCPClient. It exists so the
	// credential boundary can move out of process (issue #167): inject an
	// out-of-process broker client here and calls run there. Nil keeps the
	// in-process behavior — calls run directly against MCPClient.
	MCPBroker MCPBroker

	// MCPCatalog, when non-nil, is the tool catalog the loop advertises, replacing
	// MCPClient.GetAllTools(). Paired with MCPBroker it lets the broker be the SOLE
	// owner of the credentialed client — the main process advertises the catalog it
	// fetched from the broker (ListTools) without holding a client of its own, so
	// MCP servers are not double-spawned. Nil = discover from MCPClient as before.
	MCPCatalog []mcp.ServerTool

	// NotesProvider supplies the admin-curated knowledge base injected into the
	// system prompt for BOTH modes. Nil = no notes section. The DRIVERS read it
	// at prompt-assembly time (the run loop does not touch it); held here so the
	// process can hand the same sched-backed provider to both drivers' Deps.
	NotesProvider NotesProvider

	// LogSession is the structured session log the scheduled Observer writes
	// (interactive may pass a throwaway). Usage accounting flows into it.
	LogSession *LogSession

	// MCPServersDirty, when non-nil and it returns true at the top of a round,
	// triggers a tool-list + agent rebuild (cutlass mcp_load_servers path). The
	// loop clears the dirty flag via ClearMCPDirty after rebuilding.
	MCPServersDirty func() bool
	ClearMCPDirty   func()

	// Finalize is the interactive-only finalize hook (leaked-tool-call retry /
	// forced final summary). Nil in scheduled mode. See finalize.go.
	Finalize FinalizeHook

	// CompactionSummarizer, when set, produces the summary message inserted in
	// place of the dropped middle during a context-too-large force-compaction
	// (the interactive driver wires chat's head/summary/tail compaction here).
	// Nil falls back to the engine's deterministic placeholder summary.
	CompactionSummarizer func(ctx context.Context, droppable []fantasy.Message) fantasy.Message

	// UsageReporter, when set, is invoked after each LLM step with that step's
	// accumulated run usage (the SAME counters usageSnapshot returns). A driver
	// may wire this to ship a per-step usage event out-of-band so an external
	// accountant tracks tokens/cost as steps complete. Nil for the in-process
	// loop — it reads usage from the orch at the end.
	UsageReporter func(RunUsage)

	// HealthRegistry, when set, is the cross-run provider circuit-breaker (#267).
	// The interactive Manager owns one long-lived registry and passes it on every
	// run so error accumulation persists across turns; nil disables the breaker.
	HealthRegistry *ProviderHealthRegistry
}

// Result is the run outcome.
type Result struct {
	// FinalText is the model's final user-visible reply.
	FinalText string
	// OutputJSON is the validated terminal value when RunConfig.OutputSchema was
	// set. A successful structured run always returns a non-empty value here;
	// generation, refusal, missing output, and validation exhaustion are errors.
	OutputJSON json.RawMessage
	// Rounds is how many enforcement rounds executed.
	Rounds int
	// SwappedToFallback reports whether the run ended on the fallback model.
	SwappedToFallback bool
	// Label echoes the InputSource's task label.
	Label string

	// Entries is the ordered, neutral history of everything the run streamed:
	// reasoning / text / tool_call / tool_result records, plus any recovered
	// assistant text the finalize hook produced. The interactive driver maps
	// these onto agent.HistoryEntry for persistence; scheduled mode (which
	// persists via the session log) can ignore them.
	Entries []RunEntry

	// ModelSlug is the OpenRouter slug the run actually finished on (the
	// fallback slug after a swap).
	ModelSlug string

	// Cancelled is true when the run ended because the caller's ctx was
	// cancelled (Stop button, client disconnect, idle timeout). Partial
	// Entries / FinalText / usage are still returned.
	Cancelled bool

	// StoppedByBudget is true when the run ended because a per-turn cost/token
	// ceiling fired (ErrCostCeilingExceeded), not a caller cancel. It is set
	// alongside Cancelled on the budget path so the driver can tell the user
	// "budget reached" apart from "stopped by user" while still persisting the
	// partial transcript.
	StoppedByBudget bool

	// Usage is the accumulated token + cost accounting for the whole run.
	Usage RunUsage
}

// RunUsage is the accumulated token + cost accounting for a run. It follows the
// LogSession token convention: PromptTokens INCLUDES cache reads, CachedTokens
// is that cached subset (so uncached spend is PromptTokens - CachedTokens, the
// checkCeilings math), and LastStepInputTokens is the final step's total input
// size (fresh + cache-read — the context-window-fill signal).
type RunUsage struct {
	PromptTokens        int
	LastStepInputTokens int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	CostUSD             float64
}

// Run drives a single agent run to completion. It is the shared body both modes
// use; Mode + the seams are the only divergence axes.
func Run(ctx context.Context, mode Mode, cfg RunConfig, deps Deps) (result Result, err error) {
	panicAttribution := panicAttribution{
		runMode:        mode.String(),
		taskID:         cfg.TaskID,
		conversationID: cfg.ConversationID,
	}
	defer recoverSynchronousRunPanic(panicAttribution, &result, &err)

	if deps.Model == nil {
		return Result{}, fmt.Errorf("no language model configured")
	}
	if deps.Input == nil || deps.Policy == nil {
		return Result{}, fmt.Errorf("run requires an InputSource and a Policy")
	}

	// Observer callbacks can originate inside Fantasy's coordinator and parallel
	// tool goroutines. Wrap the seam once for the whole run, then pass only that
	// contained observer to every roster/filter/sink/finalize path.
	observerBoundary := containObserver(deps.Observer, panicAttribution)
	deps.Observer = observerBoundary.Observer()

	logSession := deps.LogSession
	if logSession == nil {
		logSession = NewLogSession()
	}

	eng := newRunEngine(cfg, deps, logSession)

	systemPrompt, messages, label, err := deps.Input.Prompt(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("input source: %w", err)
	}
	// Host-enforced ingress guardrail (#702): screen variable user/task content
	// before the first provider call. The system prefix is deliberately excluded
	// to preserve the prompt-cache contract and because it is operator-trusted.
	if err := screenSeedMessages(ctx, messages); err != nil {
		return Result{}, err
	}

	// Lifecycle hooks (#788): one per-run engine, nil (zero-overhead no-op) when
	// the bundle declares none. Hooks execute inside the sandbox via deps.Executor.
	hooks := newHookEngine(mode, deps.Executor, deps.Observer, label)
	messages, err = applyUserPromptSubmitHooks(ctx, hooks, messages)
	if err != nil {
		return Result{}, err
	}

	maxTokens := int64(DefaultMaxCompletionTokens)
	if cfg.MaxCompletionTokens > 0 {
		maxTokens = int64(cfg.MaxCompletionTokens)
	}

	optIn := cfg.Selection.OptInSet()
	hints := cfg.RemediationHints
	if hints == (RemediationHints{}) {
		hints = DefaultRemediationHints
	}
	toolCfg := toolBuildConfig{
		includeConfirmAudit: cfg.IncludeConfirmAudit,
		loaderTools:         cfg.LoaderTools,
		remediationHints:    hints,
		// Gate-4 (#294) rides into the tool build itself so the persona
		// allowlist also governs the deferrable MCP set BEFORE the disclosure
		// decision (#570) — the roster-level pass in buildTools below cannot see
		// a tool that deferred behind the tool_search/tool_call bridges.
		personaName:      cfg.PersonaName,
		personaPolicy:    cfg.PersonaPolicy,
		observer:         deps.Observer,
		panicAttribution: panicAttribution,
		hooks:            hooks,
	}

	mcpClient := deps.MCPClient
	if mcpClient == nil {
		mcpClient = mcp.NewClient()
	}
	// Calls route through a broker seam; discovery (the catalog) comes from
	// mcpClient. Default = the in-process localMCPBroker over the credentialed
	// client. An injected deps.MCPBroker (e.g. an out-of-process broker, #167)
	// takes over calls without changing the loop.
	broker := deps.MCPBroker
	if broker == nil {
		broker = NewLocalMCPBroker(mcpClient, hints)
	}
	// Gate-3 (#184): scope MCP calls to the task's permitted (server, account)
	// pairs. nil allowlist = inherit global (no-op wrap). Applied at the broker
	// seam — the single path every MCP call routes through — so the in-process
	// loop and any out-of-process broker (#167) enforce it identically.
	broker = GateMCPBrokerWithAllowlist(broker, cfg.CredentialAllowlist)
	// The catalog is data, sourced either from an injected list (broker mode) or
	// the local client. Decoupling it from the client lets the broker own the
	// client without the main process double-spawning servers just to discover.
	catalog := deps.MCPCatalog
	if catalog == nil {
		catalog = mcpClient.GetAllTools()
	}
	buildTools := func() ([]fantasy.AgentTool, error) {
		tools, err := buildFantasyTools(cfg.NativeTools, catalog, broker, cfg.Allowlist, deps.Policy, cfg.OptionalServers, optIn, toolCfg)
		if err != nil {
			return nil, err
		}
		// Gate-4 (#294): NARROW the registered roster to the persona's tool
		// allowlist BEFORE the agent (and thus the first LLM call) sees it, so a
		// denied tool never enters the model's tool list. Applied after
		// buildFantasyTools — over the slice that already survived Gates 1-3 — so
		// the persona policy can only SUBTRACT, never widen. This roster pass
		// covers the native/loader tools; the deferrable MCP set is
		// persona-filtered INSIDE buildFantasyTools before the disclosure
		// decision (#570), because a tool hidden behind the disclosure bridges
		// never appears in this roster — the bridges themselves survive an
		// allow-list here (see resolvePersonaTools) precisely because everything
		// reachable through them was already filtered. A nil/empty policy is
		// a zero-overhead passthrough (current behavior). Re-applied on every
		// rebuild (the mcp_load_servers dirty path) via this closure.
		if cfg.PersonaPolicy != nil {
			tools = resolvePersonaTools(cfg.PersonaName, *cfg.PersonaPolicy, tools, deps.Observer)
		}
		return tools, observerBoundary.Err()
	}

	fantasyTools, err := buildTools()
	if err != nil {
		return Result{}, fmt.Errorf("build tools: %w", err)
	}
	eng.setModelContextPrefix(systemPrompt, fantasyTools)

	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m,
			fantasy.WithSystemPrompt(systemPrompt),
			fantasy.WithTools(fantasyTools...),
		)
	}

	activeModel := deps.Model
	agent := buildAgent(activeModel)
	swappedToFallback := false

	// One run-wide streamSink forwards every round's text / reasoning / tool
	// events to the Observer and accumulates the run history. Shared across
	// rounds so a multi-round scheduled run builds one coherent transcript.
	sink := newStreamSink(deps.Observer, panicAttribution)
	// usageOrch is the orchestration state whose usage counters accumulate
	// across rounds (the same state the resilience layer mutates per step).
	usageOrch := policyOrch(deps.Policy)

	var finalResult *fantasy.AgentResult

	// pressureWarned dedupes the context-pressure warning across rounds (#209):
	// a multi-round run hovering above the warn threshold should surface ONE
	// banner, not one per round. A successful proactive compaction relieves the
	// pressure and resets it, so a later climb warns again.
	pressureWarned := false

	for round := 0; round < maxEnforcementRounds; round++ {
		if ctx.Err() != nil {
			// Caller cancelled (Stop / disconnect / timeout): return the partial
			// transcript + usage rather than erroring, so the driver can persist
			// what the model produced before the cancel. The interactive driver
			// uses Cancelled to emit turn.cancelled instead of turn.error.
			return cancelledResult(sink, usageOrch, label, activeModel, swappedToFallback, round), nil
		}

		// Rebuild on MCP-server dirty (cutlass mcp_load_servers path).
		if deps.MCPServersDirty != nil && deps.MCPServersDirty() {
			fantasyTools, err = buildTools()
			if err != nil {
				return Result{}, fmt.Errorf("rebuild tools: %w", err)
			}
			eng.setModelContextPrefix(systemPrompt, fantasyTools)
			agent = buildAgent(activeModel)
			if deps.ClearMCPDirty != nil {
				deps.ClearMCPDirty()
			}
			log.Printf("🔌 MCP loaded-server set changed; fantasy agent rebuilt for round %d", round+1)
		}

		// Context-window pressure check (#209): warn — and, above the higher
		// threshold, proactively compact — before the provider can reject an
		// oversized prompt. The opt-in gate is carried as RunConfig data, so the
		// trunk stays free of Mode branches. See engine.checkContextPressure.
		pressure := eng.checkContextPressure(ctx, messages, activeModel, sink, pressureWarned)
		messages = pressure.messages
		pressureWarned = pressure.warned

		orch := policyOrch(deps.Policy)
		outcome, serr := eng.streamRoundWithResilience(
			ctx, orch, sink, maxTokens, messages, agent, activeModel, swappedToFallback, buildAgent,
		)
		// Fantasy waits for its coordinator + all parallel tool goroutines before
		// streamRoundWithResilience returns. Only now convert an observer panic to
		// an ordinary run error, after every sibling has settled and paired.
		serr = observerBoundary.prefer(serr)
		if serr != nil {
			// A ctx-cancellation surfaced as a stream error is still a clean
			// cancel: return the partial transcript instead of a hard error so
			// the interactive Stop path persists partial work.
			if ctx.Err() != nil {
				return cancelledResult(sink, usageOrch, label, activeModel, swappedToFallback, round), nil
			}
			// A cost/token ceiling hit is a clean STOP, not a failure: the
			// budget-guarded PrepareStep aborted before the next paid completion.
			// Finish gracefully with the transcript accumulated so far (same
			// partial-result contract as a cancel) so the budget bounds the run
			// without erroring the turn.
			if errors.Is(serr, ErrCostCeilingExceeded) {
				res := cancelledResult(sink, usageOrch, label, activeModel, swappedToFallback, round)
				res.StoppedByBudget = true
				return res, nil
			}
			return Result{}, serr
		}
		finalResult = outcome.result
		messages = outcome.messages
		agent = outcome.agent
		activeModel = outcome.activeModel
		swappedToFallback = outcome.swappedToFallback

		// The model's user-visible text for this round comes from the streamed
		// accumulation (sink), falling back to the final AgentResult content.
		_, accumulatedText := sink.snapshot()
		finalText := strings.TrimSpace(accumulatedText)
		if finalText == "" && finalResult != nil && finalResult.Response.Content != nil {
			finalText = finalResult.Response.Content.Text()
		}

		canFinish, enforcementMsgs, policyErr := callPolicyCanFinish(deps.Policy, round, panicAttribution)
		if policyErr != nil {
			return Result{}, policyErr
		}
		if canFinish {
			// Interactive-only finalize hook (leaked-tool-call / forced summary).
			// Stubbed unless the driver supplies an impl. The hook streams its
			// own follow-up text deltas through the Observer; recovered text
			// replaces the loop's text and is appended as an assistant entry so
			// it persists.
			if deps.Finalize != nil {
				finalText, err = finalizeWithPanicBoundary(ctx, deps.Finalize, FinalizeInput{
					Mode:         mode,
					FinalText:    finalText,
					Messages:     messages,
					Tools:        fantasyTools,
					Observer:     deps.Observer,
					SystemPrompt: systemPrompt,
					OnToolCall:   finalizeToolCallCallback(sink, panicAttribution),
					OnToolResult: finalizeToolResultCallback(sink, panicAttribution),
					// Meter a recovery model call into the SAME run accounting as
					// the main loop, so the cost chip isn't undercounted. Capability
					// closure over usageOrch — the state never escapes Run, and this
					// field is set unconditionally (not a mode branch).
					RecordUsage: func(u fantasy.Usage, md fantasy.ProviderMetadata) {
						if usageOrch != nil {
							usageOrch.updateUsage(slugOf(activeModel), u, md)
						}
					},
				}, observerBoundary)
				if err != nil {
					return Result{}, err
				}
			}
			res, cerr := completeRun(ctx, runCompletion{
				engine:            eng,
				config:            cfg,
				activeModel:       activeModel,
				systemPrompt:      systemPrompt,
				messages:          messages,
				finalResult:       finalResult,
				finalText:         finalText,
				sink:              sink,
				orchestration:     usageOrch,
				maxTokens:         maxTokens,
				rounds:            round + 1,
				swappedToFallback: swappedToFallback,
				label:             label,
			})
			if cerr != nil {
				return res, cerr
			}
			// turn_end hooks (#788): observational only — a completed turn is not
			// undone, so the decision is audited but not enforced. Fired only on
			// normal completion (not cancel/budget, where ctx is dead and a
			// sandbox exec cannot run), and AFTER the terminal structured-output
			// phase so the audited text is the run's true final output.
			hooks.turnEnd(ctx, res.FinalText, round+1)
			return res, nil
		}

		// Finish blocked: carry this round's transcript into the next round's
		// input, then inject the enforcement nudges and loop. The fallback-swap
		// state carries forward (cutlass nextRoundMessages). The transcript
		// carry is what lets the next round CONTINUE the work instead of
		// restarting it — see carryRoundMessages.
		messages = append(messages, carryRoundMessages(finalResult)...)
		messages, err = appendEnforcementMessages(messages, enforcementMsgs, deps.Observer, observerBoundary)
		if err != nil {
			return Result{}, err
		}
	}

	return Result{Label: label}, fmt.Errorf("max enforcement rounds (%d) exceeded without task completion", maxEnforcementRounds)
}

// runCompletion carries the last ordinary round into the single terminal
// completion seam. Keeping result assembly here leaves Run focused on its
// governed control loop and makes the structured/free-form finish paths share
// exactly the same transcript and usage snapshot behavior.
type runCompletion struct {
	engine            *engine
	config            RunConfig
	activeModel       fantasy.LanguageModel
	systemPrompt      string
	messages          []fantasy.Message
	finalResult       *fantasy.AgentResult
	finalText         string
	sink              *streamSink
	orchestration     *orchestrationState
	maxTokens         int64
	rounds            int
	swappedToFallback bool
	label             string
}

func completeRun(ctx context.Context, in runCompletion) (Result, error) {
	var outputJSON json.RawMessage
	if len(in.config.OutputSchema) > 0 {
		// messages is the input to the last ordinary round. Carry its completed
		// assistant/tool transcript so terminal formatting uses work already done.
		terminalMessages := append([]fantasy.Message(nil), in.messages...)
		terminalMessages = append(terminalMessages, carryRoundMessages(in.finalResult)...)
		var err error
		outputJSON, err = in.engine.generateTerminalStructuredOutput(
			ctx, in.activeModel, in.systemPrompt, terminalMessages,
			in.config.OutputSchema, in.maxTokens, in.orchestration,
		)
		if err != nil {
			entries, _ := in.sink.snapshot()
			return Result{
				FinalText:         in.finalText,
				Rounds:            in.rounds,
				SwappedToFallback: in.swappedToFallback,
				Label:             in.label,
				Entries:           entries,
				ModelSlug:         slugOf(in.activeModel),
				Usage:             usageSnapshot(in.orchestration),
			}, err
		}
		in.finalText = string(outputJSON)
	}

	entries, _ := in.sink.snapshot()
	if in.finalText != "" {
		entries = append(entries, RunEntry{Role: roleAssistant, Type: "text", Text: in.finalText})
	}
	return Result{
		FinalText:         in.finalText,
		OutputJSON:        outputJSON,
		Rounds:            in.rounds,
		SwappedToFallback: in.swappedToFallback,
		Label:             in.label,
		Entries:           entries,
		ModelSlug:         slugOf(in.activeModel),
		Usage:             usageSnapshot(in.orchestration),
	}, nil
}

func runFallbackModels(deps Deps) []fantasy.LanguageModel {
	if deps.FallbackModel != nil {
		return []fantasy.LanguageModel{deps.FallbackModel}
	}
	return append([]fantasy.LanguageModel(nil), deps.FallbackModels...)
}

func newRunEngine(cfg RunConfig, deps Deps, logSession *LogSession) *engine {
	fallbackModels := runFallbackModels(deps)
	eng := &engine{
		model:                  deps.Model,
		fallbackModels:         fallbackModels,
		resilience:             loadResilienceConfigFor(cfg.EnvPrefix),
		logSession:             logSession,
		onRetry:                newRetryLogger(logSession),
		temperature:            cfg.Temperature,
		envPrefix:              cfg.EnvPrefix,
		compactionSummarizer:   deps.CompactionSummarizer,
		usageReporter:          deps.UsageReporter,
		maxIterations:          cfg.MaxIterations,
		healthRegistry:         deps.HealthRegistry,
		requireCompactionOptIn: cfg.RequireCompactionOptIn,
		thinkingConfig:         cfg.ThinkingConfig,
	}
	if len(fallbackModels) > 0 {
		eng.fallbackModel = fallbackModels[0]
	}
	return eng
}

// cancelledResult builds the partial Result returned when the run's ctx was
// cancelled mid-flight. It carries whatever transcript + usage accumulated so
// the driver can persist the partial work (chat's Stop semantics).
func cancelledResult(sink *streamSink, orch *orchestrationState, label string, activeModel fantasy.LanguageModel, swapped bool, round int) Result {
	entries, text := sink.snapshot()
	final := strings.TrimSpace(text)
	if final != "" {
		entries = append(entries, RunEntry{Role: roleAssistant, Type: "text", Text: final})
	}
	return Result{
		FinalText:         final,
		Rounds:            round,
		SwappedToFallback: swapped,
		Label:             label,
		Entries:           entries,
		ModelSlug:         slugOf(activeModel),
		Cancelled:         true,
		Usage:             usageSnapshot(orch),
	}
}

// usageSnapshot copies an orchestration state's accumulated usage counters.
func usageSnapshot(orch *orchestrationState) RunUsage {
	if orch == nil {
		return RunUsage{}
	}
	orch.mu.Lock()
	defer orch.mu.Unlock()
	return RunUsage{
		PromptTokens:        orch.PromptTokens,
		LastStepInputTokens: orch.LastStepInputTokens,
		CompletionTokens:    orch.CompletionTokens,
		CachedTokens:        orch.CachedTokens,
		CacheCreationTokens: orch.CacheCreationTokens,
		CostUSD:             orch.CostUSD,
	}
}

// slugOf returns a model's OpenRouter slug, or "" when nil.
func slugOf(m fantasy.LanguageModel) string {
	if m == nil {
		return ""
	}
	return m.Model()
}

// policyOrch extracts the orchestrationState a Policy embeds (so the resilience
// layer's usage accounting flows into the same state). A driver may WRAP a
// built-in Policy (e.g. the scheduled driver layers an end-of-run verifier onto
// ScheduledPolicy); such a wrapper exposes the inner policy via Unwrap so the
// orchestration is still found. Returns a throwaway when none is exposed.
func policyOrch(p Policy) *orchestrationState {
	for p != nil {
		if op, ok := p.(interface{ orchestration() *orchestrationState }); ok {
			if o := op.orchestration(); o != nil {
				return o
			}
		}
		w, ok := p.(PolicyUnwrapper)
		if !ok {
			break
		}
		p = w.Unwrap()
	}
	return newOrchestrationState(nil, 0)
}

// PolicyUnwrapper is implemented by a wrapping Policy that delegates to an inner
// Policy. The loop unwraps to find the orchestration state and the confirm_audit
// binding, so a driver can layer extra finish gates (the scheduled verifier)
// onto a built-in Policy without forking the loop.
type PolicyUnwrapper interface {
	Unwrap() Policy
}
