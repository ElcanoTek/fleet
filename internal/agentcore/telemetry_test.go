package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// recordSpans installs a recording provider for one test (global state: not
// parallel).
// recordSpans installs a recording provider; at cleanup it also fails the
// test if a span the test started was never ended (spantest).
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	return spantest.InstallChecked(t).SpanRecorder
}

func spanNamed(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range sr.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no %q span; got %d spans", name, len(sr.Ended()))
	return nil
}

// assertNoContent fails when any span attribute value carries text the test
// put in a prompt, an argument or a result.
func assertNoContent(t *testing.T, s sdktrace.ReadOnlySpan, secrets ...string) {
	t.Helper()
	for _, kv := range s.Attributes() {
		v := kv.Value.String()
		for _, sec := range secrets {
			if strings.Contains(v, sec) {
				t.Errorf("span %s attribute %s carries content %q", s.Name(), kv.Key, sec)
			}
		}
	}
}

// One turn is one trace: agentcore.run under the caller's span, a model.stream
// span under it carrying ids and token counts — never the prompt — and the
// run log records the trace and turn ids for the admin view.
func TestRunTracesTheLoopAndRecordsCorrelation(t *testing.T) {
	sr := recordSpans(t)
	ctx := logging.WithTurn(context.Background(), "conv-1", "turn-1")
	ctx, root := otel.Tracer("test").Start(ctx, "chat.turn")
	ls := NewLogSession()
	model := &mockModel{streamFunc: streamStop()}
	_, err := Run(ctx, ModeInteractive, RunConfig{ConversationID: "conv-1"}, Deps{
		Input:      manyMessagesInput{n: 2},
		Observer:   &captureObserver{},
		Policy:     NewInteractivePolicy(0, 0, nil, nil),
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: ls,
	})
	root.End()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := spanNamed(t, sr, "agentcore.run")
	if run.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Error("agentcore.run is not a child of the caller's span")
	}
	stream := spanNamed(t, sr, "model.stream")
	if stream.Parent().SpanID() != run.SpanContext().SpanID() {
		t.Error("model.stream is not a child of agentcore.run")
	}
	attrs := map[string]string{}
	for _, kv := range stream.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["gen_ai.system"] != "mock" || attrs["gen_ai.request.model"] != "mock-model" || attrs["gen_ai.usage.input_tokens"] != "50" {
		t.Errorf("model.stream attributes = %v", attrs)
	}
	assertNoContent(t, stream, "filler turn", "sys")
	assertNoContent(t, run, "filler turn")

	traceID, _, turnID := ls.SnapshotCorrelation()
	if traceID != root.SpanContext().TraceID().String() || turnID != "turn-1" {
		t.Errorf("log session correlation = (%q, %q), want the turn's trace and id", traceID, turnID)
	}
}

// A tool dispatch is a tool.call span sized, not copied: the call's arguments
// and its result never reach the span, and a tool-level error marks it failed.
func TestToolCallSpanCarriesSizesNotContent(t *testing.T) {
	sr := recordSpans(t)
	tool := containTool(echoTool{name: "lookup", out: "RESULT-TEXT-xyz"}, panicAttribution{runMode: "interactive"}, nil)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-9", Name: "lookup", Input: `{"q":"ARG-TEXT-abc"}`})
	if err != nil || resp.Content != "RESULT-TEXT-xyz" {
		t.Fatalf("Run = (%+v, %v)", resp, err)
	}
	span := spanNamed(t, sr, "tool.call")
	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["gen_ai.tool.name"] != "lookup" || attrs["gen_ai.tool.call.id"] != "call-9" ||
		attrs["fleet.tool.input_bytes"] != "20" || attrs["fleet.tool.output_bytes"] != "15" {
		t.Errorf("tool.call attributes = %v", attrs)
	}
	assertNoContent(t, span, "ARG-TEXT", "RESULT-TEXT")

	failing := containTool(failTool{}, panicAttribution{runMode: "interactive"}, nil)
	_, _ = failing.Run(context.Background(), fantasy.ToolCall{ID: "call-10", Input: "{}"})
	var failed sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		for _, kv := range s.Attributes() {
			if kv.Key == "gen_ai.tool.call.id" && kv.Value.AsString() == "call-10" {
				failed = s
			}
		}
	}
	if failed == nil || failed.Status().Code != codes.Error {
		t.Fatalf("a failing tool's span should carry an error status, got %+v", failed)
	}
}

// With tracing disabled (the global no-op provider) the wrappers record
// nothing and change nothing.
func TestTracingDisabledIsTransparent(t *testing.T) {
	m := withModelTracing(&mockModel{})
	if withModelTracing(m) != m {
		t.Error("re-wrapping a traced model should be a no-op")
	}
	seq, err := m.Stream(context.Background(), fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if n != 2 {
		t.Errorf("traced stream yielded %d parts, want the model's 2", n)
	}
}
