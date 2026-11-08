package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// A child process's stderr (the fleet mcp-broker's log lines) reaches the
// parent's log destination, FLEET_LOG_FILE included, one whole line at a time
// however the pipe splits the bytes.
func TestChildStderrForwardsWholeLinesToTheLogFile(t *testing.T) {
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
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "info"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	defer closer.Close()

	w := ChildStderr()
	for _, chunk := range []string{`{"msg":"mcpbroker: call`, ` failed","op":"tool call"}` + "\n" + `{"msg":"sec`, `ond"}` + "\n" + `{"msg":"partial`} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"msg":"mcpbroker: call failed","op":"tool call"}` + "\n" + `{"msg":"second"}` + "\n"
	if !strings.HasSuffix(string(data), want) {
		t.Fatalf("log file = %q, want it to end with the two whole child lines %q", data, want)
	}
	if strings.Contains(string(data), "partial") {
		t.Fatalf("an unterminated child line was written: %q", data)
	}
}

// captureOutput points the process log destination at a buffer for the test.
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := currentOutput()
	var buf bytes.Buffer
	setOutput(&buf)
	t.Cleanup(func() { setOutput(prev) })
	return &buf
}

// A child line that is not a well-formed JSON log record — raw bytes a
// connector or library wrote to the broker's stderr outside slog — is
// scrubbed by the parent's redactor before it reaches the sink; a JSON
// record from the child's own (redacting) handler is forwarded verbatim.
func TestChildStderrRedactsLinesThatAreNotJSONRecords(t *testing.T) {
	out := captureOutput(t)
	const literal = "Zt7qWm3RkP9sLx2V"
	RegisterSecretLiterals("", false, literal)
	shaped := "sk-ant-" + strings.Repeat("C", 32)
	record := `{"time":"2026-11-08T00:00:00Z","level":"INFO","msg":"mcpbroker: ready","n":1}`

	w := ChildStderr()
	for _, line := range []string{
		"Authorization: Bearer " + shaped,
		"connector echoed " + literal,
		record,
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
	if !strings.Contains(got, record+"\n") {
		t.Fatalf("JSON log record not forwarded verbatim: %q", got)
	}
	if strings.Count(got, "\n") != 4 {
		t.Fatalf("want 4 forwarded lines, got %q", got)
	}
}

// pending is capped: a line with no newline past MaxStderrLine is forwarded
// as one truncated record, the rest of that line is dropped, and the next
// line is forwarded normally.
func TestChildStderrCapsAnUnterminatedLine(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr()
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
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want the truncated line and the next line, got %d lines", len(lines))
	}
	if want := strings.Repeat("a", MaxStderrLine) + truncatedMarker; lines[0] != want {
		t.Fatalf("truncated line: len %d, suffix %q", len(lines[0]), lines[0][max(0, len(lines[0])-20):])
	}
	if lines[1] != "next line" {
		t.Fatalf("line after the overflow = %q", lines[1])
	}
}

// Close forwards the last unterminated line — the child exited without a
// trailing newline — and later writes are ignored.
func TestChildStderrCloseFlushesThePartialLine(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr()
	_, _ = w.Write([]byte("first\npanic: last words"))
	if strings.Contains(out.String(), "last words") {
		t.Fatalf("partial line forwarded before Close: %q", out)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "first\npanic: last words\n" {
		t.Fatalf("after Close = %q", got)
	}
	_ = w.Close()
	_, _ = w.Write([]byte("late\n"))
	if strings.Contains(out.String(), "late") {
		t.Fatalf("write after Close forwarded: %q", out)
	}
}

// "\r\n" is one terminator and a lone "\r" (a progress-bar redraw) ends a
// line, including when the pair is split across writes.
func TestChildStderrCarriageReturns(t *testing.T) {
	out := captureOutput(t)
	w := ChildStderr()
	for _, chunk := range []string{"crlf\r\n", "split\r", "\nbar 10%\rbar 20%\r", "\n"} {
		_, _ = w.Write([]byte(chunk))
	}
	if got, want := out.String(), "crlf\nsplit\nbar 10%\nbar 20%\n"; got != want {
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
	m := decodeLast(t, &buf)
	if m["truncated"] != true || len(m["line"].(string)) != MaxStderrLine {
		t.Fatalf("overflow record: truncated=%v len=%d", m["truncated"], len(m["line"].(string)))
	}
}
