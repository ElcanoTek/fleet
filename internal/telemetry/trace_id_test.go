package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const callerTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// callerContext is a request context continuing a caller-supplied traceparent,
// as the HTTP middleware builds it.
func callerContext() context.Context {
	return propagation.TraceContext{}.Extract(context.Background(),
		propagation.MapCarrier{"traceparent": callerTraceparent})
}

// With tracing off, a caller's traceparent still yields a valid (but
// non-recording, never exported) span context. TraceID must not publish it:
// it would name a trace fleet never recorded, or one the caller made up.
func TestTraceIDNotPublishedWhenTracingOff(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	ctx, span := Start(callerContext(), "server")
	defer span.End()
	if got := TraceID(ctx); got != "" {
		t.Fatalf("TraceID with tracing off = %q, want \"\"", got)
	}
	if got := TraceID(ContextWithRemoteParent(context.Background(), ctx)); got != "" {
		t.Fatalf("TraceID of a detached copy with tracing off = %q, want \"\"", got)
	}
}

// A sampled-out span is not exported either, so its id is not published.
func TestTraceIDNotPublishedWhenSampledOut(t *testing.T) {
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })

	ctx, span := Start(context.Background(), "server")
	defer span.End()
	if got := TraceID(ctx); got != "" {
		t.Fatalf("TraceID of a sampled-out span = %q, want \"\"", got)
	}
}

// With tracing on, the id of a recorded trace is published — on the span's
// own context and on a detached context carrying it (a chat turn).
func TestTraceIDPublishedWhenRecording(t *testing.T) {
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })

	ctx, span := Start(callerContext(), "server")
	want := span.SpanContext().TraceID().String()
	if got := TraceID(ctx); got != want {
		t.Fatalf("TraceID of a recording span = %q, want %q", got, want)
	}
	detached := ContextWithRemoteParent(context.Background(), ctx)
	span.End() // the request ends; the detached turn still names its trace
	if got := TraceID(detached); got != want {
		t.Fatalf("TraceID of a detached copy = %q, want %q", got, want)
	}
}
