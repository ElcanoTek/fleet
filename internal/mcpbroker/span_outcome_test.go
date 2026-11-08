package mcpbroker

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// endedSpan returns the one ended span named name of the given kind.
func endedSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string, kind trace.SpanKind) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name && s.SpanKind() == kind {
			if found != nil {
				t.Fatalf("more than one %s span of kind %s", name, kind)
			}
			found = s
		}
	}
	if found == nil {
		t.Fatalf("no ended %s span of kind %s (have %d spans)", name, kind, len(spans))
	}
	return found
}

func hasSpan(spans []sdktrace.ReadOnlySpan, name string, kind trace.SpanKind) bool {
	for _, s := range spans {
		if s.Name() == name && s.SpanKind() == kind {
			return true
		}
	}
	return false
}

func errClass(s sdktrace.ReadOnlySpan) string {
	for _, a := range s.Attributes() {
		if a.Key == telemetry.AttrErrClass {
			return a.Value.AsString()
		}
	}
	return ""
}

// A backend panic is answered with an incident response; the child's server
// span must be exported as a failure, not as a success that ended before the
// recovery ran (#2052).
func TestServeSpanRecordsBackendPanic(t *testing.T) {
	sr := withSpanRecorder(t)
	client := loopback(t, &fakeBroker{panicCall: "boom"})

	if _, _, err := client.CallMCP(context.Background(), "srv", "tool", nil); err == nil {
		t.Fatal("CallMCP after a backend panic returned no error")
	}
	// The server span ends on the server goroutine after the response is
	// written, so wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for !hasSpan(sr.Ended(), "mcpbroker.call", trace.SpanKindServer) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	serve := endedSpan(t, sr.Ended(), "mcpbroker.call", trace.SpanKindServer)
	if serve.Status().Code != codes.Error || errClass(serve) != "broker_error" {
		t.Fatalf("server span after a backend panic: status=%v error.type=%q, want Error/broker_error", serve.Status(), errClass(serve))
	}
}

// A tool-level error (isError=true, no transport error) marks the parent's
// client span failed with the same class the child's span carries.
func TestClientSpanRecordsToolError(t *testing.T) {
	sr := withSpanRecorder(t)
	client := loopback(t, &fakeBroker{text: "vendor said no", isErr: true})

	_, isErr, err := client.CallMCP(context.Background(), "srv", "tool", nil)
	if err != nil || !isErr {
		t.Fatalf("CallMCP = isErr %v, err %v; want a tool-level error only", isErr, err)
	}
	// The client span ends before CallMCP returns; the child's span (marked
	// by endServeSpan) may still be ending on the server goroutine.
	s := endedSpan(t, sr.Ended(), "mcpbroker.call", trace.SpanKindClient)
	if s.Status().Code != codes.Error || errClass(s) != "tool_error" {
		t.Errorf("client span: status=%v error.type=%q, want Error/tool_error", s.Status(), errClass(s))
	}
}
