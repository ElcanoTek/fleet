// Copied from gopkg.in/natefinch/lumberjack.v2 v2.2.1, chown_linux.go
// (modified: the Close error is checked). Copyright (c) 2014 Nate Finch; see
// LICENSE. SPDX-License-Identifier: MIT

package rotate

import (
	"os"
	"syscall"
)

// osChown is a var so we can mock it out during tests.
var osChown = os.Chown

func chown(name string, info os.FileInfo) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode()) //nolint:gosec // G304: the rotated log's own name
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return osChown(name, int(stat.Uid), int(stat.Gid))
}
