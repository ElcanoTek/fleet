package logging

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func callerCtx() context.Context {
	return propagation.TraceContext{}.Extract(context.Background(),
		propagation.MapCarrier{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
}

// trace_id/span_id go on a log line only when this process records and
// exports the trace — the rule telemetry.TraceID applies to the ids fleet
// publishes. With tracing off a caller's traceparent is still a valid span
// context, and a sampled-out span is never exported; neither leaks (#2052).
func TestTraceAttrsOnlyForRecordedTraces(t *testing.T) {
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	otel.SetTracerProvider(noop.NewTracerProvider())
	ctx, span := otel.Tracer("t").Start(callerCtx(), "server")
	if v := Value(ctx, KeyTraceID); v != "" {
		t.Errorf("tracing off: trace_id = %q from the caller's traceparent, want none", v)
	}
	span.End()

	never := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	otel.SetTracerProvider(never)
	ctx, span = otel.Tracer("t").Start(context.Background(), "server")
	if v := Value(ctx, KeyTraceID); v != "" {
		t.Errorf("sampled out: trace_id = %q, want none", v)
	}
	span.End()
	_ = never.Shutdown(context.Background())

	on := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(on)
	ctx, span = otel.Tracer("t").Start(context.Background(), "server")
	if v := Value(ctx, KeyTraceID); v != span.SpanContext().TraceID().String() {
		t.Errorf("recording: trace_id = %q, want the span's", v)
	}
	span.End()
	_ = on.Shutdown(context.Background())
}
