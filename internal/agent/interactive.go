package agent

import (
	"context"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The INTERACTIVE driver: a live chat turn over the unified agentcore.Run loop.
// chat's single pass is the 1-round collapse of the shared loop via an
// InteractivePolicy whose CanFinish returns true at round 0. This file assembles
// the interactive seams and COMPLETES the two interactive items P2 stubbed:
//
//   - the finalize hook (leaked-tool-call retry + forced final summary) wired
//     through agentcore.Run's Deps.Finalize; and
//   - chat's head/summary/tail compaction wired through agentcore.Run's
//     Deps.CompactionSummarizer. The shared model-aware PrepareStep independently
//     bounds replay and inner-loop tool context before every provider request.
//
// The interactive turn-loop's SSE streaming, store persistence, and approval
// staging belong to the httpapi/store layers (P6); here we provide the loop
// wiring + the finalize/compaction hooks the unified runtime needs.

// TurnConfig carries the per-turn inputs the interactive driver needs to build
// an agentcore.Run call. The HTTP layer (P6) resolves the model + history and
// supplies an EventSink-backed Observer.
type TurnConfig struct {
	SystemPrompt string
	// Messages is the replayed conversation history + the new user message
	// (built by replayHistory from the stored HistoryEntry rows).
	Messages []fantasy.Message
	Label    string
	// ConversationID is operator-only panic attribution for the governed run.
	// It is never added to the model prompt or tool arguments.
	ConversationID string

	Model          fantasy.LanguageModel
	FallbackModel  fantasy.LanguageModel
	FallbackModels []fantasy.LanguageModel
	Temperature    float64
	MaxTokens      int
	// HealthRegistry is the cross-turn provider circuit breaker (#267), owned by
	// the Manager and passed on every turn so error frequency accumulates. nil
	// disables the breaker (tests).
	HealthRegistry *agentcore.ProviderHealthRegistry
	// MaxIterations caps the agent steps in the turn's stream (0 = no cap).
	// Wired into agentcore.RunConfig.MaxIterations → the stream's StopWhen.
	MaxIterations int

	// PriorHistory / TurnHistory feed the finalize hook's force-summary replay.
	PriorHistory []HistoryEntry
	TurnHistory  []HistoryEntry

	// NativeTools are the RAW per-turn native tools (tools.NewTurnTools(sb).Tools).
	// agentcore.Run wraps each in the InteractivePolicy gate; do NOT pre-wrap.
	NativeTools []fantasy.AgentTool
	Sandbox     *sandbox.Sandbox

	// MCP wiring: the shared client + the per-conversation opt-in selection +
	// the catalog gates. agentcore.buildFantasyTools registers the MCP tools
	// through the SAME InteractivePolicy gate as the native tools, so MCP calls
	// get cost/repeat/email/approval enforcement in interactive mode too.
	MCPClient       *mcp.Client
	Allowlist       agentcore.MCPAllowlist
	OptionalServers agentcore.MCPOptionalSet
	Selection       agentcore.MCPSelection

	// Overlay is the per-user remote-MCP overlay (#443). When Active, the run
	// advertises the shared catalog merged with the overlay's and dispatches via a
	// compositeBroker that routes the overlay's servers (this user's
	// OAuth-connected servers) to the per-run overlay client. nil = no overlay
	// (behavior identical to before the feature). The Manager owns its lifecycle.
	Overlay *RemoteMCPOverlay

	// Persona is the turn's active persona basename (e.g. "assistant"), used to
	// label persona_tool_blocked audit events. PersonaPolicy is the per-persona
	// tool allowlist (Gate-4, #294): nil keeps current behavior (the persona sees
	// every tool the server/credential gates already permit). When set it NARROWS
	// the registered tool roster before the first LLM call. The Manager resolves
	// it from the bundle manifest's personas: block for the turn's persona.
	Persona       string
	PersonaPolicy *agentcore.PersonaToolPermissions

	MaxCostUSD     float64
	MaxTotalTokens int

	// ApprovalStager / MemoryProposer stage critical tool calls + memory
	// proposals for user confirmation (interactive). Wired onto the
	// InteractivePolicy's orchestration so send_email / risky bash /
	// preview_email / suggest_advanced_model / propose_memory route through the
	// approvals + memory tables.
	ApprovalStager agentcore.ApprovalStager
	MemoryProposer agentcore.MemoryProposer

	// NoteProposer stages agent-proposed admin-notes edits (propose_note),
	// wired in interactive mode too (the notes wiki is global). The user-memory
	// propose_memory path is unchanged. Nil leaves propose_note "not wired".
	NoteProposer agentcore.NoteProposer

	// SkillProposer stages agent-drafted personal skills (propose_skill) for
	// the turn's user to review (docs/SKILLS.md phase 3). Registered in
	// lockstep like propose_note.
	SkillProposer agentcore.SkillProposer

	// ThinkingConfig, when set and Enabled, activates Claude extended thinking
	// (#220) for the turn. nil = off. Resolved by the Manager from the
	// per-conversation override or the global default.
	ThinkingConfig *agentcore.ThinkingConfig

	// TurnJournal is the durable side-effect journal (#798): tool intents
	// commit before dispatch, governed results before the next provider step.
	// nil (evals, tests) = no journaling.
	TurnJournal agentcore.TurnJournal

	// SteerSource is the mid-turn input seam (#785): acknowledged messages
	// inject at PrepareStep boundaries. nil = no steering.
	SteerSource agentcore.SteerSource
}

// messagesInput adapts a pre-built message slice to agentcore.InputSource.
type messagesInput struct {
	system   string
	messages []fantasy.Message
	label    string
}

func (m messagesInput) Prompt(_ context.Context) (string, []fantasy.Message, string, error) {
	return m.system, m.messages, m.label, nil
}

// RunInteractiveTurn drives one live chat turn through the SHARED loop with an
// InteractivePolicy (CanFinish true at round 0 → single pass), the interactive
// finalize hook, and chat's compaction summarizer. obs receives the run events.
//
// MCP tools are registered by agentcore.buildFantasyTools from tc.MCPClient +
// the opt-in Selection, wrapped in the SAME InteractivePolicy gate as the native
// tools, so cost/repeat/email/approval enforcement covers both surfaces.
func RunInteractiveTurn(ctx context.Context, tc TurnConfig, obs agentcore.Observer) (agentcore.Result, error) {
	policy := agentcore.NewInteractivePolicy(tc.MaxCostUSD, tc.MaxTotalTokens, tc.ApprovalStager, tc.MemoryProposer)
	// propose_note as a single agentcore-boundary guarantee: register the tool iff
	// a NoteProposer is wired, so the advertised tool, the gate, and the actual
	// roster stay in lockstep.
	nativeTools := tc.NativeTools
	if tc.NoteProposer != nil {
		policy.SetNoteProposer(tc.NoteProposer)
		nativeTools = append(append([]fantasy.AgentTool{}, nativeTools...), tools.NewProposeNoteTool())
	}
	// propose_skill under the same lockstep guarantee (docs/SKILLS.md phase 3).
	if tc.SkillProposer != nil {
		policy.SetSkillProposer(tc.SkillProposer)
		nativeTools = append(append([]fantasy.AgentTool{}, nativeTools...), tools.NewProposeSkillTool())
	}

	deps := agentcore.Deps{
		Input:                messagesInput{system: tc.SystemPrompt, messages: tc.Messages, label: tc.Label},
		Observer:             obs,
		Policy:               policy,
		Executor:             NewSandboxExecutor(tc.Sandbox),
		Model:                tc.Model,
		FallbackModel:        tc.FallbackModel,
		FallbackModels:       tc.FallbackModels,
		MCPClient:            tc.MCPClient,
		Finalize:             buildInteractiveFinalize(tc),
		CompactionSummarizer: buildInteractiveCompactionSummarizer(tc),
		HealthRegistry:       tc.HealthRegistry,
		TurnJournal:          tc.TurnJournal,
		SteerSource:          tc.SteerSource,
	}

	// Per-user remote-MCP overlay (#443): advertise the shared catalog merged
	// with the overlay's, and dispatch via a compositeBroker so the user's
	// OAuth-connected servers route to the per-run overlay client (holding their
	// bearer) while bundle servers stay on the shared client. The overlay never
	// mutates the shared client, so concurrent users can't cross-contaminate.
	ApplyMCPOverlay(&deps, tc.MCPClient, tc.Overlay)

	cfg := agentcore.RunConfig{
		ConversationID:      tc.ConversationID,
		EnvPrefix:           agentcore.CanonicalEnvPrefix,
		Temperature:         tc.Temperature,
		MaxCompletionTokens: tc.MaxTokens,
		MaxIterations:       tc.MaxIterations,
		NativeTools:         nativeTools,
		Allowlist:           tc.Allowlist,
		OptionalServers:     tc.OptionalServers,
		Selection:           tc.Selection,
		PersonaName:         tc.Persona,
		PersonaPolicy:       tc.PersonaPolicy,
		ProviderHeaders:     agentcore.DefaultProviderHeaders,
		ThinkingConfig:      tc.ThinkingConfig,
	}
	return agentcore.Run(ctx, agentcore.ModeInteractive, cfg, deps)
}

// buildInteractiveFinalize returns the agentcore finalize hook implementing
// chat's two recovery paths:
//
//  1. leaked-tool-call retry — when the model narrated a tool call as prose
//     (`call:...{...}`), strip it; if that empties the reply, re-run WITH tools
//     and the leaked-call nudge so the action actually executes;
//  2. forced final summary — when the turn ended with no user-visible text at
//     all (a run of tool calls and nothing else), make one tool-less call with
//     the force-summary nudge to coax out a written answer.
//
// The hook captures the model + tools + temp + maxTokens so it can stream the
// follow-up calls. Returns recovered final text (empty keeps the loop's text).
func buildInteractiveFinalize(tc TurnConfig) agentcore.FinalizeHook {
	return func(ctx context.Context, in agentcore.FinalizeInput) (string, error) {
		cleaned := stripLeakedToolCalls(strings.TrimSpace(in.FinalText))
		if cleaned != "" {
			// Real text after stripping any stray leaked fragment: keep it.
			if cleaned != strings.TrimSpace(in.FinalText) {
				return cleaned, nil
			}
			return "", nil
		}

		// No user-visible text. If the original reply was a leaked tool call,
		// re-run WITH tools so the intended action actually executes.
		if strings.Contains(in.FinalText, "call:") {
			recovered, err := streamLeakedToolCallRetry(ctx, tc, in)
			if err == nil && recovered != "" {
				return recovered, nil
			}
		}

		// Otherwise force a tool-less written answer from the work already done.
		return streamForceFinalSummary(ctx, tc, in)
	}
}

// streamLeakedToolCallRetry re-runs the turn WITH tools after a leaked call,
// appending the leaked-call nudge, and returns the recovered final text.
func streamLeakedToolCallRetry(ctx context.Context, tc TurnConfig, in agentcore.FinalizeInput) (string, error) {
	convo := append(append([]fantasy.Message{}, in.Messages...), fantasy.NewUserMessage(interactiveLeakedToolCallNudge))
	prepare := chainPrepareSteps(
		agentcore.ModelContextBudgetStep(in.SystemPrompt, in.Tools, tc.MaxTokens),
		agentcore.PromptCachingStep(tc.Model.Model()),
	)
	// Stream under the run's own ceilings: the budget guard blocks the next
	// paid completion once the cost/token ceiling is hit — without it this
	// retry could keep buying steps unboundedly (its tool calls are only
	// soft-blocked by policy, which doesn't stop the paid completions).
	if in.GuardStep != nil {
		prepare = in.GuardStep(prepare)
	}
	agent := fantasy.NewAgent(tc.Model,
		fantasy.WithSystemPrompt(in.SystemPrompt),
		// Reuse agentcore.Run's final governed roster. This keeps the finalize
		// retry inside the same policy/credential/panic boundaries as the main
		// stream instead of re-registering raw driver tools.
		fantasy.WithTools(in.Tools...),
		fantasy.WithPrepareStep(prepare),
	)
	maxTokens := int64(tc.MaxTokens)
	temp := tc.Temperature
	var sb strings.Builder
	_, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Messages:        convo,
		MaxOutputTokens: &maxTokens,
		Temperature:     &temp,
		// The run's step cap (CHAT_MAX_ITERATIONS) applies to the retry too —
		// it streams with tools, so it is a real loop, not a single call.
		StopWhen: in.StopWhen,
		OnTextDelta: func(_, text string) error {
			sb.WriteString(text)
			if in.Observer != nil {
				in.Observer.Observe("text.delta", map[string]any{"text": text})
			}
			return nil
		},
		OnToolCall:   in.OnToolCall,
		OnToolResult: in.OnToolResult,
		// Meter this recovery call into the run's accounting so its tokens/cost
		// are not invisible to the cost chip (#83). Nil-safe.
		OnStepFinish: func(step fantasy.StepResult) error {
			if in.RecordUsage != nil {
				in.RecordUsage(step.Usage, step.ProviderMetadata)
			}
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	return stripLeakedToolCalls(strings.TrimSpace(sb.String())), nil
}

// streamForceFinalSummary makes one tool-less call with the force-summary nudge
// (over the replayed prior+turn history) to coax out a written answer.
func streamForceFinalSummary(ctx context.Context, tc TurnConfig, in agentcore.FinalizeInput) (string, error) {
	convo, err := buildForceSummaryMessages(tc.PriorHistory, tc.TurnHistory)
	if err != nil {
		// Fall back to the loop's messages + the nudge.
		convo = append(append([]fantasy.Message{}, in.Messages...), fantasy.NewUserMessage(interactiveForceFinalSummaryNudge))
	}
	agent := fantasy.NewAgent(tc.Model,
		fantasy.WithSystemPrompt(in.SystemPrompt),
		fantasy.WithPrepareStep(chainPrepareSteps(
			agentcore.ModelContextBudgetStep(in.SystemPrompt, nil, tc.MaxTokens),
			agentcore.PromptCachingStep(tc.Model.Model()),
		)),
	)
	maxTokens := int64(tc.MaxTokens)
	temp := tc.Temperature
	var sb strings.Builder
	_, err = agent.Stream(ctx, fantasy.AgentStreamCall{
		Messages:        convo,
		MaxOutputTokens: &maxTokens,
		Temperature:     &temp,
		OnTextDelta: func(_, text string) error {
			sb.WriteString(text)
			if in.Observer != nil {
				in.Observer.Observe("text.delta", map[string]any{"text": text})
			}
			return nil
		},
		// Meter this recovery call into the run's accounting so its tokens/cost
		// are not invisible to the cost chip (#83). Nil-safe.
		OnStepFinish: func(step fantasy.StepResult) error {
			if in.RecordUsage != nil {
				in.RecordUsage(step.Usage, step.ProviderMetadata)
			}
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	return stripLeakedToolCalls(strings.TrimSpace(sb.String())), nil
}

// interactiveLeakedToolCallNudge / interactiveForceFinalSummaryNudge mirror the
// agent-package finalize.go consts (kept distinct names to avoid colliding with
// the package-level leakedToolCallNudge/forceFinalSummaryNudge that the ported
// finalize.go already defines).
const interactiveLeakedToolCallNudge = leakedToolCallNudge
const interactiveForceFinalSummaryNudge = forceFinalSummaryNudge

// buildInteractiveCompactionSummarizer wires chat's head/summary/tail compaction
// into agentcore's compactionSummarizer hook. When the provider rejects the
// prompt as too large, agentcore drops the middle and inserts this summary —
// here a single tool-less model call condensing the droppable middle into a
// brief, tagged so the cache layer treats it as a stable boundary.
func buildInteractiveCompactionSummarizer(tc TurnConfig) func(context.Context, []fantasy.Message) fantasy.Message {
	return func(ctx context.Context, droppable []fantasy.Message) fantasy.Message {
		summary := summarizeDroppedMiddle(ctx, tc, droppable)
		// Tag with the compaction prefix so promptCachingStep's optional
		// compaction-summary breakpoint can find it.
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] " + summary)
	}
}

// compactionSummaryPrefix matches agentcore's compaction-summary marker so the
// inserted message is recognized as a compaction boundary.
const compactionSummaryPrefix = "[context compaction"

// summarizeDroppedMiddle runs one tool-less call to condense the dropped middle.
// On any failure it returns a deterministic placeholder so compaction always
// produces a structurally-sound summary (matching agentcore's fallback).
func summarizeDroppedMiddle(ctx context.Context, tc TurnConfig, droppable []fantasy.Message) string {
	if tc.Model == nil || len(droppable) == 0 {
		return placeholderCompactionSummary(len(droppable))
	}
	agent := fantasy.NewAgent(tc.Model,
		fantasy.WithSystemPrompt(compactionSummarizeSystemPrompt),
		fantasy.WithPrepareStep(agentcore.ModelContextBudgetStep(compactionSummarizeSystemPrompt, nil, 4096)),
	)
	convo := append(append([]fantasy.Message{}, droppable...), fantasy.NewUserMessage("Produce the summary as instructed above."))
	maxTokens := int64(4096)
	out, err := agent.Generate(ctx, fantasy.AgentCall{
		Messages:        convo,
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		return placeholderCompactionSummary(len(droppable))
	}
	text := strings.TrimSpace(out.Response.Content.Text())
	if text == "" {
		return placeholderCompactionSummary(len(droppable))
	}
	return text
}

func placeholderCompactionSummary(n int) string {
	return strings.TrimSpace(
		"earlier messages were dropped to fit the model's context window after the provider rejected the prompt size.",
	) + " (" + itoa(n) + " messages compacted)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// compactionSummarizeSystemPrompt drives the compaction summary call (chat's
// summarize prompt, trimmed to the compaction use).
const compactionSummarizeSystemPrompt = `You are condensing a chat between a user and an assistant so the conversation can continue with a smaller context.

Produce a structured plain-text summary covering: what the user is trying to accomplish; decisions made; concrete findings (exact file paths, numbers, metric names); open threads; and working artifacts. Be specific and do not speculate. Aim for 200–600 words. Return only the summary text, no preamble.`
