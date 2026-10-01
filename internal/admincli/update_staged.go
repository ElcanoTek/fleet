package admincli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// stagedCopyCheck is clientBundleCheck for a bundle dir that carries (or
// should carry) a staging marker. handled is false only when there is no
// marker at all — a hand-placed bundle or a checkout, which the caller checks
// as such. A staged copy is current only when its marker names THIS
// checkout's config/default and its content still matches that source.
func stagedCopyCheck(dir string) (handled, stale bool) {
	src, state := stagedBundleSource(dir)
	switch state {
	case markerAbsent:
		return false, false
	case markerUnreadable:
		// The shipped unit keeps its state dir 0700, so an operator without
		// root cannot read a copy staged there; saying nothing would let the
		// check pass without having looked.
		fmt.Printf("client bundle at %s cannot be read as this user, so whether it is current is unknown — re-run as root: sudo fleet update --check\n", dir)
		return true, true
	case markerInvalid:
		fmt.Printf("client bundle at %s has a staging marker that is not a readable regular file — `fleet update` will not recognise or refresh it.\n", dir)
		fmt.Println("  restage it: move it aside and re-run scripts/bootstrap.sh --enable-service from this checkout")
		return true, true
	case markerOK:
		// Checked against this checkout below.
	}
	want := ""
	if root := repoRoot(); root != "" {
		want = filepath.Join(root, "config", "default")
	}
	if want == "" {
		fmt.Printf("client bundle at %s is the staged copy of %s — `fleet update` refreshes it from there (and says so if it could not).\n", dir, src)
		return true, false
	}
	if canonicalPath(src) != canonicalPath(want) {
		fmt.Printf("client bundle at %s is a staged copy of %s, not of this checkout's %s — `fleet update` will not refresh it.\n", dir, src, want)
		fmt.Println("  restage it: re-run scripts/bootstrap.sh --enable-service from this checkout")
		return true, true
	}
	if rel, err := stagedCopyDiff(want, dir); err != nil || rel != "" {
		what := rel
		if err != nil {
			what = err.Error()
		}
		fmt.Printf("client bundle at %s is the staged copy of %s, but it no longer matches it (first difference: %s) — `fleet update` restages it.\n", dir, src, what)
		return true, true
	}
	fmt.Printf("client bundle at %s is the staged copy of %s — `fleet update` refreshes it from there (and says so if it could not).\n", dir, src)
	return true, false
}

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
	if d.Type()&fs.ModeSymlink == 0 && !sameOwnerBits(srcPath, fi) {
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

// sameOwnerBits compares the owner's rwx bits, which the refresh (rsync -a)
// restores: a directory gone 000 or a script that lost its executable bit is
// a copy update would repair, so --check must not call it current. Group and
// other bits are left out: the owner-side unpack applies the service user's
// umask, so they can legitimately differ from the checkout's.
func sameOwnerBits(srcPath string, fi fs.FileInfo) bool {
	si, err := os.Lstat(srcPath)
	return err == nil && si.Mode().Perm()&0o700 == fi.Mode().Perm()&0o700
}
