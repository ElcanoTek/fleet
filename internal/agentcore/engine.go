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

	// providerOptionsFn builds the per-stream OpenRouter ProviderOptions for the
	// active model slug. Defaults to a reasoning-off pin-only build; drivers may
	// override to add reasoning / long-context headers.
	providerOptionsFn func(modelSlug string) fantasy.ProviderOptions

	// temperature + maxRetries plumbing for the round stream.
	temperature float64

	// compactionSummarizer, when set, produces the summary message for a
	// force-compaction. When nil a deterministic placeholder is used.
	compactionSummarizer func(ctx context.Context, droppable []fantasy.Message) fantasy.Message

	// consecutiveCompactions tracks how many force-compactions fired in a row;
	// the resilience pre-flight surfaces ErrContextBudgetExhausted past the cap.
	consecutiveCompactions int

	// envPrefix selects the env-var family (resilience config, kill-switches).
	envPrefix EnvPrefix
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
			return nil
		},
		PrepareStep: promptCachingStep(modelSlug, WithCacheEnvPrefix(r.engine.envPrefix)),
	})
}
