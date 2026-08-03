package sandbox

// Unit coverage for runPython's failure arms, using the same faked bridge
// trick as bridge_cancel_race_test.go / bridge_teardown_race_test.go (no
// podman needed):
//
//   - cancel/timeout must POISON the sandbox and clear the bridge state —
//     the #796 straggler guard bash and file ops already had. Without it, a
//     timed-out cell keeps executing inside a container that later calls in
//     the same turn (and, in persistent mode, later TURNS) would reuse.
//   - a bridge write/read error must clear the bridge state WITHOUT
//     poisoning: the container is intact, but ensureBridge's health check
//     (ProcessState == nil until someone Wait()s) would otherwise judge the
//     dead session healthy and wedge every later run_python in the turn.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// silentBridgeContainer builds a containerImpl whose bridge process is a live
// `cat` (so terminateBridgeLocked has a real child to signal and reap) but
// whose response stream is an io.Pipe that never delivers unless the test
// writes to it — keeping the reader goroutine blocked so the timeout arm can
// fire deterministically. Callers must close the returned writer to unblock
// and reap the orphaned reader goroutine.
func silentBridgeContainer(t *testing.T) (*containerImpl, *exec.Cmd, *io.PipeWriter) {
	t.Helper()
	cat := exec.Command("cat")
	stdin, err := cat.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cat.Start(); err != nil {
		t.Fatalf("start stand-in bridge process: %v", err)
	}
	pr, pw := io.Pipe()
	c := &containerImpl{
		bridgeCmd:     cat,
		bridgeStdin:   stdin,
		bridgeStdout:  bufio.NewReader(pr),
		bridgeStarted: true,
	}
	return c, cat, pw
}

func reapBridgeProcess(cat *exec.Cmd) {
	if cat.Process != nil {
		_ = cat.Process.Kill()
		_, _ = cat.Process.Wait()
	}
}

func TestContainerRunPython_TimeoutPoisonsAndRetires(t *testing.T) {
	c, cat, pw := silentBridgeContainer(t)
	defer reapBridgeProcess(cat)
	defer func() { _ = pw.Close() }()

	_, err := c.runPython(context.Background(), PythonRequest{Code: "x=1", Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runPython = %v, want a timeout error", err)
	}
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("timeout error = %v, want it to wrap ErrPoisoned so the tool layer reports retirement", err)
	}
	if !c.poisoned() {
		t.Error("a timed-out cell must poison the sandbox so nothing reuses the container (#796)")
	}
	if c.bridgeStarted {
		t.Error("bridge state must be cleared on timeout")
	}
}

func TestContainerRunPython_WriteErrorResetsBridge(t *testing.T) {
	c, cat, pw := silentBridgeContainer(t)
	defer reapBridgeProcess(cat)
	defer func() { _ = pw.Close() }()

	// The exec client died: its stdin is closed, but nobody Wait()ed it, so
	// ensureBridge's ProcessState check still says healthy.
	if err := c.bridgeStdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	_, err := c.runPython(context.Background(), PythonRequest{Code: "x=1"})
	if err == nil || !strings.Contains(err.Error(), "send bridge request") {
		t.Fatalf("runPython = %v, want a send error", err)
	}
	if c.poisoned() {
		t.Error("a bridge write error must NOT poison the sandbox — the container is intact")
	}
	if c.bridgeStarted {
		t.Error("bridge state must be cleared so the next call boots a fresh session instead of wedging")
	}
}

func TestContainerRunPython_ReadErrorResetsBridge(t *testing.T) {
	c, cat, pw := silentBridgeContainer(t)
	defer reapBridgeProcess(cat)

	// The exec client died mid-turn: the response stream EOFs.
	_ = pw.Close()

	_, err := c.runPython(context.Background(), PythonRequest{Code: "x=1"})
	if err == nil || !strings.Contains(err.Error(), "bridge closed unexpectedly") {
		t.Fatalf("runPython = %v, want a bridge-closed error", err)
	}
	if c.poisoned() {
		t.Error("a bridge read error must NOT poison the sandbox — the container is intact")
	}
	if c.bridgeStarted {
		t.Error("bridge state must be cleared so the next call boots a fresh session instead of wedging")
	}
}
