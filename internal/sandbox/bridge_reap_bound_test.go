package sandbox

// Coverage for the bounded bridge teardown: terminateBridgeLocked runs under
// c.mu, so a cmd.Wait() that never returns — the bridge exec's pipes held open
// by something that survived the kill — used to block every other operation on
// the container forever. Same hazard class BashWaitDelay exists for on the
// bash path (see the constant's doc in sandbox.go).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTerminateBridge_BoundedWhenExecPipesHeld pins the reap bound: the bridge
// process dies but a grandchild it spawned inherited its stderr pipe, so
// cmd.Wait() stays blocked on the pipe copier long after the kill. Before the
// fix the post-kill `<-done` receive blocked until the grandchild exited —
// with c.mu held — so this test's watchdog fired; with the bound,
// terminateBridgeLocked abandons the wait at bridgeReapTimeout and clears the
// bridge state regardless.
func TestTerminateBridge_BoundedWhenExecPipesHeld(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group behavior asserted on linux only")
	}
	// The backgrounded sleep inherits the shell's stderr (our pipe) and
	// outlives cat, keeping the exec.Cmd stderr copier blocked. Deliberately
	// NO WaitDelay on this fixture cmd: the bound under test is
	// terminateBridgeLocked's own, not the exec's. The "ready" marker on
	// stderr is the handshake that the grandchild exists — signalling earlier
	// can kill the shell before it forks, and then nothing holds the pipe.
	cmd := exec.Command("sh", "-c", "sleep 30 & echo ready >&2; exec cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stderr := &syncBuffer{} // non-file writer forces a pipe + copier goroutine
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in bridge process: %v", err)
	}
	ready := time.Now()
	for !strings.Contains(stderr.Snapshot(), "ready") {
		if time.Since(ready) > 10*time.Second {
			t.Fatal("stand-in bridge never reported ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c := &containerImpl{
		bridgeCmd:     cmd,
		bridgeStdin:   stdin,
		bridgeStarted: true,
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.terminateBridgeLocked()
	}()
	select {
	case <-finished:
	case <-time.After(bridgeReapTimeout + 5*time.Second):
		t.Fatalf("terminateBridgeLocked did not return within bridgeReapTimeout (%s) + headroom — a grandchild holding the exec client's pipes blocks cmd.Wait() forever while c.mu is held", bridgeReapTimeout)
	}
	if c.bridgeCmd != nil || c.bridgeStdin != nil || c.bridgeStarted {
		t.Error("bridge state must be cleared even when the reap wait is abandoned")
	}
}

// TestEnsureBridgeSetsWaitDelay pins that the bridge exec client is started
// with a WaitDelay, so the reap goroutine terminateBridgeLocked may abandon
// still finishes on its own once the process exits (the runtime force-closes
// the held pipes) instead of leaking for the process lifetime. A fake podman
// stands in — ensureBridge only needs a startable binary.
func TestEnsureBridgeSetsWaitDelay(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec cat\n"), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	c := &containerImpl{cfg: ContainerConfig{PodmanBinary: bin}}
	c.containerID = "chat-sandbox-waitdelay-test"
	if err := c.ensureBridge(); err != nil {
		t.Fatalf("ensureBridge: %v", err)
	}
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.terminateBridgeLocked()
	}()
	if c.bridgeCmd.WaitDelay != BashWaitDelay {
		t.Errorf("bridge exec WaitDelay = %v, want BashWaitDelay (%v) — without it a held pipe blocks cmd.Wait() unboundedly", c.bridgeCmd.WaitDelay, BashWaitDelay)
	}
}
