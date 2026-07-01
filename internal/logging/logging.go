// Package logging provides an OPT-IN rotating file sink for fleet's process log.
//
// fleet logs startup diagnostics and operational lines through the standard
// library log package (and structured panic events through log/slog in
// internal/safe), all of which land on stderr by default — which, under the
// shipped systemd unit, journald captures and rotates for free (ADR-0004). That
// default is unchanged: with FLEET_LOG_FILE unset, Configure is a no-op and the
// process keeps writing exactly to stderr as before.
//
// When an operator sets FLEET_LOG_FILE — typically a container/non-systemd
// deployment where journald is not doing the rotation — this package additionally
// tees the standard log output to a lumberjack-rotated file with operator-tunable
// size/age/backup/compress knobs. It deliberately does NOT change the log FORMAT:
// it rotates the existing log lines as-is. Converting every call site to
// structured slog JSON is a separate migration (issue #178); this file sink is
// orthogonal to it and makes no claim to perform it.
package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Config holds the process-log knobs. The zero value (File == "") means the file
// sink is OFF; Format defaults to structured JSON (#178) when unset via the
// caller.
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
	// Format is "json" (default; structured log/slog JSON) or "text" (legacy
	// plaintext, exactly the prior behavior). Empty is treated as "json".
	Format string
	// Level is the minimum level for JSON output: debug|info|warn|error. Empty
	// defaults to info. Ignored for the text format.
	Level string
}

// levelVar is the process-wide slog level knob. Held at package scope so a
// config hot-reload can adjust it via SetLevel without re-plumbing the handler.
var levelVar = new(slog.LevelVar)

// SetLevel adjusts the live JSON-log level (debug|info|warn|error); an
// unrecognized value is treated as info. No-op semantics are fine in text mode
// (the level var simply isn't consulted). Safe to call concurrently — slog.LevelVar
// is atomic. Wired for FLEET_LOG_LEVEL hot-reload (#178/#286).
func SetLevel(s string) { levelVar.Set(ParseLevel(s)) }

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

// Configure sets up process logging (#178 + #298). The destination is stderr,
// plus a size/age/backup-rotated file when cfg.File is set. The FORMAT is chosen
// by cfg.Format:
//
//   - "json" (default): the standard log package AND log/slog are routed through
//     an slog JSON handler at the destination — every existing log.Printf line
//     becomes a structured JSON object (aggregation-friendly for Loki/Datadog/
//     journald-json), the level is FLEET_LOG_LEVEL (adjustable via SetLevel), and
//     secret-keyed attributes are redacted (defense-in-depth).
//   - "text": the legacy plaintext behavior, byte-for-byte — the standard log
//     package writes its usual lines to the destination and slog is untouched.
//
// It returns an io.Closer for the rotating file (nil when the file sink is off)
// for the caller to defer-close, and any error opening the file.
//
// internal/safe keeps its OWN slog handler bound to stderr by design (a recovered
// panic must still reach stderr/journald even if a misconfigured file sink is
// failing), so panic events are unaffected by the format choice here.
func Configure(cfg Config) (io.Closer, error) {
	// Destination: stderr, plus the rotating file when configured.
	var dest io.Writer = os.Stderr
	var closer io.Closer
	if cfg.Enabled() {
		rotor := &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSizeMB,
			MaxAge:     cfg.MaxAgeDays,
			MaxBackups: cfg.MaxBackups,
			Compress:   cfg.Compress,
		}
		// Lumberjack creates the file lazily on first write; force-create it now so
		// a bad path (unwritable dir) fails loudly at startup rather than silently
		// dropping every later log line.
		if _, err := rotor.Write([]byte{}); err != nil {
			return nil, err
		}
		dest = io.MultiWriter(os.Stderr, rotor)
		closer = rotor
	}

	// Legacy plaintext: standard log writes as-is to the destination; no slog.
	if strings.EqualFold(strings.TrimSpace(cfg.Format), "text") {
		log.SetOutput(dest)
		return closer, nil
	}

	// Structured JSON (default): install an slog JSON handler + redactor as the
	// default logger (gated by FLEET_LOG_LEVEL, for native slog call sites), plus
	// an UNGATED bridge logger so the standard log package emits JSON without
	// being rewritten AND without being level-filtered — see bridgeLogger.
	levelVar.Set(ParseLevel(cfg.Level))
	slog.SetDefault(slog.New(newJSONHandler(dest, levelVar)))
	bridgeLogger = slog.New(newJSONHandler(dest, slog.LevelInfo))
	log.SetFlags(0) // slog stamps its own time; drop the stdlib "2006/01/02 …" prefix
	log.SetOutput(logBridge{})
	return closer, nil
}

// bridgeLogger emits bridged standard-library log lines. It is deliberately
// pinned to a fixed info threshold and is NOT gated by FLEET_LOG_LEVEL: today
// the bulk of the process log still flows through the standard `log` package
// (the per-call-site slog conversion is ongoing, #178) and a bridged line's TRUE
// severity is unknown (assumed info). Gating it by level would let
// FLEET_LOG_LEVEL=warn silently erase the ENTIRE legacy diagnostic stream —
// including error/warning lines and a fatal-startup explanation. So legacy lines
// always emit; FLEET_LOG_LEVEL gates only native slog call sites. Reassigned once
// in Configure (before any concurrent logging); defaults to stderr pre-Configure.
var bridgeLogger = slog.New(newJSONHandler(os.Stderr, slog.LevelInfo))

// newJSONHandler builds the structured-JSON handler stack: an slog JSON handler
// at dest, gated by level, wrapped in the secret redactor. Extracted so tests
// can exercise it over a buffer without touching process-global logger state.
func newJSONHandler(dest io.Writer, level slog.Leveler) slog.Handler {
	return redactingHandler{Handler: slog.NewJSONHandler(dest, &slog.HandlerOptions{Level: level})}
}

// logBridge routes standard-library log output into slog as Info records, so
// legacy log.Printf/Println call sites emit structured JSON without being
// rewritten. One Write == one log line == one slog record.
type logBridge struct{}

func (logBridge) Write(p []byte) (int, error) {
	bridgeLogger.LogAttrs(context.Background(), slog.LevelInfo, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// redactingHandler wraps an slog.Handler and replaces the value of any attribute
// whose key looks secret with "[REDACTED]" — defense-in-depth for the
// credentials-never-in-logs invariant. Call sites are still expected not to log
// secret VALUES; this catches an accidental slog.String("token", …).
type redactingHandler struct{ slog.Handler }

func (h redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.Handler.Handle(ctx, nr)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return redactingHandler{Handler: h.Handler.WithAttrs(red)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{Handler: h.Handler.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if secretAttrKey(a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindGroup {
		gs := a.Value.Group()
		red := make([]slog.Attr, len(gs))
		for i, g := range gs {
			red[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(red...)}
	}
	return a
}

// secretAttrKey reports whether an attribute key names a likely secret.
func secretAttrKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range []string{"secret", "token", "password", "passwd", "apikey", "api_key", "authorization", "bearer", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}
