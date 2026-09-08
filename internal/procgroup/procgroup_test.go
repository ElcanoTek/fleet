package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// A grandchild that redirected its output holds no pipe, so Wait returns the
// moment the leader exits — before any deadline. RunAndKillSurvivors must not
// leave it running; plain Run leaves it to the context, by design.
func TestRunAndKillSurvivorsReapsDetachedGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "sleep 60 >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"

	cmd := exec.Command("sh", "-c", script)
	if err := RunAndKillSurvivors(context.Background(), cmd); err != nil {
		t.Fatalf("RunAndKillSurvivors: %v", err)
	}
	orphan := readPID(t, pidFile)
	requireGroupGone(t, cmd.Process.Pid, "the detached grandchild survived RunAndKillSurvivors")
	if processRunning(orphan) {
		t.Fatalf("orphan %d is still running after RunAndKillSurvivors returned", orphan)
	}

	// Contrast: Run leaves the detached grandchild alone until ctx ends.
	ctx, cancel := context.WithCancel(context.Background())
	cmd = exec.Command("sh", "-c", script)
	if err := Run(ctx, cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	orphan = readPID(t, pidFile)
	if !processRunning(orphan) {
		t.Fatalf("Run killed detached grandchild %d before ctx ended; that is RunAndKillSurvivors' contract, not Run's", orphan)
	}
	cancel()
	Kill(cmd.Process.Pid) // Run has returned, so nobody else will
	requireGroupGone(t, cmd.Process.Pid, "cleanup kill of the contrast group failed")
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

// Running must not count a zombie: a killed orphan that PID 1 has not reaped
// still has a process-table entry (and still answers kill(pid, 0)) but is not
// running. Our own child is the one zombie we can make on demand — killed but
// not yet waited for.
func TestRunningIgnoresZombies(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if !Running(pgid) {
		t.Fatalf("group %d not reported running while its leader sleeps", pgid)
	}
	Kill(pgid)
	// Until Wait, the child is a zombie: present in /proc, state Z.
	deadline := time.Now().Add(5 * time.Second)
	for Running(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("group %d still reported running 5s after SIGKILL (zombie counted as running?)", pgid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Log("note: the zombie was reaped before the check; the probe would have agreed here")
	}
	_ = cmd.Wait()
}

func TestParseStat(t *testing.T) {
	// comm with spaces and a parenthesis, as /proc really produces.
	state, pgrp, ok := parseStat("1234 (my (odd) name) S 1 4321 4321 0 -1 4194560")
	if !ok || state != "S" || pgrp != 4321 {
		t.Fatalf("parseStat = %q %d %v", state, pgrp, ok)
	}
	if _, _, ok := parseStat("garbage"); ok {
		t.Fatal("parseStat accepted a line with no comm")
	}
}

// requireGroupGone fails unless every member of the process group stops
// running. SIGKILL is delivered synchronously but takes effect on the next
// schedule, hence a short poll rather than a single check.
func requireGroupGone(t *testing.T, pgid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for Running(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still has a running member: %s", pgid, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid file %q: %v", raw, err)
	}
	return pid
}

// processRunning is Running for a single pid, via the same /proc reading.
func processRunning(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	state, _, ok := parseStat(string(stat))
	return ok && state != "Z" && state != "X"
}

type sink struct{ buf *[]byte }

func (s *sink) Write(p []byte) (int, error) { *s.buf = append(*s.buf, p...); return len(p), nil }
