package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync"
)

// output is the process log destination Configure set up: stderr, plus the
// rotating file when the file sink is on. Before Configure it is stderr.
var (
	outputMu sync.RWMutex
	output   io.Writer = os.Stderr
)

func setOutput(w io.Writer) {
	outputMu.Lock()
	output = w
	outputMu.Unlock()
}

func currentOutput() io.Writer {
	outputMu.RLock()
	defer outputMu.RUnlock()
	return output
}

// MaxStderrLine caps how many bytes of one unterminated stderr line a
// ChildStderr or StderrLogger holds while it waits for the newline. A child
// that writes megabytes with no newline (a binary blob, a minified bundle, a
// runaway progress bar) would otherwise grow the buffer without bound. When
// the cap is reached the held bytes are emitted as one record marked
// truncated, and the rest of that line is dropped up to its terminator.
const MaxStderrLine = 64 << 10

// truncatedMarker ends a forwarded line that hit MaxStderrLine.
const truncatedMarker = " …[truncated]"

// lineSplitter turns a byte stream into lines for emit. A line ends at "\n",
// "\r\n" or a lone "\r" (a progress bar's carriage-return redraw is a line of
// its own, not glue for the next one); the terminator is not passed on and
// empty lines are skipped. Pending bytes are capped at MaxStderrLine.
type lineSplitter struct {
	mu       sync.Mutex
	pending  []byte
	dropping bool // the current line overflowed; discard to its terminator
	closed   bool
	emit     func(line []byte, truncated bool) error
}

func (s *lineSplitter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	rest := p
	for len(rest) > 0 {
		i := bytes.IndexAny(rest, "\r\n")
		if i < 0 {
			s.appendCapped(rest, keep)
			break
		}
		s.appendCapped(rest[:i], keep)
		term := rest[i]
		rest = rest[i+1:]
		if term == '\r' {
			// "\r\n" is one terminator. A "\r" that ends this write may be
			// the first half of one; the next write's leading "\n" is then
			// an empty line, which is skipped.
			if len(rest) > 0 && rest[0] == '\n' {
				rest = rest[1:]
			}
		}
		s.endLine(keep)
	}
	if len(s.pending) == 0 {
		s.pending = nil
	}
	return len(p), firstErr
}

// appendCapped adds b to the pending line, emitting it as truncated once it
// reaches MaxStderrLine and dropping the remainder of that line.
func (s *lineSplitter) appendCapped(b []byte, keep func(error)) {
	if s.dropping || s.closed {
		return
	}
	room := MaxStderrLine - len(s.pending)
	if len(b) <= room {
		s.pending = append(s.pending, b...)
		return
	}
	s.pending = append(s.pending, b[:room]...)
	keep(s.emit(s.pending, true))
	s.pending = s.pending[:0]
	s.dropping = true
}

func (s *lineSplitter) endLine(keep func(error)) {
	if s.dropping {
		s.dropping = false
		return
	}
	if len(s.pending) > 0 && !s.closed {
		keep(s.emit(s.pending, false))
	}
	s.pending = s.pending[:0]
}

// Close emits a final unterminated line (a child that died mid-write, or
// whose last line had no newline) and makes later writes no-ops. Call it once
// the child has been waited on, when nothing more can arrive.
func (s *lineSplitter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	var err error
	if len(s.pending) > 0 && !s.dropping {
		err = s.emit(s.pending, false)
	}
	s.pending, s.dropping, s.closed = nil, false, true
	return err
}

// ChildStderr returns a writer for a child process's stderr that forwards
// each complete line to this process's log destination (stderr, plus
// FLEET_LOG_FILE when it is on). A child that logs through Configure with no
// file of its own (the fleet mcp-broker) so lands in the same sink, under the
// one rotator the parent owns — two processes rotating one file would rename
// it under each other. Lines are forwarded whole, so a child line never
// interleaves with a parent line mid-way.
//
// A line that is a well-formed JSON log record (the child's own slog handler,
// which already redacted it) is forwarded verbatim. Anything else — a text
// format record, a Go panic trace, raw bytes some library wrote to stderr —
// is scrubbed with this process's redactor first, so a secret a child
// printed outside slog does not reach the parent's sink as written.
//
// Lines are capped at MaxStderrLine. Close the writer after the child has
// been waited on so a last line with no newline is still forwarded.
func ChildStderr() io.WriteCloser {
	return &lineSplitter{emit: forwardChildLine}
}

func forwardChildLine(line []byte, truncated bool) error {
	var out []byte
	if !truncated && isJSONLogRecord(line) {
		out = make([]byte, 0, len(line)+1)
		out = append(out, line...)
	} else {
		out = []byte(Redact(string(line)))
		if truncated {
			out = append(out, truncatedMarker...)
		}
	}
	out = append(out, '\n')
	_, err := currentOutput().Write(out)
	return err
}

// isJSONLogRecord reports a line the JSON slog handler wrote: one JSON object
// carrying the time, level and msg keys.
func isJSONLogRecord(line []byte) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(line, &rec); err != nil {
		return false
	}
	for _, k := range []string{slog.TimeKey, slog.LevelKey, slog.MessageKey} {
		if _, ok := rec[k]; !ok {
			return false
		}
	}
	return true
}

// StderrLogger is a writer for a subprocess's stderr that logs each line as
// a slog record through the default logger, rather than copying the bytes to
// this process's stderr. Each line is scrubbed with the log redactor before
// it is logged (and again by the handler's attribute hook), and carries the
// attrs given to NewStderrLogger — the connector name, for an MCP server.
// Lines are capped at MaxStderrLine; Close flushes a last unterminated line.
type StderrLogger struct {
	lineSplitter
}

// StderrLineMessage is the message of every record a StderrLogger logs; the
// line itself is the "line" attribute.
const StderrLineMessage = "subprocess stderr"

// NewStderrLogger returns a StderrLogger whose records carry attrs.
func NewStderrLogger(attrs ...slog.Attr) *StderrLogger {
	attrs = append([]slog.Attr(nil), attrs...)
	l := &StderrLogger{}
	l.emit = func(line []byte, truncated bool) error {
		rec := make([]slog.Attr, 0, len(attrs)+2)
		rec = append(rec, attrs...)
		rec = append(rec, slog.String("line", Redact(string(line))))
		if truncated {
			rec = append(rec, slog.Bool("truncated", true))
		}
		slog.Default().LogAttrs(context.Background(), slog.LevelInfo, StderrLineMessage, rec...)
		return nil
	}
	return l
}
