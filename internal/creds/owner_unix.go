//go:build unix

package creds

import (
	"os"
	"syscall"
)

// PreserveOwner makes path belong to the same uid/gid as the file `like`
// describes, when — and only when — this process is root and they differ.
// Writers that replace a file via temp+rename (writeEnvLines here, an editor's
// save in `fleet env edit`) otherwise hand the file to whoever ran the command:
// `sudo fleet config set-openrouter-key` on an env file the service user owned
// silently re-owned it to root, and a file the unit reads as root but another
// process reads as the service user became unreadable to the latter. A non-root
// process cannot chown and never needs to (it can only have written a file it
// already owned), so this is a no-op for it. Ownership information the OS does
// not expose (a FileInfo without Stat_t) is also a no-op, never an error.
func PreserveOwner(path string, like os.FileInfo) error {
	if like == nil || os.Geteuid() != 0 {
		return nil
	}
	st, ok := like.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	uid, gid := int(st.Uid), int(st.Gid)
	cur, err := os.Stat(path)
	if err != nil {
		return err
	}
	if cst, ok := cur.Sys().(*syscall.Stat_t); ok && int(cst.Uid) == uid && int(cst.Gid) == gid {
		return nil
	}
	return os.Chown(path, uid, gid)
}

// FileOwner returns the uid/gid a FileInfo carries, when the platform exposes
// them. Used by callers that report "owned by X" in operator-facing messages.
func FileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	if fi == nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
