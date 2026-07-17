package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// capturingObserver records full event payloads so the bridge test can assert
// the streamed SSE vocabulary AND payload contents, not just event names.
type capturingObserver struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	typ     string
	payload map[string]any
}

func (o *capturingObserver) Observe(eventType string, payload map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, capturedEvent{typ: eventType, payload: payload})
}

func (o *capturingObserver) typesSeen() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := map[string]int{}
	for _, e := range o.events {
		seen[e.typ]++
	}
	return seen
}

func (o *capturingObserver) concatText() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var s string
	for _, e := range o.events {
		if e.typ == "text.delta" {
			if t, ok := e.payload["text"].(string); ok {
				s += t
			}
		}
	}
	return s
}

// bridgeMockModel streams reasoning + a tool call + tool result + final text so
// the test exercises every callback the streaming bridge forwards.
type bridgeMockModel struct {
	mu        sync.Mutex
	callCount int
}

func (m *bridgeMockModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: "mock"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (m *bridgeMockModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		// Reasoning block.
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "r1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r1", Delta: "thinking…"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r1"}) {
			return
		}
		// One tool call (run_bash) — the loop executes it and emits the result.
		if !yield(fantasy.StreamPart{
			Type:          fantasy.StreamPartTypeToolCall,
			ID:            "call_1",
			ToolCallName:  "run_bash",
			ToolCallInput: `{"command":"echo hi"}`,
		}) {
			return
		}
		// Final user-visible text.
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "All done. "}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "Result above."}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 120, OutputTokens: 30},
		})
	}, nil
}

func (m *bridgeMockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *bridgeMockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *bridgeMockModel) Provider() string { return "mock" }
func (m *bridgeMockModel) Model() string    { return "mock/bridge-model" }

// bashOnlyTool is a minimal native tool named "run_bash" the bridge test
// registers so the streamed tool call has something to execute. It returns a
// fixed result so the test can assert the tool_result entry round-trips.
type bashOnlyTool struct{ opts fantasy.ProviderOptions }

func (t *bashOnlyTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "run_bash",
		Description: "echo a command",
		Parameters:  map[string]any{"command": map[string]any{"type": "string"}},
		Required:    []string{"command"},
	}
}
func (t *bashOnlyTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("hi\n"), nil
}
func (t *bashOnlyTool) ProviderOptions() fantasy.ProviderOptions     { return t.opts }
func (t *bashOnlyTool) SetProviderOptions(o fantasy.ProviderOptions) { t.opts = o }

// TestStreamBridge_ForwardsEventsAndAccumulatesHistory is the P6b focused test:
// it proves the agentcore streaming bridge forwards every event class to the
// Observer (the SSE sink) AND accumulates a persistable transcript + usage from
// an interactive turn — without a live provider.
func TestStreamBridge_ForwardsEventsAndAccumulatesHistory(t *testing.T) {
	model := &bridgeMockModel{}
	obs := &capturingObserver{}

	tc := TurnConfig{
		SystemPrompt: "you are a test agent",
		Messages:     []fantasy.Message{fantasy.NewUserMessage("run echo hi")},
		Label:        "conv-bridge",
		Model:        model,
		Temperature:  0.2,
		MaxTokens:    1024,
		NativeTools:  []fantasy.AgentTool{&bashOnlyTool{}},
	}

	res, err := RunInteractiveTurn(context.Background(), tc, obs)
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}

	// 1. Streamed event vocabulary forwarded to the sink.
	seen := obs.typesSeen()
	for _, want := range []string{"reasoning.start", "reasoning.delta", "reasoning.end", "tool.call", "tool.result", "text.delta"} {
		if seen[want] == 0 {
			t.Errorf("expected SSE event %q to be forwarded to the sink; got events %v", want, seen)
		}
	}

	// 2. Accumulated final text matches the streamed deltas.
	if got := obs.concatText(); got != "All done. Result above." {
		t.Errorf("streamed text = %q, want %q", got, "All done. Result above.")
	}
	if res.FinalText != "All done. Result above." {
		t.Errorf("Result.FinalText = %q, want the assistant reply", res.FinalText)
	}

	// 3. Accumulated transcript persists reasoning + tool_call + tool_result +
	//    final assistant text, in order.
	wantTypes := []string{"reasoning", "tool_call", "tool_result", "text"}
	if len(res.Entries) != len(wantTypes) {
		t.Fatalf("Result.Entries = %d entries, want %d: %+v", len(res.Entries), len(wantTypes), res.Entries)
	}
	for i, want := range wantTypes {
		if res.Entries[i].Type != want {
			t.Errorf("entry[%d].Type = %q, want %q", i, res.Entries[i].Type, want)
		}
	}
	// The tool_call carries the model's raw input; the tool_result carries the
	// executed output.
	if res.Entries[1].ToolName != "run_bash" || res.Entries[1].ToolInput != `{"command":"echo hi"}` {
		t.Errorf("tool_call entry = %+v, want run_bash with echo input", res.Entries[1])
	}
	if res.Entries[2].Text != "hi\n" || res.Entries[2].IsErr {
		t.Errorf("tool_result entry = %+v, want non-error 'hi\\n'", res.Entries[2])
	}

	// 4. Usage accumulated across the two steps (round 0 + the tool follow-up
	//    step within the same stream).
	if res.Usage.PromptTokens == 0 || res.Usage.CompletionTokens == 0 {
		t.Errorf("expected non-zero usage, got %+v", res.Usage)
	}
	if res.ModelSlug != "mock/bridge-model" {
		t.Errorf("Result.ModelSlug = %q, want mock/bridge-model", res.ModelSlug)
	}
}

// mapRunEntries is exercised indirectly by RunTurn, but assert the mapping here
// so a transcript-shape regression is caught without a live turn.
func TestMapRunEntries_RoundTrip(t *testing.T) {
	entries := []agentcore.RunEntry{
		{Role: "assistant", Type: "reasoning", Text: "hmm"},
		{Role: "assistant", Type: "tool_call", ToolCallID: "c1", ToolName: "run_python", ToolInput: "{}"},
		{Role: "tool", Type: "tool_result", ToolCallID: "c1", ToolName: "run_python", Text: "out", IsErr: false},
		{Role: "assistant", Type: "text", Text: "answer"},
	}
	out := mapRunEntries(entries)
	if len(out) != 4 {
		t.Fatalf("mapRunEntries = %d, want 4", len(out))
	}
	if out[0].Type != "reasoning" || out[1].Type != entryTypeToolCall || out[2].Type != "tool_result" || out[3].Type != "text" {
		t.Errorf("unexpected mapped types: %+v", out)
	}
}

type persistedPanicInput struct{}

// TestContainedPanicResult_PersistsAndReplaysPaired exercises the actual
// agentcore -> HistoryEntry mapping used immediately before AppendHistory,
// serializes it exactly as the JSONB persistence path does, and then replays
// the stored rows for the next provider turn.
func TestContainedPanicResult_PersistsAndReplaysPaired(t *testing.T) {
	const secret = "Authorization: Bearer fake-transcript-regression-secret"
	var streams atomic.Int32
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := streams.Add(1)
		return func(yield func(fantasy.StreamPart) bool) {
			if step == 1 {
				yield(fantasy.StreamPart{
					Type:          fantasy.StreamPartTypeToolCall,
					ID:            "persisted-panic-call",
					ToolCallName:  "persisted_panic",
					ToolCallInput: "{}",
				})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "continued safely"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	panicTool := fantasy.NewAgentTool("persisted_panic", "panic persistence regression",
		func(context.Context, persistedPanicInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			panic(secret)
		})

	res, err := RunInteractiveTurn(context.Background(), TurnConfig{
		SystemPrompt:   "system",
		Messages:       []fantasy.Message{fantasy.NewUserMessage("run the panic tool")},
		Label:          "persisted-panic",
		ConversationID: "conv-persisted-panic",
		Model:          model,
		NativeTools:    []fantasy.AgentTool{panicTool},
	}, nil)
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}

	history := mapRunEntries(res.Entries)
	storedJSON, err := json.Marshal(history)
	if err != nil {
		t.Fatalf("marshal persisted history: %v", err)
	}
	if strings.Contains(string(storedJSON), secret) {
		t.Fatalf("persisted transcript leaked panic secret: %s", storedJSON)
	}
	var stored []HistoryEntry
	if err := json.Unmarshal(storedJSON, &stored); err != nil {
		t.Fatalf("unmarshal persisted history: %v", err)
	}

	var calls, results int
	var persistedResult ToolResultContent
	for _, entry := range stored {
		switch entry.Type {
		case entryTypeToolCall:
			var call ToolCallContent
			if err := json.Unmarshal(entry.Content, &call); err != nil {
				t.Fatalf("decode stored tool call: %v", err)
			}
			if call.ID == "persisted-panic-call" {
				calls++
			}
		case "tool_result":
			var result ToolResultContent
			if err := json.Unmarshal(entry.Content, &result); err != nil {
				t.Fatalf("decode stored tool result: %v", err)
			}
			if result.ID == "persisted-panic-call" {
				results++
				persistedResult = result
			}
		}
	}
	if calls != 1 || results != 1 || !persistedResult.IsErr ||
		!strings.Contains(persistedResult.Text, "inc_") {
		t.Fatalf("persisted panic pair: calls=%d results=%d result=%+v", calls, results, persistedResult)
	}

	replayed, err := replayHistory(stored)
	if err != nil {
		t.Fatalf("replay persisted panic history: %v", err)
	}
	assertWellFormedToolPairs(t, replayed)
	text, isErr, found := toolResultTextFor(t, replayed, "persisted-panic-call")
	if !found || !isErr || !strings.Contains(text, "inc_") || strings.Contains(text, secret) {
		t.Fatalf("replayed panic result: found=%v isErr=%v text=%q", found, isErr, text)
	}
}

// TestFinalizeRetryContainedPanicJoinsRunTranscript proves the leaked-call
// recovery agent reuses the governed roster AND routes its tool callbacks into
// agentcore.Run's original sink. A panic in this auxiliary Fantasy stream must
// therefore persist exactly one opaque call/result pair, not become invisible
// tool work outside the run audit.
func TestFinalizeRetryContainedPanicJoinsRunTranscript(t *testing.T) {
	const secret = "finalize retry raw panic secret"
	var modelCalls atomic.Int32
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		call := modelCalls.Add(1)
		return func(yield func(fantasy.StreamPart) bool) {
			switch call {
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "call:default_api:download_url{url:https://example.invalid}"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			case 2:
				yield(fantasy.StreamPart{
					Type: fantasy.StreamPartTypeToolCall, ID: "finalize-panic-call",
					ToolCallName: "finalize_panic", ToolCallInput: "{}",
				})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "recovered after contained panic"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}
		}, nil
	}}
	panicTool := fantasy.NewAgentTool("finalize_panic", "finalize panic regression",
		func(context.Context, persistedPanicInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			panic(secret)
		})

	res, err := RunInteractiveTurn(context.Background(), TurnConfig{
		SystemPrompt: "system", Messages: []fantasy.Message{fantasy.NewUserMessage("recover the leaked call")},
		Label: "finalize-panic", ConversationID: "conv-finalize-panic", Model: model, MaxTokens: 1024,
		NativeTools: []fantasy.AgentTool{panicTool},
	}, nil)
	if err != nil {
		t.Fatalf("RunInteractiveTurn: %v", err)
	}
	if res.FinalText != "recovered after contained panic" {
		t.Fatalf("final text = %q", res.FinalText)
	}

	var calls, results int
	var result agentcore.RunEntry
	for _, entry := range res.Entries {
		if entry.ToolCallID != "finalize-panic-call" {
			continue
		}
		switch entry.Type {
		case entryTypeToolCall:
			calls++
		case "tool_result":
			results++
			result = entry
		}
	}
	if calls != 1 || results != 1 || !result.IsErr || !strings.Contains(result.Text, "inc_") || strings.Contains(result.Text, secret) {
		t.Fatalf("finalize panic pair: calls=%d results=%d result=%+v entries=%+v", calls, results, result, res.Entries)
	}
}
