package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
)

// childLog captures what the default logger writes, as JSON records.
type childLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *childLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *childLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *childLog) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func (c *childLog) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace([]byte(c.String())), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("not a JSON record: %q", raw)
		}
		out = append(out, m)
	}
	return out
}

// lines is what each record says: the wrapped line for a "child stderr"
// record, the message otherwise.
func (c *childLog) lines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range c.records(t) {
		if m["msg"] == ChildLineMessage {
			s, _ := m["line"].(string)
			out = append(out, s)
			continue
		}
		s, _ := m["msg"].(string)
		out = append(out, s)
	}
	return out
}

// captureOutput points the default logger (which ChildStderr logs through)
// at a JSON handler over a buffer, at debug.
func captureOutput(t *testing.T) *childLog {
	t.Helper()
	c := &childLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(NewHandler("json", c, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// A child process's stderr (the fleet mcp-broker's JSON log records) is
// re-logged through the parent's configured handler — so it reaches
// FLEET_LOG_FILE in the parent's format, attributed child=<name> — one whole
// line at a time however the pipe splits the bytes.
func TestChildStderrRelogsChildRecordsToTheLogFile(t *testing.T) {
	prevSlog := slog.Default()
	prevBridge := bridgeLogger
	prevOut := currentOutput()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		bridgeLogger = prevBridge
		setOutput(prevOut)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "text", Level: "info"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	defer closer.Close()

	w := ChildStderr("mcp-broker")
	for _, chunk := range []string{`{"time":"2026-11-08T01:02:03Z","level":"WARN","msg":"mcpbroker: call`, ` failed","op":"tool call"}` + "\n" + `{"level":"DEBUG","msg":"sec`, `ond"}` + "\n" + `{"msg":"partial`} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `time=2026-11-08T01:02:03.000Z level=WARN msg="mcpbroker: call failed" op="tool call" child=mcp-broker` + "\n"
	if !strings.HasSuffix(string(data), want) {
		t.Fatalf("log file = %q, want it to end with the child record in the parent's (text) format %q", data, want)
	}
	if strings.Contains(string(data), "second") {
		t.Fatalf("a child debug record passed the parent's info level: %q", data)
	}
	if strings.Contains(string(data), "partial") {
		t.Fatalf("an unterminated child line was written: %q", data)
	}
}

// A child line that is not one of its log records — raw bytes a connector or
// library wrote to the broker's stderr outside slog — is wrapped in a warn
// record (msg="child stderr", line=<redacted>, child=<name>); a child record
// is re-logged with its own level and message.
func TestChildStderrWrapsLinesThatAreNotRecords(t *testing.T) {
	out := captureOutput(t)
	const literal = "Zt7qWm3RkP9sLx2V"
	RegisterSecretLiterals("", false, literal)
	shaped := "sk-ant-" + strings.Repeat("C", 32)

	w := ChildStderr("mcp-broker")
	for _, line := range []string{
		"Authorization: Bearer " + shaped,
		"connector echoed " + literal,
		`{"time":"2026-11-08T00:00:00Z","level":"INFO","msg":"mcpbroker: ready","n":1}`,
		`{"not":"a log record","v":"` + literal + `"}`,
	} {
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	got := out.String()
	for _, leak := range []string{shaped, literal} {
		if strings.Contains(got, leak) {
			t.Fatalf("secret %q reached the parent sink: %q", leak, got)
		}
	}
	recs := out.records(t)
	if len(recs) != 4 {
		t.Fatalf("want 4 records, got %d: %s", len(recs), got)
	}
	for _, i := range []int{0, 1, 3} {
		if recs[i]["msg"] != ChildLineMessage || recs[i]["level"] != "WARN" || recs[i]["child"] != "mcp-broker" {
			t.Fatalf("record %d not a wrapped child line: %v", i, recs[i])
		}
	}
	if recs[2]["msg"] != "mcpbroker: ready" || recs[2]["level"] != "INFO" || recs[2]["n"] != float64(1) || recs[2]["child"] != "mcp-broker" || recs[2]["time"] != "2026-11-08T00:00:00Z" {
		t.Fatalf("child record not re-logged as itself: %v", recs[2])
	}
}

// pending is capped: a line with no newline past MaxStderrLine is logged as
// one truncated record, the rest of that line is dropped, and the next line
// is logged normally.
func TestChildStderrCapsAnUnterminatedLine(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr("child")
	chunk := bytes.Repeat([]byte("a"), 16<<10)
	for range 20 { // 320 KiB, no newline
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	sp := w.(*lineSplitter)
	sp.mu.Lock()
	held := len(sp.pending)
	sp.mu.Unlock()
	if held > MaxStderrLine {
		t.Fatalf("pending = %d bytes, want <= %d", held, MaxStderrLine)
	}
	if _, err := w.Write([]byte("bbb\nnext line\n")); err != nil {
		t.Fatal(err)
	}
	lines := out.lines(t)
	if len(lines) != 2 {
		t.Fatalf("want the truncated line and the next line, got %d lines", len(lines))
	}
	shown := MaxStderrLine - redactor().MaxLiteralLen() - truncationAllowance
	if want := strings.Repeat("a", shown) + " …[truncated: line was 327683 bytes]"; lines[0] != want {
		t.Fatalf("truncated line: len %d, suffix %q", len(lines[0]), lines[0][max(0, len(lines[0])-20):])
	}
	if lines[1] != "next line" {
		t.Fatalf("line after the overflow = %q", lines[1])
	}
}

// Close logs the last unterminated line — the child exited without a
// trailing newline — and later writes are ignored.
func TestChildStderrCloseFlushesThePartialLine(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr("child")
	_, _ = w.Write([]byte("first\npanic: last words"))
	if strings.Contains(out.String(), "last words") {
		t.Fatalf("partial line logged before Close: %q", out)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.lines(t); len(got) != 2 || got[0] != "first" || got[1] != "panic: last words" {
		t.Fatalf("after Close = %q", got)
	}
	_ = w.Close()
	_, _ = w.Write([]byte("late\n"))
	if strings.Contains(out.String(), "late") {
		t.Fatalf("write after Close logged: %q", out)
	}
}

// "\r\n" is one terminator and a lone "\r" (a progress-bar redraw) ends a
// line, including when the pair is split across writes.
func TestChildStderrCarriageReturns(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr("child")
	for _, chunk := range []string{"crlf\r\n", "split\r", "\nbar 10%\rbar 20%\r", "\n"} {
		_, _ = w.Write([]byte(chunk))
	}
	if got, want := strings.Join(out.lines(t), "|"), "crlf|split|bar 10%|bar 20%"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A StderrLogger logs each subprocess stderr line as a slog record with its
// attrs and the line redacted, caps an unterminated line, and flushes the
// last partial line on Close.
func TestStderrLoggerLogsRedactedLinesWithAttribution(t *testing.T) {
	var buf bytes.Buffer
	withDefault(t, &buf)
	const literal = "Hy5nKq8VtR2mWp4Z"
	RegisterSecretLiterals("", false, literal)

	l := NewStderrLogger(slog.String("connector", "acme"))
	_, _ = l.Write([]byte("Traceback: token was " + literal + "\npartial "))
	_, _ = l.Write([]byte("tail"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raws := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	recs := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("not JSON: %q", raw)
		}
		recs = append(recs, m)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %s", len(recs), buf.String())
	}
	for _, m := range recs {
		if m["msg"] != StderrLineMessage || m["connector"] != "acme" {
			t.Fatalf("record = %v", m)
		}
	}
	if line, _ := recs[0]["line"].(string); strings.Contains(line, literal) || !strings.Contains(line, "Traceback") {
		t.Fatalf("first line = %q, want it redacted", line)
	}
	if recs[1]["line"] != "partial tail" {
		t.Fatalf("Close flush line = %v", recs[1]["line"])
	}

	buf.Reset()
	l = NewStderrLogger()
	_, _ = l.Write(bytes.Repeat([]byte("x"), MaxStderrLine+100))
	_ = l.Close() // an overflowing line goes out at its end; Close is its end here
	m := decodeLast(t, &buf)
	line, _ := m["line"].(string)
	if m["truncated"] != true || len(line) >= MaxStderrLine || !strings.HasSuffix(line, fmt.Sprintf("[truncated: line was %d bytes]", MaxStderrLine+100)) {
		t.Fatalf("overflow record: truncated=%v len=%d suffix=%q", m["truncated"], len(line), line[max(0, len(line)-40):])
	}
}

// A PEM private key arrives on a subprocess's stderr one line per write: no
// single line matches the BEGIN…END pattern, so line-by-line redaction alone
// would print its body. The splitter replaces the whole block with one
// placeholder line and resumes normal logging after END (#2074 review).
func TestChildStderrSuppressesAMultiLinePEMBlock(t *testing.T) {
	out := captureOutput(t)
	body := []string{
		"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj",
		"MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu",
		"NMoSfm76oqFvAp8Gy0iz5sxjZmSnXyCdPEovGhLa0VzMaQ8s+CLOyS56YyCFGeJZ",
	}
	block := "key dump: -----BEGIN PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n-----END PRIVATE KEY-----\nafter the key\n"
	for _, w := range []interface {
		Write([]byte) (int, error)
	}{ChildStderr("child"), NewStderrLogger(slog.String("connector", "acme"))} {
		out.Reset()
		for _, b := range []byte(block) { // the worst split: one byte per write
			_, _ = w.Write([]byte{b})
		}
		got := out.String()
		for _, l := range body {
			if strings.Contains(got, l[:16]) {
				t.Fatalf("a PEM body line leaked (%T): %s", w, got)
			}
		}
		recs := out.records(t)
		lines := make([]string, 0, len(recs))
		for _, m := range recs {
			s, _ := m["line"].(string)
			lines = append(lines, s)
		}
		if len(lines) != 2 || lines[0] != "key dump: "+pemPlaceholder || lines[1] != "after the key" {
			t.Fatalf("%T lines = %q", w, lines)
		}
	}
}

// A block that never reaches END cannot swallow the log: suppression stops
// at the first line that cannot be part of a PEM body.
func TestChildStderrPEMSuppressionEndsAtANonBodyLine(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr("child")
	_, _ = w.Write([]byte("-----BEGIN RSA PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nTraceback (most recent call last):\n"))
	if got := out.lines(t); len(got) != 2 || got[0] != pemPlaceholder || got[1] != "Traceback (most recent call last):" {
		t.Fatalf("lines = %q", got)
	}
}

// A registered multi-line secret is matched line by line in text that is
// redacted a line at a time: each of its lines is registered as well.
func TestMultiLineLiteralIsRedactedLineByLine(t *testing.T) {
	out := captureOutput(t)
	const first, second = "svcAcctLineOne-Qm7Zp2", "svcAcctLineTwo-Lx9Wt4"
	RegisterSecretLiterals("", false, first+"\n"+second)
	w := ChildStderr("child")
	_, _ = w.Write([]byte("dump " + first + "\n" + second + " end\n"))
	got := out.String()
	if strings.Contains(got, first) || strings.Contains(got, second) {
		t.Fatalf("a line of a multi-line literal leaked: %s", got)
	}
}
