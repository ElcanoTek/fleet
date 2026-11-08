package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"time"
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
	inPEM    bool // inside a PEM private-key block: suppress its lines (send)
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
//
// It also keeps the one piece of redaction state that spans lines: a PEM
// private-key block. Redaction runs per line, and neither the PEM pattern
// (BEGIN…END) nor a registered multi-line literal can match a block that
// arrives as separate lines, so the body lines would go out as written. From
// a BEGIN … PRIVATE KEY line the splitter emits one "[REDACTED PEM block]"
// line in its place and suppresses what follows up to and including the END
// line — or up to the first line that cannot be part of a PEM body (not
// base64, not an RFC 1421 header), which is emitted normally, so a block
// that never ends cannot swallow the log.
func (s *lineSplitter) send(line []byte, dropped int) {
	if s.inPEM {
		if pemEndRE.Match(line) {
			s.inPEM = false
			return
		}
		if dropped > 0 || pemBodyLine(line) {
			return
		}
		s.inPEM = false
	}
	if loc := pemBeginRE.FindIndex(line); loc != nil && !pemEndRE.Match(line[loc[1]:]) {
		s.inPEM = true
		line = append([]byte(Redact(string(line[:loc[0]]))), pemPlaceholder...)
		dropped = 0
	}
	if err := s.emit(line, dropped); err != nil {
		if !s.failing {
			s.failing = true
			sinkFailed("subprocess stderr forwarder", err)
		}
		return
	}
	s.failing = false
}

// pemPlaceholder replaces a whole PEM private-key block in subprocess output.
const pemPlaceholder = "[REDACTED PEM block]"

var (
	pemBeginRE = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEndRE   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// pemBodyRE is one line of a PEM body: base64, or an RFC 1421 header
	// (Proc-Type, DEK-Info) of an encrypted key, or blank.
	pemBodyRE = regexp.MustCompile(`^\s*(?:[A-Za-z0-9+/=]*|[A-Za-z-]+:.*)\s*$`)
)

func pemBodyLine(line []byte) bool { return pemBodyRE.Match(line) }

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

// ChildLineMessage is the message of the record a child stderr line that is
// not one of the child's own log records is wrapped in; the line itself is
// the "line" attribute.
const ChildLineMessage = "child stderr"

// ChildStderr returns a writer for a child process's stderr (child names it
// in every record: "mcp-broker") that logs each line through this process's
// configured handler, so it takes this process's format, FLEET_LOG_LEVEL,
// redactor and sinks (stderr, plus FLEET_LOG_FILE under the one rotator the
// parent owns — two processes rotating one file would rename it under each
// other). Nothing is copied to the sink as written.
//
//   - A line that parses as one JSON object with a "msg" (the child's own
//     slog record, which the broker always writes as JSON) is re-logged as a
//     record of this process: the child's time, level and message, each
//     other field an attribute (nested objects as groups), plus child=<name>.
//     The handler's redaction then applies exactly as to a parent attribute:
//     the secret-key rule at every depth, pattern and literal scrubbing of
//     every string. A child library printing {"authorization": "…"} is
//     scrubbed like a parent attribute would be.
//   - Anything else — a panic trace, a library's raw print, a line that only
//     looks like JSON, a line cut at MaxStderrLine — is logged at warn as
//     msg="child stderr" with the redacted text in "line".
//
// Lines are capped at MaxStderrLine, multi-line PEM blocks are suppressed
// (see lineSplitter.send), and Write never fails. Close the writer after the
// child has been waited on so a last line with no newline is still logged.
func ChildStderr(child string) io.WriteCloser {
	return &lineSplitter{emit: func(line []byte, dropped int) error {
		forwardChildLine(child, line, dropped)
		return nil
	}}
}

func forwardChildLine(child string, line []byte, dropped int) {
	ctx := context.Background()
	if dropped == 0 {
		if rec, ok := childRecord(line); ok {
			rec.AddAttrs(slog.String("child", child))
			if h := slog.Default().Handler(); h.Enabled(ctx, rec.Level) {
				_ = h.Handle(ctx, rec)
			}
			return
		}
	}
	attrs := []slog.Attr{slog.String("child", child)}
	if dropped > 0 {
		attrs = append(attrs, slog.String("line", truncatedLine(line, dropped)), slog.Bool("truncated", true))
	} else {
		attrs = append(attrs, slog.String("line", Redact(string(line))))
	}
	slog.Default().LogAttrs(ctx, slog.LevelWarn, ChildLineMessage, attrs...)
}

// childRecord parses line as exactly one JSON object carrying a string "msg"
// and returns it as a record: time and level from the object when they parse
// (now and info otherwise), every other field an attribute in order.
func childRecord(line []byte) (slog.Record, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return slog.Record{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return slog.Record{}, false
	}
	var (
		when   = time.Now()
		level  = slog.LevelInfo
		msg    string
		hasMsg bool
		attrs  []slog.Attr
		// the record's own keys, so a child field named like one is suffixed
		seenKeys = map[string]bool{slog.TimeKey: true, slog.LevelKey: true, slog.MessageKey: true}
	)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return slog.Record{}, false
		}
		key, isKey := tok.(string)
		if !isKey {
			return slog.Record{}, false
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return slog.Record{}, false
		}
		switch key {
		case slog.TimeKey:
			if s, ok := v.(string); ok {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					when = t
				}
			}
			continue
		case slog.LevelKey:
			if s, ok := v.(string); ok {
				var l slog.Level
				if l.UnmarshalText([]byte(s)) == nil {
					level = l
				}
			}
			continue
		case slog.MessageKey:
			if s, ok := v.(string); ok {
				msg, hasMsg = s, true
				continue
			}
		}
		// The key is scrubbed like every nested key (redactedKeys) and made
		// unique in document order.
		attrs = append(attrs, jsonAttr(uniqueKey(seenKeys, Redact(key)), v))
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return slog.Record{}, false
	}
	// Exactly one object: trailing bytes make it not a record.
	if _, err := dec.Token(); err != io.EOF {
		return slog.Record{}, false
	}
	if !hasMsg {
		return slog.Record{}, false
	}
	rec := slog.NewRecord(when, level, msg, 0)
	rec.AddAttrs(attrs...)
	return rec, true
}

// jsonAttr turns one top-level decoded JSON field into an attribute the
// handler's ReplaceAttr redacts like a native one: scalars typed (numbers as
// numbers, so the secret-key rule for numbers applies), objects and arrays
// as slog.Any — which ReplaceAttr hands to redactComposite, the one walk that
// redacts every nested key and value. The top-level key arrives scrubbed and
// unique (childRecord); nothing from the child is trusted.
func jsonAttr(key string, v any) slog.Attr {
	switch x := v.(type) {
	case string:
		return slog.String(key, x)
	case bool:
		return slog.Bool(key, x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return slog.Int64(key, n)
		}
		if f, err := x.Float64(); err == nil {
			return slog.Float64(key, f)
		}
		return slog.String(key, x.String())
	default: // nil, map[string]any, []any
		return slog.Any(key, x)
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
