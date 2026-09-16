package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

func TestStreamRecoveryContinuesAfterCompletedToolStep(t *testing.T) {
	var writes atomic.Int32
	tool := fantasy.NewAgentTool("save_record", "save a record",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			writes.Add(1)
			return fantasy.NewTextResponse(`{"saved":true,"revision":91}`), nil
		})
	primary := &namedMockModel{name: "primary"}
	var primaryCalls atomic.Int32
	primary.streamFunc = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		if primaryCalls.Add(1) == 1 {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "thinking"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "thinking", Delta: "provider-specific reasoning"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "thinking"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "saved-once", ToolCallName: "save_record", ToolCallInput: `{}`})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 20, OutputTokens: 10}})
			}, nil
		}
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "failed", Delta: "abandoned response"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{Title: "stream error", Message: "Provider returned error", ResponseBody: []byte(`{"error":{"code":503,"message":"Provider returned error"}}`)}})
		}, nil
	}
	fallback := &namedMockModel{name: "fallback"}
	fallback.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		systems, results := 0, 0
		for _, msg := range call.Prompt {
			if msg.Role == fantasy.MessageRoleSystem {
				systems++
			}
			for _, part := range msg.Content {
				if _, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
					t.Error("replayed provider-specific reasoning")
				}
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && tr.ToolCallID == "saved-once" {
					results++
				}
			}
		}
		if systems != 1 || results != 1 {
			t.Errorf("resumed history: systems=%d results=%d", systems, results)
		}
		return streamTextThenFinish("verified saved revision"), nil
	}
	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.maxIterations = 5
	sink := newStreamSink(nil)
	build := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"), fantasy.WithTools(tool))
	}
	orch := newOrchestrationState(e.logSession, 50)
	out, err := e.streamRoundWithResilience(context.Background(), orch, sink, 1000,
		[]fantasy.Message{fantasy.NewUserMessage("save and verify")}, build(primary), primary, false, build)
	if err != nil {
		t.Fatal(err)
	}
	if !out.swappedToFallback || writes.Load() != 1 {
		t.Fatalf("fallback=%v writes=%d", out.swappedToFallback, writes.Load())
	}
	entries, text := sink.snapshot()
	if strings.Contains(text, "abandoned") || !strings.Contains(text, "verified saved") {
		t.Fatalf("partial response spliced into final: %q", text)
	}
	results := 0
	for _, entry := range entries {
		if entry.Type == "tool_result" {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("tool transcript duplicated: %d", results)
	}
	if usageSnapshot(orch).PromptTokens != 70 {
		t.Fatalf("completed step usage lost/doubled: %+v", usageSnapshot(orch))
	}
}

func TestResumedStreamHonorsRemainingStepLimit(t *testing.T) {
	var calls atomic.Int32
	tool := fantasy.NewAgentTool("inspect_record", "inspect a record",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			calls.Add(1)
			return fantasy.NewTextResponse(`{"found":true}`), nil
		})
	model := &namedMockModel{name: "fallback"}
	model.streamFunc = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "inspect", ToolCallName: "inspect_record", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	}
	e := newMockEngine(t, model)
	e.maxIterations = 3
	round := newRoundState(e, newOrchestrationState(e.logSession, 50), 1000)
	round.sink = newStreamSink(nil)
	round.priorSteps = 2
	agent := fantasy.NewAgent(model, fantasy.WithTools(tool))
	_, err := round.stream(context.Background(), agent, model, []fantasy.Message{fantasy.NewUserMessage("inspect records")})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("recovery reset the iteration budget: %d new tool steps", calls.Load())
	}
}

func TestProviderStreamStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		code   string
		want   streamErrorClass
		status int
	}{
		{`503`, streamErrorRetryExhausted, 503}, {`"429"`, streamErrorRetryExhausted, 429},
		{`401`, streamErrorFatal, 401}, {`400`, streamErrorFatal, 400},
		{`"invalid_api_key"`, streamErrorFatal, 0}, {`"server_error"`, streamErrorRetryExhausted, 0},
	} {
		t.Run(tc.code, func(t *testing.T) {
			original := &fantasy.ProviderError{Title: "stream error", Message: "Provider returned error", ResponseBody: []byte(`{"error":{"code":` + tc.code + `}}`)}
			class, pe := classifyStreamError(original)
			if class != tc.want || pe.StatusCode != tc.status {
				t.Fatalf("got %s/status=%d", class, pe.StatusCode)
			}
			if original.StatusCode != 0 {
				t.Fatal("mutated provider error")
			}
			wrapped, _ := classifyStreamError(&fantasy.RetryError{Errors: []error{original}})
			if wrapped != tc.want {
				t.Fatalf("retry wrapper masked status classification: %s", wrapped)
			}
		})
	}
	class, pe := classifyStreamError(errors.New("stream error: Provider returned error"))
	if class != streamErrorStreamBlip || pe.StatusCode != 0 {
		t.Fatal("opaque stream failure not bounded/retryable")
	}
	class, _ = classifyStreamError(errors.New("invalid arguments"))
	if class != streamErrorFatal {
		t.Fatal("ordinary errors became retryable")
	}
}

func TestToolLogKeepsCompleteRedactedJSONWhileObserverReceivesPreview(t *testing.T) {
	secret := "unit-test-only-tool-secret-for-redaction"
	RegisterSecretLiteral(secret)
	observer := &previewObserver{}
	sink := newStreamSink(observer)
	sink.logSession = NewLogSession()
	raw := `{"description":"` + strings.Repeat("large contract ", 600) + `","value":"` + secret + `","unchanged":true}`
	sink.onToolCall("inspect", "inspect", `{}`)
	sink.onToolResult("inspect", "inspect", raw, true)
	messages := sink.logSession.SnapshotMessages()
	if len(messages) != 2 || !messages[1].IsError || !json.Valid([]byte(messages[1].Content)) || strings.Contains(messages[1].Content, secret) {
		t.Fatal("lost complete/redacted error evidence")
	}
	if len(observer.text) == 0 || len(observer.text) > toolResultMaxStreamBytes {
		t.Fatal("display preview missing or unbounded")
	}
}

type previewObserver struct{ text string }

func (o *previewObserver) Observe(event string, payload map[string]any) {
	if event == "tool.result" {
		o.text, _ = payload["text"].(string)
	}
}
