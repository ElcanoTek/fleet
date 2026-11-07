// Package systemd is the handful of read-only `systemctl` probes the operator
// CLI (internal/admincli) and the box doctor (internal/boxdoctor) share: is a
// unit installed, what is its ActiveState word, what is one property's value.
//
// Each probe runs under a built-in ProbeTimeout on top of the caller's ctx, so
// a wedged systemd cannot hang a CLI verb or a login banner; a caller that
// wants a tighter bound (or one budget across several probes) passes a ctx
// carrying it. The probes do not check that systemctl is on PATH — callers
// decide what "no systemd" means for them (a skip, "n/a", "") and test
// exec.LookPath first, as before. Every unit name reaching here is operator
// configuration or a compile-time constant, never request input.
package systemd

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ProbeTimeout bounds every probe in this package.
const ProbeTimeout = 5 * time.Second

func command(ctx context.Context, args ...string) *exec.Cmd {
	//nolint:gosec // G204: fixed "systemctl" binary; args are fixed verbs plus an operator-configured or constant unit name.
	return exec.CommandContext(ctx, "systemctl", args...)
}

// Installed reports whether unit is installed, via `systemctl cat <unit>`.
// (`journalctl -u` and `systemctl show` both answer for a unit that was never
// installed, so `cat` is the probe that tells the two apart.)
func Installed(ctx context.Context, unit string) bool {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	return command(ctx, "cat", unit).Run() == nil
}

// ActiveState returns `systemctl is-active <unit>`'s word (active, inactive,
// failed, activating, …), trimmed. is-active exits non-zero for every state
// but active while still printing the state, so the exit status is ignored:
// "" means the probe printed nothing (systemctl failed or timed out).
func ActiveState(ctx context.Context, unit string) string {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	out, _ := command(ctx, "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}

// ShowProp returns one property of unit via `systemctl show -p <prop> --value
// <unit>`, trimmed, and the command's error. The value is whatever stdout
// held even when err is non-nil, so a caller that ignored the error before
// can keep doing so.
func ShowProp(ctx context.Context, unit, prop string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	out, err := command(ctx, "show", "-p", prop, "--value", unit).Output()
	return strings.TrimSpace(string(out)), err
}
