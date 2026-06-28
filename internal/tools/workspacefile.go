package tools

import (
	"errors"
	"path/filepath"
	"strings"
)

// Path-safety guard for serving files OUT of a fixed workspace root over HTTP.
//
// This is the single, shared implementation of the "a request must not escape a
// workspace directory" guard. It was first proven inline in the chat workspace
// file handler (internal/httpapi handleWorkspaceFile) and is reused verbatim by
// the scheduled-task workspace browser (#287) so there is exactly one
// path-traversal defence to audit, not two divergent copies.
//
// The guard is deliberately layered — a fast syntactic reject is NOT enough on
// its own, because a symlink the agent (or a malicious upload) plants inside the
// workspace can point outside it without any ".." in the request path. The
// load-bearing safety net is the EvalSymlinks + prefix check.
//
//  1. Syntactic reject (ErrUnsafePath): the relative path must not be absolute,
//     must not contain a ".." component, and must not contain a NUL byte. This
//     gives a clean 400 before we ever touch the filesystem.
//  2. Symlink resolution + containment: the joined path is resolved with
//     filepath.EvalSymlinks and the result must still live UNDER the (also
//     symlink-resolved) workspace root. This rejects symlink escapes — e.g. a
//     `ln -s /etc/passwd report` planted in the workspace — which step 1 cannot
//     see. A non-existent path resolves to ErrNotExist.
//
// Note the structural symlinks EnsureWorkspaceDir plants in a chat workspace
// (protocols/, personas/, system_prompts/, skills/ → absolute config dirs) are
// correctly NON-followable through this guard: they resolve outside the
// workspace root, so a request that traverses one is rejected as an escape. That
// is the intended behaviour — those are agent plumbing, not user artifacts.

// ErrUnsafePath is returned when a requested relative path fails the syntactic
// guard (absolute, contains "..", or contains a NUL byte). Callers map this to
// HTTP 400.
var ErrUnsafePath = errors.New("invalid path")

// ErrPathEscapesWorkspace is returned when a path resolves (after symlink
// evaluation) to a location outside the workspace root. Callers map this to HTTP
// 400 as well — it is an attempted escape, not a missing file.
var ErrPathEscapesWorkspace = errors.New("path escapes workspace")

// SafeWorkspaceJoin validates relPath against root and returns the resolved,
// symlink-evaluated absolute path that is guaranteed to live under root.
//
// root must be an absolute path that already exists (it is the workspace dir
// itself). relPath is a slash-or-OS-separated path RELATIVE to root, exactly as
// it arrives from a URL segment (already percent-decoded by the caller).
//
// On success the returned path is safe to open. Errors:
//   - ErrUnsafePath           — syntactic violation (absolute / ".." / NUL).
//   - ErrPathEscapesWorkspace — symlink escape (resolved outside root).
//   - any os error (e.g. fs.ErrNotExist) from EvalSymlinks on the joined path.
//
// The caller is responsible for the authorization decision (who may read this
// workspace at all) BEFORE calling this — SafeWorkspaceJoin only enforces the
// filesystem containment invariant.
func SafeWorkspaceJoin(root, relPath string) (string, error) {
	// 1. Fast syntactic reject. A leading "/" (absolute), any ".." component, or
	//    a NUL byte is rejected before we touch the filesystem. We check ".." as a
	//    path component (not a substring) so a legitimate filename like
	//    "my..report.csv" is not falsely rejected, while "../" and a bare ".."
	//    are.
	if relPath == "" {
		return "", ErrUnsafePath
	}
	if strings.ContainsRune(relPath, 0) {
		return "", ErrUnsafePath
	}
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") {
		return "", ErrUnsafePath
	}
	// Normalize separators and split into components; reject any "..".
	for _, comp := range strings.Split(filepath.ToSlash(relPath), "/") {
		if comp == ".." {
			return "", ErrUnsafePath
		}
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Resolve the workspace root's own symlinks first so the containment compare
	// below is between two fully-resolved paths (matters where the workspace root
	// is itself a symlink, e.g. /opt/fleet/workspace → /var/lib/fleet/workspace).
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}

	full := filepath.Join(rootResolved, filepath.FromSlash(relPath))

	// 2. Load-bearing check: resolve symlinks in the joined path and confirm the
	//    result is still under the resolved root. EvalSymlinks fails with
	//    fs.ErrNotExist for a missing file — surfaced to the caller as a 404.
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if resolvedAbs != rootResolved && !strings.HasPrefix(resolvedAbs, rootResolved+string(filepath.Separator)) {
		return "", ErrPathEscapesWorkspace
	}
	return resolvedAbs, nil
}
