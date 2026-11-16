//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

func dirOwnedAndWritable(info os.FileInfo, euid int) bool {
	// Rootless cgroupfs delegation applies only to unprivileged service users
	// (such as User=fleet in fleet.service), where systemd's Delegate=yes sets
	// the unit cgroup directory's UID to euid (> 0). Non-delegated system units
	// are owned by uid 0, so rejecting euid <= 0 both proves actual systemd
	// delegation and avoids mutating unrelated root-owned system services when
	// invoked as root (rootful Podman uses systemd D-Bus directly).
	if euid <= 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == euid && info.Mode().Perm()&0o200 != 0
}
