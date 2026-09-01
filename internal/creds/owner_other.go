//go:build !unix

package creds

import "os"

// PreserveOwner is a no-op where the OS exposes no POSIX ownership.
func PreserveOwner(string, os.FileInfo) error { return nil }

// FileOwner reports no ownership information on non-unix platforms.
func FileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
