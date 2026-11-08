package sandbox

import (
	"io"
	"log/slog"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// bridgeStderrLogger is where a sandbox bridge process's stderr goes: each
// line a slog record ("subprocess stderr", subprocess=sandbox-bridge),
// redacted and in the configured format, level and sinks — never the raw
// bytes copied to this process's stderr. The bridge runs model-authored
// Python, so its tracebacks can quote anything the code touched. The last
// unterminated line is lost if the bridge dies mid-line: the bridge is torn
// down by kill, with no point where its stderr is known to be drained.
func bridgeStderrLogger() io.Writer {
	return logging.NewStderrLogger(slog.String("subprocess", "sandbox-bridge"))
}
