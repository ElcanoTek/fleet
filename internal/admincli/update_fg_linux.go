//go:build linux

package admincli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
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
		if !jobControlled() {
			// No job-control shell can continue our process group (e.g. the shell
			// runs without job control, `set +m`, and we share its group): a
			// stopped job would never be resumed, so stop nothing and let the
			// script carry on.
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

// suspendSelf stops this process's whole job — its process group (the wrapper
// and, in a pipeline such as `fleet update | tee log` or `producer | fleet
// update`, the rest of the pipeline; the shell only reports a job stopped once
// every member is) — and
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

// jobControlled reports whether a job-control shell can continue this
// process's group: POSIX's "not orphaned" — some member has a parent in a
// different process group of the same session (the shell that started the
// job). It is false when we share the shell's own group (`set +m`), where
// stopping the group would stop the shell with nothing left to resume it.
func jobControlled() bool {
	pgrp := syscall.Getpgrp()
	sid, err := unix.Getsid(0)
	if err != nil {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		member, ok := readProcStat(e.Name())
		if !ok || member.pgrp != pgrp {
			continue
		}
		parent, ok := readProcStat(strconv.Itoa(member.ppid))
		if ok && parent.pgrp != pgrp && parent.session == sid {
			return true
		}
	}
	return false
}

type procStat struct{ ppid, pgrp, session int }

func readProcStat(pid string) (procStat, bool) {
	if pid == "" || pid[0] < '0' || pid[0] > '9' {
		return procStat{}, false
	}
	data, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return procStat{}, false
	}
	return parseProcStat(string(data))
}

// parseProcStat reads ppid, pgrp and session from a /proc/<pid>/stat line:
// "pid (comm) state ppid pgrp session ...". comm may hold spaces and
// parentheses, so the fields are counted from its last ')'.
func parseProcStat(line string) (procStat, bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := strings.Fields(line[i+1:])
	if len(f) < 4 {
		return procStat{}, false
	}
	ppid, e1 := strconv.Atoi(f[1])
	pgrp, e2 := strconv.Atoi(f[2])
	session, e3 := strconv.Atoi(f[3])
	if e1 != nil || e2 != nil || e3 != nil {
		return procStat{}, false
	}
	return procStat{ppid: ppid, pgrp: pgrp, session: session}, true
}
