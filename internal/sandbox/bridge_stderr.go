package sandbox

import (
	"context"
	"log/slog"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// bridgeStderrLogger is where a sandbox bridge process's stderr goes: each
// line a slog record ("subprocess stderr", subprocess=sandbox-bridge),
// redacted and in the configured format, level and sinks — never the raw
// bytes copied to this process's stderr. The bridge runs model-authored
// Python, so its tracebacks can quote anything the code touched.
//
// The caller Closes it once the bridge's stderr is drained — after cmd.Wait
// (podman, host) or the exec session's end (kubernetes) — so a last line the
// bridge wrote without a newline (a traceback cut short by the kill) is
// still logged.
//
// Its lines carry the correlation of the run_python request in flight (the
// backend Attributes each request to it), else the session-level ids of the
// request that started the bridge (sessionContext): a traceback is tied to
// the request, conversation and run that caused it.
func bridgeStderrLogger(ctx context.Context) *logging.StderrLogger {
	return logging.NewStderrLoggerContext(sessionContext(ctx), slog.String("subprocess", "sandbox-bridge"))
}

// sessionContext keeps only the session-level correlation of ctx — the
// conversation, the user, the task — for lines no request is attributed to:
// the starting request's own ids (request_id, turn_id, run_id, trace) would
// name a request that has long finished.
func sessionContext(ctx context.Context) context.Context {
	var attrs []slog.Attr
	for _, a := range logging.Attrs(ctx) {
		switch a.Key {
		case logging.KeyConvID, logging.KeyUserID, logging.KeyTaskID:
			attrs = append(attrs, a)
		}
	}
	return logging.WithAttrs(context.Background(), attrs...)
}
