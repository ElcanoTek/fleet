package agentcore

import (
	"context"
	"fmt"
	"log"

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

	var finalResult *fantasy.AgentResult

	for round := 0; round < maxEnforcementRounds; round++ {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("context cancelled: %w", ctx.Err())
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
			ctx, orch, maxTokens, messages, agent, activeModel, swappedToFallback, buildAgent,
		)
		if serr != nil {
			return Result{}, serr
		}
		finalResult = outcome.result
		messages = outcome.messages
		agent = outcome.agent
		activeModel = outcome.activeModel
		swappedToFallback = outcome.swappedToFallback

		finalText := ""
		if finalResult != nil && finalResult.Response.Content != nil {
			finalText = finalResult.Response.Content.Text()
		}

		canFinish, enforcementMsgs := deps.Policy.CanFinish(round)
		if canFinish {
			// Interactive-only finalize hook (leaked-tool-call / forced summary).
			// Stubbed unless the driver supplies an impl.
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
			return Result{
				FinalText:         finalText,
				Rounds:            round + 1,
				SwappedToFallback: swappedToFallback,
				Label:             label,
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
