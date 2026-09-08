// Package procgroup runs a command as the leader of its own process group and
// SIGKILLs the whole group when its context ends — including after the direct
// child has already exited.
//
// That last clause is the reason this package exists. The obvious shape,
// exec.CommandContext plus a cmd.Cancel that kills -pgid, only fires while the
// direct child is alive: once Process.Wait returns, os/exec stops watching the
// context, so a gate or shell that exits at once but leaves a backgrounded
// grandchild (`sleep 60 & exit 0`) never has that grandchild killed. The
// orphan runs on, and if it inherited the stdout/stderr pipes it holds cmd.Wait
// open until WaitDelay force-closes them. Two callers shared that bug: the
// scheduler's run_if gate (whose comment promised "SIGKILL the WHOLE group on
// cancel/timeout") and the host sandbox's bash path (#796).
//
// Run watches the context itself, so the group kill fires at the deadline
// whether or not the leader is still running. RunAndKillSurvivors additionally
// kills whatever is left in the group the moment the leader has been waited
// for — for commands that are checks, not launchers. cmd.WaitDelay remains the
// backstop for a grandchild that escaped the group (setsid).
package procgroup

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Run starts cmd as its own process-group leader, waits for it, and SIGKILLs
// the entire group the moment ctx is done — even if the leader has already
// exited. A group member that outlives the leader without holding its pipes
// (`server >/dev/null 2>&1 &`) is left running until ctx ends; use
// RunAndKillSurvivors when nothing may outlive the command.
//
// Build cmd with exec.Command, not exec.CommandContext: Run owns the
// cancellation. It returns what cmd.Wait returns; callers inspect ctx.Err() to
// tell a timeout from an ordinary exit.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	return run(ctx, cmd, false)
}

// RunAndKillSurvivors is Run for a command that must leave nothing behind: once
// the leader has been waited for, whatever remains in its process group is
// SIGKILLed before RunAndKillSurvivors returns, whether or not ctx has ended.
// A recurring check that backgrounds a helper with its output redirected would
// otherwise leak one host process per run.
func RunAndKillSurvivors(ctx context.Context, cmd *exec.Cmd) error {
	return run(ctx, cmd, true)
}

func run(ctx context.Context, cmd *exec.Cmd, killSurvivors bool) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pgid := cmd.Process.Pid // the leader's pid is the group id (Setpgid)

	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			Kill(pgid)
		case <-stop:
		}
	}()

	err := cmd.Wait()
	// Reap survivors before the watcher is stopped: the caller's deferred
	// cancel runs after we return, when nobody is watching the group any more.
	if killSurvivors {
		Kill(pgid)
	}
	close(stop)
	<-watcherDone
	return err
}

// Kill SIGKILLs every process in the group pgid. Errors are ignored: the group
// is usually already empty (ESRCH), and there is nothing more to do otherwise.
func Kill(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// Running reports whether any process in the group pgid is still running. It
// reads /proc rather than probing with kill(-pgid, 0): a killed orphan stays a
// zombie — and keeps answering the probe — until its new parent reaps it, and
// under a PID 1 that never reaps adopted children that is forever. A zombie
// (state Z) or a process being torn down (state X) is not running.
func Running(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// No /proc: fall back to the probe, which over-reports zombies.
		return syscall.Kill(-pgid, 0) == nil
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited between ReadDir and ReadFile
		}
		state, grp, ok := parseStat(string(stat))
		if ok && grp == pgid && state != "Z" && state != "X" {
			return true
		}
	}
	return false
}

// parseStat pulls the state and pgrp fields out of a /proc/<pid>/stat line:
// "pid (comm) state ppid pgrp ...". comm may contain spaces and parentheses,
// so the fields are taken after the LAST ')'.
func parseStat(stat string) (state string, pgrp int, ok bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return "", 0, false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 3 {
		return "", 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[0], pgrp, true
}
