package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"unicode/utf8"
)

// output is the process log destination Configure set up: stderr, plus the
// rotating file when the file sink is on. Before Configure it is stderr —
// through the same lock (lockedStderr), never the raw file.
var (
	outputMu sync.RWMutex
	output   = lockedStderr
)

// stderrFile writes to whatever os.Stderr is at the time of the write.
type stderrFile struct{}

func (stderrFile) Write(p []byte) (int, error) { return os.Stderr.Write(p) }

// lockedStderr is stderr behind the process log lock: what every writer uses
// when no file sink is configured, and before Configure.
var lockedStderr io.Writer = newTee(teeSink{name: "stderr", w: stderrFile{}})

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
// the cap is reached the rest of that line is counted and dropped up to its
// terminator, and the line is emitted then as one truncated record (see
// truncatedLine for what of it is shown).
const MaxStderrLine = 64 << 10

// truncationAllowance is the part of the redaction margin covering the
// longest shape-pattern secret: a PEM private key block (an 8192-bit RSA key
// is ~6.4 KB) is the longest the canonical patterns match whole.
const truncationAllowance = 8 << 10

// truncatedLine renders a line cut at MaxStderrLine (head is its first
// MaxStderrLine bytes, dropped the bytes discarded after them). It fails
// closed on the cut: a registered literal or a vendor-shaped key that
// straddles it is never matched — only its prefix is in head — so that
// prefix would leak as written. head is redacted whole (so every secret
// that lies inside it is matched), then a margin as long as the longest
// registered literal plus truncationAllowance is withheld from its end. Any
// secret straddling the cut starts inside that margin, so none of it is
// shown. The marker gives the line's real length.
func truncatedLine(head []byte, dropped int) string {
	red := Redact(string(head))
	keep := len(red) - (redactor().MaxLiteralLen() + truncationAllowance)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(red[keep]) {
		keep--
	}
	return red[:keep] + fmt.Sprintf(" …[truncated: line was %d bytes]", len(head)+dropped)
}

// lineSplitter turns a byte stream into lines for emit. A line ends at "\n",
// "\r\n" or a lone "\r" (a progress bar's carriage-return redraw is a line of
// its own, not glue for the next one); the terminator is not passed on and
// empty lines are skipped. Pending bytes are capped at MaxStderrLine.
//
// Write NEVER fails. It is a subprocess's stderr: os/exec's copier stops
// reading the pipe at the first Write error, the pipe fills, and the child
// blocks on its next stderr write — a full log disk or a failed rotation
// would wedge the MCP broker. An emit error is reported once (until emits
// succeed again) through sinkFailed and the line is dropped; draining goes on.
type lineSplitter struct {
	mu       sync.Mutex
	pending  []byte
	dropping bool // the current line overflowed; discard to its terminator
	dropped  int  // bytes of the current line discarded past the cap
	closed   bool
	failing  bool // the last emit failed; its failure was reported
	// emit receives one line; dropped > 0 marks it truncated (head is then
	// the first MaxStderrLine bytes, dropped the bytes discarded after them).
	emit func(line []byte, dropped int) error
}

func (s *lineSplitter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rest := p
	for len(rest) > 0 {
		i := bytes.IndexAny(rest, "\r\n")
		if i < 0 {
			s.appendCapped(rest)
			break
		}
		s.appendCapped(rest[:i])
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
		s.endLine()
	}
	if len(s.pending) == 0 {
		s.pending = nil
	}
	return len(p), nil
}

// send emits one line and keeps the failure state: the first failure of a
// streak is reported, the rest are silent, a success ends the streak.
func (s *lineSplitter) send(line []byte, dropped int) {
	if err := s.emit(line, dropped); err != nil {
		if !s.failing {
			s.failing = true
			sinkFailed("subprocess stderr forwarder", err)
		}
		return
	}
	s.failing = false
}

// appendCapped adds b to the pending line up to MaxStderrLine; past the cap
// it only counts what it drops, and the line goes out truncated at its end.
func (s *lineSplitter) appendCapped(b []byte) {
	if s.closed {
		return
	}
	if s.dropping {
		s.dropped += len(b)
		return
	}
	room := MaxStderrLine - len(s.pending)
	if len(b) <= room {
		s.pending = append(s.pending, b...)
		return
	}
	s.pending = append(s.pending, b[:room]...)
	s.dropping, s.dropped = true, len(b)-room
}

func (s *lineSplitter) endLine() {
	if len(s.pending) > 0 && !s.closed {
		s.send(s.pending, s.dropped)
	}
	s.pending, s.dropping, s.dropped = s.pending[:0], false, 0
}

// Close emits a final unterminated line (a child that died mid-write, or
// whose last line had no newline) and makes later writes no-ops. Call it once
// the child has been waited on, when nothing more can arrive. Like Write it
// never fails.
func (s *lineSplitter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if len(s.pending) > 0 {
		s.send(s.pending, s.dropped)
	}
	s.pending, s.dropping, s.dropped, s.closed = nil, false, 0, true
	return nil
}

// sinkFailedHook reports a log-sink write failure; tests replace it.
var sinkFailedHook = func(sink string, err error) {
	// Asynchronous on purpose: this runs inside a sink's Write, which an slog
	// handler may be calling with its own lock held — logging from here
	// synchronously through the same handler would deadlock. The record
	// reaches every sink that still works.
	go slog.Default().LogAttrs(context.Background(), slog.LevelWarn,
		"logging: a log sink write failed; its lines are dropped until it recovers",
		slog.String("sink", sink), slog.String("err", err.Error()))
}

func sinkFailed(sink string, err error) { sinkFailedHook(sink, err) }

// ChildStderr returns a writer for a child process's stderr that forwards
// each complete line to this process's log destination (stderr, plus
// FLEET_LOG_FILE when it is on). A child that logs through Configure with no
// file of its own (the fleet mcp-broker) so lands in the same sink, under the
// one rotator the parent owns — two processes rotating one file would rename
// it under each other. Lines are forwarded whole, so a child line never
// interleaves with a parent line mid-way.
//
// Nothing the child writes is trusted, whatever its shape. A line that parses
// as one JSON object (the child's own slog records) is walked and redacted
// with this process's redactor — every key, nested ones included, by the same
// secret-key rule the slog handler applies, and every string value by pattern
// and registered literal — then re-serialised. Anything else (a text-format
// record, a panic trace, a line that only looks like JSON) is redacted as
// text. A child library that prints {"authorization": "…"} to stderr is
// scrubbed exactly like a parent attribute would be.
//
// Lines are capped at MaxStderrLine, and Write never fails (see lineSplitter).
// Close the writer after the child has been waited on so a last line with no
// newline is still forwarded.
func ChildStderr() io.WriteCloser {
	return &lineSplitter{emit: forwardChildLine}
}

func forwardChildLine(line []byte, dropped int) error {
	var out []byte
	if dropped > 0 {
		out = []byte(truncatedLine(line, dropped))
	} else if rec, ok := redactJSONRecord(line); ok {
		out = rec
	} else {
		out = []byte(Redact(string(line)))
	}
	out = append(out, '\n')
	_, err := currentOutput().Write(out)
	return err
}

// redactJSONRecord parses line as exactly one JSON object and returns it
// redacted and re-serialised, top-level keys in their original order (so a
// record still starts time, level, msg). ok is false for anything else.
func redactJSONRecord(line []byte) (out []byte, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	buf.WriteByte('{')
	for first := true; dec.More(); first = false {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isKey := tok.(string)
		if !isKey {
			return nil, false
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		if !first {
			buf.WriteByte(',')
		}
		if err := enc.Encode(Redact(key)); err != nil {
			return nil, false
		}
		trimNewline(&buf)
		buf.WriteByte(':')
		if err := enc.Encode(redactJSONField(key, v)); err != nil {
			return nil, false
		}
		trimNewline(&buf)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	// Exactly one object: trailing bytes make it not a record.
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	buf.WriteByte('}')
	return buf.Bytes(), true
}

func trimNewline(b *bytes.Buffer) {
	if n := b.Len(); n > 0 && b.Bytes()[n-1] == '\n' {
		b.Truncate(n - 1)
	}
}

// redactJSONField redacts one decoded JSON value under key: a secret-named
// key loses its whole value unless that value is a number or boolean (a
// token count), the same rule replaceAttr applies to a parent attribute;
// otherwise objects and arrays recurse and strings are scrubbed.
func redactJSONField(key string, v any) any {
	if secretAttrKey(key) && !isNumberOrBool(v) {
		return redactedValue
	}
	return redactJSONValue(v)
}

func redactJSONValue(v any) any {
	switch x := v.(type) {
	case string:
		return Redact(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactJSONValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[Redact(k)] = redactJSONField(k, e)
		}
		return out
	default: // json.Number, bool, nil
		return x
	}
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
	l.emit = func(line []byte, dropped int) error {
		rec := make([]slog.Attr, 0, len(attrs)+2)
		rec = append(rec, attrs...)
		if dropped > 0 {
			rec = append(rec, slog.String("line", truncatedLine(line, dropped)), slog.Bool("truncated", true))
		} else {
			rec = append(rec, slog.String("line", Redact(string(line))))
		}
		slog.Default().LogAttrs(context.Background(), slog.LevelInfo, StderrLineMessage, rec...)
		return nil
	}
	return l
}

// destMu is THE process log lock. Every path that writes a log line — the
// default handler, the stdlib-log bridge handler, the startup flush, the
// panic logger and the ChildStderr forwarder — writes through a teeWriter,
// and every teeWriter takes this one lock, holding it for the whole record.
// Each slog handler serialises only its own writes, so without it a parent
// record and a 64 KiB forwarded child record could interleave mid-line in
// the journald pipe (a pipe write over PIPE_BUF is not atomic).
var destMu sync.Mutex

// teeWriter is a log destination: every write goes to every sink, each
// independently, under destMu, and a failing sink never fails the write or
// starves the others. io.MultiWriter stopped at the first error and returned
// it — to a subprocess-stderr forwarder that was enough to stop the pipe
// drain. A sink's first failure in a streak is reported (sinkFailed); its
// lines are dropped until a write to it succeeds again. Callers hand it one
// whole record per Write.
type teeWriter struct {
	sinks   []teeSink
	failing []bool
}

type teeSink struct {
	name string
	w    io.Writer
}

func newTee(sinks ...teeSink) *teeWriter {
	return &teeWriter{sinks: sinks, failing: make([]bool, len(sinks))}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	destMu.Lock()
	defer destMu.Unlock()
	for i, s := range t.sinks {
		_, err := s.w.Write(p)
		switch {
		case err != nil && !t.failing[i]:
			t.failing[i] = true
			sinkFailed(s.name, err)
		case err == nil:
			t.failing[i] = false
		}
	}
	return len(p), nil
}
