// Package logging owns fleet's process log: its format, its level, its secret
// redactor, the optional rotating file sink, and the context logger every call
// site uses (#2052, completing #178).
//
// Every server log line is a log/slog record. Call sites log through
// From(ctx), which returns the default logger already carrying the request's
// correlation attributes (conv_id, turn_id, task_id, run_id, user_id,
// request_id, and trace_id/span_id when a span is active — see context.go), or
// through the package-level slog functions where no context exists. The
// standard library log package's print and fatal functions are banned by a
// forbidigo rule (.golangci.yml); whatever a third-party library still writes
// through it is bridged into slog by logBridge, so the stream stays one format.
//
// FORMAT. FLEET_LOG_FORMAT=json (the default) emits one JSON object per line
// ({"time":…,"level":…,"msg":…,<attrs>}), which journald stores verbatim and
// Loki/Datadog/Vector parse natively. FLEET_LOG_FORMAT=text emits slog's logfmt
// lines (time=… level=… msg="…" key=value) for an operator reading journalctl
// by eye. Both formats go through the same redactor and level gate.
//
// DESTINATION. stderr, which journald captures and rotates under the shipped
// systemd unit (ADR-0004). When an operator sets FLEET_LOG_FILE (typically a
// container/non-systemd deployment) the same lines are also teed to a
// size-rotated file (internal/logging/rotate, in-housed from lumberjack).
//
// REDACTION. replaceAttr (redact.go) runs on every attribute and on the
// message: a value whose KEY names a secret is blanked, and every string value
// is passed through internal/redact — the same canonical patterns and literal
// set that scrub tool output (agentcore/redact.go) and broker errors
// (mcpbroker/redact.go).
package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/ElcanoTek/fleet/internal/logging/rotate"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// Config holds the process-log knobs. The zero value (File == "") means the file
// sink is OFF; Format defaults to structured JSON when unset.
type Config struct {
	// File is the path to the active log file. EMPTY DISABLES the file sink
	// entirely (the default) — the only switch that turns the feature on.
	File string
	// MaxSizeMB is the size at which the active file is rotated, in megabytes.
	MaxSizeMB int
	// MaxAgeDays is the maximum age of a rotated file before it is deleted. 0
	// disables age-based deletion (size/backup limits still apply).
	MaxAgeDays int
	// MaxBackups is how many rotated files to retain; older ones are deleted. 0
	// retains all (subject to MaxAgeDays).
	MaxBackups int
	// Compress gzips rotated files when true.
	Compress bool
	// Format is "json" (default) or "text" (slog logfmt). Empty is "json".
	Format string
	// Level is the minimum level: debug|info|warn|error. Empty defaults to info.
	Level string
}

// levelVar is the process-wide slog level knob, set once by Configure from
// FLEET_LOG_LEVEL. The level is not reloadable at runtime (docs/CONFIG-RELOAD.md).
var levelVar = new(slog.LevelVar)

// ParseLevel maps a level string to an slog.Level, defaulting to info on any
// unrecognized/empty value.
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(s)))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Enabled reports whether the file sink is configured on.
func (c Config) Enabled() bool { return c.File != "" }

// Configure sets up process logging. The destination is stderr, plus a
// size/age/backup-rotated file when cfg.File is set. The format is cfg.Format
// ("json" by default, or "text" for logfmt); both install a redacting slog
// handler as the default logger, gated by cfg.Level, and route the standard
// log package (third-party code only — fleet's own call sites are slog) into
// an ungated bridge logger of the same format.
//
// It returns an io.Closer for the rotating file (nil when the file sink is off)
// for the caller to defer-close, and any error opening the file.
//
// Recovered panics (internal/safe) go through a handler of the same format,
// destination and redactor, installed with safe.SetPanicHandler; safe falls
// back to its own direct-stderr JSON logger only if that handler fails, so a
// panic always reaches stderr/journald even with a broken file sink.
func Configure(cfg Config) (io.Closer, error) {
	// The redactor knows the process's secret-named env values by literal
	// before the first configured line is written — and before Configure
	// replays the startup buffer below. Every later secret source used to
	// register lazily (the tool-output or broker redactor's first use), so a
	// line logged before then — a connector's boot-time HTTP 401 echoing its
	// opaque API key — was scrubbed by shape patterns alone.
	seedEnvLiterals()
	// Destination: stderr, plus the rotating file when configured.
	dest := lockedStderr
	var closer io.Closer
	if cfg.Enabled() {
		rotor := &rotate.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSizeMB,
			MaxAge:     cfg.MaxAgeDays,
			MaxBackups: cfg.MaxBackups,
			Compress:   cfg.Compress,
		}
		// The rotator creates the file lazily on first write; force-create it now so
		// a bad path (unwritable dir) fails loudly at startup rather than silently
		// dropping every later log line.
		if _, err := rotor.Write([]byte{}); err != nil {
			return nil, err
		}
		// Each sink independently: a full disk or a failed rotation must not
		// fail a write (a subprocess-stderr forwarder would stop draining).
		dest = newTee(teeSink{name: "stderr", w: stderrFile{}}, teeSink{name: "file", w: rotor})
		closer = rotor
	}

	newHandler := newJSONHandler
	if strings.EqualFold(strings.TrimSpace(cfg.Format), "text") {
		newHandler = newTextHandler
	}
	configured.Store(true)
	levelVar.Set(ParseLevel(cfg.Level))
	setOutput(dest)
	slog.SetDefault(slog.New(newHandler(dest, levelVar)))
	// Lines logged before Configure (BufferStartup) go out now, through the
	// configured format, level and file sink.
	replayStartup(slog.Default().Handler())
	bridgeLogger = slog.New(newHandler(dest, slog.LevelInfo))
	// contextAttrHandler: a panic recovered under a request or a run carries
	// that context's correlation (safe.EmitPanicWithMetadataContext).
	safe.SetPanicHandler(contextAttrHandler{inner: newHandler(dest, slog.LevelError)})
	log.SetFlags(0) // slog stamps its own time; drop the stdlib "2006/01/02 …" prefix
	log.SetOutput(logBridge{})
	return closer, nil
}

// bridgeLogger emits bridged standard-library log lines. Fleet's own code no
// longer writes through the log package (forbidigo bans it), so what arrives
// here is a third-party library's diagnostic — net/http's server error log, a
// driver's warning — whose TRUE severity is unknown (assumed info). It is
// deliberately NOT gated by FLEET_LOG_LEVEL: raising the level to warn must not
// silently erase a library's report of a failure it could only express as a
// plain print. Reassigned once in Configure (before any concurrent logging);
// defaults to stderr pre-Configure.
var bridgeLogger = slog.New(newJSONHandler(lockedStderr, slog.LevelInfo))

// newJSONHandler builds the structured-JSON handler: an slog JSON handler at
// dest, gated by level, with the secret redactor as its ReplaceAttr. Extracted
// so tests can exercise it over a buffer without touching process-global state.
func newJSONHandler(dest io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewJSONHandler(dest, &slog.HandlerOptions{Level: level, ReplaceAttr: replaceAttr})
}

// newTextHandler is newJSONHandler's logfmt sibling (FLEET_LOG_FORMAT=text).
func newTextHandler(dest io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewTextHandler(dest, &slog.HandlerOptions{Level: level, ReplaceAttr: replaceAttr})
}

// NewHandler returns a redacting handler of the named format ("json" or
// "text") at dest — the same handler Configure installs, for a process that
// must log to a destination of its own (tests, a subprocess).
func NewHandler(format string, dest io.Writer, level slog.Leveler) slog.Handler {
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		return newTextHandler(dest, level)
	}
	return newJSONHandler(dest, level)
}

// logBridge routes standard-library log output into slog as Info records, so a
// library writing through log.Printf still emits a line in the configured
// format. One Write == one log line == one slog record.
//
// Interior CR/LF in the message are flattened to spaces before it reaches the
// handler (log-injection guard). The JSON handler already escapes them, so this
// is defense-in-depth; strings.ReplaceAll is the spelling CodeQL's
// go/log-injection query models as a sanitizer.
type logBridge struct{}

func (logBridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r", " "), "\n", " ")
	bridgeLogger.LogAttrs(context.Background(), slog.LevelInfo, msg)
	return len(p), nil
}

// Fatal logs msg at error level with args (slog key/value pairs or Attrs) on
// the default logger and exits the process with status 1 — the structured
// replacement for log.Fatalf. Like log.Fatal it does not run deferred calls,
// so use it only where the process has nothing left to unwind (main, a CLI
// verb's top level).
//
// Before Configure there is no redacting handler yet: Go's default slog
// handler, or the startup buffer. Fatal then logs through the emergency
// handler (redacting JSON on stderr, as FlushStartup uses) after seeding the
// redactor with the process's secret-named env values, so a config or bundle
// error quoting an interpolated secret does not reach stderr as written.
func Fatal(msg string, args ...any) {
	seedEnvLiterals()
	FlushStartup() // a fatal error before Configure must not lose the startup lines
	logger := slog.Default()
	if !configured.Load() {
		logger = slog.New(emergencyHandler())
	}
	logger.Error(msg, args...)
	exitFunc(1)
}

// exitFunc is os.Exit; tests replace it.
var exitFunc = os.Exit

// configured records that Configure installed the process handler.
var configured atomic.Bool
