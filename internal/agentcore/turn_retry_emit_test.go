package agentcore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// #833: turn.retry had consumers on both ends — journal recovery resets
// accumulated text on it (so a rolled-back attempt's discarded partial output
// is not projected into recovered history) and the web client renders an
// inline retry badge — but nothing ever emitted it. These tests pin the two
// producers: the rollbackAttempt re-drive and fantasy's inner-retry backoff.

type retryEventRecorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func (o *retryEventRecorder) Observe(eventType string, payload map[string]any) {
	if eventType != "turn.retry" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, payload)
}

func (o *retryEventRecorder) retries() []map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]map[string]any(nil), o.events...)
}

// A mid-stream blip that succeeds on the in-place re-drive must emit exactly
// one turn.retry — the marker journal recovery uses to drop the abandoned
// pre-retry partial.
func TestStreamRoundEmitsTurnRetryOnBlipRedrive(t *testing.T) {
	calls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				if atomic.AddInt32(&calls, 1) == 1 {
					return nil, errors.New(`received error while streaming: {"code":502,"message":"Network connection lost."}`)
				}
				return streamStop()(nil, call)
			},
		},
		name: "primary-model",
	}

	e := newMockEngine(t, primary)
	e.resilience = resilienceConfig{maxAttempts: 0}

	rec := &retryEventRecorder{}
	sink := newStreamSink(rec)
	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}

	_, err := e.streamRoundWithResilience(
		context.Background(), orch, sink, 1000,
		[]fantasy.Message{fantasy.NewUserMessage("test task")}, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected success via in-place retry, got: %v", err)
	}

	retries := rec.retries()
	if len(retries) != 1 {
		t.Fatalf("turn.retry emitted %d times, want 1", len(retries))
	}
	if got, ok := retries[0]["status_code"].(int); !ok || got != 502 {
		t.Errorf("turn.retry status_code = %v, want 502", retries[0]["status_code"])
	}
	if _, ok := retries[0]["delay_ms"]; !ok {
		t.Error("turn.retry payload is missing delay_ms (RetryEventPayload contract)")
	}
}

// A fallback swap also re-drives after rolling back the failed attempt, so it
// too must mark the discard point.
func TestStreamRoundEmitsTurnRetryOnFallbackSwap(t *testing.T) {
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				return nil, errors.New(`received error while streaming: {"code":502,"message":"Network connection lost."}`)
			},
		},
		name: "primary-model",
	}
	fallback := &namedMockModel{
		mockModel: mockModel{streamFunc: streamStop()},
		name:      "fallback-model",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.resilience = resilienceConfig{maxAttempts: 0}

	rec := &retryEventRecorder{}
	sink := newStreamSink(rec)
	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), orch, sink, 1000,
		[]fantasy.Message{fantasy.NewUserMessage("test task")}, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected success via fallback, got: %v", err)
	}
	if !outcome.swappedToFallback {
		t.Fatal("expected a fallback swap")
	}
	if got := len(rec.retries()); got < 1 {
		t.Fatalf("turn.retry emitted %d times, want >= 1 (each re-drive marks its discard point)", got)
	}
}

// The payload builder honors the documented RetryEventPayload shape and is
// nil-safe on both the sink and the provider error.
func TestEmitTurnRetryPayload(t *testing.T) {
	emitTurnRetry(nil, nil, time.Second) // must not panic

	rec := &retryEventRecorder{}
	sink := newStreamSink(rec)
	emitTurnRetry(sink, nil, 1500*time.Millisecond)
	emitTurnRetry(sink, &fantasy.ProviderError{StatusCode: 429, Message: "slow down"}, 2*time.Second)

	retries := rec.retries()
	if len(retries) != 2 {
		t.Fatalf("emitted %d events, want 2", len(retries))
	}
	if got := retries[0]["delay_ms"].(int64); got != 1500 {
		t.Errorf("delay_ms = %v, want 1500", got)
	}
	if _, ok := retries[0]["status_code"]; ok {
		t.Error("nil provider error must not fabricate a status_code")
	}
	if got := retries[1]["status_code"].(int); got != 429 {
		t.Errorf("status_code = %v, want 429", got)
	}
	if got, _ := retries[1]["title"].(string); got == "" {
		t.Error("title must be derived from the status code when the provider omits one")
	}
}
