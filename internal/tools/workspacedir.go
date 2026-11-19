package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// Opening a directory inside the workspace tree without following a symbolic
// link.
//
// Everything under the workspace root is writable by a sandbox that mounts the
// whole root — a chat sandbox in the default per-turn mode (ADR-0058) — and
// that sandbox's user owns the tree (--userns=keep-id), so any directory under
// the root, a conversation's included, can be renamed and replaced with a
// symbolic link while host code works in it.
//
// The rule: host-side code never opens a directory under the workspace root by
// path. OpenWorkspaceDir opens the root (operator configuration, so a
// symlinked root is followed) and then every directory below it one name at a
// time: the name is Lstat'ed (a symbolic link, or anything that is not a
// directory, is refused), opened as a directory (so a FIFO swapped in fails at
// once instead of blocking), and the opened directory must be the very one
// that was Lstat'ed (os.SameFile), so a link swapped in between the check and
// the open is refused too. The returned WorkspaceDir holds a descriptor for
// that directory; every later read, write, listing, mkdir, chmod, copy or
// removal goes through it (os.Root, *at calls), so a link swapped in after the
// open is not followed either — the work stays in the directory that was
// opened.
//
// Below the opened directory os.Root still follows a symbolic link that stays
// inside it (an agent's own `latest -> run-3/` link keeps working) and refuses
// one that leaves it. Resolve and OpenNoFollow are the two stricter tools for
// callers that need the canonical name (an owner-private subtree check) or
// must refuse every link (files served to someone other than the owner).
//
// The structural symlinks EnsureWorkspaceDir plants in a chat workspace
// (protocols/, personas/, system_prompts/, skills/, shared/ → absolute config
// dirs) lead out of the directory, so a request that traverses one is refused:
// those are agent plumbing, not user artifacts.

// ErrWorkspaceDirLink is returned when a directory being opened under the
// workspace root — a conversation's directory, or any directory between the
// root and it — is a symbolic link or not a directory. It is never followed.
var ErrWorkspaceDirLink = errors.New("workspace directory is a symbolic link or not a directory (not followed)")

// ErrNotConversationID is returned for a conversation id that cannot name a
// single directory directly under the workspace root.
var ErrNotConversationID = errors.New("conversation id is not usable as a workspace directory name")

// WorkspaceDir is a directory under the workspace root, opened by
// OpenWorkspaceDir. The embedded os.Root anchors every operation to the
// directory that was opened; Close releases it.
type WorkspaceDir struct {
	*os.Root
	// path is the directory's absolute host path with the workspace root as
	// configured; real is the same with the root's own symlinks resolved ("" if
	// the root could not be resolved). Both name the directory that was opened
	// at the time it was opened — they are for messages, prompts and the
	// sandbox, and for recognising an absolute path a model wrote. Nothing on
	// the host is opened by them.
	path string
	real string
}

// Path returns the absolute host path the directory was opened at. Use it for
// messages and for paths handed to the sandbox; open nothing by it.
func (d *WorkspaceDir) Path() string { return d.path }

// RealPath is Path with the workspace root's own symlinks resolved (Path when
// they could not be). For display only, like Path.
func (d *WorkspaceDir) RealPath() string {
	if d.real != "" {
		return d.real
	}
	return d.path
}

// RelOf converts an absolute host path to a path relative to d. It accepts
// either spelling of d's path (the workspace root as configured, or with its
// symlinks resolved) and reports false for a path outside d. Lexical only.
func (d *WorkspaceDir) RelOf(abs string) (string, bool) {
	abs = filepath.Clean(abs)
	for _, base := range []string{d.path, d.real} {
		if base == "" {
			continue
		}
		if rel, err := filepath.Rel(base, abs); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return rel, true
		}
	}
	return "", false
}

// OpenWorkspaceDir opens dir — workspaceRoot itself, or a directory beneath it
// — without following a symbolic link at any level below workspaceRoot. A link
// or a non-directory at any of those levels fails with ErrWorkspaceDirLink, and
// the refusal is logged (with ctx's correlation; see logWorkspaceRefusal). A
// missing level fails with an fs.ErrNotExist error.
//
// A dir that is not under workspaceRoot at all is refused: callers that hold a
// directory no sandbox mounts use OpenHostDir instead.
func OpenWorkspaceDir(ctx context.Context, workspaceRoot, dir string) (*WorkspaceDir, error) {
	return openWorkspaceDir(ctx, workspaceRoot, dir, false, 0)
}

// OpenDirNoFollow opens dir without following a symbolic link at its own name:
// its parent is opened as given (followed), and dir itself must be a real
// directory there. It is OpenWorkspaceDir with dir's parent as the root, for
// a dir that sits directly under the workspace root — a conversation's
// directory — when the caller holds the path rather than the id.
func OpenDirNoFollow(ctx context.Context, dir string) (*WorkspaceDir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return OpenWorkspaceDir(ctx, filepath.Dir(abs), abs)
}

// OpenHostDir opens a directory that no sandbox can write — the host-only
// private stash, the read-only shared library — as a WorkspaceDir, so code
// that handles both kinds goes through one type. dir itself is followed if it
// is a symlink; never use it for anything under a sandbox's writable mount.
func OpenHostDir(dir string) (*WorkspaceDir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	resolvedPath := ""
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		resolvedPath = resolved
	}
	return &WorkspaceDir{Root: r, path: abs, real: resolvedPath}, nil
}

// OpenConversationDir opens a conversation's workspace directory
// (<workspace root>/<conversationID>) without following a symbolic link. A
// directory that does not exist yet fails with an fs.ErrNotExist error.
func OpenConversationDir(ctx context.Context, conversationID string) (*WorkspaceDir, error) {
	return OpenConversationDirIn(ctx, workspaceRootForContainment(), conversationID)
}

// OpenConversationDirIn is OpenConversationDir under an explicit workspace
// root.
func OpenConversationDirIn(ctx context.Context, workspaceRoot, conversationID string) (*WorkspaceDir, error) {
	if !isWorkspaceSegment(conversationID) {
		return nil, ErrNotConversationID
	}
	return OpenWorkspaceDir(ctx, workspaceRoot, filepath.Join(workspaceRoot, conversationID))
}

// EnsureConversationDir creates a conversation's workspace directory if it is
// missing, opens it without following a symbolic link, makes it 0o755 (see
// EnsureWorkspaceDir) through the opened descriptor, and plants the
// supporting-doc symlinks inside it. The caller closes the result.
func EnsureConversationDir(ctx context.Context, conversationID string) (*WorkspaceDir, error) {
	if !isWorkspaceSegment(conversationID) {
		return nil, ErrNotConversationID
	}
	root := workspaceRootForContainment()
	// 0o755: see EnsureWorkspaceDir for why the sandbox user must traverse it.
	d, err := openWorkspaceDir(ctx, root, filepath.Join(root, conversationID), true, 0o755)
	if err != nil {
		return nil, err
	}
	// Older versions created the directory 0o750; re-assert 0o755 on the
	// opened directory itself (fchmod), never on a path that could have been
	// replaced since.
	_ = d.chmodSelf(0o755)
	seedSupportingDocSymlinksIn(d.Root, d.path)
	return d, nil
}

// chmodSelf changes the mode of the opened directory itself.
func (d *WorkspaceDir) chmodSelf(mode fs.FileMode) error {
	f, err := d.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}

// isWorkspaceSegment reports whether name can be one directory name directly
// under the workspace root.
func isWorkspaceSegment(name string) bool {
	return name != "" && name != "." && filepath.IsLocal(name) && !strings.ContainsAny(name, `/\`)
}

func openWorkspaceDir(ctx context.Context, workspaceRoot, dir string, create bool, perm fs.FileMode) (*WorkspaceDir, error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, errors.New("open workspace directory: no workspace root")
	}
	rootAbs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, err
	}
	rootAbs = filepath.Clean(rootAbs)
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dirAbs = filepath.Clean(dirAbs)
	rel, err := filepath.Rel(rootAbs, dirAbs)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return nil, fmt.Errorf("open workspace directory: %s is not inside the workspace root %s", dirAbs, rootAbs)
	}
	if create {
		// The root itself is operator configuration, not a sandbox-writable
		// name (sandboxes mount its contents), so MkdirAll is fine for it;
		// every level below is created one name at a time.
		if err := os.MkdirAll(rootAbs, perm); err != nil {
			return nil, err
		}
	}
	cur, err := os.OpenRoot(rootAbs)
	if err != nil {
		return nil, err
	}
	if rel != "." {
		walked := rootAbs
		for _, name := range strings.Split(rel, string(filepath.Separator)) {
			walked = filepath.Join(walked, name)
			next, err := openChildNoFollow(cur, name, create, perm)
			_ = cur.Close()
			if err != nil {
				if errors.Is(err, ErrWorkspaceDirLink) {
					logWorkspaceRefusal(ctx, walked, dirAbs)
				}
				return nil, &fs.PathError{Op: "open workspace directory", Path: walked, Err: err}
			}
			cur = next
		}
	}
	resolvedPath := ""
	if resolved, rerr := filepath.EvalSymlinks(rootAbs); rerr == nil {
		resolvedPath = filepath.Join(resolved, rel)
	}
	return &WorkspaceDir{Root: cur, path: dirAbs, real: resolvedPath}, nil
}

// refusalLogEvery bounds how often the same refused directory is logged at
// Warn: a page polling a conversation whose directory is a link would
// otherwise log one Warn per request. Repeats inside the window log at Debug.
const refusalLogEvery = 10 * time.Minute

var refusalLogged sync.Map // refused path → time.Time of its last Warn

// logWorkspaceRefusal logs a directory refused by openWorkspaceDir with ctx's
// correlation (run, turn or request ids), at Warn the first time per path in
// refusalLogEvery and at Debug for repeats.
func logWorkspaceRefusal(ctx context.Context, path, opening string) {
	now := time.Now()
	level := slog.LevelWarn
	if last, ok := refusalLogged.Load(path); ok && now.Sub(last.(time.Time)) < refusalLogEvery {
		level = slog.LevelDebug
	} else {
		refusalLogged.Store(path, now)
	}
	logging.From(ctx).Log(ctx, level, "workspace: refused a directory that is a symbolic link or not a directory; it was not followed",
		"path", safe.LogSafe(path), "opening", safe.LogSafe(opening))
}

// openChildNoFollow opens the directory name inside parent, refusing a
// symbolic link or a non-directory, and proves the directory opened is the
// one that was checked. With create, a missing name is made first (Mkdir
// never follows a link at the name it creates).
//
// The open goes through "<name>/." rather than "<name>": os.Root opens every
// component but the last with O_DIRECTORY, so name itself is opened as a
// directory — a FIFO swapped in for it fails at once with ENOTDIR instead of
// blocking forever with no writer (Root.OpenRoot(name) opens the last
// component without O_DIRECTORY or O_NONBLOCK) — and "." is then opened
// relative to that descriptor, never by a second lookup of name. A symbolic
// link swapped in is still followed by os.Root if it stays inside parent;
// the os.SameFile check against the Lstat is what refuses it.
func openChildNoFollow(parent *os.Root, name string, create bool, perm fs.FileMode) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	want, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !want.IsDir() { // a symlink's Lstat mode is ModeSymlink, never a directory
		return nil, ErrWorkspaceDirLink
	}
	if openChildAfterLstat != nil {
		openChildAfterLstat(name)
	}
	child, err := parent.OpenRoot(name + string(filepath.Separator) + ".")
	if err != nil {
		// Replaced since the Lstat — by a link os.Root refused to follow out
		// of parent, or by something that is not a directory (a FIFO, a
		// file): report it as the refusal it is.
		if now, lerr := parent.Lstat(name); lerr == nil && !now.IsDir() {
			return nil, ErrWorkspaceDirLink
		}
		return nil, err
	}
	got, err := child.Stat(".")
	if err != nil || !os.SameFile(want, got) {
		_ = child.Close()
		return nil, ErrWorkspaceDirLink
	}
	return child, nil
}

// openChildAfterLstat is a test seam run between a directory's Lstat and its
// open in openChildNoFollow; nil in production.
var openChildAfterLstat func(name string)

// OpenSubdirNoFollow opens the directory name (one path component) inside
// parent the way OpenWorkspaceDir opens each level: a symbolic link or a
// non-directory is refused with ErrWorkspaceDirLink, a FIFO cannot block the
// open, and the directory opened is the one that was checked.
func OpenSubdirNoFollow(parent *os.Root, name string) (*os.Root, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, ErrUnsafePath
	}
	return openChildNoFollow(parent, name, false, 0)
}

// WalkNoFollowFunc is called by WalkNoFollow for each entry below the root:
// rel is the entry's path relative to the root, d its DirEntry and parent the
// opened directory holding it. As with fs.WalkDirFunc, a directory that
// cannot be opened or listed is reported by a second call with err set (rel
// "." and a nil d for the root itself); returning nil then skips it.
// Returning fs.SkipDir for a directory skips its contents, fs.SkipAll stops
// the walk, and any other error stops it and is returned.
type WalkNoFollowFunc func(rel string, d fs.DirEntry, parent *os.Root, err error) error

// WalkNoFollow walks the tree under root. Each directory is listed with
// (*os.File).ReadDir on a descriptor opened through its parent
// (OpenSubdirNoFollow), so a symbolic link is never descended, nothing is
// looked up again from the top, and names that are not valid UTF-8 are walked
// like any other (Root.FS, which fs.WalkDir needs, refuses them).
func WalkNoFollow(root *os.Root, fn WalkNoFollowFunc) error {
	err := walkNoFollow(root, ".", nil, root, fn)
	if errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

func walkNoFollow(dir *os.Root, rel string, d fs.DirEntry, parent *os.Root, fn WalkNoFollowFunc) error {
	f, err := dir.Open(".")
	var entries []fs.DirEntry
	if err == nil {
		entries, err = f.ReadDir(-1)
		_ = f.Close()
	}
	if err != nil {
		return fn(rel, d, parent, err)
	}
	for _, e := range entries {
		childRel := e.Name()
		if rel != "." {
			childRel = filepath.Join(rel, e.Name())
		}
		if err := fn(childRel, e, dir, nil); err != nil {
			if errors.Is(err, fs.SkipDir) && e.IsDir() {
				continue
			}
			return err
		}
		if !e.IsDir() {
			continue
		}
		sub, err := OpenSubdirNoFollow(dir, e.Name())
		if err != nil {
			if err := fn(childRel, e, dir, err); err != nil {
				return err
			}
			continue
		}
		err = walkNoFollow(sub, childRel, e, dir, fn)
		_ = sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ErrUnsafePath is returned when a requested relative path fails the syntactic
// guard (absolute, contains "..", or contains a NUL byte). Callers map this to
// HTTP 400.
var ErrUnsafePath = errors.New("invalid path")

// ErrPathEscapesWorkspace is returned when a path resolves (after symlink
// evaluation) to a location outside the directory it was resolved in. Callers
// map this to HTTP 400 as well — it names somewhere else, not a missing file.
var ErrPathEscapesWorkspace = errors.New("path escapes workspace")

// outsideError is ErrPathEscapesWorkspace carrying the absolute path a symbolic
// link led to, for a caller with a second permitted directory (the shared
// library a conversation's `shared` link points at).
type outsideError struct{ target string }

func (e *outsideError) Error() string        { return ErrPathEscapesWorkspace.Error() }
func (e *outsideError) Is(target error) bool { return target == ErrPathEscapesWorkspace }

// OutsideTarget returns the absolute path a resolution that failed with
// ErrPathEscapesWorkspace was led to by a symbolic link, or "" when there is
// none. Lexical: nothing at that path has been opened.
func OutsideTarget(err error) string {
	var e *outsideError
	if errors.As(err, &e) {
		return e.target
	}
	return ""
}

// CheckRelativePath is the syntactic half of the workspace path guard: rel
// must be non-empty, relative, free of NUL bytes and of any ".." component.
// filepath.IsLocal is the load-bearing lexical gate (it is also the barrier
// CodeQL's path-injection query recognizes); the ".." scan is stricter than
// IsLocal, which permits an interior "a/../b" — a workspace file reference
// never legitimately carries "..". ".." is matched as a component, not a
// substring, so "my..report.csv" passes.
func CheckRelativePath(rel string) error {
	if strings.ContainsRune(rel, 0) || !filepath.IsLocal(rel) {
		return ErrUnsafePath
	}
	for _, comp := range strings.Split(filepath.ToSlash(rel), "/") {
		if comp == ".." {
			return ErrUnsafePath
		}
	}
	return nil
}

// maxResolveLinks bounds the symbolic links one Resolve follows, like the
// kernel's own limit.
const maxResolveLinks = 40

// Resolve validates rel (CheckRelativePath) and resolves it inside d, through
// d's descriptor, following only symbolic links that stay inside d. It returns
// the canonical relative path — no symbolic link in any component at the time
// it was resolved, "." for d itself — for an owner-private subtree check and
// for OpenNoFollow, which refuses the open if anything along it has become a
// link since.
//
// A relative link target is resolved against the link's own directory; an
// absolute one is honoured only when it names a path inside d by either
// spelling of d's host path (sandboxes see the workspace at the same absolute
// path as the host, so such links are ordinary). A link that leads out of d
// fails with ErrPathEscapesWorkspace (OutsideTarget says where it led); a
// missing component fails with an fs.ErrNotExist error.
func (d *WorkspaceDir) Resolve(rel string) (string, error) {
	if err := CheckRelativePath(rel); err != nil {
		return "", err
	}
	var done []string
	pending := splitPath(rel)
	links := 0
	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			if len(done) == 0 {
				return "", &outsideError{target: filepath.Join(append([]string{filepath.Dir(d.path)}, pending...)...)}
			}
			done = done[:len(done)-1]
			continue
		}
		cand := filepath.Join(append(append([]string{}, done...), comp)...)
		info, err := d.Lstat(cand)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			done = append(done, comp)
			continue
		}
		links++
		if links > maxResolveLinks {
			return "", &fs.PathError{Op: "resolve", Path: rel, Err: syscall.ELOOP}
		}
		target, err := d.Readlink(cand)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			inside, ok := d.RelOf(target)
			if !ok {
				return "", &outsideError{target: filepath.Join(append([]string{filepath.Clean(target)}, pending...)...)}
			}
			done = nil
			pending = append(splitPath(inside), pending...)
			continue
		}
		pending = append(splitPath(target), pending...)
	}
	if len(done) == 0 {
		return ".", nil
	}
	return filepath.Join(done...), nil
}

func splitPath(p string) []string {
	return strings.Split(filepath.ToSlash(p), "/")
}

// ErrNotRegularFile is returned by OpenNoFollow for anything other than a
// regular file (a directory, FIFO, socket or device).
var ErrNotRegularFile = errors.New("not a regular file")

// errLinkInPath is OpenNoFollow's refusal of a symbolic link (or a directory
// swapped) along the path.
var errLinkInPath = errors.New("a symbolic link or a changed directory along the path")

// OpenNoFollow opens rel inside root for reading, refusing a symbolic link at
// ANY component, not just one that leaves root. Each directory is Lstat'ed
// (must be a real directory), opened, and proven to be the SAME directory that
// was Lstat'ed; the leaf is Lstat'ed (must be a regular file), opened
// non-blocking, and proven to be that same file. os.Root follows a symbolic
// link that stays inside root even when O_NOFOLLOW is passed (it resolves the
// link itself on ELOOP), so the identity check, not the flag, is what refuses
// a link swapped in between check and open; a FIFO swapped in for the leaf
// cannot hang the caller. For a directory or
// another non-regular leaf the error is ErrNotRegularFile and info describes
// it.
//
// rel must be a canonical relative path (Resolve's result, or a path the
// caller requires to be literal): "." and ".." components are refused.
func OpenNoFollow(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	segs := splitPath(rel)
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, nil, ErrUnsafePath
		}
	}
	roots := []*os.Root{}
	defer func() {
		for _, r := range roots {
			_ = r.Close()
		}
	}()
	cur := root
	for _, dir := range segs[:len(segs)-1] {
		next, err := openChildNoFollow(cur, dir, false, 0)
		if err != nil {
			if errors.Is(err, ErrWorkspaceDirLink) {
				return nil, nil, errLinkInPath
			}
			return nil, nil, err
		}
		roots = append(roots, next)
		cur = next
	}
	leaf := segs[len(segs)-1]
	want, err := cur.Lstat(leaf)
	if err != nil {
		return nil, nil, err
	}
	if want.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, errLinkInPath
	}
	if !want.Mode().IsRegular() {
		return nil, want, ErrNotRegularFile
	}
	if openNoFollowAfterLstat != nil {
		openNoFollowAfterLstat(rel)
	}
	// O_NONBLOCK: the leaf was a regular file when Lstat'ed, but a sandbox can
	// swap it for a FIFO before the open, and a blocking open of a FIFO with
	// no writer never returns. Non-blocking, the open returns at once and the
	// identity check below refuses it. For a regular file it changes nothing.
	// (No O_NOFOLLOW: os.Root would follow an in-root link regardless; the
	// SameFile check below refuses one.)
	f, err := cur.OpenFile(leaf, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	got, err := f.Stat()
	if err != nil || !got.Mode().IsRegular() || !os.SameFile(want, got) {
		_ = f.Close()
		return nil, nil, errLinkInPath
	}
	return f, got, nil
}

// openNoFollowAfterLstat is a test seam run between the leaf's Lstat and its
// open in OpenNoFollow; nil in production.
var openNoFollowAfterLstat func(rel string)

// OpenResolved resolves rel inside d (Resolve) and opens the result with
// OpenNoFollow, returning the file, its descriptor's FileInfo and the
// canonical relative path. It is the read path for a file named by a request
// or a model: links inside d still work, nothing outside d is reachable, and
// nothing that changes between the resolution and the open is followed.
func (d *WorkspaceDir) OpenResolved(rel string) (*os.File, fs.FileInfo, string, error) {
	canon, err := d.Resolve(rel)
	if err != nil {
		return nil, nil, "", err
	}
	if canon == "." {
		info, serr := d.Stat(".")
		if serr != nil {
			return nil, nil, canon, serr
		}
		return nil, info, canon, ErrNotRegularFile
	}
	f, info, err := OpenNoFollow(d.Root, canon)
	return f, info, canon, err
}
