package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// A child line shaped like a JSON log record is not trusted either: every
// key, nested ones included, gets the secret-key rule and every string the
// pattern and literal scrub, and the record is re-serialised with its
// top-level key order (#2074 review).
func TestChildStderrRedactsJSONRecords(t *testing.T) {
	out := captureOutput(t)
	const opaque = "Op4queReg1steredTok3nXyZ"
	RegisterSecretLiterals("", false, opaque)
	shaped := "sk-ant-" + strings.Repeat("E", 32)
	line := `{"time":"2026-11-08T00:00:00Z","level":"INFO","msg":"lib said ` + opaque + `",` +
		`"authorization":"Basic dXNlcjpwYXNz","req":{"headers":{"X-Api-Key":"plainkeyvalue123","accept":"json"},"body":["` + shaped + `"]},` +
		`"prompt_tokens":12,"note":"<ok> & fine"}`
	w := ChildStderr()
	if _, err := w.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(out.String(), "\n")
	for _, leak := range []string{opaque, shaped, "dXNlcjpwYXNz", "plainkeyvalue123"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %q: %s", leak, got)
		}
	}
	if !strings.HasPrefix(got, `{"time":"2026-11-08T00:00:00Z","level":"INFO","msg":"lib said `) {
		t.Fatalf("top-level order not kept: %s", got)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("forwarded record is not JSON: %v: %s", err, got)
	}
	if m["authorization"] != redactedValue || m["prompt_tokens"] != float64(12) || m["note"] != "<ok> & fine" {
		t.Fatalf("record = %v", m)
	}
	req, _ := m["req"].(map[string]any)
	hdrs, _ := req["headers"].(map[string]any)
	if hdrs["X-Api-Key"] != redactedValue || hdrs["accept"] != "json" {
		t.Fatalf("nested headers = %v", hdrs)
	}
}

// A line that only looks like a record (two objects, trailing text) is
// redacted as text, never forwarded raw.
func TestChildStderrNearJSONIsRedactedAsText(t *testing.T) {
	out := captureOutput(t)
	const opaque = "Tr41lingOpaqueTokenVal"
	RegisterSecretLiterals("", false, opaque)
	w := ChildStderr()
	_, _ = w.Write([]byte(`{"msg":"a"} {"authorization":"` + opaque + `"}` + "\n"))
	if got := out.String(); strings.Contains(got, opaque) {
		t.Fatalf("leaked: %s", got)
	}
}

// failingWriter fails every write, like a full log disk.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

type failureLog struct {
	mu    sync.Mutex
	sinks []string
}

func (f *failureLog) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sinks...)
}

func captureSinkFailures(t *testing.T) *failureLog {
	t.Helper()
	f := &failureLog{}
	prev := sinkFailedHook
	sinkFailedHook = func(sink string, _ error) {
		f.mu.Lock()
		f.sinks = append(f.sinks, sink)
		f.mu.Unlock()
	}
	t.Cleanup(func() { sinkFailedHook = prev })
	return f
}

func setOutputForTest(t *testing.T, w io.Writer) {
	t.Helper()
	prev := currentOutput()
	setOutput(w)
	t.Cleanup(func() { setOutput(prev) })
}

// A sink that fails must never stop the drain: os/exec's copier quits at the
// first Write error, the pipe fills and the child blocks. A child writing far
// more than a pipe buffer to stderr while every sink write fails still runs
// to completion, and the failure is reported once (#2074 review).
func TestChildStderrKeepsDrainingWhenTheSinkFails(t *testing.T) {
	setOutputForTest(t, failingWriter{})
	failures := captureSinkFailures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// ~4 MB of stderr in 100-byte lines: dozens of pipe buffers.
	cmd := exec.CommandContext(ctx, "sh", "-c", `i=0; while [ $i -lt 400 ]; do head -c 10000 /dev/zero | tr '\0' 'a' | fold -w 100 >&2; i=$((i+1)); done`)
	w := ChildStderr()
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		t.Fatalf("child did not run to completion (blocked on stderr?): %v", err)
	}
	_ = w.Close()
	if got := failures.list(); len(got) != 1 {
		t.Fatalf("sink failure reported %d times, want once", len(got))
	}
	if n, err := w.Write([]byte("x\n")); n != 2 || err != nil {
		t.Fatalf("Write = (%d, %v), want (2, nil) always", n, err)
	}
}

type toggleWriter struct{ fail bool }

func (w *toggleWriter) Write(p []byte) (int, error) {
	if w.fail {
		return 0, os.ErrClosed
	}
	return len(p), nil
}

// The Configure tee writes every sink independently: a failing file sink
// neither fails the write nor starves stderr, is reported once per failure
// streak, and is reported again after it recovered and failed anew.
func TestTeeWriterIsolatesAFailingSink(t *testing.T) {
	failures := captureSinkFailures(t)
	var good bytes.Buffer
	bad := &toggleWriter{fail: true}
	tee := newTee(teeSink{name: "stderr", w: bad}, teeSink{name: "file", w: &good})
	for range 3 {
		if n, err := tee.Write([]byte("line\n")); n != 5 || err != nil {
			t.Fatalf("Write = (%d, %v)", n, err)
		}
	}
	if good.String() != strings.Repeat("line\n", 3) {
		t.Fatalf("the sink after a failing one got %q", good.String())
	}
	if got := failures.list(); len(got) != 1 || got[0] != "stderr" {
		t.Fatalf("failures = %v, want [stderr]", got)
	}
	bad.fail = false
	_, _ = tee.Write([]byte("ok\n"))
	bad.fail = true
	_, _ = tee.Write([]byte("again\n"))
	if got := failures.list(); len(got) != 2 {
		t.Fatalf("a new failure streak was not reported: %v", got)
	}
}
