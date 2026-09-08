package procgroup

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// The leader exits at once and leaves a grandchild holding the stdout pipe.
// At the deadline the grandchild must be dead and Run must have returned —
// that is the case exec.CommandContext+Cancel gets wrong, because the context
// stops being watched once the leader has been reaped.
func TestRunKillsOrphanedGrandchildAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := exec.Command("sh", "-c", "sleep 60 & exit 0")
	cmd.WaitDelay = 30 * time.Second // must not be what frees the pipe
	var out []byte
	cmd.Stdout = &sink{buf: &out}

	start := time.Now()
	err := Run(ctx, cmd)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v (the leader exited 0; the orphan's death is not the leader's exit status)", err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
	requireGroupGone(t, cmd.Process.Pid, "the orphan was not killed at the deadline")
	// The pipe was freed by the kill, not by the 30s WaitDelay. A very loose
	// bound: only a regression to "wait for WaitDelay" gets anywhere near it.
	if elapsed >= cmd.WaitDelay {
		t.Fatalf("Run returned after %v — the pipe was freed by WaitDelay, not by the group kill", elapsed)
	}
}

// The ordinary case: a leader that finishes on its own returns its status and
// the watcher goroutine goes away with it.
func TestRunReturnsExitStatusWithoutDeadline(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	err := Run(context.Background(), cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("Run = %v, want exit status 3", err)
	}
	requireGroupGone(t, cmd.Process.Pid, "the group outlived the leader")
}

// A leader still running at the deadline is killed along with its group.
func TestRunKillsRunningLeaderAndGroupAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60")
	err := Run(ctx, cmd)
	if err == nil {
		t.Fatal("Run returned nil for a leader that was killed")
	}
	requireGroupGone(t, cmd.Process.Pid, "the group survived the deadline")
}

func TestRunStartFailure(t *testing.T) {
	cmd := exec.Command("/nonexistent/binary/for/procgroup")
	if err := Run(context.Background(), cmd); err == nil {
		t.Fatal("Run returned nil for a binary that cannot start")
	}
}

// requireGroupGone fails unless the process group empties out. SIGKILL is
// delivered synchronously but a killed orphan stays a zombie — and so still
// answers kill(-pgid, 0) — until PID 1 reaps it, which is asynchronous; hence
// a short poll rather than a single check.
func requireGroupGone(t *testing.T, pgid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for Alive(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still has a live member: %s", pgid, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type sink struct{ buf *[]byte }

func (s *sink) Write(p []byte) (int, error) { *s.buf = append(*s.buf, p...); return len(p), nil }
