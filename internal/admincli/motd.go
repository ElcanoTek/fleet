package admincli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/version"
)

// cmdMOTD prints the fleet message-of-the-day banner (#461): the build version,
// the systemd service state, and the handful of commands an operator reaches for
// most. It is deliberately FAST and SECRET-FREE — version is in-binary, the
// service state is a 2s `systemctl is-active`, and it touches NO database,
// config, credential, or connector. That makes it safe to run on every login
// (via /etc/profile.d/fleet-motd.sh) without slowing the shell or leaking
// anything. Colors are emitted only to a TTY (or forced off with --no-color), so
// piping it into a file stays clean.
func cmdMOTD(argv []string) int {
	fs := flag.NewFlagSet("motd", flag.ContinueOnError)
	service := fs.String("service", "", "systemd unit name (default: $FLEET_SERVICE_NAME or \"fleet\")")
	noColor := fs.Bool("no-color", false, "disable ANSI color (e.g. when redirecting to a file)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	color := !*noColor && isTTY(os.Stdout) && os.Getenv("NO_COLOR") == ""
	unit := serviceName(*service) + ".service"
	fmt.Fprint(os.Stdout, renderMOTD(version.String(), unit, motdServiceState(unit), color))
	return 0
}

// renderMOTD builds the banner string. Pure (no I/O) so it is table-testable: it
// takes the build version, the unit name, the `systemctl is-active` word, and
// whether to emit ANSI color. It can only ever print version/unit/state +
// static command hints — there is no path for a secret to reach it.
func renderMOTD(ver, unit, svcState string, color bool) string {
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	const (
		cyan = "36"
		dim  = "2"
		bold = "1"
		grn  = "32"
		ylw  = "33"
	)
	// A compact masted-ship mark — fleet is a fleet of ships. Kept small so it
	// fits a login banner without dominating the terminal.
	banner := strings.Join([]string{
		`    .  .  .   `,
		`  __|__|__|__   ` + paint(bold, "fleet") + "  " + paint(dim, ver),
		`  \  fleet  /   ` + paint(dim, "self-hosted agent platform"),
		`   \______/    `,
	}, "\n")

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(paint(cyan, banner))
	b.WriteString("\n\n")

	// Service state — green active / yellow otherwise / dim when there's no
	// systemd at all (a dev box). Never an error; the MOTD is informational.
	switch svcState {
	case "active":
		b.WriteString("  service   " + paint(grn, "● active") + paint(dim, "  ("+unit+")") + "\n")
	case "n/a":
		b.WriteString("  service   " + paint(dim, "— no systemd (dev box; run `fleet serve`)") + "\n")
	default:
		b.WriteString("  service   " + paint(ylw, "○ "+svcState) + paint(dim, "  ("+unit+" — `fleet status` / `fleet logs`)") + "\n")
	}

	b.WriteString("  commands  " + paint(dim, "fleet status · fleet update · fleet logs · fleet chat · fleet --help") + "\n")
	b.WriteString("\n")
	return b.String()
}

// isTTY reports whether f is an interactive terminal (a character device), so
// the MOTD only emits ANSI color when a human is watching — stdlib-only, no
// x/term dependency.
func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// motdServiceState returns the unit's `systemctl is-active` word (active,
// inactive, failed, activating, …), "n/a" when there's no systemd, or "unknown"
// on any probe error. Bounded to 2s so a login banner never hangs.
func motdServiceState(unit string) string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "n/a"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "systemctl" binary; unit is operator-config (FLEET_SERVICE_NAME / --service), not request input.
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	state := strings.TrimSpace(string(out))
	if state == "" {
		return "unknown"
	}
	return state
}
