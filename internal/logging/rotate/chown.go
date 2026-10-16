//go:build !linux

// Copied from gopkg.in/natefinch/lumberjack.v2 v2.2.1, chown.go. Copyright (c)
// 2014 Nate Finch; see LICENSE. SPDX-License-Identifier: MIT

package rotate

import (
	"os"
)

func chown(_ string, _ os.FileInfo) error {
	return nil
}
