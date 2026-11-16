//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

func dirOwnedAndWritable(info os.FileInfo, euid int) bool {
	if euid == 0 {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == euid && info.Mode().Perm()&0o200 != 0
}
