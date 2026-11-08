package mcpbroker

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// Trace and log correlation across the broker's process boundary (#2052,
// docs/TELEMETRY.md).
//
// A context does not cross the stdio pipe, so the parent writes what the
// child needs to rejoin the caller's trace onto every request frame:
//
//   - request.Trace: the W3C trace context (traceparent/tracestate) of the
//     parent's client span, injected with the TraceContext propagator. The
//     child extracts it, so its mcpbroker.* server span — and the MCP server
//     call underneath — lands in the SAME trace as the turn or task run that
//     made the call, exported by the child's own OTLP exporter.
//   - request.Log: the correlation ids (conv_id, turn_id, task_id, run_id,
//     user_id, request_id) of the parent's context, so the child's log lines
//     about the call carry them too.
//
// Both are public identifiers, never credentials — the frame's credential
// rule (protocol.go) is unchanged. The child accepts only the correlation keys
// it knows (logCorrelationKeys); anything else on request.Log is dropped, so
// the parent cannot inject arbitrary attributes into the credential owner's
// log.

// tracePropagator is pinned to W3C TraceContext rather than read from the
// global propagator, so propagation across the pipe does not depend on either
// process having installed one.
var tracePropagator = propagation.TraceContext{}

// logCorrelationKeys are the request.Log keys the child honors. The child
// drops any other key, and any value past maxCorrelationValue bytes.
var logCorrelationKeys = []string{
	logging.KeyConvID, logging.KeyTurnID, logging.KeyTaskID,
	logging.KeyRunID, logging.KeyUserID, logging.KeyRequestID,
}

const maxCorrelationValue = 256

const (
	attrBrokerMethod = attribute.Key("fleet.broker.method")
	attrBrokerServer = attribute.Key("fleet.mcp.server")
	attrBrokerTool   = attribute.Key("fleet.mcp.tool")
	attrBrokerScoped = attribute.Key("fleet.broker.scoped")
	attrBrokerToolEr = attribute.Key("fleet.tool.is_error")
)

// stampCorrelation writes ctx's trace context and correlation ids onto req.
// A context with no valid span and no ids leaves both fields nil, so the
// frame is byte-identical to before for an uncorrelated caller.
func stampCorrelation(ctx context.Context, req *request) {
	carrier := propagation.MapCarrier{}
	tracePropagator.Inject(ctx, carrier)
	if len(carrier) > 0 {
		req.Trace = carrier
	}
	for _, key := range logCorrelationKeys {
		if v := logging.Value(ctx, key); v != "" && len(v) <= maxCorrelationValue {
			if req.Log == nil {
				req.Log = make(map[string]string, len(logCorrelationKeys))
			}
			req.Log[key] = v
		}
	}
}

// adoptCorrelation is stampCorrelation's child side: ctx gains the parent's
// span context (as a remote parent) and its known correlation ids.
func adoptCorrelation(ctx context.Context, req request) context.Context {
	if len(req.Trace) > 0 {
		ctx = tracePropagator.Extract(ctx, propagation.MapCarrier(req.Trace))
	}
	if len(req.Log) > 0 {
		attrs := make([]slog.Attr, 0, len(logCorrelationKeys))
		for _, key := range logCorrelationKeys {
			if v, ok := req.Log[key]; ok && v != "" && len(v) <= maxCorrelationValue {
				attrs = append(attrs, slog.String(key, v))
			}
		}
		ctx = logging.WithAttrs(ctx, attrs...)
	}
	return ctx
}

// startClientSpan opens the parent's span for one broker request.
func startClientSpan(ctx context.Context, name, server, tool, scope string) (context.Context, trace.Span) {
	ctx, span := telemetry.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient))
	if span.IsRecording() {
		span.SetAttributes(
			attrBrokerServer.String(server),
			attrBrokerTool.String(tool),
			attrBrokerScoped.Bool(scope != ""),
		)
	}
	return ctx, span
}

// startServeSpan opens the child's span for one request, under the parent's
// propagated span context.
func startServeSpan(ctx context.Context, req request, location string) (context.Context, trace.Span) {
	ctx = adoptCorrelation(ctx, req)
	ctx, span := telemetry.Start(ctx, location, trace.WithSpanKind(trace.SpanKindServer))
	if span.IsRecording() {
		span.SetAttributes(
			attrBrokerMethod.String(string(req.Method)),
			attrBrokerServer.String(req.Server),
			attrBrokerTool.String(req.Tool),
			attrBrokerScoped.Bool(req.Scope != ""),
		)
	}
	return ctx, span
}

// endServeSpan records the response's outcome: a dispatch error as
// "broker_error" (its text is a fixed errBroker* string or a scrubbed vendor
// answer, and still stays off the span), a tool-level error as "tool_error".
func endServeSpan(span trace.Span, resp response) {
	if span.IsRecording() {
		span.SetAttributes(attrBrokerToolEr.Bool(resp.IsError))
		switch {
		case resp.Err != "":
			span.SetAttributes(telemetry.AttrErrClass.String("broker_error"))
			span.SetStatus(codes.Error, "broker_error")
		case resp.IsError:
			span.SetAttributes(telemetry.AttrErrClass.String("tool_error"))
			span.SetStatus(codes.Error, "tool_error")
		}
	}
	span.End()
}

// endToolErrorSpan ends a client span whose call returned a tool-level error
// (isError=true, no transport error) with the same marks endServeSpan gives
// the child's span: fleet.tool.is_error, error.type=tool_error, and an error
// status.
func endToolErrorSpan(span trace.Span) {
	if span.IsRecording() {
		span.SetAttributes(attrBrokerToolEr.Bool(true), telemetry.AttrErrClass.String("tool_error"))
		span.SetStatus(codes.Error, "tool_error")
	}
	span.End()
}
