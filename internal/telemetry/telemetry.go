// Package telemetry is the span vocabulary fleet's instrumented seams share
// (#2052): one tracer, one error-recording rule, and the attribute keys a
// span may carry. Exporter wiring (OTLP, TLS, sampling) lives in
// internal/otelsetup; this package only starts and ends spans, so any package
// can import it without pulling in the HTTP middleware.
//
// TWO RULES every span here keeps:
//
//  1. Attributes are identifiers and sizes only. A span never carries prompt
//     or completion text, tool arguments or results, file contents, command
//     lines, URLs with credentials, or an error MESSAGE — those can hold a
//     secret or a user's words, and a trace backend is not redacted. The log
//     line for the same event (which carries the span's trace_id) is where
//     the redacted detail lives.
//  2. Disabled tracing costs nothing that matters. With FLEET_OTEL_ENDPOINT
//     unset the global provider is OpenTelemetry's no-op: Start returns a
//     non-recording span without allocating, and attribute work is skipped
//     behind span.IsRecording() where it would cost anything.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope of every fleet span.
const TracerName = "github.com/ElcanoTek/fleet"

// Attribute keys fleet spans use. The fleet.* ids mirror the log
// correlation keys (internal/logging), so a span and its log lines join on
// the same values.
const (
	AttrConvID   = attribute.Key("fleet.conv_id")
	AttrTurnID   = attribute.Key("fleet.turn_id")
	AttrTaskID   = attribute.Key("fleet.task_id")
	AttrRunID    = attribute.Key("fleet.run_id")
	AttrRunMode  = attribute.Key("fleet.run_mode")
	AttrErrClass = attribute.Key("error.type")
)

// Start starts a span named name under ctx's span (if any) with the global
// tracer provider. kind defaults to internal.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, name, opts...)
}

// End ends span, recording err as the span's error status first. Only the
// error's CLASS is recorded (ErrorClass), never its message — see the
// package comment.
func End(span trace.Span, err error) {
	RecordError(span, err)
	span.End()
}

// RecordError marks span as failed with err's class. A nil err is a no-op.
func RecordError(span trace.Span, err error) {
	if err == nil || !span.IsRecording() {
		return
	}
	class := ErrorClass(err)
	span.SetAttributes(AttrErrClass.String(class))
	span.SetStatus(codes.Error, class)
}

// ErrorClass names err without its message: "canceled" and
// "deadline_exceeded" for the context sentinels, otherwise the Go type of the
// outermost error (e.g. "*url.Error", "*errors.errorString").
func ErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	return fmt.Sprintf("%T", err)
}

// TraceID returns the trace id of ctx's span as 32 hex characters, or "" when
// ctx carries no valid span context (tracing disabled, or no span started).
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// ContextWithRemoteParent returns ctx carrying parent's span context, so a
// span started under the returned context joins parent's trace. It is how a
// DETACHED context (a chat turn outlives its HTTP request) keeps the request's
// trace without inheriting its cancellation.
func ContextWithRemoteParent(ctx, parent context.Context) context.Context {
	sc := trace.SpanContextFromContext(parent)
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithSpanContext(ctx, sc)
}
