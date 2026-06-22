package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Day-2 service-lifecycle verbs: restart / stop / logs. These wrap the host
// systemd unit (resolved via serviceName → $FLEET_SERVICE_NAME or "fleet") so an
// operator never has to drop to raw systemctl/journalctl to recover a crashed box
// or tail logs. They are host-side ops — not an agent run path — so they touch
// neither agentcore.Run nor any sandbox/credential surface.
//
// restart/stop manage a SYSTEM unit installed under /etc/systemd/system, so like
// the systemd unit itself they typically require root/sudo; systemctl's own
// permission error is surfaced verbatim via the child exit code. journalctl reads
// are usually permitted without privilege.

// logsArgs builds the journalctl argv for tailing a unit's logs. Pure so it can
// be table-tested without execing journalctl.
func logsArgs(unit string, lines int, follow bool) []string {
	args := []string{"-u", unit, "-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-f")
	}
	return args
}

// systemctlUnitInstalled reports whether <unit> is installed, via a short-lived
// `systemctl cat`. journalctl -u succeeds (empty, exit 0) even for a never-installed
// unit, so this is what turns "not installed" into an explicit error instead of a
// silent empty result. Mirrors checkService's probe.
func systemctlUnitInstalled(unit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "systemctl" binary; unit is the operator-supplied unit name.
	return exec.CommandContext(ctx, "systemctl", "cat", unit).Run() == nil
}

// cmdRestart / cmdStop run `systemctl <verb> <unit>` on the resolved unit.
func cmdRestart(argv []string) int { return systemctlVerb("restart", argv) }
func cmdStop(argv []string) int    { return systemctlVerb("stop", argv) }

func systemctlVerb(verb string, argv []string) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	service := fs.String("service", "", "systemd unit name (default: $FLEET_SERVICE_NAME or \"fleet\")")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errf(5, "systemctl not on PATH (no systemd)")
	}
	unit := serviceName(*service) + ".service"
	if !systemctlUnitInstalled(unit) {
		return errf(5, "%s not installed (run scripts/bootstrap.sh --enable-service to install it)", unit)
	}

	// Signal-cancelled (not time-bounded): a restart can take a moment; Ctrl-C / SIGTERM
	// tears it down cleanly. restart/stop manage a system unit and may need root/sudo —
	// systemctl's own permission error propagates via the child exit code.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	//nolint:gosec // G204: fixed "systemctl" binary; unit is the operator-supplied unit name.
	cmd := exec.CommandContext(ctx, "systemctl", verb, unit)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return errf(5, "systemctl %s %s: %v", verb, unit, err)
	}
	return 0
}

// cmdLogs tails the unit's journal: `journalctl -u <unit> -n <N> [-f]`. -f streams
// until Ctrl-C. NOT time-bounded — a large -n dump or a follow must not be
// truncated by a context timeout (only the not-installed probe is short-lived).
func cmdLogs(argv []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	service := fs.String("service", "", "systemd unit name (default: $FLEET_SERVICE_NAME or \"fleet\")")
	lines := fs.Int("n", 50, "number of log lines to show")
	follow := fs.Bool("f", false, "follow: stream new log lines until Ctrl-C")
	fs.BoolVar(follow, "follow", false, "alias for -f")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		return errf(5, "journalctl not on PATH (no systemd journal)")
	}
	unit := serviceName(*service) + ".service"
	// Make "not installed" explicit (journalctl would otherwise just print nothing).
	// Only gate when systemctl is present; if it isn't, fall through to journalctl.
	if _, err := exec.LookPath("systemctl"); err == nil && !systemctlUnitInstalled(unit) {
		return errf(5, "%s not installed (run scripts/bootstrap.sh --enable-service to install it)", unit)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	//nolint:gosec // G204: fixed "journalctl" binary; unit is the operator-supplied unit name, flags built by logsArgs.
	cmd := exec.CommandContext(ctx, "journalctl", logsArgs(unit, *lines, *follow)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return errf(5, "journalctl -u %s: %v", unit, err)
	}
	return 0
}
