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
	"io"
	"log"
	"os"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Config holds the rotating-file-sink knobs. The zero value (File == "") means
// the sink is OFF and Configure leaves stderr logging untouched.
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
}

// Enabled reports whether the file sink is configured on.
func (c Config) Enabled() bool { return c.File != "" }

// Configure installs the rotating file sink onto the standard log package when
// cfg.File is set, teeing every standard-log line to BOTH stderr and the
// rotating file. It returns an io.Closer for the underlying rotating writer
// (which the caller defers to flush/close on shutdown) and any error opening the
// file. When cfg.File is empty it is a no-op: it returns (nil, nil) and stderr
// logging is left exactly as the standard library set it up — so the default,
// journald-friendly behaviour is unchanged.
//
// Only the standard log package's destination is changed. internal/safe keeps
// its own slog handler bound to stderr by design (a recovered panic must still
// reach stderr/journald even if a misconfigured file sink is failing), so panic
// events are unaffected.
func Configure(cfg Config) (io.Closer, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	rotor := &lumberjack.Logger{
		Filename:   cfg.File,
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		Compress:   cfg.Compress,
	}
	// Lumberjack creates the file lazily on first write; force-create it now so a
	// bad path (unwritable dir) fails loudly at startup rather than silently
	// dropping every later log line.
	if _, err := rotor.Write([]byte{}); err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(os.Stderr, rotor))
	return rotor, nil
}
