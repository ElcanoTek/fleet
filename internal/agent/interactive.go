package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcp"
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

	MaxCostUSD     float64
	MaxTotalTokens int

	// Runtime selects the execution flavor for this turn: "" / "native-inprocess"
	// run today's in-process loop (default + parity oracle); "native-acp" routes
	// the SAME loop through a sandboxed ACP agent (acpruntime.ClientRuntime) over
	// podman-stdio, with tool execution delegated back to the host sandbox
	// (governed identically). NativeAgentImage names the agent image.
	Runtime          string
	NativeAgentImage string

	// RuntimeFlavor is the resolved clientconfig descriptor for Runtime. For an
	// EXTERNAL (type: acp) flavor it carries the provider image, the model_only
	// egress posture, the delegated-policy bit, and the model-cred env var names
	// the external path needs to spawn the provider's agent at the containment
	// tier.
	RuntimeFlavor clientconfig.Runtime

	// PermissionBroker routes an EXTERNAL agent's session/request_permission to a
	// human (default-deny on timeout, no approve-all). Nil for the native
	// flavors. Required for the external flavor; nil fails permission requests
	// closed (deny).
	PermissionBroker acpruntime.PermissionBroker

	// Lockdown mirrors the conversation's lockdown bit. native-acp does not yet
	// reproduce the lockdown no-network guarantee for the agent container, so a
	// lockdown turn falls back to the in-process path (see acpInteractiveFallback).
	Lockdown bool

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
	// native-acp routes the SAME loop through a sandboxed ACP agent. The host
	// keeps governance for the tool-execution surface: bash/python delegate back
	// to the host sandbox (tc.Sandbox) via the real Executor, and obs receives
	// the same run events.
	//
	// HONESTY GATE: the P-ACP-1 interactive ACP path does NOT yet reproduce the
	// full in-process governance surface — approval staging (send_email / risky
	// bash / preview_email / suggest_advanced_model), memory/note proposals, MCP
	// tools + per-task credential brokering, and the lockdown no-network
	// guarantee for the agent container. Rather than SILENTLY under-govern, fall
	// back to the fully-governed in-process loop whenever any of those features
	// is active for this turn. native-acp then runs only when it can behave AND
	// govern identically (no MCP selection, no approval/memory/note staging, not
	// lockdown). The remaining surfaces land in P-ACP-2.
	if tc.Runtime == clientconfig.RuntimeNativeACP {
		if reason := acpInteractiveFallback(tc); reason != "" {
			log.Printf("native-acp: falling back to in-process for this turn (%s)", reason)
		} else {
			return runInteractiveTurnACP(ctx, tc, obs)
		}
	}

	// An EXTERNAL (type: acp) flavor drives a third-party provider's agent
	// (Claude Code / Goose) at the CONTAINMENT tier: it self-executes in a
	// locked, model-only-egress sandbox, fleet observes (does not enforce) its
	// self-report, and session/request_permission is routed to a human. This is a
	// distinct, weaker trust posture from native — never silently conflated. We
	// route here whenever the resolved flavor is acp-typed.
	if tc.RuntimeFlavor.Type == clientconfig.RuntimeTypeACP {
		return runExternalTurnACP(ctx, tc, obs)
	}

	policy := agentcore.NewInteractivePolicy(tc.MaxCostUSD, tc.MaxTotalTokens, tc.ApprovalStager, tc.MemoryProposer)
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
		MCPClient:            tc.MCPClient,
		Finalize:             buildInteractiveFinalize(tc),
		CompactionSummarizer: buildInteractiveCompactionSummarizer(tc),
	}

	cfg := agentcore.RunConfig{
		EnvPrefix:           agentcore.CanonicalEnvPrefix,
		Temperature:         tc.Temperature,
		MaxCompletionTokens: tc.MaxTokens,
		NativeTools:         tc.NativeTools,
		Allowlist:           tc.Allowlist,
		OptionalServers:     tc.OptionalServers,
		Selection:           tc.Selection,
		ProviderHeaders:     agentcore.DefaultProviderHeaders,
	}
	return agentcore.Run(ctx, agentcore.ModeInteractive, cfg, deps)
}

// acpInteractiveFallback returns a non-empty reason when the turn uses a
// governed feature the P-ACP-1 interactive ACP path cannot yet reproduce, so
// the caller falls back to the fully-governed in-process loop instead of
// silently under-governing. Empty = native-acp may run.
func acpInteractiveFallback(tc TurnConfig) string {
	switch {
	case tc.Lockdown:
		// The agent container is not yet sealed to no-network for lockdown.
		return "lockdown conversation"
	case len(tc.Selection) > 0:
		// MCP tools + per-task credential brokering are not yet delegated.
		return "MCP servers enabled"
	case tc.ApprovalStager != nil:
		// Critical-tool approval staging (send_email / risky bash / …) is not
		// yet wired through the ACP permission surface.
		return "approval staging active"
	case tc.MemoryProposer != nil || tc.NoteProposer != nil:
		// Memory / note proposal staging is not yet delegated.
		return "memory/note proposal staging active"
	default:
		return ""
	}
}

// runInteractiveTurnACP drives one interactive turn through the native-acp
// flavor: a sandboxed ACP agent (acpruntime.ClientRuntime) runs the SAME
// agentcore.Run loop, but its tool execution delegates back to the HOST sandbox
// (tc.Sandbox, via the real Executor) and its streamed events flow to obs — so
// the turn is governed identically to the in-process path. The host owns the
// sandbox; the agent container holds no executor (no Podman-in-Podman).
func runInteractiveTurnACP(ctx context.Context, tc TurnConfig, obs agentcore.Observer) (agentcore.Result, error) {
	if tc.NativeAgentImage == "" {
		return agentcore.Result{}, fmt.Errorf("native-acp runtime selected but no agent image configured")
	}

	messagesJSON, err := json.Marshal(tc.Messages)
	if err != nil {
		return agentcore.Result{}, fmt.Errorf("marshal turn messages: %w", err)
	}

	rt := acpruntime.NewClientRuntime(acpruntime.ClientConfig{
		Image: tc.NativeAgentImage,
		// Pass ONLY the model-endpoint credentials into the agent container —
		// its one allowed egress. MCP creds are never shipped (host-brokered).
		ModelEnv: modelEndpointEnv(),
	})
	res, err := rt.Run(ctx,
		acpruntime.RunSpec{
			Mode:                agentcore.ModeInteractive.String(),
			ModelSlug:           slugOf(tc.Model),
			FallbackSlug:        slugOf(tc.FallbackModel),
			SystemPrompt:        tc.SystemPrompt,
			Temperature:         tc.Temperature,
			MaxTokens:           tc.MaxTokens,
			MaxCostUSD:          tc.MaxCostUSD,
			MaxTotalTokens:      tc.MaxTotalTokens,
			Label:               tc.Label,
			ProviderXTitle:      agentcore.DefaultProviderHeaders.XTitle,
			ProviderHTTPReferer: agentcore.DefaultProviderHeaders.HTTPReferer,
		},
		latestUserText(tc.Messages),
		acpruntime.PromptMeta{MessagesJSON: string(messagesJSON)},
		acpruntime.Deps{
			Executor: NewSandboxExecutor(tc.Sandbox),
			Observer: obs,
		},
	)
	if err != nil {
		return agentcore.Result{}, err
	}

	// Map the ACP result onto an agentcore.Result. The final reply is persisted
	// as a single assistant text entry (the streamed text already reached obs →
	// SSE). Token/cost accounting for the ACP path lands in P-ACP-1's follow-up;
	// the turn behaves + governs identically here.
	entries := []agentcore.RunEntry{}
	if res.FinalText != "" {
		entries = append(entries, agentcore.RunEntry{Role: "assistant", Type: "text", Text: res.FinalText})
	}
	return agentcore.Result{
		FinalText: res.FinalText,
		Rounds:    1,
		Label:     tc.Label,
		Entries:   entries,
		ModelSlug: slugOf(tc.Model),
		Cancelled: res.Cancelled,
	}, nil
}

// runExternalTurnACP drives one interactive turn through an EXTERNAL acp flavor:
// fleet's ExternalRuntime spawns the provider's agent (Claude Code / Goose) in a
// locked, model-only-egress sandbox and drives it over ACP. The external agent
// SELF-EXECUTES — it does not delegate to the host. fleet stamps governance:
// delegated, captures the agent's self-reported session/update stream onto obs
// (→ SSE; the containment-tier audit), and routes session/request_permission to
// the human via tc.PermissionBroker (default-deny on timeout, no approve-all).
//
// Honesty: this is NOT the governed tier. fleet does not apply per-tool policy,
// notes, or MCP-credential brokering, and the agent may transmit the workspace
// to its own model endpoint. The trade-off and the data-residency caveat are
// documented in docs/USING-AGENTS.md and stamped in the session log.
func runExternalTurnACP(ctx context.Context, tc TurnConfig, obs agentcore.Observer) (agentcore.Result, error) {
	flavor := tc.RuntimeFlavor
	if flavor.Image == "" {
		return agentcore.Result{}, fmt.Errorf("external acp flavor %q selected but no agent image configured", flavor.Name)
	}

	rt := acpruntime.NewExternalRuntime(acpruntime.ExternalConfig{
		Image: flavor.Image,
		Args:  flavor.Args,
		// SCRUBBED env: ONLY the provider's own model-endpoint credential(s),
		// named by the manifest's model_env. fleet secrets / MCP creds are never
		// shipped to an external agent.
		ProviderEnv: providerEnv(flavor.ModelEnv),
	})

	res, err := rt.Run(ctx, latestUserText(tc.Messages), acpruntime.ExternalDeps{
		Observer:         obs,
		PermissionBroker: tc.PermissionBroker,
	})
	if err != nil {
		return agentcore.Result{}, err
	}

	entries := []agentcore.RunEntry{}
	if res.FinalText != "" {
		entries = append(entries, agentcore.RunEntry{Role: "assistant", Type: "text", Text: res.FinalText})
	}
	return agentcore.Result{
		FinalText: res.FinalText,
		Rounds:    1,
		Label:     tc.Label,
		Entries:   entries,
		ModelSlug: flavor.Name, // external: the flavor name, not an OpenRouter slug
		Cancelled: res.Cancelled,
	}, nil
}

// providerEnv reads the named env vars from the host environment and returns the
// set that is present. These are the external provider's OWN model-endpoint
// credentials (e.g. ANTHROPIC_API_KEY) — the only secrets that enter an external
// agent's container. A missing var is simply omitted (the provider agent will
// surface its own auth error), never substituted.
func providerEnv(names []string) map[string]string {
	env := map[string]string{}
	for _, k := range names {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			env[k] = v
		}
	}
	return env
}

// slugOf returns a model's OpenRouter slug, or "" when nil.
func slugOf(m fantasy.LanguageModel) string {
	if m == nil {
		return ""
	}
	return m.Model()
}

// modelEndpointEnv collects the model-endpoint env vars the native ACP agent
// needs to drive the LLM loop inside its container: the OpenRouter API key and
// (for dev/E2E) the base-URL override. These are the ONLY secrets that enter
// the agent container — MCP credentials are brokered host-side at delegation
// and never shipped in. Vendor-named (un-prefixed), matching how the rest of
// the codebase reads them.
func modelEndpointEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"OPENROUTER_API_KEY", "OPENROUTER_BASE_URL"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			env[k] = v
		}
	}
	return env
}

// latestUserText returns the text of the last user message in the slice (the new
// turn), for the ACP prompt's spec-required content block. The full replayed
// history rides PromptMeta.
func latestUserText(messages []fantasy.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		var sb strings.Builder
		for _, part := range m.Content {
			if tp, ok := part.(fantasy.TextPart); ok {
				sb.WriteString(tp.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
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
