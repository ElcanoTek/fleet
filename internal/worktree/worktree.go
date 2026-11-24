// Package worktree owns the on-disk layout of per-run git worktrees and the
// reclamation of the ones that were left behind.
//
// Scheduled runs with worktree_config enabled get an isolated worktree per run
// under <workspace>/.fleet-worktrees/<task>-<run> (internal/scheduledrun
// creates them; see that package's doc for why they live under the workspace
// root rather than /tmp). The run's own cleanup removes it — but only on the
// paths that reach cleanup. A crashed process or a task with auto_cleanup off
// leaves a full checkout on disk, and a large repository's worktree is not
// small.
//
// Creating, listing, removing and reclaiming worktrees, and deleting a run's
// branch, run no git on the host. A worktree run's sandbox writes its own
// record and checkout, the object store, refs and reflogs, and host git would
// read (and follow) all of it; this package writes and deletes records,
// checkouts and branch refs itself, through os.Root handles reached without
// following a symlink at any component, and reads only regular files with
// non-blocking opens.
//
// It also records, per workspace root, what the root's .git was when fleet
// started (RepoPin). Host-side worktree code acts only on that .git: a
// repository that appeared, or a .git that was replaced, after startup is left
// alone until a restart looks again.
//
// Every directory it opens under the workspace root goes through
// tools.OpenWorkspaceDir / tools.OpenSubdirNoFollow, the one no-follow opener
// host code uses for that tree (ADR-0036), so a symlink planted at any level
// is refused rather than followed. Both the operator CLI (`fleet worktree
// prune`) and the server's in-process maintenance loop reclaim worktrees
// through this one package.
package worktree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Subdir is the directory under the workspace root that holds per-run
// worktrees. Kept out of the main working tree's `git status` via
// .git/info/exclude (see scheduledrun.ensureWorktreeExcluded).
const Subdir = ".fleet-worktrees"

// DefaultPruneAge is how old a worktree directory must be before an unattended
// sweep will reclaim it.
//
// Generous on purpose. The age is the ONLY signal this sweep has: a worktree
// belonging to a task that is still running looks exactly like one a crash left
// behind, and deleting a live run's checkout out from under it would destroy
// work in progress. A day is comfortably longer than the default task
// wall-clock ceiling (4h), so a directory this old cannot belong to a run that
// is still going, while a crash's leftovers are reclaimed the next day.
const DefaultPruneAge = 24 * time.Hour

// MinPruneAge is the floor applied to any caller-supplied age. It is tied to
// the default per-run wall-clock ceiling (FLEET_TASK_WALL_TIMEOUT, 4h — see
// docs/MAINTENANCE.md): a worktree younger than the ceiling can belong to a
// run that is still inside its budget, so no age below it is honoured, however
// it was requested (`--older-than 10m`, a mistyped FLEET_WORKTREE_PRUNE_AGE).
// A box with a raised ceiling needs a raised prune age too; the floor cannot
// know the operator's override, which is why it stays at the default ceiling.
const MinPruneAge = 4 * time.Hour

// Result reports what one sweep did. Removed and Warnings are returned rather
// than printed so the caller decides the surface: the CLI prints them, the
// server logs a summary.
type Result struct {
	// Removed lists the worktree directories reclaimed (or, on a dry run, the
	// ones that would have been).
	Removed []string
	// Kept counts directories skipped for being younger than the age bound
	// or locked.
	Kept int
	// Warnings records per-entry problems that did not stop the sweep — a stat
	// failure, an entry that could not be removed. Best-effort reclamation
	// means one bad entry must never abort the rest.
	Warnings []string
}

// Count is the number of directories removed (or that would be removed).
func (r Result) Count() int { return len(r.Removed) }

// ---------------------------------------------------------------------------
// The startup record of the workspace repository.

type pinState int

const (
	pinNotRecorded pinState = iota // nothing recorded for this root in this process
	pinNoRepo                      // no .git at startup
	pinRepo                        // .git was a real directory at startup
	pinUnusual                     // .git was a gitfile, a symlink or something else
)

// RepoPin is what a workspace root's .git was when this process recorded it
// (PinAtStartup) — at startup, for the server.
type RepoPin struct {
	state  pinState
	gitDir os.FileInfo // the .git directory as opened at startup
	detail string
}

// IsRepo reports whether the root's .git was a real directory at startup.
func (p RepoPin) IsRepo() bool { return p.state == pinRepo }

var (
	pinMu sync.Mutex
	pins  = map[string]RepoPin{}
)

// pinKey is the map key for root: absolute and cleaned. Symlinks are not
// resolved; every caller derives the root the same way.
func pinKey(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(root)
}

// PinAtStartup records what root's .git is now, replacing any earlier record.
// fleet serve (and every other entrypoint that builds the sandbox pool) calls
// it once, before any sandbox exists.
func PinAtStartup(root string) RepoPin {
	p := inspectRepo(root)
	pinMu.Lock()
	pins[pinKey(root)] = p
	pinMu.Unlock()
	return p
}

// Pin returns the startup record for root. A root nothing recorded at startup
// is not a repository as far as host-side worktree code is concerned: Pin
// never records one later, so a repository that appeared after startup stays
// unused until a restart records it. (`fleet worktree prune`, a one-off CLI
// process, records the root at its own start.)
func Pin(root string) RepoPin {
	pinMu.Lock()
	defer pinMu.Unlock()
	if p, ok := pins[pinKey(root)]; ok {
		return p
	}
	return RepoPin{state: pinNotRecorded}
}

func inspectRepo(root string) RepoPin {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return RepoPin{state: pinNoRepo}
	case err != nil:
		return RepoPin{state: pinUnusual, detail: err.Error()}
	case info.Mode()&fs.ModeSymlink != 0:
		return RepoPin{state: pinUnusual, detail: ".git is a symlink"}
	case !info.IsDir():
		return RepoPin{state: pinUnusual, detail: ".git is not a directory (a gitfile, for a root that is itself a linked worktree)"}
	}
	// Startup runs before any request or run exists, so there is no
	// correlation to carry into the opener's refusal log.
	fi, err := statNoFollow(context.Background(), root, ".git")
	if err != nil {
		return RepoPin{state: pinUnusual, detail: err.Error()}
	}
	return RepoPin{state: pinRepo, gitDir: fi}
}

// ErrNotPinnedRepo is returned (wrapped) by Check when the root's .git is not
// the real directory recorded at startup, is laid out in a way fleet does
// not act on, or points out of itself. The wrapped detail says which.
var ErrNotPinnedRepo = errors.New("the workspace repository cannot be used for worktree tasks")

// Check reports whether root's .git is still the real directory pinned at
// startup and lays out no pointer out of itself: no commondir file (which
// would make git use another directory as the repository); real refs/,
// worktrees/, logs/, info/, lfs/, objects/ and objects/info/ directories,
// reached without following a link (CreateWorktree writes the new worktree's
// record into worktrees/; a worktree run's sandbox mounts objects/, refs/,
// logs/ and lfs/ writable); no objects/info/alternates (which would make git read objects
// from another repository); regular HEAD, config and packed-refs files; and
// no reftable ref storage (whose tables worktree runs could not write; see
// ADR-0118). Anything Check cannot establish is a refusal, never read as
// "absent". The worktree bookkeeping calls it before acting.
func (p RepoPin) Check(ctx context.Context, root string) error {
	return p.check(ctx, root, false)
}

// CheckBeforeStart is Check for a process that is not the server, such as
// `fleet doctor`: a directory PrepareRepoAtStartup creates (worktrees/,
// logs/, info/, lfs/, objects/info/) may still be missing, because the
// server creates it when it starts.
func (p RepoPin) CheckBeforeStart(ctx context.Context, root string) error {
	return p.check(ctx, root, true)
}

// check is Check; with startupDirsMayBeMissing, a missing directory that
// PrepareRepoAtStartup creates is not an error.
func (p RepoPin) check(ctx context.Context, root string, startupDirsMayBeMissing bool) error {
	switch p.state {
	case pinNotRecorded:
		return fmt.Errorf("%w: %s was not recorded when fleet started", ErrNotPinnedRepo, root)
	case pinNoRepo:
		return fmt.Errorf("%w: %s had no .git when fleet started; restart fleet after making the workspace root a git repository", ErrNotPinnedRepo, root)
	case pinUnusual:
		return fmt.Errorf("%w: at startup %s (%s); worktree tasks need .git to be a real directory", ErrNotPinnedRepo, filepath.Join(root, ".git"), p.detail)
	case pinRepo:
		// Checked below against the directory recorded at startup.
	}
	gitDir, err := tools.OpenWorkspaceDir(ctx, root, filepath.Join(root, ".git"))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotPinnedRepo, err)
	}
	defer gitDir.Close()
	now, err := gitDir.Stat(".")
	if err != nil || !os.SameFile(now, p.gitDir) {
		return fmt.Errorf("%w: %s was replaced after startup; restart fleet to use it", ErrNotPinnedRepo, filepath.Join(root, ".git"))
	}
	if err := mustBeAbsent(gitDir.Root, "commondir", filepath.Join(root, ".git")); err != nil {
		return err
	}
	if err := mustBeAbsent(gitDir.Root, "reftable", filepath.Join(root, ".git")); err != nil {
		return fmt.Errorf("%w (worktree tasks support the files ref storage only)", err)
	}
	// Directories host code writes into, or a worktree run's sandbox
	// mounts: each a real directory reached without following a link.
	createdAtStart := func(rel string, err error) bool {
		return startupDirsMayBeMissing && errors.Is(err, fs.ErrNotExist) && slices.Contains(startupDirs, rel)
	}
	for _, name := range []string{"refs", "worktrees", "logs", "info", "lfs"} {
		d, err := tools.OpenSubdirNoFollow(gitDir.Root, name)
		if createdAtStart(name, err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrNotPinnedRepo, filepath.Join(root, ".git", name), err)
		}
		_ = d.Close()
	}
	// Files git in a worktree run reads through the shared .git: regular
	// files, never links or FIFOs.
	for _, name := range []string{"HEAD", "config", "packed-refs"} {
		info, err := gitDir.Lstat(name)
		if name == "packed-refs" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrNotPinnedRepo, filepath.Join(root, ".git", name), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file (%s)", ErrNotPinnedRepo, filepath.Join(root, ".git", name), info.Mode().Type())
		}
	}
	objects, err := tools.OpenSubdirNoFollow(gitDir.Root, "objects")
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotPinnedRepo, filepath.Join(root, ".git", "objects"), err)
	}
	defer objects.Close()
	info, err := tools.OpenSubdirNoFollow(objects, "info")
	if createdAtStart(filepath.Join("objects", "info"), err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotPinnedRepo, filepath.Join(root, ".git", "objects", "info"), err)
	}
	defer info.Close()
	return mustBeAbsent(info, "alternates", filepath.Join(root, ".git", "objects", "info"))
}

// mustBeAbsent succeeds only when name provably does not exist in dir.
func mustBeAbsent(dir *os.Root, name, dirPath string) error {
	_, err := dir.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil:
		return fmt.Errorf("%w: %s exists; fleet does not run git in a repository laid out this way", ErrNotPinnedRepo, filepath.Join(dirPath, name))
	default:
		return fmt.Errorf("%w: %s: %w", ErrNotPinnedRepo, filepath.Join(dirPath, name), err)
	}
}

// startupDirs are the directories inside .git that the worktree code acts
// on, or that a worktree run's sandbox mounts (lfs/, the Git LFS store,
// is mounted writable for worktree runs; the host never reads or acts on
// anything under it), and
// that therefore must be real directories from the first sandbox on: they
// are created at startup when missing (git init does not always create all
// of them — logs/ comes with the first ref update, worktrees/ with the first
// worktree, and other tools skip objects/info).
var startupDirs = []string{
	filepath.Join("objects", "info"),
	"worktrees",
	"logs",
	filepath.Join("logs", "refs"),
	filepath.Join("logs", "refs", "heads"),
	"info",
	"lfs",
}

// PrepareRepoAtStartup creates the startupDirs that are missing under
// <root>/.git, one level at a time without following a link, and fails when
// one of them exists as anything but a real directory. Called at startup,
// for a root recorded as a repository, before any sandbox exists.
func PrepareRepoAtStartup(ctx context.Context, root string) error {
	gitDir, err := tools.OpenWorkspaceDir(ctx, root, filepath.Join(root, ".git"))
	if err != nil {
		return err
	}
	defer gitDir.Close()
	var errs []error
	for _, rel := range startupDirs {
		cur := gitDir.Root
		var opened []*os.Root
		for _, name := range strings.Split(rel, string(filepath.Separator)) {
			if err := cur.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				errs = append(errs, fmt.Errorf("create %s: %w", filepath.Join(root, ".git", rel), err))
				break
			}
			next, err := tools.OpenSubdirNoFollow(cur, name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", filepath.Join(root, ".git", rel), err))
				break
			}
			opened = append(opened, next)
			cur = next
		}
		for _, r := range opened {
			_ = r.Close()
		}
	}
	return errors.Join(errs...)
}

// DeleteBranch deletes a run's branch (refs/heads/<branch>), its reflog and
// any packed-refs entry, without running git. A worktree run's sandbox can
// write .git/refs and .git/logs while the host works, and git's own ref
// deletion unlinks by path, so a directory replaced by a symlink in between
// would make it unlink elsewhere. Here every directory on the way is opened
// without following a link (tools.OpenWorkspaceDir) and the entry is removed
// relative to that opened directory, so nothing outside it is touched.
// packed-refs sits in .git itself, which no sandbox can write, so it is
// rewritten in place under git's packed-refs.lock. The repository must be
// the one recorded at startup.
//
// The order never shows an older value of the branch. packed-refs.lock is
// taken first; if it cannot be (another writer holds it past
// packedRefsLockWait), nothing is removed and the error says so. With the
// lock held, the packed line goes first (the rewritten file replaces
// packed-refs, which releases the lock), and only then the loose ref and
// its reflog: while a loose ref exists it is what git reads, so removing it
// before the packed line would expose the older packed value. When nothing
// is packed for the branch, the loose ref and reflog are removed while the
// lock is still held.
func DeleteBranch(ctx context.Context, root, branch string) error {
	clean := filepath.Clean(branch)
	if clean == "." || !filepath.IsLocal(clean) || strings.ContainsAny(branch, "\x00\n") {
		return fmt.Errorf("delete branch: %q is not a plain ref name", branch)
	}
	if err := Pin(root).Check(ctx, root); err != nil {
		return err
	}
	gitDir, err := tools.OpenWorkspaceDir(ctx, root, filepath.Join(root, ".git"))
	if err != nil {
		return err
	}
	defer gitDir.Close()
	lock, err := takePackedRefsLock(ctx, gitDir.Root)
	if err != nil {
		return fmt.Errorf("delete branch %s: %w; nothing was removed", branch, err)
	}
	held := true
	release := func() {
		if held {
			_ = lock.Close()
			_ = gitDir.Remove("packed-refs.lock")
			held = false
		}
	}
	defer release()
	replaced, err := dropPackedRef(gitDir.Root, lock, "refs/heads/"+filepath.ToSlash(clean))
	if err != nil {
		return fmt.Errorf("delete branch %s: packed-refs: %w; nothing was removed", branch, err)
	}
	if replaced {
		held = false // the lock file is packed-refs now
	}
	errs := removeLooseRef(ctx, root, clean)
	release()
	return errors.Join(errs...)
}

// removeLooseRef removes refs/heads/<branch> and logs/refs/heads/<branch>,
// each relative to its directory opened without following a link.
func removeLooseRef(ctx context.Context, root, branch string) []error {
	var errs []error
	for _, base := range []string{filepath.Join(".git", "refs", "heads"), filepath.Join(".git", "logs", "refs", "heads")} {
		dir := filepath.Join(root, base, filepath.Dir(branch))
		d, err := tools.OpenWorkspaceDir(ctx, root, dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if rmErr := d.Remove(filepath.Base(branch)); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			errs = append(errs, rmErr)
		}
		_ = d.Close()
	}
	return errs
}

// packedRefsLockWait bounds how long DeleteBranch waits for another
// writer's packed-refs.lock, like git's core.packedRefsTimeout (1s).
var packedRefsLockWait = time.Second

// dropPackedRef removes ref (and its peeled line) from .git/packed-refs, if
// listed, with packed-refs.lock already held through lock (DeleteBranch);
// it reads the file only now, so an operator's `git pack-refs` or a
// concurrent cleanup cannot slip a write in between (and have it undone).
// It reports whether it replaced packed-refs with the rewritten lock file,
// which releases the lock; when ref is not listed it writes nothing.
func dropPackedRef(gitDir *os.Root, lock *os.File, ref string) (bool, error) {
	data, err := readRegularNoFollow(gitDir, "packed-refs", 256<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := gitDir.Lstat("packed-refs")
	if err != nil {
		return false, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	kept := make([]string, 0, len(lines))
	dropped, skipPeeled := false, false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\n")
		if skipPeeled && strings.HasPrefix(trimmed, "^") {
			continue
		}
		skipPeeled = false
		if fields := strings.Fields(trimmed); len(fields) == 2 && !strings.HasPrefix(trimmed, "#") && fields[1] == ref {
			dropped, skipPeeled = true, true
			continue
		}
		kept = append(kept, line)
	}
	if !dropped {
		return false, nil
	}
	// The new file keeps the old one's permissions and is on disk before it
	// replaces it, so a crash leaves either the old file or the new one.
	if err := lock.Chmod(info.Mode().Perm()); err != nil {
		return false, err
	}
	if _, err := lock.WriteString(strings.Join(kept, "")); err != nil {
		return false, err
	}
	if err := lock.Sync(); err != nil {
		return false, err
	}
	if err := lock.Close(); err != nil {
		return false, err
	}
	if err := gitDir.Rename("packed-refs.lock", "packed-refs"); err != nil {
		return false, err
	}
	return true, nil
}

// takePackedRefsLock creates .git/packed-refs.lock exclusively (O_EXCL never
// follows a link), retrying while another writer holds it.
func takePackedRefsLock(ctx context.Context, gitDir *os.Root) (*os.File, error) {
	deadline := time.Now().Add(packedRefsLockWait)
	for {
		lock, err := gitDir.OpenFile("packed-refs.lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, fs.ErrExist) || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("lock packed-refs: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// IsShallow reports whether root's repository is a shallow clone: its .git,
// opened without following a link, holds a regular `shallow` file (Lstat
// only; nothing is read). A worktree run's sandbox mounts .git's own
// entries read-only, so fetches that change the shallow boundary
// (--depth, --deepen, --shallow-since, --shallow-exclude, --unshallow) fail
// there when git creates .git/shallow.lock; ordinary fetches do not write
// it.
func IsShallow(ctx context.Context, root string) bool {
	gitDir, err := tools.OpenWorkspaceDir(ctx, root, filepath.Join(root, ".git"))
	if err != nil {
		return false
	}
	defer gitDir.Close()
	info, err := gitDir.Lstat("shallow")
	return err == nil && info.Mode().IsRegular()
}

// LFSStorage returns the lfs.storage value root's repository sets in its own
// .git/config, when that names anything other than the default .git/lfs, and
// true; otherwise "" and false. Only .git/config is read (a regular file,
// opened without following a link, at most 1 MiB; no sandbox can write it),
// and only an `[lfs]` section's `storage` key counts: includes, and global
// or system configuration, are not consulted. A worktree run's sandbox can
// write only .git/lfs, so Git LFS cannot store objects anywhere else.
func LFSStorage(ctx context.Context, root string) (string, bool) {
	gitDir, err := openUnder(ctx, root, ".git")
	if err != nil {
		return "", false
	}
	defer gitDir.Close()
	data, err := readRegularNoFollow(gitDir, "config", 1<<20)
	if err != nil {
		return "", false
	}
	storage := lfsStorageIn(string(data))
	if storage == "" {
		return "", false
	}
	if p := filepath.Clean(storage); p == "lfs" || p == filepath.Join(pinKey(root), ".git", "lfs") {
		return "", false
	}
	return storage, true
}

// lfsStorageIn returns the last `storage` value in an `[lfs]` section of a
// git config file's text (section and key names compared case-insensitively,
// a quoted value unquoted, a trailing comment dropped), or "".
func lfsStorageIn(config string) string {
	inLFS, value := false, ""
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				inLFS = false
				continue
			}
			inLFS = strings.EqualFold(strings.TrimSpace(line[1:end]), "lfs")
			continue
		}
		if !inLFS {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "storage") {
			continue
		}
		val = strings.TrimSpace(val)
		if strings.HasPrefix(val, `"`) {
			if end := strings.IndexByte(val[1:], '"'); end >= 0 {
				val = val[1 : 1+end]
			}
		} else if i := strings.IndexAny(val, "#;"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		value = val
	}
	return value
}

// statNoFollow opens rel beneath root without following a link at any level
// and returns what it opened.
func statNoFollow(ctx context.Context, root, rel string) (os.FileInfo, error) {
	d, err := tools.OpenWorkspaceDir(ctx, root, filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Stat(".")
}

// openUnder opens rel beneath root as an os.Root without following a link at
// any level (tools.OpenWorkspaceDir). Every operation through it stays inside
// the directory that was opened.
func openUnder(ctx context.Context, root, rel string) (*os.Root, error) {
	d, err := tools.OpenWorkspaceDir(ctx, root, filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	return d.Root, nil
}

// CheckSubdir returns <root>/.fleet-worktrees when it is a real directory
// directly beneath root, and an error when it is a symlink or anything else.
// The name sits in the workspace root, which whole-root sandboxes mount
// read-write. When the root is a git repository at startup the pool creates
// the directory and every whole-root sandbox gets it as its own read-only
// bind mount (agent.wholeRootGitDirs); otherwise a sandbox could put anything
// there. So both the creator and the sweeper act only on a real directory and
// never on whatever a link names. A missing directory is returned as an
// fs.ErrNotExist error for the caller to treat as it sees fit.
func CheckSubdir(root string) (string, error) {
	parent := filepath.Join(root, Subdir)
	info, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory (%s); remove it so fleet can recreate it", parent, info.Mode().Type())
	}
	return parent, nil
}

// ---------------------------------------------------------------------------
// The repository's worktree records.

// adminRecord is one entry of .git/worktrees proven to be the record of a
// worktree under .fleet-worktrees.
type adminRecord struct {
	id     string // the entry's name under .git/worktrees
	locked string // the lock reason
	isLock bool
}

// initializingLock is the lock reason CreateWorktree writes first and removes
// once the record is complete (git worktree add does the same). One still
// there on a checkout past the prune age was left by a server stopped in the
// middle of CreateWorktree, and the sweep gives it no weight.
const initializingLock = "initializing"

// recordSet is what fleetRecords proved about .git/worktrees.
type recordSet struct {
	// live maps a .fleet-worktrees/<name> directory to its record, proven
	// from both sides: the checkout's own .git file names the record, and the
	// record's gitdir names the checkout. A worktree run can rewrite its own
	// checkout's .git and its own record, never another run's, so it can only
	// ever prove itself.
	live map[string]adminRecord
	// stale lists records whose gitdir names a .fleet-worktrees/<name>
	// directory that no longer exists.
	stale []string
	// unfinished maps a record that holds nothing but an initializing lock
	// (see unfinishedRecord) to the lock's modification time.
	unfinished map[string]time.Time
}

// openRegularNoFollow opens name in dir only when it is a regular file and
// not a link. os.Root follows a link that stays inside dir even with
// O_NOFOLLOW, so the entry is Lstat'ed first (a link or anything but a
// regular file is refused) and the opened file must be that very file
// (os.SameFile), so a swap between the two is refused too. O_NONBLOCK keeps
// a FIFO swapped in from blocking the open. With O_CREATE the file may be
// missing; it is then created (O_EXCL, which never follows a link).
func openRegularNoFollow(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	pre, err := dir.Lstat(name)
	switch {
	case err == nil && !pre.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file (%s)", name, pre.Mode().Type())
	case errors.Is(err, fs.ErrNotExist) && flag&os.O_CREATE != 0:
		return dir.OpenFile(name, flag|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	case err != nil:
		return nil, err
	}
	f, err := dir.OpenFile(name, (flag&^os.O_CREATE)|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	post, err := f.Stat()
	if err != nil || !post.Mode().IsRegular() || !os.SameFile(pre, post) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was opened", name)
	}
	return f, nil
}

// smallRegularFile reads name from dir when it is a regular file that is
// not a link (openRegularNoFollow), reading at most limit bytes. Every host
// read of a file a sandbox can write goes through this.
func smallRegularFile(dir *os.Root, name string, limit int64) (string, bool) {
	data, err := readRegularNoFollow(dir, name, limit)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// readRegularNoFollow is smallRegularFile with the reason for a refusal.
func readRegularNoFollow(dir *os.Root, name string, limit int64) ([]byte, error) {
	f, err := openRegularNoFollow(dir, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, nil
}

// fleetRecords reads .git/worktrees (through admin) and .fleet-worktrees
// (through sub, nil when it does not exist) and returns the records it can
// prove are fleet's: live ones (see recordSet) and stale ones. Entries that
// are symlinks or not directories, records whose gitdir names anything but a
// directory directly under .fleet-worktrees, and pairs that do not name each
// other are left alone. A lock is read only from a live record.
func fleetRecords(admin, sub *os.Root, root string) recordSet {
	set := recordSet{live: map[string]adminRecord{}, unfinished: map[string]time.Time{}}
	entries, err := fs.ReadDir(admin.FS(), ".")
	if err != nil {
		return set
	}
	roots := []string{pinKey(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	inParent := func(path string, parts ...string) (string, bool) {
		path = filepath.Clean(path)
		for _, r := range roots {
			if filepath.Dir(path) == filepath.Join(append([]string{r}, parts...)...) {
				return filepath.Base(path), true
			}
		}
		return "", false
	}
	for _, e := range entries {
		info, err := admin.Lstat(e.Name())
		if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		raw, ok := smallRegularFile(admin, filepath.Join(e.Name(), "gitdir"), 64<<10)
		if !ok {
			if at, unfinished := unfinishedRecord(admin, e.Name()); unfinished {
				set.unfinished[e.Name()] = at
			}
			continue
		}
		line, _, _ := strings.Cut(raw, "\n")
		line = filepath.Clean(strings.TrimSpace(line))
		if !filepath.IsAbs(line) || filepath.Base(line) != ".git" {
			continue
		}
		name, ok := inParent(filepath.Dir(line), Subdir)
		if !ok {
			continue
		}
		if sub == nil {
			set.stale = append(set.stale, e.Name())
			continue
		}
		ci, err := sub.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			set.stale = append(set.stale, e.Name())
			continue
		}
		if err != nil || ci.Mode()&fs.ModeSymlink != 0 || !ci.IsDir() {
			continue
		}
		gitfile, ok := smallRegularFile(sub, filepath.Join(name, ".git"), 64<<10)
		if !ok {
			continue
		}
		target, found := strings.CutPrefix(strings.TrimSpace(firstLine(gitfile)), "gitdir: ")
		if !found {
			continue
		}
		if id, ok := inParent(target, ".git", "worktrees"); !ok || id != e.Name() {
			continue
		}
		rec := adminRecord{id: e.Name()}
		lockPath := filepath.Join(e.Name(), "locked")
		if _, err := admin.Lstat(lockPath); err == nil {
			rec.isLock = true
			rec.locked = "no reason given"
			if b, ok := smallRegularFile(admin, lockPath, 4<<10); ok && strings.TrimSpace(b) != "" {
				rec.locked = strings.TrimSpace(firstLine(b))
			}
		}
		set.live[name] = rec
	}
	return set
}

// unfinishedRecord reports whether the record id holds nothing but a
// regular `locked` file reading "initializing" — what CreateWorktree has
// written before it writes gitdir — and returns that file's modification
// time.
//
// The record directory may be a run's own, which its sandbox can fill with
// any number of entries, so at most two are read.
func unfinishedRecord(admin *os.Root, id string) (time.Time, bool) {
	record, err := tools.OpenSubdirNoFollow(admin, id)
	if err != nil {
		return time.Time{}, false
	}
	defer record.Close()
	dir, err := record.Open(".")
	if err != nil {
		return time.Time{}, false
	}
	entries, err := dir.ReadDir(2)
	_ = dir.Close()
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) != 1 || entries[0].Name() != "locked" {
		return time.Time{}, false
	}
	info, err := record.Lstat("locked")
	if err != nil || !info.Mode().IsRegular() {
		return time.Time{}, false
	}
	if b, ok := smallRegularFile(record, "locked", 4<<10); !ok || strings.TrimSpace(b) != initializingLock {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

// repoRoots is what openRepoRoots opened, and why something was not.
type repoRoots struct {
	// admin is .git/worktrees, nil unless the repository passed its check
	// and the directory could be opened (adminErr says why not;
	// fs.ErrNotExist when it is absent).
	admin    *os.Root
	adminErr error
	// sub is .fleet-worktrees (subErr says why not; fs.ErrNotExist when it
	// is absent).
	sub    *os.Root
	subErr error
	// noRepo: root has no .git at all, so no record can exist.
	noRepo bool
	// checkErr: root has a .git, but it is not the repository recorded at
	// startup or is laid out in a way fleet does not act on.
	checkErr error
}

func (r repoRoots) close() { closeRoots(r.admin, r.sub) }

// openRepoRoots opens .git/worktrees and .fleet-worktrees, each without
// following a link. The repository is checked with RepoPin.CheckBeforeStart:
// a one-off `fleet worktree prune|list` may run before an upgraded server
// has created the directories it requires at start (logs/, lfs/, …), and
// none of them matters here, where only .git/worktrees and .fleet-worktrees
// are read or touched; a directory that is present must still be a real
// one.
func openRepoRoots(ctx context.Context, root string) repoRoots {
	var r repoRoots
	if _, err := os.Lstat(filepath.Join(root, ".git")); errors.Is(err, fs.ErrNotExist) {
		r.noRepo = true
	}
	r.checkErr = Pin(root).CheckBeforeStart(ctx, root)
	if r.noRepo {
		r.checkErr = nil
	}
	if r.checkErr == nil && !r.noRepo {
		r.admin, r.adminErr = openUnder(ctx, root, filepath.Join(".git", "worktrees"))
	}
	r.sub, r.subErr = openUnder(ctx, root, Subdir)
	return r
}

// checkoutLock reads the checkout .fleet-worktrees/<name>'s .git file and
// the record it names, so a checkout is removed only once its record has
// been read and its lock honoured. It returns the record's lock reason (""
// when unlocked, and for an orphan: a checkout with no .git file, or whose
// record provably does not exist — no repository, no .git/worktrees, no
// such entry), or an error when the .git file or the record cannot be read
// or parsed, or the .git file names something other than a record under
// this repository's .git/worktrees; the checkout is then kept.
//
// The lock is read from the record the checkout names whether or not the
// pair is proven (fleetRecords): honouring it can only keep a checkout.
func checkoutLock(r repoRoots, root, name string) (string, error) {
	gitfile, err := readRegularNoFollow(r.sub, filepath.Join(name, ".git"), 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("its .git cannot be read: %w", err)
	}
	target, found := strings.CutPrefix(strings.TrimSpace(firstLine(string(gitfile))), "gitdir: ")
	if !found {
		return "", errors.New("its .git is not a `gitdir:` file")
	}
	id, ok := recordID(root, target)
	if !ok {
		return "", fmt.Errorf("its .git names %s, not a record under %s", SanitizeForTerminal(target), filepath.Join(root, ".git", "worktrees"))
	}
	switch {
	case r.noRepo, errors.Is(r.adminErr, fs.ErrNotExist):
		return "", nil
	case r.admin == nil:
		return "", fmt.Errorf("its record %s cannot be read: %w", id, errors.Join(r.adminErr, r.checkErr))
	}
	info, err := r.admin.Lstat(id)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("its record %s cannot be read: %w", id, err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("its record %s is not a directory (%s)", id, info.Mode().Type())
	}
	lockPath := filepath.Join(id, "locked")
	if _, err := r.admin.Lstat(lockPath); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("its record %s's lock cannot be read: %w", id, err)
	}
	locked := "no reason given"
	if b, ok := smallRegularFile(r.admin, lockPath, 4<<10); ok && strings.TrimSpace(b) != "" {
		locked = strings.TrimSpace(firstLine(b))
	}
	return locked, nil
}

// recordID returns the record id a checkout's `gitdir:` target names when
// it is an entry directly under root's .git/worktrees (by the root as
// configured or resolved).
func recordID(root, target string) (string, bool) {
	target = filepath.Clean(target)
	roots := []string{pinKey(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	for _, r := range roots {
		if filepath.Dir(target) == filepath.Join(r, ".git", "worktrees") {
			return filepath.Base(target), true
		}
	}
	return "", false
}

func closeRoots(roots ...*os.Root) {
	for _, r := range roots {
		if r != nil {
			_ = r.Close()
		}
	}
}

// ErrLocked is returned (wrapped) by RemoveWorktree for a worktree whose
// proven record is locked.
var ErrLocked = errors.New("worktree is locked")

// ErrRecordNotRemoved is returned (wrapped) by RemoveWorktree when the
// checkout is gone but its record could not be removed. Every other error
// from RemoveWorktree means the checkout may still be there.
var ErrRecordNotRemoved = errors.New("worktree record not removed")

// RemoveWorktree removes the worktree at <root>/.fleet-worktrees/<name> and
// the repository's record of it, without running git. The directory is
// deleted through a Root on .fleet-worktrees and the record through a Root on
// .git/worktrees, both opened without following a symlink at any level. A
// locked worktree is refused (ErrLocked), as `git worktree remove --force`
// refuses it, and nothing is removed. The record goes only once the
// checkout is fully gone: a checkout that cannot be removed completely (a
// directory inside it this process cannot search) keeps its record, and the
// error says so; only a failure to remove the record after the checkout is
// gone is ErrRecordNotRemoved.
// The checkout is removed only once its record has been read
// (checkoutLock): a repository that is present but fails its check, or a
// record that cannot be read, keeps the checkout. The record is removed only
// when the pair is proven to be this worktree's (fleetRecords).
func RemoveWorktree(ctx context.Context, root, name string) error {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("worktree remove: %q is not a worktree directory name", name)
	}
	r := openRepoRoots(ctx, root)
	defer r.close()
	if r.subErr != nil {
		if errors.Is(r.subErr, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("worktree remove: %w", r.subErr)
	}
	path := filepath.Join(root, Subdir, name)
	if r.checkErr != nil {
		return fmt.Errorf("worktree remove: %s kept: %w", path, r.checkErr)
	}
	locked, err := checkoutLock(r, root, name)
	if err != nil {
		return fmt.Errorf("worktree remove: %s kept: %w", path, err)
	}
	if locked != "" {
		return fmt.Errorf("worktree remove: %s: %w (%s)", path, ErrLocked, SanitizeForTerminal(locked))
	}
	var rec adminRecord
	var known bool
	if r.admin != nil {
		rec, known = fleetRecords(r.admin, r.sub, root).live[name]
	}
	if err := r.sub.RemoveAll(name); err != nil {
		return fmt.Errorf("worktree remove: %s was not fully removed, its record is kept: %w", path, err)
	}
	if known {
		if err := r.admin.RemoveAll(rec.id); err != nil {
			return fmt.Errorf("worktree remove: record %s: %w: %w", rec.id, ErrRecordNotRemoved, err)
		}
	}
	return nil
}

// EnsureExcluded appends "<Subdir>/" to .git/info/exclude unless it is
// already listed, so per-run worktrees stay out of the main tree's
// `git status`. .git/info is opened without following a link, and the file
// is read and appended to only as a regular file that is not a link
// (openRegularNoFollow).
func EnsureExcluded(ctx context.Context, root string) error {
	info, err := openUnder(ctx, root, filepath.Join(".git", "info"))
	if err != nil {
		return err
	}
	defer info.Close()
	line := Subdir + "/"
	if data, err := readRegularNoFollow(info, "exclude", 1<<20); err == nil {
		for _, existing := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(existing) == line {
				return nil // already excluded
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := openRegularNoFollow(info, "exclude", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# fleet per-run worktrees (#180)\n%s\n", line)
	return err
}

// CreateWorktree registers a per-run worktree without running git: it writes
// the repository's record .git/worktrees/<name> and the checkout's .git file
// in .fleet-worktrees/<name>, both through directories opened without
// following a link — and both directories are read-only to every sandbox, so
// nothing a sandbox wrote is read or followed. The record is what
// `git worktree add --no-checkout` writes, in the same order: a `locked`
// file first, then gitdir (the checkout's .git), commondir ("../..") and HEAD
// (an unborn ref: refs/heads/<branch>), then the checkout's .git file
// ("gitdir: <record>"), and `locked` is removed once the record is complete,
// as git does (a lock left behind would make the sweep keep the worktree
// forever). The run's sandbox then creates the branch and fills the checkout
// (`git switch --create <branch> <base>`). Paths are written with the
// workspace root as configured, which is the path sandboxes mount it at. It
// returns the record's directory.
//
// A record or checkout directory that already exists under name fails the
// call and is left as it is. On any failure the call removes what it
// created itself, and nothing else.
func CreateWorktree(ctx context.Context, root, name, branch string) (recordDir string, err error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("worktree create: %q is not a worktree directory name", name)
	}
	if err := validBranch(branch); err != nil {
		return "", fmt.Errorf("worktree create: %w", err)
	}
	if err := Pin(root).Check(ctx, root); err != nil {
		return "", err
	}
	admin, err := openUnder(ctx, root, filepath.Join(".git", "worktrees"))
	if err != nil {
		return "", err
	}
	defer admin.Close()
	sub, err := openUnder(ctx, root, Subdir)
	if err != nil {
		return "", err
	}
	defer sub.Close()
	base := pinKey(root)
	recordPath := filepath.Join(base, ".git", "worktrees", name)
	checkoutPath := filepath.Join(base, Subdir, name)

	if err := admin.Mkdir(name, 0o755); err != nil {
		return "", fmt.Errorf("worktree create: record %s: %w", recordPath, err)
	}
	madeCheckout := false
	defer func() {
		if err == nil {
			return
		}
		if madeCheckout {
			err = errors.Join(err, sub.RemoveAll(name))
		}
		err = errors.Join(err, admin.RemoveAll(name))
	}()
	record, err := tools.OpenSubdirNoFollow(admin, name)
	if err != nil {
		return "", err
	}
	defer record.Close()
	writeNew := func(dir *os.Root, file, content string) error {
		f, err := dir.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(content)
		cerr := f.Close()
		return errors.Join(werr, cerr)
	}
	if err := writeNew(record, "locked", "initializing\n"); err != nil {
		return "", err
	}
	// 0o755 so the rootless-podman container user can traverse into it.
	if err := sub.Mkdir(name, 0o755); err != nil {
		return "", fmt.Errorf("worktree create: checkout %s: %w", checkoutPath, err)
	}
	madeCheckout = true
	checkout, err := tools.OpenSubdirNoFollow(sub, name)
	if err != nil {
		return "", err
	}
	defer checkout.Close()
	for _, w := range []struct {
		dir           *os.Root
		file, content string
	}{
		{record, "gitdir", filepath.Join(checkoutPath, ".git") + "\n"},
		{record, "commondir", "../..\n"},
		{record, "HEAD", "ref: refs/heads/" + branch + "\n"},
		{checkout, ".git", "gitdir: " + recordPath + "\n"},
	} {
		if err := writeNew(w.dir, w.file, w.content); err != nil {
			return "", fmt.Errorf("worktree create: %s: %w", w.file, err)
		}
	}
	if err := record.Remove("locked"); err != nil {
		return "", err
	}
	return recordPath, nil
}

// validBranch accepts a branch name that can be written into a record's HEAD
// as one line: no control characters, no "..", not starting with "-" or "/".
// git checks the name fully when the run's sandbox creates the branch.
func validBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.Contains(branch, "..") {
		return fmt.Errorf("%q is not a usable branch name", branch)
	}
	for _, r := range branch {
		if unicode.IsControl(r) || r == ' ' {
			return fmt.Errorf("%q is not a usable branch name", branch)
		}
	}
	return nil
}

// RemoveRecord removes the record .git/worktrees/<id> a run's own setup
// created, whatever is inside it now (the run's sandbox could write it), by a
// no-follow RemoveAll through a Root on .git/worktrees. It needs no proof:
// the caller is the code that created the record and knows its id. Like
// every other action on the repository, it runs only while root's .git is
// the one recorded at startup (RepoPin.Check).
func RemoveRecord(ctx context.Context, root, id string) error {
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return fmt.Errorf("worktree record: %q is not a record name", id)
	}
	if err := Pin(root).Check(ctx, root); err != nil {
		return err
	}
	admin, err := openUnder(ctx, root, filepath.Join(".git", "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer admin.Close()
	return admin.RemoveAll(id)
}

// Entry is one per-run worktree as `fleet worktree list` shows it.
type Entry struct {
	Name     string // directory name under .fleet-worktrees
	Path     string // absolute checkout path
	RecordID string // its proven record under .git/worktrees ("" when none)
	Locked   string // the record's lock reason ("" when not locked)
}

// List returns the per-run worktrees under .fleet-worktrees with what
// fleetRecords proves about their records, without running git. Lock reasons
// come from records a run can write; callers display them with
// SanitizeForTerminal.
func List(ctx context.Context, root string) ([]Entry, error) {
	r := openRepoRoots(ctx, root)
	defer r.close()
	admin, sub, subErr := r.admin, r.sub, r.subErr
	if errors.Is(subErr, fs.ErrNotExist) {
		return nil, nil
	}
	if subErr != nil {
		return nil, subErr
	}
	var records recordSet
	if admin != nil {
		records = fleetRecords(admin, sub, root)
	}
	entries, err := fs.ReadDir(sub.FS(), ".")
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range entries {
		info, err := sub.Lstat(e.Name())
		if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		en := Entry{Name: e.Name(), Path: filepath.Join(root, Subdir, e.Name())}
		if rec, ok := records.live[e.Name()]; ok {
			en.RecordID = rec.id
			if rec.isLock {
				en.Locked = rec.locked
			}
		}
		out = append(out, en)
	}
	return out, nil
}

// SanitizeForTerminal replaces every control character (escape sequences
// included) and invalid UTF-8 in s with '?', so text a run could write is
// printed as text.
func SanitizeForTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// ---------------------------------------------------------------------------
// The sweep.

// reportedRoots remembers the roots whose unpinned .git the sweep has already
// reported, so the hourly pass says it once per process, not every hour.
var reportedRoots sync.Map

// PruneStale reclaims orphaned per-run worktrees under root, without running
// git, in steps that target DIFFERENT things:
//
//  1. Records in .git/worktrees for fleet's worktrees whose directory is
//     already gone are removed. This frees no disk; it stops the metadata
//     from accumulating. Only records whose gitdir names a directory under
//     .fleet-worktrees are considered — anything else in .git/worktrees is
//     not fleet's.
//  2. <root>/.fleet-worktrees/* directories older than olderThan are removed
//     — the part that actually frees disk — but only once the record each
//     checkout's .git file names has been read (checkoutLock). A lock in
//     that record keeps the checkout, whatever its age (a lock is the one
//     explicit "in use" signal git offers), except the "initializing" lock
//     CreateWorktree removes once the record is complete: on a checkout this
//     old it was left by a server stopped part-way. A .git file or record
//     that cannot be read keeps the checkout too. A checkout with no .git
//     file, or whose record provably does not exist, is an orphan and goes.
//     A record proven to be the checkout's own (the checkout's .git and the
//     record's gitdir name each other) is removed together with it.
//  3. A record that holds nothing but an "initializing" lock older than
//     olderThan — a CreateWorktree stopped before it wrote gitdir — is
//     removed.
//
// Nothing at all is removed while root has a .git that is not the
// repository pinned at startup, or is laid out in a way fleet does not act
// on (RepoPin.CheckBeforeStart, which lets the directories the server
// creates at start still be missing, so a one-off `fleet worktree prune`
// before an upgraded server's first start still reads records and locks); a
// .git that appeared or changed since is reported once (a Warning on the
// first pass) and left alone until a restart. When root has no .git at
// all, no record can exist and aged checkouts are orphans.
//
// Every deletion goes through an os.Root on .fleet-worktrees or on
// .git/worktrees, each opened without following a symlink at any level
// (tools.OpenWorkspaceDir), and entries that are symlinks are skipped:
// whatever a sandbox planted, the sweep deletes only inside those two
// directories.
//
// dryRun reports what would be removed and deletes nothing.
//
// A missing worktree directory is "nothing to prune", not an error: most boxes
// never enable worktree isolation. One that is a symlink or not a directory is
// an error and nothing is swept. olderThan <= 0 is replaced with
// DefaultPruneAge, and any positive value below MinPruneAge is raised to it
// (with a Warning saying so), so neither a zero-valued caller nor a too-small
// operator flag can delete a live run's checkout.
func PruneStale(ctx context.Context, root string, olderThan time.Duration, dryRun bool) (Result, error) {
	var res Result
	if strings.TrimSpace(root) == "" {
		return res, fmt.Errorf("worktree prune: empty workspace root")
	}
	if olderThan <= 0 {
		olderThan = DefaultPruneAge
	}
	if olderThan < MinPruneAge {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("age %s is below the %s floor (the default task wall-clock ceiling); using %s so a running task's checkout cannot be reclaimed", olderThan, MinPruneAge, MinPruneAge))
		olderThan = MinPruneAge
	}

	r := openRepoRoots(ctx, root)
	defer r.close()
	admin, sub, subErr := r.admin, r.sub, r.subErr
	if r.checkErr != nil {
		// A .git is there but it is not the recorded repository, or not one
		// fleet acts on: no checkout is removed without its record read, so
		// nothing is removed. Say so once per process.
		if _, seen := reportedRoots.LoadOrStore(pinKey(root), true); !seen {
			res.Warnings = append(res.Warnings, fmt.Sprintf("worktree checkouts under %s and records under %s left alone: %v", filepath.Join(root, Subdir), filepath.Join(root, ".git"), r.checkErr))
		}
		return res, nil
	}
	if subErr != nil && !errors.Is(subErr, fs.ErrNotExist) {
		return res, fmt.Errorf("worktree prune: %w", subErr)
	}
	cutoff := time.Now().Add(-olderThan)
	var records recordSet
	if admin != nil {
		records = fleetRecords(admin, sub, root)
	}
	stale := records.stale
	for id, at := range records.unfinished {
		if at.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	if sub == nil {
		if !dryRun && admin != nil {
			pruneRecords(admin, stale, &res)
		}
		return res, nil
	}
	parent := filepath.Join(root, Subdir)
	entries, err := fs.ReadDir(sub.FS(), ".")
	if err != nil {
		return res, fmt.Errorf("read %s: %w", parent, err)
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		path := filepath.Join(parent, e.Name())
		info, statErr := sub.Lstat(e.Name())
		if statErr != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("stat %s: %v", path, statErr))
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		if info.ModTime().After(cutoff) {
			res.Kept++
			continue
		}
		// The checkout goes only once its record has been read: a lock
		// (other than the initializing one, which counts for nothing on a
		// checkout this old) keeps it, and so does a .git file or record
		// that cannot be read.
		locked, recErr := checkoutLock(r, root, e.Name())
		if recErr != nil {
			res.Kept++
			res.Warnings = append(res.Warnings, fmt.Sprintf("kept %s: %v", path, recErr))
			continue
		}
		if locked != "" && locked != initializingLock {
			res.Kept++
			res.Warnings = append(res.Warnings, fmt.Sprintf("kept %s: locked (%s)", path, SanitizeForTerminal(locked)))
			continue
		}
		rec, known := records.live[e.Name()]
		if dryRun {
			res.Removed = append(res.Removed, path)
			continue
		}
		if rmErr := sub.RemoveAll(e.Name()); rmErr != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("remove %s: %v", path, rmErr))
			continue
		}
		if known {
			stale = append(stale, rec.id)
		}
		res.Removed = append(res.Removed, path)
	}
	if !dryRun && admin != nil && ctx.Err() == nil {
		pruneRecords(admin, stale, &res)
	}
	return res, nil
}

// pruneRecords removes the given records from .git/worktrees.
func pruneRecords(admin *os.Root, ids []string, res *Result) {
	for _, id := range ids {
		if err := admin.RemoveAll(id); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("remove worktree record %s: %v", id, err))
		}
	}
}

// ResolveWorkspaceRoot mirrors how the running server resolves the workspace
// root (internal/agent/manager.go): an explicit value wins, else
// FLEET_WORKSPACE_ROOT, else ./workspace.
//
// Shared so the CLI's `--workspace` flag and the server's in-process sweep can
// never disagree about which tree they are reclaiming.
func ResolveWorkspaceRoot(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := envreg.Get("FLEET_WORKSPACE_ROOT"); v != "" {
		return v
	}
	if abs, err := filepath.Abs("workspace"); err == nil {
		return abs
	}
	return "workspace"
}
