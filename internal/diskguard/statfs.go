package diskguard

import "syscall"

// Usage reports the capacity and the unprivileged-writable free space of the
// filesystem holding path. Exported so the admin storage panel measures the
// same numbers, the same way, as the guard that acts on them — before this,
// three copies of the same statfs lived in three packages and could drift.
//
// Bavail, not Bfree: the difference is the root reserve, which fleet — running
// as an unprivileged service user under the shipped systemd unit — cannot write
// into. Measuring Bfree would report space the process can never use and let
// the disk "run out" while the guard still saw headroom.
func Usage(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) // #nosec G115 -- kernel block sizes are non-negative and bounded.
	return st.Blocks * bs, st.Bavail * bs, nil
}

// statfsBytes is the Guard's measurement seam, wired to Usage in production and
// replaced in tests.
var statfsBytes = Usage
