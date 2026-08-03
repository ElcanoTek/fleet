package sandbox

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

// TestContainerRunPython_CancelMidReadNoRace is the #583 regression guard: a
// context cancelled while the bridge-reader goroutine is blocked in ReadBytes
// makes runPython call terminateBridgeLocked, which nils c.bridgeStdout. Before
// the fix the goroutine re-read that FIELD unsynchronized — a data race, and a
// nil-deref panic in the ctx-already-cancelled ordering. The fix snapshots the
// reader under c.mu before launching the goroutine, so this test must pass
// under -race with no report and no panic.
//
// No podman needed: the bridge state is faked with a live `cat` process (so
// terminateBridgeLocked has a real process to signal) and a pipe-backed stdout
// that never delivers a response, keeping the reader goroutine blocked when the
// cancel arm fires.
func TestContainerRunPython_CancelMidReadNoRace(t *testing.T) {
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

	// Short deadline (not pre-cancelled): the reader goroutine gets scheduled
	// and blocks in ReadBytes first, then the ctx.Done arm tears the bridge
	// down while the goroutine is mid-read — the racy interleaving.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = c.runPython(ctx, PythonRequest{Code: "x=1"})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("runPython = %v, want a cancellation error", err)
	}
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("cancellation error = %v, want it to wrap ErrPoisoned — a cancelled cell retires the sandbox (#796)", err)
	}
	if !c.poisoned() {
		t.Error("a cancelled cell must poison the sandbox so nothing reuses the container (#796)")
	}
	if c.bridgeStarted {
		t.Error("bridge state must be cleared so ensureBridge starts fresh")
	}

	// Unblock and reap the orphaned reader goroutine.
	_ = pw.Close()
}
