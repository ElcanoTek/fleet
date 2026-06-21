package agent

import (
	"context"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// The INTERACTIVE driver: a live chat turn over the unified agentcore.Run loop.
// chat's single pass is the 1-round collapse of the shared loop via an
// InteractivePolicy whose CanFinish returns true at round 0. This file assembles
// the interactive seams and COMPLETES the two interactive items P2 stubbed:
//
//   - the finalize hook (leaked-tool-call retry + forced final summary) wired
//     through agentcore.Run's Deps.Finalize; and
//   - chat's head/summary/tail compaction wired through agentcore.Run's
//     Deps.CompactionSummarizer (overflow-file spill stays in overflow.go's
//     PrepareStep, which the native tools attach).
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

	Model         fantasy.LanguageModel
	FallbackModel fantasy.LanguageModel
	Temperature   float64
	MaxTokens     int

	// PriorHistory / TurnHistory feed the finalize hook's force-summary replay.
	PriorHistory []HistoryEntry
	TurnHistory  []HistoryEntry

	NativeTools []fantasy.AgentTool
	Sandbox     *sandbox.Sandbox

	MaxCostUSD     float64
	MaxTotalTokens int

	// NoteProposer stages agent-proposed admin-notes edits (propose_note),
	// wired in interactive mode too (the notes wiki is global). The user-memory
	// propose_memory path is unchanged. Nil leaves propose_note "not wired".
	NoteProposer agentcore.NoteProposer
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
func RunInteractiveTurn(ctx context.Context, tc TurnConfig, obs agentcore.Observer) (agentcore.Result, error) {
	policy := agentcore.NewInteractivePolicy(tc.MaxCostUSD, tc.MaxTotalTokens, nil, nil)
	if tc.NoteProposer != nil {
		policy.SetNoteProposer(tc.NoteProposer)
	}

	deps := agentcore.Deps{
		Input:                messagesInput{system: tc.SystemPrompt, messages: tc.Messages, label: tc.Label},
		Observer:             obs,
		Policy:               policy,
		Executor:             NewSandboxExecutor(tc.Sandbox),
		Model:                tc.Model,
		FallbackModel:        tc.FallbackModel,
		Finalize:             buildInteractiveFinalize(tc),
		CompactionSummarizer: buildInteractiveCompactionSummarizer(tc),
	}

	cfg := agentcore.RunConfig{
		EnvPrefix:           agentcore.CanonicalEnvPrefix,
		Temperature:         tc.Temperature,
		MaxCompletionTokens: tc.MaxTokens,
		NativeTools:         tc.NativeTools,
		ProviderHeaders:     agentcore.DefaultProviderHeaders,
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
	agent := fantasy.NewAgent(tc.Model,
		fantasy.WithSystemPrompt(in.SystemPrompt),
		fantasy.WithTools(tc.NativeTools...),
		fantasy.WithPrepareStep(chainPrepareSteps(
			overflowTruncationStep(),
			agentcore.PromptCachingStep(tc.Model.Model()),
		)),
	)
	maxTokens := int64(tc.MaxTokens)
	temp := tc.Temperature
	var sb strings.Builder
	_, err := agent.Stream(ctx, fantasy.AgentStreamCall{
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
			overflowTruncationStep(),
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
	agent := fantasy.NewAgent(tc.Model, fantasy.WithSystemPrompt(compactionSummarizeSystemPrompt))
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
