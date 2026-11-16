//go:build windows

package sandbox

import "os"

func dirOwnedAndWritable(_ os.FileInfo, _ int) bool {
	return false
}
