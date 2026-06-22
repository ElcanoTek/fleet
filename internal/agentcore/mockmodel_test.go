package agentcore

// Mock language model + test engine builder (lifted+adapted from cutlass
// execute_test.go). cutlass built a full *Agent; agentcore's focused engine
// holds only the resilience-relevant fields, so newMockEngine returns *engine.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"charm.land/fantasy"
)

type mockModel struct {
	mu         sync.Mutex
	callCount  int
	streamFunc func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error)
}

func (m *mockModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: "mock"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (m *mockModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()
	if m.streamFunc != nil {
		return m.streamFunc(ctx, call)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 100, OutputTokens: 20},
		})
	}, nil
}

func (m *mockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockModel) Provider() string { return "mock" }
func (m *mockModel) Model() string    { return "mock-model" }

// namedMockModel overrides Model() so fallback-swap tests can distinguish the
// primary and fallback by slug (canSwapFallback compares by Model() string).
type namedMockModel struct {
	mockModel
	name string
}

func (m *namedMockModel) Model() string { return m.name }

// streamStop builds a stream that finishes immediately with text.
func streamStop() func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{
				Type:         fantasy.StreamPartTypeFinish,
				FinishReason: fantasy.FinishReasonStop,
				Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
			})
		}, nil
	}
}

// newMockEngine builds an agentcore engine wired to a mock model with fantasy's
// inner retry disabled (maxAttempts=0) so retry-exhaustion tests don't wait on
// real backoff.
func newMockEngine(t *testing.T, model fantasy.LanguageModel) *engine {
	t.Helper()
	session := NewLogSession()
	return &engine{
		model:         model,
		fallbackModel: model,
		logSession:    session,
		resilience:    resilienceConfig{maxAttempts: 0},
		onRetry:       newRetryLogger(session),
	}
}
