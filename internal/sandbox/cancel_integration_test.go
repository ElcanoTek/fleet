package sandbox

// #796 integration coverage: cancelling or timing out Sandbox.RunBash must
// kill the in-container process tree before it can complete a side effect,
// and the sandbox must remain usable when cleanup is proved. Podman-gated
// like the other container tests (skipped off-linux / without podman).

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newCancelTestContainer(t *testing.T) *Sandbox {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: t.TempDir(),
		BridgeScript:     []byte("# unused for bash-only test\n"),
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	t.Cleanup(sb.Close)
	return sb
}

// assertMarkerNeverAppears waits past the delayed write and asserts the
// marker file does not exist inside the container.
func assertMarkerNeverAppears(t *testing.T, sb *Sandbox, marker string, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "test -f " + marker + " && echo survived || echo clean",
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("marker probe: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "clean" {
		t.Fatalf("marker probe = %q — the cancelled command completed its side effect", got)
	}
}

func TestContainerBashTimeout_KillsDelayedMarkerWrite(t *testing.T) {
	sb := newCancelTestContainer(t)

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "sleep 3; printf survived > /tmp/fleet-cancel-marker",
		Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if !res.TimedOut || res.Cancelled {
		t.Fatalf("result = %+v, want TimedOut=true Cancelled=false", res)
	}
	if !res.CleanupConfirmed {
		t.Fatalf("cleanup not confirmed — killer failed: %+v", res)
	}
	if sb.Poisoned() {
		t.Fatal("proved cleanup must not poison the sandbox")
	}
	assertMarkerNeverAppears(t, sb, "/tmp/fleet-cancel-marker", 3*time.Second)
}

func TestContainerBashCancel_KillsSigtermIgnorerAndBackgroundChild(t *testing.T) {
	sb := newCancelTestContainer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan BashResult, 1)
	go func() {
		res, err := sb.RunBash(ctx, BashRequest{
			// A TERM-ignoring foreground shell AND a backgrounded subshell:
			// both must die with the invocation.
			Command: `trap '' TERM; (sleep 3; printf bg > /tmp/fleet-bg-marker) & sleep 3; printf fg > /tmp/fleet-fg-marker`,
			Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Errorf("RunBash: %v", err)
		}
		done <- res
	}()
	time.Sleep(1 * time.Second) // let the command start and write its pidfile
	cancel()
	res := <-done
	if !res.Cancelled || res.TimedOut {
		t.Fatalf("result = %+v, want Cancelled=true TimedOut=false", res)
	}
	if !res.CleanupConfirmed {
		t.Fatalf("cleanup not confirmed: %+v", res)
	}
	assertMarkerNeverAppears(t, sb, "/tmp/fleet-fg-marker", 3*time.Second)
	assertMarkerNeverAppears(t, sb, "/tmp/fleet-bg-marker", 0)
}

func TestContainerBash_NormalRunsUnaffectedByWrapper(t *testing.T) {
	sb := newCancelTestContainer(t)

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "echo -n out-here; echo -n err-here 1>&2; exit 4",
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if string(res.Stdout) != "out-here" || string(res.Stderr) != "err-here" {
		t.Errorf("stdout=%q stderr=%q — wrapper must not disturb output", res.Stdout, res.Stderr)
	}
	if res.ExitCode != 4 {
		t.Errorf("exit code = %d, want 4 preserved through the wrapper", res.ExitCode)
	}
	// No stale pidfiles accumulate from completed invocations.
	probe, err := sb.RunBash(context.Background(), BashRequest{
		Command: "ls /tmp/.fleet-exec-*.pid 2>/dev/null | wc -l",
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("pidfile probe: %v", err)
	}
	// The probe's own pidfile exists while it runs, so expect exactly 1.
	if got := strings.TrimSpace(string(probe.Stdout)); got != "1" {
		t.Errorf("stale pidfiles = %s, want 1 (only the live probe's own)", got)
	}
}

func TestContainerBashPoisoned_FailsClosed(t *testing.T) {
	sb := newCancelTestContainer(t)
	ci, ok := sb.impl.(*containerImpl)
	if !ok {
		t.Fatal("expected container impl")
	}
	ci.execPoisoned.Store(true)
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("RunBash on poisoned container = %v, want ErrPoisoned", err)
	}
}
