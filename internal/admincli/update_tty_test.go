package admincli

import "testing"

// The update script runs in its own process group; on a terminal that group
// must be the terminal's foreground group, or update.sh's y/N prompt is
// stopped by SIGTTIN (fleetdev, 2026-11-03). Off a terminal (ssh -T, the
// deploy bot, systemd) it stays a plain group and never touches a tty.
func TestUpdateSysProcAttr(t *testing.T) {
	attr, fd := updateSysProcAttr(7, func(int) bool { return true })
	if !attr.Setpgid || !attr.Foreground || attr.Ctty != 7 || fd != 7 {
		t.Fatalf("terminal stdin: want Setpgid+Foreground on fd 7, got %+v fd=%d", attr, fd)
	}
	attr, fd = updateSysProcAttr(7, func(int) bool { return false })
	if !attr.Setpgid || attr.Foreground || attr.Ctty != 0 || fd != -1 {
		t.Fatalf("non-terminal stdin: want a plain process group, got %+v fd=%d", attr, fd)
	}
}
