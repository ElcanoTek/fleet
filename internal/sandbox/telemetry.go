package sandbox

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// One sandbox.exec span per dispatch through the Sandbox front door (#2052,
// docs/TELEMETRY.md): RunBash, RunPython and RunFileOp. Instrumenting the
// shared methods rather than each backend covers podman, kubernetes and the
// test host executor alike, with the backend named on the span.
//
// Ids and sizes only: the operation, the backend, the timeout, the exit code
// or status, and the byte counts. Never the command, the code, a path or any
// output — those are the model's and the user's data.
const (
	attrSandboxOp       = attribute.Key("fleet.sandbox.op")
	attrSandboxBackend  = attribute.Key("fleet.sandbox.backend")
	attrSandboxTimeout  = attribute.Key("fleet.sandbox.timeout_ms")
	attrSandboxExitCode = attribute.Key("fleet.sandbox.exit_code")
	attrSandboxStatus   = attribute.Key("fleet.sandbox.status")
	attrSandboxTimedOut = attribute.Key("fleet.sandbox.timed_out")
	attrSandboxInBytes  = attribute.Key("fleet.sandbox.input_bytes")
	attrSandboxOutBytes = attribute.Key("fleet.sandbox.output_bytes")
)

func (s *Sandbox) startExecSpan(ctx context.Context, op string, timeout time.Duration, inBytes int) (context.Context, trace.Span) {
	ctx, span := telemetry.Start(ctx, "sandbox.exec")
	if span.IsRecording() {
		span.SetAttributes(
			attrSandboxOp.String(op),
			attrSandboxBackend.String(s.ModeName()),
			attrSandboxTimeout.Int64(timeout.Milliseconds()),
			attrSandboxInBytes.Int(inBytes),
		)
	}
	return ctx, span
}

func endBashSpan(span trace.Span, res BashResult, err error) {
	if span.IsRecording() {
		span.SetAttributes(
			attrSandboxExitCode.Int(res.ExitCode),
			attrSandboxTimedOut.Bool(res.TimedOut),
			attrSandboxOutBytes.Int(len(res.Stdout)+len(res.Stderr)),
		)
	}
	telemetry.End(span, err)
}

func endPythonSpan(span trace.Span, res PythonResult, err error) {
	if span.IsRecording() {
		span.SetAttributes(
			attrSandboxStatus.String(res.Status),
			attrSandboxOutBytes.Int(len(res.Output)+len(res.Stdout)+len(res.Stderr)+len(res.Result)),
		)
	}
	telemetry.End(span, err)
}

func endFileOpSpan(span trace.Span, res FileOpResult, err error) {
	if span.IsRecording() {
		span.SetAttributes(attrSandboxOutBytes.Int64(res.Size))
	}
	telemetry.End(span, err)
}
