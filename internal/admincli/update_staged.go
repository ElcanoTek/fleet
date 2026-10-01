package admincli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// stagedMarkers are the staging bookkeeping files update.sh keeps in a staged
// copy and excludes from its sync; they are not part of the bundle.
var stagedMarkers = map[string]bool{".fleet-staged-from": true, ".fleet-staged-from.new": true}

// stagedCopyDiff names the first path at which the staged copy dst differs
// from its source src ("" when they match): a file missing, extra, of another
// type, or with other bytes. It is the read-only twin of update.sh's
// `rsync --checksum --delete`, so `fleet update --check` reports a copy whose
// source moved on outside `fleet update` (a hand fast-forward of the checkout)
// rather than calling it current because the marker still names the path.
//
// dst is the service user's tree and this runs as root, so it is read through
// an os.Root: no symlink in it, at any depth, can lead the comparison out to
// a root-only file. Nothing from either tree is printed but a relative path.
func stagedCopyDiff(src, dst string) (string, error) {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return "", err
	}
	defer root.Close()
	seen := map[string]bool{}
	errDiff := errors.New("differs")
	var diff string
	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		seen[rel] = true
		if !sameEntry(root, p, rel, d) {
			diff = rel
			return errDiff
		}
		return nil
	})
	if errors.Is(walkErr, errDiff) {
		return diff, nil
	}
	if walkErr != nil {
		return "", walkErr
	}
	// Anything in the copy the source no longer has: rsync --delete would
	// remove it, so it is a difference too.
	walkErr = fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." || stagedMarkers[p] {
			return nil
		}
		if !seen[filepath.FromSlash(p)] {
			diff = filepath.FromSlash(p)
			return errDiff
		}
		return nil
	})
	if errors.Is(walkErr, errDiff) {
		return diff, nil
	}
	return "", walkErr
}

// sameEntry reports whether rel in the staged root has the source entry's
// type and, for a file, its bytes (for a link, its target).
func sameEntry(root *os.Root, srcPath, rel string, d fs.DirEntry) bool {
	fi, err := root.Lstat(rel)
	if err != nil || fi.Mode().Type() != d.Type() {
		return false
	}
	switch {
	case d.IsDir():
		return true
	case d.Type()&fs.ModeSymlink != 0:
		want, err1 := os.Readlink(srcPath)
		got, err2 := root.Readlink(rel)
		return err1 == nil && err2 == nil && want == got
	case d.Type().IsRegular():
		return sameFile(root, srcPath, rel)
	default:
		return false
	}
}

func sameFile(root *os.Root, srcPath, rel string) bool {
	//nolint:gosec // G304: a path walked inside this checkout's own config/default.
	a, err := os.ReadFile(srcPath)
	if err != nil {
		return false
	}
	// Non-blocking, and re-checked through the descriptor: the entry was a
	// regular file at Lstat, but the service user can swap it before the open.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(len(a)) {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(len(a))+1))
	return err == nil && bytes.Equal(a, b)
}
