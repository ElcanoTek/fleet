package boxdoctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ElcanoTek/fleet/internal/worktree"
)

// CheckWorktreeGit reports whether worktree_config tasks can run against the
// workspace root's repository (ADR-0118), by the check they run
// (worktree.RepoPin.CheckBeforeStart, which lets the directories the server
// creates at its start still be missing): skipped when the root has no .git
// or this process cannot look into the root, a warning when the repository
// was not recorded at startup, its .git was replaced or is a gitfile or a
// symlink, a directory inside it is a link (or missing, other than those the
// server creates), it points at another repository, or it uses reftable. An
// ok for a shallow clone says that fetches changing its depth fail in
// worktree runs (ShallowNote), and one whose config sets a non-default
// lfs.storage says that Git LFS cannot store objects in worktree runs. It reads the record this
// process made (worktree.Pin): the server's from its own startup; a one-off
// CLI process records the root before calling it.
func CheckWorktreeGit(ctx context.Context, workspaceRoot string) Check {
	c := Check{Name: "workspace repository for worktree tasks"}
	if workspaceRoot == "" {
		c.Status, c.Detail = StatusSkip, "no workspace root configured"
		return c
	}
	_, err := os.Lstat(filepath.Join(workspaceRoot, ".git"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.Status, c.Detail = StatusSkip, "the workspace root is not a git repository; worktree tasks are not in use"
		return c
	case err != nil:
		// A root this process cannot look into (/var/lib/fleet is 0700 to
		// the service user) says nothing about the server's view of it.
		c.Status, c.Detail = StatusSkip, "cannot read the workspace root as this user ("+err.Error()+"); run as root to check it"
		return c
	}
	// CheckBeforeStart: the directories the server creates inside .git when
	// it starts (worktrees/, logs/, info/, objects/info/) may still be
	// missing in a repository it has not started on yet.
	if err := worktree.Pin(workspaceRoot).CheckBeforeStart(ctx, workspaceRoot); err != nil {
		c.Status, c.Detail = StatusWarn, err.Error()+"; worktree tasks are refused"
		c.Fix = "restart fleet once the workspace repository's .git is a plain directory with nothing inside it replaced by a link"
		return c
	}
	c.Status, c.Detail = StatusOK, OKDetail
	if worktree.IsShallow(ctx, workspaceRoot) {
		c.Detail += ShallowNote
	}
	if storage, set := worktree.LFSStorage(ctx, workspaceRoot); set {
		c.Detail += fmt.Sprintf(lfsStorageNote, worktree.SanitizeForTerminal(storage))
	}
	return c
}

// OKDetail is CheckWorktreeGit's detail for a usable repository, before any
// notes.
const OKDetail = "worktree tasks can run against the workspace repository"

// lfsStorageNote is what CheckWorktreeGit adds when the repository's config
// sets lfs.storage to anything but .git/lfs.
const lfsStorageNote = "; its lfs.storage is %q, but only .git/lfs is writable in worktree runs, so Git LFS cannot fetch or check out objects there — keep the default storage"

// ShallowNote is what CheckWorktreeGit adds for a shallow workspace
// repository.
const ShallowNote = "; it is a shallow clone, so fetches that change its depth (--depth, --deepen, --shallow-since, --shallow-exclude, --unshallow) fail in worktree runs — unshallow it on the host for full history"
