package agentcore

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

type toolResultObserver struct {
	mu      sync.Mutex
	results []map[string]any
}

func (o *toolResultObserver) Observe(eventType string, payload map[string]any) {
	if eventType != "tool.result" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = append(o.results, payload)
}

// TestToolResultCarriesDispatchDuration drives a real Run: the tool.result
// frame for a dispatched tool carries duration_ms measured from the dispatch
// wrapper, covering at least the tool's own run time.
func TestToolResultCarriesDispatchDuration(t *testing.T) {
	const sleep = 40 * time.Millisecond
	tool := fantasy.NewAgentTool("slow_tool", "sleeps briefly",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			time.Sleep(sleep)
			return fantasy.NewTextResponse("done"), nil
		})
	obs := &toolResultObserver{}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:      CanonicalEnvPrefix,
		ConversationID: "conv-timing",
		NativeTools:    []fantasy.AgentTool{tool},
	}, Deps{
		Input:    stubInput{system: "sys", user: "use tool", label: "timing"},
		Observer: obs,
		Policy:   passPolicy{},
		Model:    scriptedToolModel([]scriptedToolCall{{id: "timed-call", name: "slow_tool"}}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.results) != 1 {
		t.Fatalf("tool.result frames = %d, want 1", len(obs.results))
	}
	ms, ok := obs.results[0][evtFieldDurationMS].(int64)
	if !ok {
		t.Fatalf("tool.result has no int64 duration_ms: %#v", obs.results[0])
	}
	if ms < sleep.Milliseconds() {
		t.Fatalf("duration_ms = %d, want >= %d", ms, sleep.Milliseconds())
	}
}

// TestStreamSinkToolTiming covers the sink directly: a result with no
// dispatch stamp carries no duration, the first stamp wins over a nested
// re-stamp under the same id, and a stamp is consumed by its result.
func TestStreamSinkToolTiming(t *testing.T) {
	obs := &toolResultObserver{}
	sink := newStreamSink(obs)
	ctx := withToolTiming(context.Background(), sink)

	sink.onToolResult("never-dispatched", "t", "x", true)

	noteToolStart(ctx, "c1")
	sink.mu.Lock()
	first := sink.toolStarts["c1"]
	sink.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	noteToolStart(ctx, "c1") // nested bridge dispatch, same id
	sink.mu.Lock()
	if !sink.toolStarts["c1"].Equal(first) {
		t.Fatal("a second stamp under the same id replaced the first")
	}
	sink.mu.Unlock()
	sink.onToolResult("c1", "t", "ok", false)

	noteToolStart(context.Background(), "no-sink") // no sink on ctx: no-op
	var nilSink *streamSink
	nilSink.noteToolStart("nil-safe")

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if _, ok := obs.results[0][evtFieldDurationMS]; ok {
		t.Errorf("un-dispatched result carried duration_ms: %#v", obs.results[0])
	}
	if ms, ok := obs.results[1][evtFieldDurationMS].(int64); !ok || ms < 5 {
		t.Errorf("c1 duration_ms = %v, want >= 5", obs.results[1][evtFieldDurationMS])
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.toolStarts) != 0 {
		t.Errorf("toolStarts not drained: %v", sink.toolStarts)
	}
}
