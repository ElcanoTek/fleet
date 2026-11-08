package sandbox

import (
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
func bridgeStderrLogger() *logging.StderrLogger {
	return logging.NewStderrLogger(slog.String("subprocess", "sandbox-bridge"))
}
