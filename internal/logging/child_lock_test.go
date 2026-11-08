package logging

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
)

// Every log writer — the default handler, the stdlib-log bridge and the
// ChildStderr forwarder — shares one lock even with stderr as the only sink,
// so concurrent parent records and 60 KiB forwarded child records (over
// PIPE_BUF, so a pipe would not keep them atomic) all reach the pipe whole
// (#2074 review).
func TestConcurrentParentAndChildRecordsStayWhole(t *testing.T) {
	restoreLogging(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = prevStderr })
	if _, err := Configure(Config{Format: "json", Level: "info"}); err != nil {
		t.Fatal(err)
	}

	var lines []string
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
	}()

	big := strings.Repeat("p", 60<<10)
	child := `{"time":"2026-11-08T00:00:00Z","level":"INFO","msg":"child","pad":"` + strings.Repeat("c", 60<<10) + `"}` + "\n"
	const per = 20
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for range per {
				slog.Info("parent", "pad", big)
			}
		})
		wg.Go(func() {
			fw := ChildStderr()
			for range per {
				// Split the record across writes the way a pipe read would.
				_, _ = fw.Write([]byte(child[:len(child)/3]))
				_, _ = fw.Write([]byte(child[len(child)/3:]))
			}
		})
		wg.Go(func() {
			for range per {
				_, _ = logBridge{}.Write([]byte("bridged " + big)) // the stdlib-log bridge path
			}
		})
	}
	wg.Wait()
	os.Stderr = prevStderr
	_ = w.Close()
	<-readDone
	_ = r.Close()

	if len(lines) != 3*3*per {
		t.Fatalf("got %d lines, want %d", len(lines), 3*3*per)
	}
	for i, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %d is not one whole JSON record (len %d): %v", i, len(l), err)
		}
	}
}

// A registered opaque secret straddling the MaxStderrLine cut is never
// matched whole, so the truncated line withholds a margin before the cut:
// no 8-byte piece of the secret survives (#2074 review).
func TestTruncatedLineLeaksNoPrefixOfAStraddlingSecret(t *testing.T) {
	out := captureOutput(t)
	const secret = "StraddleOpaqueSecretQ7w2Z9xK4mV8" // matches no shape pattern
	RegisterSecretLiterals("", false, secret)
	w := ChildStderr()
	line := strings.Repeat("x", MaxStderrLine-12) + secret + " and more after it\n"
	_, _ = io.WriteString(w, line)
	got := out.String()
	if !strings.Contains(got, "[truncated: line was") {
		t.Fatalf("no truncation marker: %q", got[max(0, len(got)-80):])
	}
	for i := 0; i+8 <= len(secret); i++ {
		if strings.Contains(got, secret[i:i+8]) {
			t.Fatalf("a piece of the straddling secret leaked: %q", secret[i:i+8])
		}
	}
	// A secret wholly inside the shown head is still redacted, not withheld
	// by luck: put one early and check the head shows the placeholder.
	out.Reset()
	_, _ = io.WriteString(w, "early "+secret+" "+strings.Repeat("y", MaxStderrLine)+"\n")
	if got := out.String(); strings.Contains(got, secret[:8]) || !strings.HasPrefix(got, "early "+redactedValue) {
		t.Fatalf("head = %q", got[:min(len(got), 60)])
	}
}
