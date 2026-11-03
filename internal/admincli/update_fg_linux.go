//go:build linux

package admincli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// waitForeground waits for an update script that runs as the terminal's
// foreground group (updateSysProcAttr) and keeps shell job control working:
// Ctrl-Z stops only the script's group, so this wrapper — blocked in Wait —
// would never stop, and the invoking shell would neither see a stopped job
// nor get the terminal back. On a stop it takes the terminal back, stops
// itself (the shell then shows the job as stopped), and on `fg` hands the
// terminal to the script's group again and continues it. The exit is reaped
// by cmd.Wait, so its exit-code and WaitDelay handling are unchanged.
func waitForeground(cmd *exec.Cmd, ttyFD int) error {
	pid := cmd.Process.Pid
	for {
		var info unix.Siginfo
		// Peek without reaping: WNOWAIT leaves an exit for cmd.Wait below.
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WSTOPPED|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || info.Code != cldStopped {
			return cmd.Wait()
		}
		// Consume the stop notification (WSTOPPED only never reaps an exit).
		var consumed unix.Siginfo
		for {
			if err := unix.Waitid(unix.P_PID, pid, &consumed, unix.WSTOPPED, nil); !errors.Is(err, syscall.EINTR) {
				break
			}
		}
		// Take the terminal back only from the script's own group. After `bg`
		// the shell holds it, and a script that reads it stops with SIGTTIN:
		// leave the shell's terminal alone and just stop, so the shell
		// reports the job stopped until the operator brings it back with fg.
		if syscall.Getpgrp() != syscall.Getpid() {
			// Not our own job (e.g. the shell runs without job control, `set +m`,
			// and we share its group): no shell would ever continue a stopped
			// job, so don't stop anything — let the script carry on.
			_ = syscall.Kill(-pid, syscall.SIGCONT)
			continue
		}
		if fg, err := unix.IoctlGetInt(ttyFD, unix.TIOCGPGRP); err == nil && fg == pid {
			reclaimForeground(ttyFD)
		}
		suspendSelf()
		// Resumed by `fg` (we hold the terminal again): hand it to the script.
		// Resumed by `bg`: leave the terminal to the shell — a script that then
		// reads it stops again (SIGTTIN) and the shell reports the job stopped.
		if fg, err := unix.IoctlGetInt(ttyFD, unix.TIOCGPGRP); err == nil && fg == unix.Getpgrp() {
			giveForeground(ttyFD, pid)
		}
		_ = syscall.Kill(-pid, syscall.SIGCONT)
	}
}

// cldStopped is siginfo's si_code for a stopped child (CLD_STOPPED).
const cldStopped = 5

// suspendSelf stops this process's whole job — its process group, which the
// caller has checked this process leads (the wrapper and, in a pipeline such
// as `fleet update | tee log`, the rest of the pipeline; the shell only
// reports a job stopped once every member is) — and
// returns only once it has been continued. kill(SIGSTOP) can return before
// the stop takes effect, so it waits for the SIGCONT the shell's fg / bg
// sends; going on early would hand the terminal back while the shell still
// thinks the job is running.
func suspendSelf() {
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	_ = syscall.Kill(-syscall.Getpgrp(), syscall.SIGSTOP)
	<-cont
}

// giveForeground makes pgid the terminal's foreground group (SIGTTOU ignored:
// tcsetpgrp from a background group raises it).
func giveForeground(ttyFD, pgid int) {
	ignoreTTOU(func() { _ = unix.IoctlSetPointerInt(ttyFD, unix.TIOCSPGRP, pgid) })
}
