package sandbox

// #796 integration coverage: cancelling or timing out Sandbox.RunBash must
// stop the in-container process tree — every straggler, including a daemon
// that escaped the process group via setsid — before it can complete a side
// effect, and the sandbox must be poisoned so no later turn reuses it. Killing
// the whole container (PID-namespace teardown) is what makes the setsid case
// tractable; a process-group/ppid-walk kill cannot reach it. Podman-gated like
// the other container tests (skipped off-linux / without podman); runs in the
// e2e-live CI lane where the sandbox image exists.

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

// markerAbsentInFreshContainer waits past the delayed write, then — because the
// cancelled sandbox is poisoned and refuses RunBash — verifies the workspace
// marker never appeared by reading the host-side workspace dir the container
// bind-mounts. hostWorkspace is that directory.
func markerAbsent(t *testing.T, hostWorkspace, name string, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	if _, err := exec.Command("test", "-e", hostWorkspace+"/"+name).Output(); err == nil {
		t.Fatalf("marker %q appeared — the cancelled command completed its side effect", name)
	}
}

func TestContainerBashTimeout_KillsDelayedMarkerWrite(t *testing.T) {
	sb := newCancelTestContainer(t)

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "sleep 3; printf survived > /workspace/timeout-marker",
		Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if !res.TimedOut || res.Cancelled {
		t.Fatalf("result = %+v, want TimedOut=true Cancelled=false", res)
	}
	if !res.CleanupConfirmed {
		t.Fatalf("container kill not confirmed on timeout: %+v", res)
	}
	if !sb.Poisoned() {
		t.Fatal("a timed-out bash must poison the sandbox so it is retired")
	}
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("poisoned sandbox must refuse further work, got %v", err)
	}
}

func TestContainerBashCancel_KillsSetsidDaemonAndBackgroundChild(t *testing.T) {
	sb := newCancelTestContainer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan BashResult, 1)
	go func() {
		res, err := sb.RunBash(ctx, BashRequest{
			// A TERM-ignoring foreground shell, a backgrounded subshell, AND a
			// setsid daemon that leaves the process group entirely. Only
			// killing the container reaches all three.
			Command: `trap '' TERM
setsid bash -c 'sleep 3; printf daemon > /workspace/daemon-marker' &
(sleep 3; printf bg > /workspace/bg-marker) &
sleep 3; printf fg > /workspace/fg-marker`,
			Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Errorf("RunBash: %v", err)
		}
		done <- res
	}()
	time.Sleep(1 * time.Second) // let the command spawn its children
	cancel()
	res := <-done
	if !res.Cancelled || res.TimedOut {
		t.Fatalf("result = %+v, want Cancelled=true", res)
	}
	if !res.CleanupConfirmed {
		t.Fatalf("container kill not confirmed on cancel: %+v", res)
	}
	if !sb.Poisoned() {
		t.Fatal("a cancelled bash must poison the sandbox")
	}
	// The container is dead; assert none of the three markers reached the
	// bind-mounted workspace on the host.
	ws := sb.defaultWorkingDir
	if ws == "" {
		t.Skip("workspace dir not resolvable for host-side marker check")
	}
	markerAbsent(t, ws, "fg-marker", 3*time.Second)
	markerAbsent(t, ws, "bg-marker", 0)
	markerAbsent(t, ws, "daemon-marker", 0)
}

func TestContainerBash_NormalRunUnaffected(t *testing.T) {
	sb := newCancelTestContainer(t)

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "echo -n out-here; echo -n err-here 1>&2; exit 4",
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if string(res.Stdout) != "out-here" || string(res.Stderr) != "err-here" {
		t.Errorf("stdout=%q stderr=%q — normal runs must be untouched", res.Stdout, res.Stderr)
	}
	if res.ExitCode != 4 {
		t.Errorf("exit code = %d, want 4", res.ExitCode)
	}
	if sb.Poisoned() {
		t.Fatal("a normally-completing command must not poison the sandbox")
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
	if !strings.Contains(ErrPoisoned.Error(), "retired") {
		t.Errorf("ErrPoisoned message should explain retirement: %q", ErrPoisoned.Error())
	}
}
