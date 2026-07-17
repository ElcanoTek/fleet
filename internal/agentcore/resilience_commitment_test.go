package agentcore

// Side-effect-gated stream commitment (ADR-0035): a transient mid-stream
// provider failure is recoverable (in-place retry, then the fallback chain)
// as long as the failing ATTEMPT executed no tool — text/reasoning-only
// output is rolled back and regenerated. Once a tool ran, in-run recovery is
// suppressed and the error carries the transient ErrCommittedSideEffects
// sentinel so the scheduler's whole-task RetryPolicy owns the decision.
//
// These tests drive a REAL streamSink through the resilience loop — the
// regression they pin down (a 504 mid-final-answer dead-lettering a whole
// scheduled run) was invisible to the sink=nil tests in resilience_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// midStream504 is the SSE error event observed in the field (task edfb110c):
// the provider dropped the stream mid-answer with an idle-timeout 504.
const midStream504 = `received error while streaming: {"code":504,"message":"Upstream idle timeout exceeded","metadata":{"error_type":"timeout"}}`

// streamTextThenError yields a text delta and then a mid-stream error part —
// the shape of a provider dying while the model is composing its answer.
func streamTextThenError(text string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: text}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New(midStream504)})
	}
}

// streamTextThenFinish yields a text delta and finishes cleanly.
func streamTextThenFinish(text string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: text}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
		})
	}
}

func TestStreamRoundRetriesBlipAfterTextOnlyOutput(t *testing.T) {
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				if atomic.AddInt32(&primaryCalls, 1) == 1 {
					return streamTextThenError("partial answer, then the provider died: "), nil
				}
				return streamTextThenFinish("clean regenerated answer"), nil
			},
		},
		name: "primary-model",
	}
	fallbackCalls := int32(0)
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return streamTextThenFinish("fallback answer"), nil
			},
		},
		name: "fallback-model",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.resilience = resilienceConfig{maxAttempts: 0}

	sink := newStreamSink(nil)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), sink, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected in-place retry to recover a text-only mid-stream blip, got: %v", err)
	}
	if outcome.swappedToFallback {
		t.Error("expected swappedToFallback=false (same-model retry recovered)")
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 2 {
		t.Errorf("primary called %d times, want 2 (fail mid-text, then succeed)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Errorf("fallback called %d times, want 0", got)
	}
	// The rollback must have discarded the failed attempt's partial text: the
	// accumulated final answer is exactly the retried attempt's output.
	if _, text := sink.snapshot(); text != "clean regenerated answer" {
		t.Errorf("accumulated text = %q, want the regenerated answer only (no duplicated partial)", text)
	}
}

func TestStreamRoundFallsBackAfterTextOnlyBlipPersists(t *testing.T) {
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&primaryCalls, 1)
				return streamTextThenError("primary partial "), nil
			},
		},
		name: "primary-model",
	}
	fallbackCalls := int32(0)
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return streamTextThenFinish("fallback answer"), nil
			},
		},
		name: "fallback-model",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.resilience = resilienceConfig{maxAttempts: 0}

	sink := newStreamSink(nil)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), sink, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected fallback to recover a persistent text-only blip, got: %v", err)
	}
	if !outcome.swappedToFallback {
		t.Error("expected swappedToFallback=true after the in-place retry also blipped")
	}
	if got := outcome.activeModel.Model(); got != "fallback-model" {
		t.Errorf("active model = %q, want fallback-model", got)
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 2 {
		t.Errorf("primary called %d times, want 2 (initial + in-place retry)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
	// Both failed primary attempts were rolled back; only the fallback's
	// regeneration persists.
	if _, text := sink.snapshot(); text != "fallback answer" {
		t.Errorf("accumulated text = %q, want the fallback answer only", text)
	}
}

func TestStreamRoundSuppressesRecoveryAfterToolExecution(t *testing.T) {
	sink := newStreamSink(nil)
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&primaryCalls, 1)
				// Simulate fantasy's callbacks for a tool that EXECUTED during
				// this attempt (side effect committed), then the provider dies.
				sink.onToolCall("call-1", "bash", `{"command":"send the email"}`)
				sink.onToolResult("call-1", "bash", "email sent", false)
				return nil, errors.New(midStream504)
			},
		},
		name: "primary-model",
	}
	fallbackCalls := int32(0)
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return streamStop()(nil, call)
			},
		},
		name: "fallback-model",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.resilience = resilienceConfig{maxAttempts: 0}

	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	_, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), sink, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err == nil {
		t.Fatal("expected suppressed recovery to surface an error")
	}
	if !errors.Is(err, ErrCommittedSideEffects) {
		t.Errorf("error = %v, want errors.Is ErrCommittedSideEffects (transient for the runner's RetryPolicy)", err)
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Errorf("primary called %d times, want 1 (no in-place retry after a tool ran)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Errorf("fallback called %d times, want 0 (failover suppressed)", got)
	}
	// The executed tool's record must survive — rollback never runs on the
	// suppressed path.
	entries, _ := sink.snapshot()
	if len(entries) != 2 || entries[0].Type != "tool_call" || entries[1].Type != "tool_result" {
		t.Errorf("entries = %+v, want the executed tool_call + tool_result preserved", entries)
	}
}

// Context-too-large rides the same side-effect gate: it typically fires
// mid-round right after a large tool result balloons the next request, so the
// compact-and-re-drive recovery would re-issue the executed call. Once a tool
// ran in the attempt, the round must surface ErrCommittedSideEffects instead
// of compacting and re-driving in-run.
func TestStreamRoundSuppressesContextTooLargeRecoveryAfterToolExecution(t *testing.T) {
	sink := newStreamSink(nil)
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&primaryCalls, 1)
				// A tool EXECUTED during this attempt (side effect committed),
				// then the provider rejects the ballooned follow-up request.
				sink.onToolCall("call-1", "bash", `{"command":"send the email"}`)
				sink.onToolResult("call-1", "bash", strings.Repeat("huge result ", 100), false)
				return nil, &fantasy.ProviderError{ContextTooLargeErr: true, Message: "prompt too large"}
			},
		},
		name: "primary-model",
	}
	fallbackCalls := int32(0)
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return streamStop()(nil, call)
			},
		},
		name: "fallback-model",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.resilience = resilienceConfig{maxAttempts: 0}

	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	_, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), sink, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err == nil {
		t.Fatal("expected suppressed recovery to surface an error")
	}
	if !errors.Is(err, ErrCommittedSideEffects) {
		t.Errorf("error = %v, want errors.Is ErrCommittedSideEffects (whole-task RetryPolicy owns the re-run)", err)
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Errorf("primary called %d times, want 1 (no compact-and-re-drive after a tool ran)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Errorf("fallback called %d times, want 0", got)
	}
	// The executed tool's record must survive for the audit trail.
	entries, _ := sink.snapshot()
	if len(entries) != 2 || entries[0].Type != "tool_call" || entries[1].Type != "tool_result" {
		t.Errorf("entries = %+v, want the executed tool_call + tool_result preserved", entries)
	}
}

func TestStreamSinkMarkRollback(t *testing.T) {
	sink := newStreamSink(nil)
	sink.onTextDelta("kept text ")
	sink.onToolCall("c1", "bash", `{}`)
	sink.onToolResult("c1", "bash", "ok", false)

	m := sink.mark()

	sink.onTextDelta("abandoned partial")
	sink.onReasoningStart("r1", "half a thought")
	if sink.toolEventCount() != 2 {
		t.Fatalf("toolEventCount = %d, want 2", sink.toolEventCount())
	}

	sink.rollbackTo(m)

	entries, text := sink.snapshot()
	if text != "kept text " {
		t.Errorf("finalText = %q, want pre-mark text only", text)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want the 2 pre-mark tool entries", len(entries))
	}
	if sink.toolEventCount() != 2 {
		t.Errorf("toolEventCount = %d, want 2 (restored to mark)", sink.toolEventCount())
	}
	// The abandoned attempt's in-flight reasoning buffer is gone: ending it now
	// commits nothing.
	sink.onReasoningEnd("r1", "")
	if entries, _ := sink.snapshot(); len(entries) != 2 {
		t.Errorf("entries after orphan reasoning end = %d, want 2 (no stale commit)", len(entries))
	}
	// Post-rollback accumulation appends cleanly after the kept prefix.
	sink.onTextDelta("regenerated")
	if _, text := sink.snapshot(); !strings.HasPrefix(text, "kept text ") || !strings.HasSuffix(text, "regenerated") {
		t.Errorf("post-rollback text = %q", text)
	}
}

// The committed-side-effects error must carry the partial transcript with it:
// the executed tool's records are the driver's only chance to project the
// side effect into canonical history before the turn seals — discarded, the
// retried turn re-issues the call blind (#798's exact hazard).
func TestStreamErrorResultPreservesCommittedSideEffects(t *testing.T) {
	sink := newStreamSink(nil)
	sink.onToolCall("c1", "bash", `{"command":"send the email"}`)
	sink.onToolResult("c1", "bash", "email sent", false)
	res, err := streamErrorResult(context.Background(),
		fmt.Errorf("%w: provider died", ErrCommittedSideEffects),
		RunConfig{}, sink, nil, "label", nil, false, 1)
	if !errors.Is(err, ErrCommittedSideEffects) {
		t.Fatalf("error = %v, want ErrCommittedSideEffects preserved", err)
	}
	if len(res.Entries) != 2 || res.Entries[0].Type != "tool_call" || res.Entries[1].Type != "tool_result" {
		t.Errorf("entries = %+v, want the executed tool_call + tool_result", res.Entries)
	}
	if res.Cancelled {
		t.Error("a committed-side-effect failure must not read as a user cancel")
	}
}
