//go:build fleet_host_executor

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Every dispatch through the Sandbox front door is one sandbox.exec span under
// the caller's span, naming the backend and the operation and sizing the
// command and output — never copying them.
func TestRunBashEmitsExecSpanWithSizesOnly(t *testing.T) {
	prev := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })

	sb := NewHost(nil)
	defer sb.Close()
	ctx, parent := otel.Tracer("test").Start(context.Background(), "tool.call")
	res, err := sb.RunBash(ctx, BashRequest{Command: "echo SECRET-OUTPUT-42; exit 3", Timeout: 10 * time.Second})
	parent.End()
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d", res.ExitCode)
	}

	var exec sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "sandbox.exec" {
			exec = s
		}
	}
	if exec == nil {
		t.Fatal("no sandbox.exec span")
	}
	if exec.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("sandbox.exec is not a child of the caller's span")
	}
	attrs := map[string]string{}
	for _, kv := range exec.Attributes() {
		v := kv.Value.String()
		if strings.Contains(v, "SECRET-OUTPUT") || strings.Contains(v, "echo") {
			t.Errorf("attribute %s carries the command or its output: %q", kv.Key, v)
		}
		attrs[string(kv.Key)] = v
	}
	if attrs["fleet.sandbox.op"] != "bash" || attrs["fleet.sandbox.backend"] != "host" ||
		attrs["fleet.sandbox.exit_code"] != "3" || attrs["fleet.sandbox.timeout_ms"] != "10000" {
		t.Errorf("sandbox.exec attributes = %v", attrs)
	}
}
