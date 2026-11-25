package admincli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Connector state: the folders client-bundle connectors keep for the whole
// deployment under <workspace root>/mcp-runs/ — written host-side only, read-only
// in every sandbox (docs/MCP-BUNDLE-ENV.md) — such as a campaign-code counter
// (mcp-runs/campaign-codes) or the team's email templates
// (mcp-runs/email-templates). They are plain files, so no database dump holds
// them, yet losing one can matter more than losing a day of chat: a counter that
// comes back empty can hand out a code it already gave.
//
// `fleet backup --db=all` therefore also writes fleet-files-<stamp>.tar.gz beside
// the dump: every directory directly under mcp-runs/ except the per-run
// task-<id> directories (one scheduled occurrence's ledger, removed by the
// maintenance sweep once its task is done — transient, not state; matched by
// their exact "task-<uuid>" name). Symlinks,
// sockets, FIFOs and devices are never archived or followed. The archive is
// verified by reading it back before the run reports it, pruned with the dumps,
// and restored by hand with tar (docs/BACKUP_RESTORE.md "Connector state").
//
// The copy takes no connector's lock: each file is copied up to the size it had
// when it was opened. A connector whose files are an append-only log plus a
// counter replaced by rename (the campaign-code store) reads such a copy as the
// state at that moment, at worst minus a line torn mid-append, which its reader
// skips.

const connectorStateParent = "mcp-runs"

// runDirName matches exactly the per-run directories under mcp-runs/:
// "task-" + the task's UUID (scheduledrun.taskMCPRunKey), which the maintenance
// sweep removes once the task is done. Anything else — a connector's own
// "task-board", say — is kept state.
var runDirName = regexp.MustCompile(`^task-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// maxConnectorStateBytes bounds what one run archives. Connector state is
// counters, logs and templates (kilobytes); a tree past this is something
// else, and archiving it nightly for the retention window would fill the
// backup disk, so the run fails loudly instead. A var only so a test can lower it.
var maxConnectorStateBytes int64 = 1 << 30

// connectorStateFileName is fleet-files-<UTC stamp>.tar.gz, the same stamp as
// the dump the run writes beside it.
func connectorStateFileName(now time.Time) string {
	return fmt.Sprintf("fleet-files-%s.tar.gz", now.UTC().Format("20060102T150405Z"))
}

// stateEntry is one directory or regular file to archive, relative to the
// workspace root (always "mcp-runs/<dir>[/...]", slash-separated).
type stateEntry struct {
	rel  string
	info fs.FileInfo
}

// connectorStateEntries lists what the archive holds: every kept directory
// under root/mcp-runs and the directories and regular files inside it, in
// lexical order. It returns nothing (and no error) when there is no mcp-runs
// directory or it holds only per-run directories. A symlinked mcp-runs is
// refused; symlinks and special files inside are skipped.
func connectorStateEntries(root string) ([]stateEntry, int64, error) {
	parent := filepath.Join(root, connectorStateParent)
	info, err := os.Lstat(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", parent, err)
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("%s is not a real directory (a symlink or a file): refusing to archive through it", parent)
	}
	tops, err := os.ReadDir(parent)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", parent, err)
	}
	var entries []stateEntry
	var total int64
	for _, top := range tops {
		if !top.IsDir() || runDirName.MatchString(top.Name()) {
			continue // a symlink, a stray file, or one run's own directory
		}
		dir := filepath.Join(parent, top.Name())
		walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return nil // symlinks, sockets, FIFOs, devices: never archived or followed
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				total += fi.Size()
				if total > maxConnectorStateBytes {
					return fmt.Errorf("connector state under %s is over %d MiB: refusing to archive it (it is not counters and logs)", parent, maxConnectorStateBytes>>20)
				}
			}
			entries = append(entries, stateEntry{rel: filepath.ToSlash(rel), info: fi})
			return nil
		})
		if walkErr != nil {
			return nil, 0, walkErr
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, total, nil
}

// writeConnectorStateArchive archives the connector state under root into
// outPath (0600, written to a temporary name and renamed into place, then read
// back). It returns the kept directories it archived; with none, it writes
// nothing and returns an empty list.
func writeConnectorStateArchive(root, outPath string) (dirs []string, err error) {
	entries, _, err := connectorStateEntries(root)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".fleet-files-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create the archive: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath) // no partial archive left for a restore to find
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("chmod 0600 %s: %w", tmpPath, err)
	}
	// Every file is opened through an os.Root over mcp-runs/, so a directory
	// swapped for a symlink after the listing cannot lead the copy (run as
	// root) outside it.
	parent, err := os.OpenRoot(filepath.Join(root, connectorStateParent))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Join(root, connectorStateParent), err)
	}
	defer parent.Close()
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	// The bound holds for what is copied, not only for what was listed: a file
	// that grew after the listing is copied at its new size, out of this budget.
	budget := maxConnectorStateBytes
	for _, e := range entries {
		if err = addStateEntry(tw, parent, e, &budget); err != nil {
			return nil, err
		}
		if e.info.IsDir() && strings.Count(e.rel, "/") == 1 {
			dirs = append(dirs, path.Base(e.rel))
		}
	}
	if err = tw.Close(); err != nil {
		return nil, fmt.Errorf("write the archive: %w", err)
	}
	if err = gz.Close(); err != nil {
		return nil, fmt.Errorf("write the archive: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync the archive: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return nil, fmt.Errorf("close the archive: %w", err)
	}
	if err = verifyConnectorStateArchive(tmpPath, len(entries)); err != nil {
		return nil, err
	}
	if err = os.Rename(tmpPath, outPath); err != nil {
		return nil, fmt.Errorf("move the archive into place: %w", err)
	}
	return dirs, nil
}

// addStateEntry writes one directory or file. A file is opened inside parent
// (the os.Root over mcp-runs/) without following a symlink planted since the
// listing and without blocking on a FIFO, checked to still be a regular file,
// and copied up to the size it has now: an append-only log that grows during
// the copy is archived as it was when opened. The size is taken out of
// *budget, the bytes the archive may still hold; a file past it fails the
// archive.
func addStateEntry(tw *tar.Writer, parent *os.Root, e stateEntry, budget *int64) error {
	if e.info.IsDir() {
		hdr, err := tar.FileInfoHeader(e.info, "")
		if err != nil {
			return fmt.Errorf("archive %s: %w", e.rel, err)
		}
		hdr.Name = e.rel + "/"
		return tw.WriteHeader(hdr)
	}
	inParent := strings.TrimPrefix(e.rel, connectorStateParent+"/")
	f, err := parent.OpenFile(filepath.FromSlash(inParent), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil // removed, or replaced by a symlink, since the listing: nothing to keep
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", e.rel, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", e.rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil // replaced by something that is not a file since the listing
	}
	if info.Size() > *budget {
		return fmt.Errorf("connector state is over %d MiB by %s: refusing to archive it (it is not counters and logs)", maxConnectorStateBytes>>20, e.rel)
	}
	*budget -= info.Size()
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("archive %s: %w", e.rel, err)
	}
	hdr.Name = e.rel
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("archive %s: %w", e.rel, err)
	}
	if _, err := io.CopyN(tw, f, info.Size()); err != nil {
		return fmt.Errorf("archive %s (did it shrink while being copied?): %w", e.rel, err)
	}
	return nil
}

// verifyConnectorStateArchive reads the archive back end to end (gzip
// checksum, every tar header and body) and checks it holds want entries, so a
// run never reports an archive it cannot restore.
func verifyConnectorStateArchive(p string, want int) error {
	f, err := os.Open(p) //nolint:gosec // G304: the archive this run just wrote.
	if err != nil {
		return fmt.Errorf("verify the archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("verify the archive: %w", err)
	}
	tr := tar.NewReader(gz)
	n := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("verify the archive: %w", err)
		}
		if !strings.HasPrefix(hdr.Name, connectorStateParent+"/") {
			return fmt.Errorf("verify the archive: unexpected entry %q", hdr.Name)
		}
		total += hdr.Size
		if hdr.Size < 0 || total > maxConnectorStateBytes {
			return fmt.Errorf("verify the archive: entry %q takes it past %d MiB", hdr.Name, maxConnectorStateBytes>>20)
		}
		if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
			return fmt.Errorf("verify the archive: %w", err)
		}
		n++
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("verify the archive: %w", err)
	}
	// A file removed between the listing and the copy is skipped, so the
	// archive may hold fewer entries than were listed, never more.
	if n == 0 || n > want {
		return fmt.Errorf("verify the archive: it holds %d entries, %d were listed", n, want)
	}
	return nil
}

// backupConnectorState writes the connector-state archive for a --db=all run
// into outDir and reports it. No kept directory is not an error.
func backupConnectorState(outDir string, now time.Time) error {
	root := resolveWorkspaceRoot("")
	outPath := filepath.Join(outDir, connectorStateFileName(now))
	dirs, err := writeConnectorStateArchive(root, outPath)
	if err != nil {
		return fmt.Errorf("connector state under %s: %w", filepath.Join(root, connectorStateParent), err)
	}
	if len(dirs) == 0 {
		fmt.Fprintf(os.Stderr, "no connector state under %s to back up\n", filepath.Join(root, connectorStateParent))
		return nil
	}
	fmt.Fprintf(os.Stderr, "backed up connector state (%s) → %s (verified)\n", strings.Join(dirs, ", "), outPath)
	fmt.Println(outPath)
	return nil
}
