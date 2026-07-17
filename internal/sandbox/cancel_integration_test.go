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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newCancelTestContainer(t *testing.T) (*Sandbox, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workspace := t.TempDir()
	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: workspace,
		BridgeScript:     []byte("# unused for bash-only test\n"),
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	// The workspace is mounted at the same absolute path on both sides.
	// Making it the default cwd lets marker writes use relative names and
	// makes the host-side assertion below observe the exact same files.
	sb.SetDefaultWorkingDir(workspace)
	t.Cleanup(sb.Close)

	// Rootless podman maps the container's uid 1000 to a subordinate host
	// uid, so a root-owned t.TempDir() is NOT writable from inside the
	// container. If left that way, every marker write below fails silently
	// and the "marker never appeared" assertions pass vacuously — green even
	// if the kill did nothing. Widen the mode so the container user can
	// write, then prove it actually can with a canary: a normal marker write
	// that MUST reach the host. Failing loudly here is the guard against the
	// marker assertions ever going vacuous again.
	if err := os.Chmod(workspace, 0o777); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	canary, err := sb.RunBash(context.Background(), BashRequest{Command: "printf ok > .canary", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("canary RunBash: %v", err)
	}
	if canary.ExitCode != 0 {
		t.Fatalf("canary write failed (exit %d, stderr %q) — workspace not writable by the container user; marker assertions would be vacuous", canary.ExitCode, string(canary.Stderr))
	}
	if _, err := os.Stat(filepath.Join(workspace, ".canary")); err != nil {
		t.Fatalf("canary marker not visible on host (%v) — marker assertions would be vacuous", err)
	}
	return sb, workspace
}

// markerAbsentInFreshContainer waits past the delayed write, then — because the
// cancelled sandbox is poisoned and refuses RunBash — verifies the workspace
// marker never appeared by reading the host-side workspace dir the container
// bind-mounts. hostWorkspace is that directory.
func markerAbsent(t *testing.T, hostWorkspace, name string, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	_, err := os.Stat(filepath.Join(hostWorkspace, name))
	if err == nil {
		t.Fatalf("marker %q appeared — the cancelled command completed its side effect", name)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect marker %q: %v", name, err)
	}
}

func TestContainerBashTimeout_KillsDelayedMarkerWrite(t *testing.T) {
	sb, workspace := newCancelTestContainer(t)

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "sleep 3; printf survived > timeout-marker",
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
	if !res.SandboxRetired {
		t.Fatalf("timeout result must report sandbox retirement: %+v", res)
	}
	if !sb.Poisoned() {
		t.Fatal("a timed-out bash must poison the sandbox so it is retired")
	}
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("poisoned sandbox must refuse further work, got %v", err)
	}
	markerAbsent(t, workspace, "timeout-marker", 3*time.Second)
}

func TestContainerBashCancel_KillsSetsidDaemonAndBackgroundChild(t *testing.T) {
	sb, workspace := newCancelTestContainer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan BashResult, 1)
	go func() {
		res, err := sb.RunBash(ctx, BashRequest{
			// A TERM-ignoring foreground shell, a backgrounded subshell, AND a
			// setsid daemon that leaves the process group entirely. Only
			// killing the container reaches all three.
			Command: `trap '' TERM
setsid bash -c 'sleep 3; printf daemon > daemon-marker' &
(sleep 3; printf bg > bg-marker) &
sleep 3; printf fg > fg-marker`,
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
	if !res.SandboxRetired {
		t.Fatalf("cancel result must report sandbox retirement: %+v", res)
	}
	if !sb.Poisoned() {
		t.Fatal("a cancelled bash must poison the sandbox")
	}
	// The container is dead; assert none of the three markers reached the
	// bind-mounted workspace on the host.
	markerAbsent(t, workspace, "fg-marker", 3*time.Second)
	markerAbsent(t, workspace, "bg-marker", 0)
	markerAbsent(t, workspace, "daemon-marker", 0)
}

func TestContainerBash_NormalRunUnaffected(t *testing.T) {
	sb, _ := newCancelTestContainer(t)

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
	sb, _ := newCancelTestContainer(t)
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
