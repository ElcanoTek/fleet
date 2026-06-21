package agent

import (
	"context"
	"fmt"
	"sync"
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
