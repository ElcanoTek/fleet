package agentcore

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

// engine holds the per-run model + resilience state the enforcement loop and
// the resilience layer close over. It is the focused agentcore analogue of
// cutlass's much larger Agent struct: only the fields the SHARED run loop +
// resilience need live here. The interactive/scheduled DRIVERS (P3) construct
// an engine, wire the seams, and call Run.
//
// Compaction is parameterized: the heavy head/summary/tail machinery is a
// driver concern (it depends on captain's-log pinning, an LLM summarizer, and
// overflow files), so the engine takes an optional summarizer hook. When none
// is set, forceCompactMessageHistory falls back to a deterministic placeholder
// summary so the context-too-large recovery path is still structurally sound.
type engine struct {
	model         fantasy.LanguageModel
	fallbackModel fantasy.LanguageModel

	resilience resilienceConfig
	logSession *LogSession
	onRetry    fantasy.OnRetryCallback

	// healthRegistry tracks per-model circuit state across runs (#267). When the
	// primary model's circuit is open the round skips straight to the fallback
	// instead of burning attempts on a known-bad model. nil = disabled (no-op).
	healthRegistry *ProviderHealthRegistry

	// providerOptionsFn builds the per-stream OpenRouter ProviderOptions for the
	// active model slug. Defaults to a reasoning-off pin-only build; drivers may
	// override to add reasoning / long-context headers.
	providerOptionsFn func(modelSlug string) fantasy.ProviderOptions

	// temperature + maxRetries plumbing for the round stream.
	temperature float64

	// maxIterations caps the agent steps within a single round's stream (0 = no
	// cap). Wired into AgentStreamCall.StopWhen via stepStopConditions so a model
	// that never stops calling tools is bounded by the configured budget rather
	// than only the per-turn timeout + cost ceiling.
	maxIterations int

	// compactionSummarizer, when set, produces the summary message for a
	// force-compaction. When nil a deterministic placeholder is used.
	compactionSummarizer func(ctx context.Context, droppable []fantasy.Message) fantasy.Message

	// consecutiveCompactions tracks how many force-compactions fired in a row;
	// the resilience pre-flight surfaces ErrContextBudgetExhausted past the cap.
	consecutiveCompactions int

	// envPrefix selects the env-var family (resilience config, kill-switches).
	envPrefix EnvPrefix

	// requireCompactionOptIn, when set by the driver, gates proactive context
	// compaction (#209) behind the FLEET_SCHEDULED_AUTO_COMPACT env var. It is
	// driver-supplied data (scheduled sets it true), never a trunk Mode branch.
	requireCompactionOptIn bool

	// usageReporter, when set, is called after each step with the run's
	// accumulated usage so a driver can ship it out-of-band to an external
	// accountant. Nil for the in-process loop.
	usageReporter func(RunUsage)
}

// ErrContextBudgetExhausted is returned when compaction has fired on
// maxConsecutiveCompactions steps in a row without a clean step in between.
var ErrContextBudgetExhausted = errors.New("context budget exhausted after repeated compaction")

const (
	// maxConsecutiveCompactions caps consecutive force-compactions before the
	// resilience loop surfaces a terminal error.
	maxConsecutiveCompactions = 3
	// compactionKeepTail is how many trailing messages to preserve verbatim.
	compactionKeepTail = 20
)

// providerOptions builds OpenRouter options for the active model slug.
// Reasoning is suppressed for `~`-alias slugs (fantasy drops their thinking
// signatures). The 1M-context beta header is attached when the primary or
// fallback is a long-context Claude slug.
func (e *engine) providerOptions(modelSlug string) fantasy.ProviderOptions {
	if e.providerOptionsFn != nil {
		return e.providerOptionsFn(modelSlug)
	}
	opts := &openrouter.ProviderOptions{}

	primarySlug := ""
	if e.model != nil {
		primarySlug = e.model.Model()
	}
	fallbackSlug := ""
	if e.fallbackModel != nil {
		fallbackSlug = e.fallbackModel.Model()
	}
	if anthropicLongContextSlug(primarySlug) || anthropicLongContextSlug(fallbackSlug) {
		if opts.ExtraBody == nil {
			opts.ExtraBody = make(map[string]any)
		}
		opts.ExtraBody["anthropic_beta"] = []string{"context-1m-2025-08-07"}
	}
	opts.Provider = upstreamPinFor(modelSlug)
	return fantasy.ProviderOptions{openrouter.Name: opts}
}

// compactionHeadLen returns how many leading messages to preserve verbatim
// during compaction (the system prompt + initial task framing).
func compactionHeadLen(messages []fantasy.Message) int {
	if len(messages) == 0 {
		return 0
	}
	// Keep the first user/system framing message.
	return 1
}

// forceCompactMessageHistory runs a head/summary/tail compaction unconditionally
// (the context-too-large recovery path: the provider already rejected the size).
// Uses the engine's compactionSummarizer when set, else a deterministic
// placeholder summary tagged with compactionSummaryPrefix.
func (e *engine) forceCompactMessageHistory(ctx context.Context, messages []fantasy.Message) []fantasy.Message {
	keepHead := compactionHeadLen(messages)
	if len(messages) <= keepHead+compactionKeepTail+1 {
		return messages
	}
	head := append([]fantasy.Message{}, messages[:keepHead]...)
	tailStart := len(messages) - compactionKeepTail
	tail := append([]fantasy.Message{}, messages[tailStart:]...)
	middle := messages[keepHead:tailStart]
	if len(middle) == 0 {
		return messages
	}

	var summary fantasy.Message
	if e.compactionSummarizer != nil {
		summary = e.compactionSummarizer(ctx, middle)
	} else {
		summary = fantasy.NewUserMessage(fmt.Sprintf(
			"%s] %d earlier messages were dropped to fit the model's context window after the provider rejected the prompt size.",
			compactionSummaryPrefix, len(middle)))
	}

	out := make([]fantasy.Message, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, summary)
	out = append(out, tail...)
	e.consecutiveCompactions++
	return out
}

// proactiveCompactResult reports what proactiveCompact did, so the run loop can
// emit a fleet.context_compacted event with the right metadata.
type proactiveCompactResult struct {
	messages      []fantasy.Message
	removedTurns  int  // how many old messages were summarized away
	summaryTokens int  // estimated token size of the inserted summary
	compacted     bool // false when there was nothing worth compacting
}

// proactiveCompact summarizes the OLDEST HALF of the conversation history BEFORE
// the provider rejects an oversized prompt (#209), preserving the pinned head
// and the recent half verbatim. It mirrors forceCompactMessageHistory's
// head + summary + kept structure but, instead of reacting to a rejection with a
// fixed 20-message tail, it drops the oldest 50% of the messages after the head.
//
// It uses the engine's compactionSummarizer when set, else the deterministic
// placeholder (tagged with compactionSummaryPrefix, so the cache layer treats it
// identically to a reactive summary). A clean proactive compaction RESETS
// consecutiveCompactions: it is not a reactive resilience compaction and must not
// count toward the ErrContextBudgetExhausted cap.
func (e *engine) proactiveCompact(ctx context.Context, messages []fantasy.Message) proactiveCompactResult {
	head := compactionHeadLen(messages)
	active := messages[head:]
	midpoint := len(active) / 2
	// Nothing worth doing if there is fewer than one droppable message or the
	// kept half would be empty.
	if midpoint < 1 || len(active)-midpoint < 1 {
		return proactiveCompactResult{messages: messages}
	}
	droppable := active[:midpoint]
	keep := active[midpoint:]

	var summary fantasy.Message
	if e.compactionSummarizer != nil {
		summary = e.compactionSummarizer(ctx, droppable)
	} else {
		summary = fantasy.NewUserMessage(fmt.Sprintf(
			"%s] %d earlier messages were summarized to relieve context-window pressure before the prompt overflowed.",
			compactionSummaryPrefix, len(droppable)))
	}

	out := make([]fantasy.Message, 0, head+1+len(keep))
	out = append(out, messages[:head]...)
	out = append(out, summary)
	out = append(out, keep...)

	// Proactive compaction is a clean reduction, not a reactive resilience
	// compaction — reset the consecutive counter so it never trips
	// ErrContextBudgetExhausted.
	e.consecutiveCompactions = 0

	return proactiveCompactResult{
		messages:      out,
		removedTurns:  len(droppable),
		summaryTokens: estimateMessageTokens(summary),
		compacted:     true,
	}
}

// estimateMessageTokens gives a rough token count for a message by summing its
// text-part lengths and dividing by 4 (the usual ~4-chars-per-token heuristic).
// Used only for the fleet.context_compacted event metadata and the pre-first-step
// pressure estimate (estimateMessagesTokens), never for billing.
func estimateMessageTokens(m fantasy.Message) int {
	chars := 0
	for _, part := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			chars += len(tp.Text)
		}
	}
	return chars / 4
}

// estimateMessagesTokens sums estimateMessageTokens across the whole history.
// It is the pre-first-step fallback for the context-pressure check (#209): at
// the top of a turn's first round no provider-reported per-call input size
// exists yet (LastStepPromptTokens is 0), so a single-round interactive turn
// that STARTS near the window would otherwise never be checked. This estimate
// of the carried-over history fills that gap. It is deliberately a lower bound
// (it omits the system prompt + tool schemas the provider also counts), so it
// errs toward warning/compacting slightly late rather than spuriously early.
func estimateMessagesTokens(messages []fantasy.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

// contextPressureResult carries a pre-round context-pressure check's outcome
// back to the run loop: the possibly-compacted history and the updated
// warn-dedupe flag.
type contextPressureResult struct {
	messages []fantasy.Message
	warned   bool
}

// checkContextPressure runs the #209 pre-round context-window pressure check:
// it warns as the prompt nears the active model's window and, above the
// compaction threshold, proactively summarizes the oldest history — BEFORE the
// provider can reject an oversized request.
//
// The size signal is LastStepPromptTokens (the per-CALL input size, overwritten
// each step), never the cumulative PromptTokens, which only grows and would
// ratchet the trigger into a compaction spiral (see the session_log
// token-semantics doc). Before a turn's first step it is 0, so we fall back to a
// char-heuristic estimate of the carried-over history: that is what makes the
// check fire on a single-round interactive turn that STARTS near the window, not
// only on multi-round runs where a prior step reported real usage.
//
// A driver may gate compaction behind an opt-in (requireCompactionOptIn): an
// unattended scheduled run must not silently rewrite its own transcript, so the
// scheduled driver sets that flag and compaction only fires when
// FLEET_SCHEDULED_AUTO_COMPACT is also set — otherwise the run only warns (event
// + a session-log breadcrumb). This is driver-supplied data, NOT a trunk Mode
// branch (see TestSeamPurity_NoModeBranchInTrunk). The `warned` in/out flag
// dedupes the warning across rounds; a successful compaction relieves the
// pressure and resets it.
func (e *engine) checkContextPressure(ctx context.Context, messages []fantasy.Message, activeModel fantasy.LanguageModel, sink *streamSink, warned bool) contextPressureResult {
	out := contextPressureResult{messages: messages, warned: warned}
	if activeModel == nil || e.logSession == nil {
		return out
	}
	window := contextWindowForModel(activeModel.Model())
	if window <= 0 {
		return out
	}
	used := e.logSession.LastStepPromptTokens
	if used <= 0 {
		used = estimateMessagesTokens(messages)
	}
	if used <= 0 {
		return out
	}

	pct := float64(used) / float64(window)
	pressure := map[string]any{evtFieldUsedTokens: used, evtFieldWindowSize: window, evtFieldPct: pct}

	switch {
	case pct >= contextCompactionThreshold(e.envPrefix):
		if e.requireCompactionOptIn && !e.envPrefix.lookupBool("SCHEDULED_AUTO_COMPACT") {
			if !out.warned {
				sink.emit(evtContextPressure, pressure)
				e.logSession.AddMessage(roleUser, fmt.Sprintf(
					"[context_pressure] used=%d window=%d pct=%.2f — set %s_SCHEDULED_AUTO_COMPACT=1 to enable proactive compaction",
					used, window, pct, e.envPrefix.normalize()), nil, nil)
				out.warned = true
			}
			return out
		}
		if res := e.proactiveCompact(ctx, messages); res.compacted {
			out.messages = res.messages
			out.warned = false
			sink.emit(evtContextCompacted, map[string]any{
				evtFieldRemovedTurns:  res.removedTurns,
				evtFieldSummaryTokens: res.summaryTokens,
			})
		} else if !out.warned {
			// Nothing compactible (e.g. one enormous message: head + a single
			// active turn — proactiveCompact's documented no-op). Still surface
			// the pressure so the worst case isn't silent; the reactive overflow
			// path is the only backstop left.
			sink.emit(evtContextPressure, pressure)
			out.warned = true
		}
	case pct >= contextPressureWarnThreshold(e.envPrefix):
		if !out.warned {
			sink.emit(evtContextPressure, pressure)
			out.warned = true
		}
	}
	return out
}

// dropTrailingAssistant strips the final message when it is an assistant
// message, so a fallback model doesn't see a half-finished/errored response as
// its own last turn.
func dropTrailingAssistant(messages []fantasy.Message) []fantasy.Message {
	if len(messages) == 0 {
		return messages
	}
	last := messages[len(messages)-1]
	if last.Role != fantasy.MessageRoleAssistant {
		return messages
	}
	return messages[:len(messages)-1]
}

// canSwapFallback reports whether the fallback model is a meaningful alternative
// to the currently active model. Compared by slug to avoid panicking on
// unhashable interface values.
func canSwapFallback(e *engine, activeModel fantasy.LanguageModel, alreadySwapped bool) bool {
	if alreadySwapped {
		return false
	}
	if e == nil || e.fallbackModel == nil || activeModel == nil {
		return false
	}
	return e.fallbackModel.Model() != activeModel.Model()
}

// roundState carries the per-round streaming accumulators. Lean compared to
// cutlass's roundState: the SHARED loop needs text capture + usage accounting;
// the rich live-log / reasoning rendering flows through the run-wide streamSink
// (the Observer + history bridge), which is shared across rounds.
type roundState struct {
	engine    *engine
	orch      *orchestrationState
	maxTokens int64

	// sink forwards this round's streamed events to the Observer and
	// accumulates the run history. Shared across rounds so a multi-round
	// scheduled run builds one coherent transcript. nil is tolerated (the
	// callbacks become no-ops) so the lifted parity tests that build a
	// roundState directly keep working.
	sink *streamSink

	// activeModelSlug is the model this round actually streams with (differs
	// from engine.model after a fallback swap).
	activeModelSlug string
}

func newRoundState(e *engine, orch *orchestrationState, maxTokens int64) *roundState {
	return &roundState{engine: e, orch: orch, maxTokens: maxTokens}
}

// ErrCostCeilingExceeded is returned by the budget-guarded PrepareStep to abort
// the stream BEFORE the next paid LLM completion once the run's cost/token
// ceiling is met. The run loop catches it and finishes GRACEFULLY with the
// transcript accumulated so far (it is a clean stop, not a failure) — the same
// way a ctx-cancellation is handled. This is the hard backstop behind the
// soft BeforeToolCall nudge: a model that ignores the nudge (or emits prose with
// no tool call) is still bounded by the budget, matching cutlass's prepareStep
// checkBudget. Sentinel so the loop can errors.Is it.
var ErrCostCeilingExceeded = errors.New("cost/token ceiling exceeded")

// budgetGuardedStep wraps a PrepareStep with a pre-completion ceiling check.
// When the orchestration's ceiling is already met it returns ErrCostCeilingExceeded
// so fantasy aborts before the next completion; otherwise it defers to inner.
func budgetGuardedStep(orch *orchestrationState, inner fantasy.PrepareStepFunction) fantasy.PrepareStepFunction {
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		if orch != nil {
			if blocked, msg := orch.checkCeilings(); blocked {
				return ctx, fantasy.PrepareStepResult{}, fmt.Errorf("%w: %s", ErrCostCeilingExceeded, msg)
			}
		}
		return inner(ctx, opts)
	}
}

// stepStopConditions turns the configured per-round step cap into a fantasy
// stop condition. Zero (or negative) means "no cap" — loop until the model
// stops on its own (bounded by the per-turn timeout + cost ceiling). This is
// what wires CHAT_MAX_ITERATIONS / a task's max_iterations into the loop; before
// it, the value was read into config but never applied, so a model that never
// stopped calling tools ran unbounded within a round.
func stepStopConditions(maxIterations int) []fantasy.StopCondition {
	if maxIterations <= 0 {
		return nil
	}
	return []fantasy.StopCondition{fantasy.StepCountIs(maxIterations)}
}

// stream drives one fantasy stream call for the round, wiring the resilience
// retry budget, usage accounting, the prompt-cache prepare step, AND the full
// streaming bridge: text / reasoning / tool-call / tool-result callbacks forward
// to the run's Observer and accumulate into the run history (the part chat's
// session.go::RunTurn owned, now shared by both modes through streamSink).
func (r *roundState) stream(ctx context.Context, ag fantasy.Agent, activeModel fantasy.LanguageModel, messages []fantasy.Message) (*fantasy.AgentResult, error) {
	maxRetries := r.engine.resilience.maxAttempts
	temp := r.engine.temperature
	modelSlug := ""
	if activeModel != nil {
		modelSlug = activeModel.Model()
	}
	r.activeModelSlug = modelSlug
	sink := r.sink
	return ag.Stream(ctx, fantasy.AgentStreamCall{
		Messages:        messages,
		MaxOutputTokens: &r.maxTokens,
		Temperature:     &temp,
		ProviderOptions: r.engine.providerOptions(modelSlug),
		MaxRetries:      &maxRetries,
		StopWhen:        stepStopConditions(r.engine.maxIterations),
		OnRetry:         r.engine.onRetry,
		OnTextDelta: func(_, text string) error {
			if sink != nil {
				sink.onTextDelta(text)
			}
			return nil
		},
		OnReasoningStart: func(id string, content fantasy.ReasoningContent) error {
			if sink != nil {
				sink.onReasoningStart(id, content.Text)
			}
			return nil
		},
		OnReasoningDelta: func(id, text string) error {
			if sink != nil {
				sink.onReasoningDelta(id, text)
			}
			return nil
		},
		OnReasoningEnd: func(id string, content fantasy.ReasoningContent) error {
			if sink != nil {
				sink.onReasoningEnd(id, content.Text)
			}
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			if sink != nil {
				sink.onToolCall(tc.ToolCallID, tc.ToolName, tc.Input)
			}
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			if sink != nil {
				text, isErr := toolResultText(tr)
				sink.onToolResult(tr.ToolCallID, tr.ToolName, text, isErr)
			}
			return nil
		},
		OnStepFinish: func(step fantasy.StepResult) error {
			r.orch.updateUsage(step.Usage, step.ProviderMetadata)
			if r.engine.usageReporter != nil {
				r.engine.usageReporter(usageSnapshot(r.orch))
			}
			return nil
		},
		PrepareStep: budgetGuardedStep(r.orch, promptCachingStep(modelSlug, WithCacheEnvPrefix(r.engine.envPrefix))),
	})
}
