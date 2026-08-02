package sandbox

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"testing"
)

// fakeBridgeContainer builds a containerImpl whose bridge state is faked with
// a live `cat` process (same trick as TestContainerRunPython_CancelMidReadNoRace):
// ensureBridge sees a healthy session and no podman is needed. cat echoes any
// request line back, which parseBridgeResponse accepts, so a runPython that
// wins the race completes instead of hanging.
func fakeBridgeContainer(t *testing.T) (*containerImpl, *exec.Cmd) {
	t.Helper()
	cat := exec.Command("cat")
	stdin, err := cat.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cat.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cat.Start(); err != nil {
		t.Fatalf("start stand-in bridge process: %v", err)
	}
	c := &containerImpl{
		bridgeCmd:     cat,
		bridgeStdin:   stdin,
		bridgeStdout:  bufio.NewReader(stdout),
		bridgeStarted: true,
	}
	return c, cat
}

// TestContainerRunPython_TornDownBridgeFailsClosed pins the post-lock guard
// for the ensureBridge/close race: ensureBridge validates the bridge and
// releases c.mu, a concurrent close() nils bridgeStdin in that window, and
// runPython then re-acquires c.mu to write. Before the fix that write was
// fmt.Fprintf(nil-interface) — a panic outside any safe.Go recover, taking
// down the whole process. The torn-down state (session "healthy" for
// ensureBridge, stdin already nil'd) must yield a clean ErrClosed instead.
func TestContainerRunPython_TornDownBridgeFailsClosed(t *testing.T) {
	c, cat := fakeBridgeContainer(t)
	defer func() {
		_ = cat.Process.Kill()
		_, _ = cat.Process.Wait()
	}()

	// Simulate close() winning the window: the write-side fields are gone but
	// bridgeCmd/bridgeStarted still look alive to ensureBridge's health check.
	_ = c.bridgeStdin.Close()
	c.bridgeStdin = nil
	c.bridgeStdout = nil

	_, err := c.runPython(context.Background(), PythonRequest{Code: "x=1"})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("runPython on a torn-down bridge = %v, want ErrClosed", err)
	}
}

// TestContainerClose_ReapsBridgeChild verifies close() tears the bridge down
// like terminateBridgeLocked does — signal, Wait(), clear state. Before the
// fix, close() only closed stdin and nil'd bridgeCmd without ever Wait()ing,
// so every sandbox that ran Python leaked a zombie `podman exec` child plus
// its pipe FDs and stderr-copier goroutine for the life of the process.
func TestContainerClose_ReapsBridgeChild(t *testing.T) {
	c, cat := fakeBridgeContainer(t)

	c.close()

	if cat.ProcessState == nil {
		t.Error("bridge child was not Wait()ed by close() — zombie leaked")
	}
	if c.bridgeCmd != nil || c.bridgeStdin != nil || c.bridgeStdout != nil || c.bridgeStarted {
		t.Error("bridge state must be fully cleared by close()")
	}
}

// TestContainerRunPython_ConcurrentCloseNoPanic races runPython against
// close() under -race: whichever side wins, the outcome must be a clean
// result or error — never the nil-interface Fprintf panic (#pre-fix) and
// never a race report on the bridge fields or containerID.
func TestContainerRunPython_ConcurrentCloseNoPanic(t *testing.T) {
	for i := 0; i < 25; i++ {
		c, cat := fakeBridgeContainer(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			// Either a clean error (torn down mid-flight) or a success (the
			// request beat close and cat echoed it back) is acceptable.
			_, _ = c.runPython(context.Background(), PythonRequest{Code: "x=1"})
		}()
		c.close()
		<-done
		// close() must have reaped the stand-in child either way.
		if cat.ProcessState == nil {
			_ = cat.Process.Kill()
			_, _ = cat.Process.Wait()
			t.Fatal("bridge child not reaped after concurrent close")
		}
	}
}

// TestContainerRunBash_ClosedReturnsErrClosed pins the containerID snapshot:
// runBash on a closed container must observe the cleared id under idMu and
// fail closed instead of racing close()'s write (or exec'ing into "").
func TestContainerRunBash_ClosedReturnsErrClosed(t *testing.T) {
	c := &containerImpl{}
	c.close()
	if _, err := c.runBash(context.Background(), BashRequest{Command: "echo hi"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("runBash after close = %v, want ErrClosed", err)
	}
	if _, err := c.executeFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: "/x", Root: "/x"}, "/x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("executeFileOp after close = %v, want ErrClosed", err)
	}
}
