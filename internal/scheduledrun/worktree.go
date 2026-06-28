package scheduledrun

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Git worktree isolation for scheduled runs (#180).
//
// When a task's WorktreeConfig is enabled, each run gets its own git worktree +
// branch so concurrent tasks targeting the same repository cannot corrupt each
// other's working tree. The worktree is created as a SUBDIRECTORY of the
// workspace root rather than at an arbitrary /tmp path, because a git worktree's
// `.git` is a file pointing back to "<mainrepo>/.git/worktrees/<name>" — git
// only resolves it if BOTH the worktree and the main repo are reachable at their
// host absolute paths inside the sandbox. The sandbox bind-mounts the workspace
// root at the same absolute path, so a subdir of it satisfies that linkage; a
// standalone /tmp worktree (which the original issue sketched) would break git
// inside the container because the main repo would be unreachable.
//
// Scoping the run into the worktree uses two complementary seams (see runWorker):
//   - Sandbox.SetDefaultWorkingDir scopes the native-acp flavor — the in-container
//     agent delegates bash/run_python to the host Executor, which drops the
//     per-call working dir, so the sandbox default applies host-side.
//   - tools.WithForcedWorkingDir scopes the in-process tool layer (bash,
//     run_python, and the relative-path file tools), whose resolvers would
//     otherwise default to the process cwd.
// Together these cover bash, run_python, and the relative-path file tools on both
// flavors. NOT covered: an agent that writes via the ACP-native fs/* capability
// directly (a fallback the native agent does not use — it routes file ops through
// bash/run_python); those host-side fs handlers are not redirected here.

const (
	// worktreeSubdir is the directory under the workspace root that holds per-run
	// worktrees. Kept out of the main working tree's `git status` via
	// .git/info/exclude (see ensureWorktreeExcluded).
	worktreeSubdir = ".fleet-worktrees"
	// gitCmdTimeout bounds a single host git invocation. `git worktree add` can
	// take a few seconds on a large repo; this is generous without being
	// unbounded if git wedges.
	gitCmdTimeout = 2 * time.Minute
)

// prepareWorktree creates a per-run git worktree when the task has
// worktree_config enabled. On the enabled path it returns the worktree path (to
// scope the run into via Sandbox.SetDefaultWorkingDir), the branch name, and a
// cleanup closure that removes the worktree + branch. On the disabled path it
// returns an EMPTY wtPath — the signal to leave the sandbox's working directory
// untouched (current shared-workspace behaviour). cleanup is always non-nil and
// safe to call even when disabled (then it is a no-op). The caller decides
// whether/when to invoke cleanup based on AutoCleanup / CleanupDelaySeconds.
func prepareWorktree(ctx context.Context, workspaceRoot string, task *models.Task, runID string) (wtPath, branch string, cleanup func(), err error) {
	noop := func() {}
	cfg := task.WorktreeConfig
	if cfg == nil || !cfg.Enabled {
		return "", "", noop, nil
	}

	// Guard: the workspace must be a git repository root. This fires before any
	// worktree creation so a misconfigured task fails clearly rather than
	// silently running in a non-isolated directory.
	if err := verifyGitRepoRoot(ctx, workspaceRoot); err != nil {
		return "", "", noop, err
	}

	prefix := cfg.BranchPrefix
	if prefix == "" {
		prefix = models.DefaultWorktreeBranchPrefix
	}
	branch = fmt.Sprintf("%s%s-%s", prefix, task.ID, runID)

	// Worktree dir lives under the workspace root (see package doc for why).
	parent := filepath.Join(workspaceRoot, worktreeSubdir)
	// 0o755 so the rootless-podman container user (uid 1000) can traverse into
	// the worktree, matching tools.EnsureWorkspaceDir's rationale.
	if err := os.MkdirAll(parent, 0o755); err != nil { //nolint:gosec // bind-mount path must be traversable by the lockdown container user
		return "", "", noop, fmt.Errorf("create worktree parent dir: %w", err)
	}
	if err := ensureWorktreeExcluded(workspaceRoot); err != nil {
		// Non-fatal: a polluted `git status` in the main tree is cosmetic, not a
		// correctness problem. Log and continue.
		log.Printf("scheduled worktree: could not add %s to git exclude: %v", worktreeSubdir, err)
	}
	wtPath = filepath.Join(parent, fmt.Sprintf("%s-%s", task.ID, runID))

	base := strings.TrimSpace(cfg.BaseBranch)
	if base == "" {
		base = "HEAD"
	}

	if out, gErr := runGit(ctx, workspaceRoot, "worktree", "add", "-b", branch, wtPath, base); gErr != nil {
		return "", "", noop, fmt.Errorf("git worktree add: %w\n%s", gErr, out)
	}

	cleanup = func() {
		// Detached, bounded context: cleanup runs after the run's ctx is
		// cancelled (and possibly on a delay timer), so it must not inherit that
		// cancellation.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCmdTimeout)
		defer cancel()
		if out, rErr := runGit(cctx, workspaceRoot, "worktree", "remove", "--force", wtPath); rErr != nil {
			log.Printf("scheduled worktree: remove %s failed: %v\n%s", wtPath, rErr, out)
			// Fall through to branch deletion anyway; a leftover dir is reclaimed
			// later by `fleet-admin worktree prune`.
		}
		// Delete the per-run branch too: an enabled+auto_cleanup task throws the
		// run away, so leaving one branch per run would accumulate ref litter.
		// Operators who want to keep the work set auto_cleanup:false instead.
		if out, bErr := runGit(cctx, workspaceRoot, "branch", "-D", branch); bErr != nil {
			log.Printf("scheduled worktree: delete branch %s failed: %v\n%s", branch, bErr, out)
		}
	}
	return wtPath, branch, cleanup, nil
}

// verifyGitRepoRoot returns nil only if dir is the top level of a git working
// tree. A non-repo dir, or a dir that is merely a subdirectory of a repo, is
// rejected with a clear error.
func verifyGitRepoRoot(ctx context.Context, dir string) error {
	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("workspace %q is not a git repository root (worktree_config requires one): %w: %s", dir, err, strings.TrimSpace(out))
	}
	top := strings.TrimSpace(out)
	// Normalize both sides (symlinks, e.g. /opt/fleet/workspace) before comparing
	// so a symlinked workspace root still matches its resolved toplevel.
	wantAbs, werr := normalizePath(dir)
	gotAbs, gerr := normalizePath(top)
	if werr == nil && gerr == nil && wantAbs != gotAbs {
		return fmt.Errorf("workspace %q is inside git repository %q but is not its root; worktree_config requires the workspace to be the repo root", dir, top)
	}
	return nil
}

// normalizePath resolves symlinks and returns a cleaned absolute path. If the
// path cannot be resolved (e.g. does not exist), it falls back to filepath.Abs.
func normalizePath(p string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, nil
	}
	return filepath.Abs(p)
}

// excludeMu serializes the read-then-append on .git/info/exclude so concurrent
// runs of different tasks against the same repo (the multi-task-same-repo
// scenario this feature targets) cannot both read "absent" and append duplicate
// lines. Same-process only — adequate because the scheduler runs all task
// goroutines in one process.
var excludeMu sync.Mutex

// ensureWorktreeExcluded appends the worktree subdir to the repo's
// .git/info/exclude (idempotent) so per-run worktrees never show up as untracked
// noise in the main working tree's `git status`. This is a LOCAL exclude — it is
// never committed and does not touch a tracked .gitignore.
func ensureWorktreeExcluded(workspaceRoot string) error {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	excludePath := filepath.Join(workspaceRoot, ".git", "info", "exclude")
	line := worktreeSubdir + "/"
	if data, err := os.ReadFile(excludePath); err == nil { //nolint:gosec // path derived from the configured workspace root, not user input
		for _, existing := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(existing) == line {
				return nil // already excluded
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil { //nolint:gosec // git metadata dir
		return err
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // git metadata file
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# fleet per-run worktrees (#180)\n%s\n", line)
	return err
}

// runGit invokes host git in dir and returns combined output. The sandbox is not
// involved: worktree management is a host-side operation on the host's git +
// filesystem (the same posture as fleet-admin's host commands).
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	//nolint:gosec // G204: fixed "git" binary; args are fleet-controlled subcommands + validated paths/refs (branch prefix is ref-name validated at task creation), passed as separate argv with no shell interpolation.
	cmd := exec.CommandContext(ctx, "git", full...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
