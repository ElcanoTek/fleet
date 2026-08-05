package sandbox

// Unit coverage for the capped bridge-response read: the bridge's response
// line was read into host memory with no ceiling (unlike bash output, which
// cappedBuffer bounds), so a cell returning a huge variable via return_vars —
// the one response field python_bridge.py does not size-bound itself —
// inflated host RSS without limit. Uses the same faked-bridge trick as
// bridge_error_reset_test.go: no podman needed.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadCappedLine(t *testing.T) {
	t.Run("under the cap is returned whole", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("hello\nrest"))
		data, discarded, err := readCappedLine(r, 64)
		if err != nil {
			t.Fatalf("readCappedLine: %v", err)
		}
		if string(data) != "hello\n" {
			t.Errorf("data = %q, want %q (delimiter included, like ReadBytes)", data, "hello\n")
		}
		if discarded != 0 {
			t.Errorf("discarded = %d, want 0", discarded)
		}
		// The next line must still be readable — the reader stops at the delimiter.
		if next, _ := io.ReadAll(r); string(next) != "rest" {
			t.Errorf("stream after the line = %q, want %q", next, "rest")
		}
	})

	t.Run("over the cap stores the prefix and drains to the newline", func(t *testing.T) {
		// A line far larger than the bufio buffer, so ReadSlice's
		// ErrBufferFull continuation is exercised, followed by a second line
		// that must survive the drain intact.
		big := strings.Repeat("x", 64<<10)
		r := bufio.NewReaderSize(strings.NewReader(big+"\nnext\n"), 4096)
		data, discarded, err := readCappedLine(r, 8)
		if err != nil {
			t.Fatalf("readCappedLine: %v", err)
		}
		if string(data) != "xxxxxxxx" {
			t.Errorf("data = %q, want the first 8 bytes", data)
		}
		// Everything past the cap through the newline is drained and counted.
		if want := int64(len(big) + 1 - 8); discarded != want {
			t.Errorf("discarded = %d, want %d", discarded, want)
		}
		// The stream stays framed: the following response is untouched.
		next, _, err := readCappedLine(r, 64)
		if err != nil || string(next) != "next\n" {
			t.Errorf("next line = %q, %v — the drain broke the framing", next, err)
		}
	})

	t.Run("EOF without a newline mirrors ReadBytes", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("partial"))
		data, discarded, err := readCappedLine(r, 64)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if string(data) != "partial" || discarded != 0 {
			t.Errorf("data = %q discarded = %d, want the partial bytes and 0", data, discarded)
		}
	})
}

// TestContainerRunPython_OversizedResponseFailsBounded is the regression guard
// for the unbounded read: a response line past bridgeResponseCaptureCap must
// fail THIS call with a truncation error — never buffer the whole payload,
// never poison the sandbox, and (since the reader drained to the delimiter)
// never reset an otherwise healthy bridge session. Before the fix this
// returned a bare JSON parse error after buffering every byte.
func TestContainerRunPython_OversizedResponseFailsBounded(t *testing.T) {
	c, cat, pw := silentBridgeContainer(t)
	defer reapBridgeProcess(cat)
	defer func() { _ = pw.Close() }()

	// Feed a response line just past the cap, in chunks (io.Pipe writes block
	// until the reader consumes them).
	go func() {
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		remaining := bridgeResponseCaptureCap + 2
		for remaining > 0 {
			n := len(chunk)
			if n > remaining {
				n = remaining
			}
			if _, err := pw.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
		_, _ = pw.Write([]byte("\n"))
	}()

	_, err := c.runPython(context.Background(), PythonRequest{Code: "x=1"})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("runPython = %v, want a response-exceeded error", err)
	}
	if c.poisoned() {
		t.Error("an oversized response must NOT poison the sandbox — the container is intact")
	}
	if !c.bridgeStarted {
		t.Error("an oversized response must NOT reset the bridge — the reader drained to the delimiter, so the session is still framed")
	}
}
