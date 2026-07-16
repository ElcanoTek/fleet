package agentcore

// #795: no panic in a tool (native/loader/pre-gated/MCP), a policy gate, a
// guardrail, or an Observer callback may escape the run boundary or kill the
// process. These tests drive the REAL fantasy stream/goroutine machinery
// through mock models — a panicking tool must become exactly one in-band error
// result, the transcript must stay paired, and the run must complete.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// panickingTool panics on Run with the given value.
type panickingTool struct {
	name  string
	value any
}

func (p *panickingTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: p.name, Description: "panics", Parameters: map[string]any{"type": "object"}}
}
func (p *panickingTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (p *panickingTool) SetProviderOptions(_ fantasy.ProviderOptions) {}
func (p *panickingTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	panic(p.value)
}

// recordingPolicy records RecordToolResult calls and can be told to panic in a
// chosen gate, to exercise boundary attribution.
type recordingPolicy struct {
	recorded          atomic.Int32
	panicBefore       bool
	panicRecord       bool
	lastRecordedError atomic.Bool
}

func (p *recordingPolicy) BeforeToolCall(string, string, string) (bool, string) {
	if p.panicBefore {
		panic("boom in before")
	}
	return false, ""
}
func (p *recordingPolicy) RecordToolResult(_, _, _ string, ok bool) {
	p.recorded.Add(1)
	p.lastRecordedError.Store(!ok)
	if p.panicRecord {
		panic("boom in record")
	}
}
func (p *recordingPolicy) CanFinish(int) (bool, []string) { return true, nil }

func TestPanicContainedTool_RecoversInBand(t *testing.T) {
	for _, val := range []any{"string panic", errors.New("error panic"), 42} {
		pol := &recordingPolicy{}
		tool := &panicContainedTool{
			inner: &panickingTool{name: "boom", value: val}, name: "boom",
			policy: pol, mode: "scheduled", label: "test-task",
		}
		before := safePanicCount("agentcore.tool_dispatch.tool")
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: "{}"})
		if err != nil {
			t.Fatalf("val %v: Run returned a Go error (would abort the stream), want nil: %v", val, err)
		}
		if !resp.IsError {
			t.Errorf("val %v: want IsError result", val)
		}
		if strings.Contains(resp.Content, "goroutine") || strings.Contains(resp.Content, "string panic") ||
			strings.Contains(resp.Content, "error panic") {
			t.Errorf("val %v: panic value/stack leaked to the model: %q", val, resp.Content)
		}
		if !strings.Contains(resp.Content, "reference ") {
			t.Errorf("val %v: expected an incident reference in %q", val, resp.Content)
		}
		if safePanicCount("agentcore.tool_dispatch.tool") <= before {
			t.Errorf("val %v: expected PanicCounts increment", val)
		}
		if pol.recorded.Load() != 1 || !pol.lastRecordedError.Load() {
			t.Errorf("val %v: expected one RecordToolResult(ok=false), got %d", val, pol.recorded.Load())
		}
	}
}

// panickyObserver panics in Observe, to prove the streamSink.emit containment
// keeps a run alive when observation fails.
type panickyObserver struct{ observed atomic.Int32 }

func (o *panickyObserver) Observe(string, map[string]any) {
	o.observed.Add(1)
	panic("observer boom")
}

func TestStreamSink_ObserverPanicContained(t *testing.T) {
	obs := &panickyObserver{}
	sink := newStreamSink(obs)
	before := safePanicCount("agentcore.observer.tool.call")
	// Must not panic even though the observer does.
	sink.onToolCall("c1", "bash", `{"command":"ls"}`)
	sink.onToolResult("c1", "bash", "ok", false)
	if obs.observed.Load() == 0 {
		t.Fatal("observer was never invoked")
	}
	if safePanicCount("agentcore.observer.tool.call") <= before {
		t.Error("observer panic not recorded to its boundary counter")
	}
	// The accumulated entries are unaffected by the observer failure.
	entries, _ := sink.snapshot()
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (tool_call + tool_result accumulated despite observer panic)", len(entries))
	}
}

func TestPanicContainedTool_RecordPanicDoesNotRepanic(t *testing.T) {
	pol := &recordingPolicy{panicRecord: true}
	tool := &panicContainedTool{inner: &panickingTool{name: "boom", value: "x"}, name: "boom", policy: pol}
	// Must not re-panic even though RecordToolResult itself panics.
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: "{}"})
	if err != nil || !resp.IsError {
		t.Fatalf("nested record panic not contained: err=%v isErr=%v", err, resp.IsError)
	}
}

func TestPanicContainedTool_BoundaryAttribution(t *testing.T) {
	// A panic in the policy BeforeToolCall gate (via policyGuardedTool wrapped in
	// panicContainedTool) is attributed to the policy boundary, not "tool".
	pol := &recordingPolicy{panicBefore: true}
	guarded := &policyGuardedTool{inner: &panickingTool{name: "x", value: "unused"}, policy: pol}
	contained := &panicContainedTool{inner: guarded, name: "x", policy: nil, mode: "scheduled"}
	before := safePanicCount("agentcore.tool_dispatch.policy.before_tool_call")
	resp, err := contained.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: "{}"})
	if err != nil || !resp.IsError {
		t.Fatalf("policy-gate panic not contained: err=%v isErr=%v", err, resp.IsError)
	}
	if safePanicCount("agentcore.tool_dispatch.policy.before_tool_call") <= before {
		t.Errorf("policy-gate panic not attributed to its boundary")
	}
}

// TestRun_SequentialToolPanic_ProcessSurvives drives the full agentcore.Run
// loop: a native tool panics, the run must complete with a paired error result.
func TestRun_SequentialToolPanic_ProcessSurvives(t *testing.T) {
	session := NewLogSession()
	model := &toolThenTextModel{slug: "panic-test-model", toolName: "boom"}
	res, err := Run(context.Background(), ModeInteractive,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, NativeTools: []fantasy.AgentTool{&panickingTool{name: "boom", value: "kaboom"}}},
		Deps{
			Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("go")}, label: "panic"},
			Policy:     newRoundsPolicy(session, 0),
			Executor:   &stubExecutor{},
			Model:      model,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run should complete despite a tool panic, got: %v", err)
	}
	var calls, results int
	for _, e := range res.Entries {
		switch e.Type {
		case "tool_call":
			calls++
		case "tool_result":
			results++
			if e.ToolCallID != "c1" || !e.IsErr {
				t.Errorf("tool_result = %+v, want paired to c1 with IsErr", e)
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Errorf("want exactly one tool_call + one paired tool_result, got %d/%d", calls, results)
	}
}

// sleepThenOKTool succeeds after a short delay (parallel-sibling of the
// panicking tool), to prove one tool's panic doesn't disturb a concurrent one.
type sleepThenOKTool struct {
	name string
	ran  atomic.Bool
}

func (s *sleepThenOKTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: s.name, Description: "ok", Parameters: map[string]any{"type": "object"}, Parallel: true}
}
func (s *sleepThenOKTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (s *sleepThenOKTool) SetProviderOptions(_ fantasy.ProviderOptions) {}
func (s *sleepThenOKTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	s.ran.Store(true)
	return fantasy.NewTextResponse("sibling ok"), nil
}

// TestRun_ParallelToolPanicAndSuccess drives two parallel tool calls in one
// step — one panics, one succeeds — and asserts both settle with correct
// paired results and the run completes. Run under -race in CI.
func TestRun_ParallelToolPanicAndSuccess(t *testing.T) {
	session := NewLogSession()
	sibling := &sleepThenOKTool{name: "ok_tool"}
	model := &twoToolThenTextModel{slug: "parallel-panic-model"}
	res, err := Run(context.Background(), ModeInteractive,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, NativeTools: []fantasy.AgentTool{
			&panickingTool{name: "boom", value: "kaboom"}, sibling,
		}},
		Deps{
			Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("go")}, label: "pp"},
			Policy:     newRoundsPolicy(session, 0),
			Executor:   &stubExecutor{},
			Model:      model,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run should complete despite a parallel tool panic, got: %v", err)
	}
	if !sibling.ran.Load() {
		t.Error("sibling tool should have run to completion")
	}
	results := map[string]bool{} // callID → isErr
	for _, e := range res.Entries {
		if e.Type == "tool_result" {
			results[e.ToolCallID] = e.IsErr
		}
	}
	if isErr, ok := results["boom1"]; !ok || !isErr {
		t.Errorf("panicking call boom1: want paired error result, got ok=%v isErr=%v", ok, isErr)
	}
	if isErr, ok := results["ok1"]; !ok || isErr {
		t.Errorf("sibling call ok1: want paired success result, got ok=%v isErr=%v", ok, isErr)
	}
}

// twoToolThenTextModel emits two parallel tool calls, then text on retry.
type twoToolThenTextModel struct {
	mockModel
	slug  string
	calls atomic.Int32
}

func (m *twoToolThenTextModel) Model() string { return m.slug }
func (m *twoToolThenTextModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	n := m.calls.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if n == 1 {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "boom1", ToolCallName: "boom", ToolCallInput: "{}"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "ok1", ToolCallName: "ok_tool", ToolCallInput: "{}"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2}})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "done"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5}})
	}, nil
}

// toolThenTextModel emits one tool call on the first Stream, then plain text.
type toolThenTextModel struct {
	mockModel
	slug     string
	toolName string
	calls    atomic.Int32
}

func (m *toolThenTextModel) Model() string { return m.slug }
func (m *toolThenTextModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	n := m.calls.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		if n == 1 {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: m.toolName, ToolCallInput: "{}"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2}})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "recovered and continued"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5}})
	}, nil
}

// safePanicCount reads a PanicCounts location from internal/safe.
func safePanicCount(loc string) int {
	return int(safe.PanicCounts()[loc])
}
