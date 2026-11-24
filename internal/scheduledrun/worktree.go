package scheduledrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/worktree"
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
//   - Sandbox.SetDefaultWorkingDir fills the cwd for any bash/run_python call
//     that arrives with an empty per-call working dir, so the sandbox default
//     applies host-side.
//   - tools.WithForcedWorkingDir scopes the in-process tool layer (bash,
//     run_python, and the relative-path file tools), whose resolvers would
//     otherwise default to the process cwd.
// Together these cover bash, run_python, and the relative-path file tools.

const (
	// worktreeSubdir is the directory under the workspace root that holds per-run
	// worktrees. Kept out of the main working tree's `git status` via
	// .git/info/exclude (see ensureWorktreeExcluded). Shared with the reclaimer
	// in internal/worktree so the creator and the sweeper can never disagree
	// about where these live.
	worktreeSubdir = worktree.Subdir
	// gitCmdTimeout bounds the in-sandbox checkout (a large repository's
	// first checkout can take a while) and the host-side cleanup.
	gitCmdTimeout = 2 * time.Minute
	// defaultBase is the base a run branches from when base_branch is unset:
	// the repository's HEAD. Inside the new worktree HEAD is the run's own
	// (unborn) branch, so the repository's is named main-worktree/HEAD.
	defaultBase = "main-worktree/HEAD"
	// minSandboxGitMajor and minSandboxGitMinor are the oldest git the
	// in-sandbox checkout works with: `git switch` arrived in 2.23
	// (main-worktree/ refs in 2.20).
	minSandboxGitMajor = 2
	minSandboxGitMinor = 23
)

// minSandboxGit is minSandboxGitMajor.minSandboxGitMinor, for messages.
var minSandboxGit = fmt.Sprintf("%d.%d", minSandboxGitMajor, minSandboxGitMinor)

// runWorktree is one run's git worktree: its checkout directory under
// .fleet-worktrees, the repository's record of it under .git/worktrees, and
// its branch. The host writes only the record and the checkout's .git file
// (worktree.CreateWorktree), running no git; the first worker pass creates
// the branch from its base and fills the checkout inside the run's sandbox
// (checkedOut); the host runs no git in the repository.
type runWorktree struct {
	path       string
	adminDir   string
	branch     string
	base       string // what the branch starts from, resolved by the sandbox's git
	shallow    bool   // the repository is a shallow clone (worktree.IsShallow)
	checkedOut atomic.Bool
}

// dir is the checkout directory, or "" for a run without a worktree.
func (w *runWorktree) dir() string {
	if w == nil {
		return ""
	}
	return w.path
}

// mount is the sandbox's view of this worktree, or nil for a run without one.
func (w *runWorktree) mount() *sandbox.WorktreeMount {
	if w == nil {
		return nil
	}
	return &sandbox.WorktreeMount{Path: w.path, AdminDir: w.adminDir}
}

// prepareWorktree creates a per-run git worktree when the task has
// worktree_config enabled, and returns it with a cleanup closure that removes
// the worktree and its branch. On the disabled path it returns a nil worktree
// — the signal to leave the sandbox's working directory untouched (current
// shared-workspace behaviour). cleanup is always non-nil and safe to call even
// when disabled (then it is a no-op). The caller decides whether/when to
// invoke cleanup based on AutoCleanup / CleanupDelaySeconds.
//
// The workspace repository is written from sandboxes — a live worktree run's
// sandbox writes its record and checkout, .git/objects, .git/refs and
// .git/logs — so the host runs no git in it at all: it checks that the
// repository is the one recorded at startup and points nowhere else
// (worktree.RepoPin.Check), then writes the new worktree's record and
// checkout .git file itself (worktree.CreateWorktree), in directories no
// sandbox can write. Resolving the base, creating the branch and the
// checkout happen in the run's sandbox; the branch is deleted afterwards
// without git (worktree.DeleteBranch).
func prepareWorktree(ctx context.Context, workspaceRoot string, task *models.Task, runID string) (wt *runWorktree, cleanup func(), err error) {
	noop := func() {}
	cfg := task.WorktreeConfig
	if cfg == nil || !cfg.Enabled {
		return nil, noop, nil
	}

	// Guard: the workspace must be the git repository recorded at startup, and
	// its root. This fires before any worktree creation so a misconfigured task
	// fails clearly rather than silently running in a non-isolated directory.
	if err := worktree.Pin(workspaceRoot).Check(ctx, workspaceRoot); err != nil {
		return nil, noop, fmt.Errorf("worktree_config: %w", err)
	}

	prefix := cfg.BranchPrefix
	if prefix == "" {
		prefix = models.DefaultWorktreeBranchPrefix
	}
	branch := fmt.Sprintf("%s%s-%s", prefix, task.ID, runID)

	// Worktree dir lives under the workspace root (see package doc for why).
	// 0o755 so the rootless-podman container user (uid 1000) can traverse into
	// the worktree, matching tools.EnsureWorkspaceDir's rationale.
	if err := os.Mkdir(filepath.Join(workspaceRoot, worktreeSubdir), 0o755); err != nil && !errors.Is(err, fs.ErrExist) { //nolint:gosec // bind-mount path must be traversable by the lockdown container user
		return nil, noop, fmt.Errorf("create worktree parent dir: %w", err)
	}
	// The worktree is created only inside a real directory: never through a
	// symlink standing in its place (see worktree.CheckSubdir).
	parent, err := worktree.CheckSubdir(workspaceRoot)
	if err != nil {
		return nil, noop, fmt.Errorf("worktree parent dir: %w", err)
	}
	if err := ensureWorktreeExcluded(ctx, workspaceRoot); err != nil {
		// Non-fatal: a polluted `git status` in the main tree is cosmetic, not a
		// correctness problem. Log and continue.
		logging.From(ctx).Error("scheduled worktree: could not add to git exclude", "path", worktreeSubdir, "err", err)
	}
	name := fmt.Sprintf("%s-%s", task.ID, runID)
	wtPath := filepath.Join(parent, name)

	// The base is resolved by the run's sandbox, not here.
	base := worktreeBase(cfg.BaseBranch)

	run := &runWorktree{path: wtPath, branch: branch, base: base, shallow: worktree.IsShallow(ctx, workspaceRoot)}
	cleanup = func() {
		// Detached, bounded context: cleanup runs after the run's ctx is
		// cancelled (and possibly on a delay timer), so it must not inherit that
		// cancellation.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCmdTimeout)
		defer cancel()
		// The checkout and the repository's record of it are removed without
		// git (worktree.RemoveWorktree, worktree.RemoveRecord by the id this
		// setup gave it); a leftover is reclaimed later by the server's
		// maintenance loop (or `fleet worktree prune`).
		//
		// The record and the branch go only once the checkout is gone. A
		// worktree the run locked (`git worktree lock`) is kept whole, as
		// `git worktree remove` and the sweep leave a locked worktree alone;
		// so is one whose checkout could not be fully removed (a directory
		// inside it the service user cannot search): its record and branch
		// stay, so its commits keep a ref and a later cleanup or the sweep
		// can finish once the checkout can go. Only when the checkout is gone
		// and just its record could not be removed does cleanup go on.
		if rErr := worktree.RemoveWorktree(cctx, workspaceRoot, name); rErr != nil {
			switch {
			case errors.Is(rErr, worktree.ErrLocked):
				logging.From(ctx).Info("scheduled worktree: kept, the run locked it", "path", wtPath, "branch", branch)
				return
			case errors.Is(rErr, worktree.ErrRecordNotRemoved):
				logging.From(ctx).Error("scheduled worktree: checkout removed, record removal failed", "path", wtPath, "err", rErr)
			default:
				logging.From(ctx).Warn("scheduled worktree: kept its record and branch, the checkout could not be removed", "path", wtPath, "branch", branch, "err", rErr)
				return
			}
		}
		if rErr := worktree.RemoveRecord(cctx, workspaceRoot, name); rErr != nil {
			logging.From(ctx).Error("scheduled worktree: remove record failed", "record", name, "err", rErr)
		}
		// Delete the per-run branch too: an enabled+auto_cleanup task throws the
		// run away, so leaving one branch per run would accumulate ref litter.
		// Operators who want to keep the work set auto_cleanup:false instead.
		// Only a branch this run's sandbox created (checkedOut) is deleted: a
		// branch of that name that existed before was never reset, and is not
		// removed either.
		if !run.checkedOut.Load() {
			return
		}
		if bErr := worktree.DeleteBranch(cctx, workspaceRoot, branch); bErr != nil {
			logging.From(ctx).Error("scheduled worktree: delete branch failed", "branch", branch, "err", bErr)
		}
	}

	// The record and the checkout's .git file are written without git
	// (worktree.CreateWorktree); the sandbox creates the branch and fills the
	// checkout on the first worker pass (checkoutInSandbox). A failed
	// CreateWorktree has already removed what it made, and whatever existed
	// under this name or branch before is not this run's, so cleanup is not
	// called here.
	adminDir, err := worktree.CreateWorktree(ctx, workspaceRoot, name, branch)
	if err != nil {
		return nil, noop, fmt.Errorf("create worktree: %w", err)
	}
	run.adminDir = adminDir
	return run, cleanup, nil
}

// worktreeBase is the start point the run's sandbox passes to git switch for
// base_branch. Inside the new worktree, HEAD and the other per-worktree
// pseudo-refs (FETCH_HEAD, ORIG_HEAD, MERGE_HEAD, …) are the run's own, so a
// base naming one of them — or "@", git's short name for HEAD — means the
// repository's and is read through main-worktree/. An empty base is the
// repository's HEAD (defaultBase).
func worktreeBase(baseBranch string) string {
	base := strings.TrimSpace(baseBranch)
	switch {
	case base == "", base == "@":
		return defaultBase
	case base == "HEAD", pseudoRef.MatchString(base):
		return "main-worktree/" + base
	default:
		return base
	}
}

// pseudoRef matches git's per-worktree pseudo-refs such as FETCH_HEAD.
var pseudoRef = regexp.MustCompile(`^[A-Z][A-Z_]*_HEAD$`)

// checkoutGit is how fleet's own checkout commands run git in the sandbox.
// safe.directory covers a sandbox user whose uid differs from the files'
// owner (a pod on the kubernetes backend). core.hooksPath=/dev/null keeps the
// repository's hooks (a post-checkout hook such as git-lfs's) out of fleet's
// checkout, so one that fails, or needs what the sandbox lacks, cannot fail
// the run before it starts; the run's own git commands run them as usual.
const checkoutGit = "git -c safe.directory='*' -c core.hooksPath=/dev/null"

// Exit statuses checkoutInSandbox's command uses for what it checks before
// git switch runs.
const (
	// oldGitExit: the sandbox's git is older than minSandboxGit, or its
	// version could not be read; the command prints `git version` first.
	oldGitExit = 98
	// baseMissingExit: the base does not name a commit.
	baseMissingExit = 99
)

// gitVersionCheck is the shell that exits oldGitExit, printing what
// `git version` said, unless the sandbox's git is at least minSandboxGit.
var gitVersionCheck = fmt.Sprintf(`v=$(git version 2>&1); set -- $v; ver=${3:-}; major=${ver%%%%.*}; rest=${ver#*.}; minor=${rest%%%%.*}; `+
	`case "$major.$minor" in *[!0-9.]*|.*|*.) printf '%%s\n' "$v" >&2; exit %d;; esac; `+
	`if [ "$major" -lt %d ] || { [ "$major" -eq %d ] && [ "$minor" -lt %d ]; }; then printf '%%s\n' "$v" >&2; exit %d; fi; `,
	oldGitExit, minSandboxGitMajor, minSandboxGitMajor, minSandboxGitMinor, oldGitExit)

// checkoutInSandbox creates the run's branch from its base and fills the
// worktree, inside the run's sandbox: `git switch --create <branch> <base>`
// from the record's unborn HEAD writes the ref, its reflog, the index and the
// working tree there, so the base is resolved, and whatever filters the
// repository configures run, in the sandbox — not on the host.
//
//   - --create, not --force-create: the branch is new for every run, and an
//     existing branch of that name is reported, never reset.
//   - --no-track: a remote-tracking base (origin/main) would otherwise make
//     git record an upstream in .git/config, which the sandbox mounts
//     read-only.
//   - The sandbox's git version is checked first: older than minSandboxGit
//     (or unreadable) fails the run naming the version.
//   - A base that does not name a commit fails the run here with a message
//     naming it; for the default base (the repository's HEAD) that means the
//     repository has no commits yet.
func checkoutInSandbox(ctx context.Context, sb *sandbox.Sandbox, wtPath, branch, base string) error {
	command := gitVersionCheck + fmt.Sprintf("%s rev-parse --quiet --verify %s >/dev/null; rc=$?; "+
		"if [ $rc -eq 1 ]; then exit %d; elif [ $rc -ne 0 ]; then exit $rc; fi; "+
		"%s switch --quiet --no-track --create %s %s",
		checkoutGit, shellQuote(base+"^{commit}"), baseMissingExit,
		checkoutGit, shellQuote(branch), shellQuote(base))
	res, err := sb.RunBash(ctx, sandbox.BashRequest{
		Command:    command,
		WorkingDir: wtPath,
		Timeout:    gitCmdTimeout,
	})
	if err != nil {
		return err
	}
	return checkoutResult(res.ExitCode, string(res.Stderr)+string(res.Stdout), branch, base)
}

// checkoutResult turns checkoutInSandbox's exit status and output into the
// run's error.
func checkoutResult(exit int, output, branch, base string) error {
	output = strings.TrimSpace(output)
	switch {
	case exit == 0:
		return nil
	case exit == oldGitExit:
		return fmt.Errorf("the sandbox image's git (%q) is older than %s, or its version could not be read; worktree runs need git %s or later in the sandbox image", output, minSandboxGit, minSandboxGit)
	case exit == baseMissingExit && base == defaultBase:
		return errors.New("the workspace repository has no commits: its HEAD names a branch with no commits yet, so there is nothing to branch from; commit to it, or set base_branch")
	case exit == baseMissingExit:
		return fmt.Errorf("base %q does not name a commit in the workspace repository", base)
	default:
		return fmt.Errorf("creating branch %s from base %q in the worktree exited %d: %s", branch, base, exit, output)
	}
}

// withWorktreeNotice prefixes a worktree run's task prompt with what its git
// can and cannot do. The sandbox mounts the repository's .git read-only
// except the run's own checkout and record, the object store, refs, reflogs
// and Git LFS store (sandbox.WorktreeMount), so git prints a packed-refs.lock error
// after most ref updates while the command itself succeeds, and anything
// that rewrites packed-refs, the config (an upstream, a submodule),
// .git/shallow (a fetch that changes a shallow clone's depth) or another
// worktree fails; the shallow sentence is added only for a shallow clone.
// Appended to the task prompt, never the cached system prefix.
func withWorktreeNotice(prompt string, wt *runWorktree) string {
	if wt == nil {
		return prompt
	}
	return fmt.Sprintf("[worktree] You are working in a git worktree of the workspace repository, at %s on branch %s. "+
		"Commits, new branches, fetches and pushes work. After most of them git prints "+
		"`error: Unable to create '.../packed-refs.lock': Read-only file system`; that line is expected and the command "+
		"still succeeded — judge by its exit status. The repository's config is read-only here, so nothing can record "+
		"an upstream: create a branch from a remote-tracking ref (origin/...) with --no-track, and push with "+
		"`git push <remote> <branch>`, not `git push -u` / `--set-upstream`. Deleting or renaming a branch, "+
		"`git gc` / `git pack-refs`, `git config` writes (including `branch --set-upstream-to`), initialising "+
		"submodules and changes to other worktrees fail here.%s\n\n%s",
		wt.path, wt.branch, shallowNotice(wt), prompt)
}

// shallowNotice is the worktree notice's sentence for a shallow repository.
func shallowNotice(wt *runWorktree) string {
	if !wt.shallow {
		return ""
	}
	return " The repository is a shallow clone: ordinary fetches work, but fetches that change its depth " +
		"(`--depth`, `--deepen`, `--shallow-since`, `--shallow-exclude`, `--unshallow`) fail here with " +
		"`Unable to create '.../shallow.lock'`; full history has to be fetched on the host."
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
// never committed and does not touch a tracked .gitignore. The read and the
// append follow no link and cannot block on a FIFO (worktree.EnsureExcluded).
func ensureWorktreeExcluded(ctx context.Context, workspaceRoot string) error {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	return worktree.EnsureExcluded(ctx, workspaceRoot)
}
