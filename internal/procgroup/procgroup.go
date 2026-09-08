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
// whether or not the leader is still running. cmd.WaitDelay remains the
// backstop for a grandchild that escaped the group (setsid).
package procgroup

import (
	"context"
	"os/exec"
	"syscall"
)

// Run starts cmd as its own process-group leader, waits for it, and SIGKILLs
// the entire group the moment ctx is done — even if the leader has already
// exited. Build cmd with exec.Command, not exec.CommandContext: Run owns the
// cancellation. It returns what cmd.Wait returns; callers inspect ctx.Err() to
// tell a timeout from an ordinary exit.
func Run(ctx context.Context, cmd *exec.Cmd) error {
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
	close(stop)
	<-watcherDone
	return err
}

// Kill SIGKILLs every process in the group pgid. Errors are ignored: the group
// is usually already empty (ESRCH), and there is nothing more to do otherwise.
func Kill(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// Alive reports whether any process in the group pgid still exists.
func Alive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}
