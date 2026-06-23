package agentcore

import (
	"context"
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
	// RemediationHints configures the fast.io guard (defaults to
	// DefaultRemediationHints, which exposes both remediation paths).
	RemediationHints RemediationHints
	// IncludeConfirmAudit appends the scheduled confirm_audit tool.
	IncludeConfirmAudit bool
	// LoaderTools are extra always-registered tools (scheduled mcp_list/load).
	LoaderTools []fantasy.AgentTool
	// NativeTools are the mode's native tools (bash/python via Executor, etc.).
	NativeTools []fantasy.AgentTool
	// ProviderHeaders identify the run to OpenRouter.
	ProviderHeaders ProviderHeaders

	// PreGatedTools are already-policy-aware tools registered VERBATIM (NOT
	// wrapped in the policyGuardedTool gate, because they call BeforeToolCall +
	// RecordToolResult themselves — exactly like the built-in mcpTool). The
	// native-acp agent uses this for its delegating MCP tools, which mirror the
	// in-process mcpTool's policy handling while delegating execution host-side.
	// Empty for the in-process modes (their MCP tools come from MCPClient).
	PreGatedTools []fantasy.AgentTool
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

	// MCPClient holds the registered (and credential-bound) MCP servers — the
	// merged P1 client. May be nil when a run registers no MCP servers (a fresh
	// empty client is used instead).
	MCPClient *mcp.Client

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
	// accumulated run usage (the SAME counters usageSnapshot returns). The
	// native-acp agent wires this to ship a per-step usage event back to the host
	// over `_fleet/event` so the host accounts for tokens/cost identically to the
	// in-process path (which reads the counters directly off the orch). Nil in
	// both in-process modes — they read usage from the orch at the end.
	UsageReporter func(RunUsage)
}

// Result is the run outcome.
type Result struct {
	// FinalText is the model's final user-visible reply.
	FinalText string
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

	// Usage is the accumulated token + cost accounting for the whole run.
	Usage RunUsage
}

// RunUsage is the accumulated token + cost accounting for a run.
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
func Run(ctx context.Context, mode Mode, cfg RunConfig, deps Deps) (Result, error) {
	if deps.Model == nil {
		return Result{}, fmt.Errorf("no language model configured")
	}
	if deps.Input == nil || deps.Policy == nil {
		return Result{}, fmt.Errorf("run requires an InputSource and a Policy")
	}

	logSession := deps.LogSession
	if logSession == nil {
		logSession = NewLogSession()
	}

	eng := &engine{
		model:                deps.Model,
		fallbackModel:        deps.FallbackModel,
		resilience:           loadResilienceConfigFor(cfg.EnvPrefix),
		logSession:           logSession,
		onRetry:              newRetryLogger(logSession),
		temperature:          cfg.Temperature,
		envPrefix:            cfg.EnvPrefix,
		compactionSummarizer: deps.CompactionSummarizer,
		usageReporter:        deps.UsageReporter,
		maxIterations:        cfg.MaxIterations,
	}

	systemPrompt, messages, label, err := deps.Input.Prompt(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("input source: %w", err)
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
	}

	mcpClient := deps.MCPClient
	if mcpClient == nil {
		mcpClient = mcp.NewClient()
	}
	toolCfg.preGatedTools = cfg.PreGatedTools
	buildTools := func() ([]fantasy.AgentTool, error) {
		return buildFantasyTools(cfg.NativeTools, mcpClient, cfg.Allowlist, deps.Policy, cfg.OptionalServers, optIn, toolCfg)
	}

	fantasyTools, err := buildTools()
	if err != nil {
		return Result{}, fmt.Errorf("build tools: %w", err)
	}

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
	sink := newStreamSink(deps.Observer)
	// usageOrch is the orchestration state whose usage counters accumulate
	// across rounds (the same state the resilience layer mutates per step).
	usageOrch := policyOrch(deps.Policy)

	var finalResult *fantasy.AgentResult

	for round := 0; round < maxEnforcementRounds; round++ {
		if ctx.Err() != nil {
			// Caller cancelled (Stop / disconnect / timeout): return the partial
			// transcript + usage rather than erroring, so the driver can persist
			// what the model produced before the cancel. The interactive driver
			// uses Cancelled to emit turn.cancelled instead of turn.error.
			//nolint:nilerr // intentional: a cancelled context is a clean stop, not a failure; returning nil error is the contract so the driver persists partial work and emits turn.cancelled.
			return cancelledResult(ctx, sink, usageOrch, label, activeModel, swappedToFallback, round), nil
		}

		// Rebuild on MCP-server dirty (cutlass mcp_load_servers path).
		if deps.MCPServersDirty != nil && deps.MCPServersDirty() {
			fantasyTools, err = buildTools()
			if err != nil {
				return Result{}, fmt.Errorf("rebuild tools: %w", err)
			}
			agent = buildAgent(activeModel)
			if deps.ClearMCPDirty != nil {
				deps.ClearMCPDirty()
			}
			log.Printf("🔌 MCP loaded-server set changed; fantasy agent rebuilt for round %d", round+1)
		}

		orch := policyOrch(deps.Policy)
		outcome, serr := eng.streamRoundWithResilience(
			ctx, orch, sink, maxTokens, messages, agent, activeModel, swappedToFallback, buildAgent,
		)
		if serr != nil {
			// A ctx-cancellation surfaced as a stream error is still a clean
			// cancel: return the partial transcript instead of a hard error so
			// the interactive Stop path persists partial work.
			if ctx.Err() != nil {
				//nolint:nilerr // intentional: ctx cancellation that surfaced as a stream error is a clean stop; returning nil error is the contract so the Stop path persists partial work.
				return cancelledResult(ctx, sink, usageOrch, label, activeModel, swappedToFallback, round), nil
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

		canFinish, enforcementMsgs := deps.Policy.CanFinish(round)
		if canFinish {
			// Interactive-only finalize hook (leaked-tool-call / forced summary).
			// Stubbed unless the driver supplies an impl. The hook streams its
			// own follow-up text deltas through the Observer; recovered text
			// replaces the loop's text and is appended as an assistant entry so
			// it persists.
			if deps.Finalize != nil {
				recovered, ferr := deps.Finalize(ctx, FinalizeInput{
					Mode:         mode,
					FinalText:    finalText,
					Messages:     messages,
					Observer:     deps.Observer,
					SystemPrompt: systemPrompt,
				})
				if ferr != nil {
					log.Printf("finalize hook error: %v", ferr)
				} else if recovered != "" {
					finalText = recovered
				}
			}
			entries, _ := sink.snapshot()
			if finalText != "" {
				entries = append(entries, RunEntry{Role: roleAssistant, Type: "text", Text: finalText})
			}
			return Result{
				FinalText:         finalText,
				Rounds:            round + 1,
				SwappedToFallback: swappedToFallback,
				Label:             label,
				Entries:           entries,
				ModelSlug:         slugOf(activeModel),
				Usage:             usageSnapshot(usageOrch),
			}, nil
		}

		// Finish blocked: inject enforcement nudges and loop. The fallback-swap
		// state carries forward (cutlass nextRoundMessages).
		for _, nudge := range enforcementMsgs {
			messages = append(messages, fantasy.NewUserMessage(nudge))
			if deps.Observer != nil {
				deps.Observer.Observe("enforcement", map[string]any{"message": nudge})
			}
		}
	}

	return Result{Label: label}, fmt.Errorf("max enforcement rounds (%d) exceeded without task completion", maxEnforcementRounds)
}

// cancelledResult builds the partial Result returned when the run's ctx was
// cancelled mid-flight. It carries whatever transcript + usage accumulated so
// the driver can persist the partial work (chat's Stop semantics).
func cancelledResult(_ context.Context, sink *streamSink, orch *orchestrationState, label string, activeModel fantasy.LanguageModel, swapped bool, round int) Result {
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
