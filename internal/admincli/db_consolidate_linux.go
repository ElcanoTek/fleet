// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

//go:build linux

package admincli

import "syscall"

// freeBytesAt is the space available to an unprivileged writer on the
// filesystem holding path (fleet db consolidate's disk preflight).
func freeBytesAt(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // G115: Bsize is a positive block size
}
