// Package worktree owns the on-disk layout of per-run git worktrees and the
// reclamation of the ones that were left behind.
//
// Scheduled runs with worktree_config enabled get an isolated worktree per run
// under <workspace>/.fleet-worktrees/<task>-<run> (internal/scheduledrun
// creates them; see that package's doc for why they live under the workspace
// root rather than /tmp). The run's own cleanup removes it — but only on the
// paths that reach cleanup. A crashed process, a task with auto_cleanup off, or
// a `git worktree remove` that fails all leave a full checkout on disk, and a
// large repository's worktree is not small.
//
// This package is a LEAF: it depends on nothing in fleet. That is deliberate —
// both the operator CLI (`fleet worktree prune`) and the server's in-process
// maintenance loop reclaim worktrees, and neither should have to pull in the
// agent runtime to do it. Before it existed, the CLI carried its own copy of
// the sweep and its own copy of the ".fleet-worktrees" literal, and the server
// had no reclaimer at all.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// gitCmdTimeout bounds a single host git invocation during a sweep. Generous
// for a large repository without being unbounded if git wedges.
const gitCmdTimeout = 2 * time.Minute

// Result reports what one sweep did. Removed and Warnings are returned rather
// than printed so the caller decides the surface: the CLI prints them, the
// server logs a summary.
type Result struct {
	// Removed lists the worktree directories reclaimed (or, on a dry run, the
	// ones that would have been).
	Removed []string
	// Kept counts directories skipped for being younger than the age bound.
	Kept int
	// Warnings records per-entry problems that did not stop the sweep — a stat
	// failure, a git record prune that errored. Best-effort reclamation means
	// one bad entry must never abort the rest.
	Warnings []string
}

// Count is the number of directories removed (or that would be removed).
func (r Result) Count() int { return len(r.Removed) }

// PruneStale reclaims orphaned per-run worktrees under root in two
// complementary steps that target DIFFERENT things:
//
//  1. `git worktree prune` cleans git-side admin records for worktrees whose
//     directory is already gone. This frees no disk; it stops git's
//     .git/worktrees metadata from accumulating.
//  2. A filesystem sweep removes <root>/.fleet-worktrees/* directories older
//     than olderThan — the part that actually frees disk.
//
// Directories are removed with `git worktree remove --force` first, so git's
// records are updated in the same motion, falling back to a plain RemoveAll for
// a directory git no longer knows about.
//
// dryRun reports what would be removed and skips both the git record prune
// (which only discards already-orphaned metadata, nothing an operator would
// want to inspect) and every deletion.
//
// A missing worktree directory is "nothing to prune", not an error: most boxes
// never enable worktree isolation. olderThan <= 0 is replaced with
// DefaultPruneAge rather than honoured, so a zero-valued caller cannot
// accidentally delete a live run's checkout.
func PruneStale(ctx context.Context, root string, olderThan time.Duration, dryRun bool) (Result, error) {
	var res Result
	if strings.TrimSpace(root) == "" {
		return res, fmt.Errorf("worktree prune: empty workspace root")
	}
	if olderThan <= 0 {
		olderThan = DefaultPruneAge
	}

	if !dryRun {
		if out, err := gitOutput(ctx, root, "worktree", "prune"); err != nil {
			// Non-fatal: the directory sweep below is the part that frees disk.
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("git worktree prune (workspace %s): %v: %s", root, err, strings.TrimSpace(out)))
		}
	}

	parent := filepath.Join(root, Subdir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read %s: %w", parent, err)
	}

	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(parent, e.Name())
		info, statErr := e.Info()
		if statErr != nil {
			// Vanished mid-sweep, or unreadable: skip it, keep going.
			res.Warnings = append(res.Warnings, fmt.Sprintf("stat %s: %v", path, statErr))
			continue
		}
		if info.ModTime().After(cutoff) {
			res.Kept++
			continue
		}
		if dryRun {
			res.Removed = append(res.Removed, path)
			continue
		}
		if out, gErr := gitOutput(ctx, root, "worktree", "remove", "--force", path); gErr != nil {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("remove %s: git: %v (%s); rmdir: %v", path, gErr, strings.TrimSpace(out), rmErr))
				continue
			}
		}
		res.Removed = append(res.Removed, path)
	}
	return res, nil
}

// ResolveWorkspaceRoot mirrors how the running server resolves the workspace
// root (internal/agent/manager.go): an explicit value wins, else
// FLEET_WORKSPACE_ROOT (legacy CHAT_WORKSPACE_ROOT), else ./workspace.
//
// Shared so the CLI's `--workspace` flag and the server's in-process sweep can
// never disagree about which tree they are reclaiming.
func ResolveWorkspaceRoot(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := os.Getenv("FLEET_WORKSPACE_ROOT"); v != "" {
		return v
	}
	if v := os.Getenv("CHAT_WORKSPACE_ROOT"); v != "" {
		return v
	}
	if abs, err := filepath.Abs("workspace"); err == nil {
		return abs
	}
	return "workspace"
}

// gitOutput runs host git in dir and returns combined stdout+stderr. A
// per-invocation timeout is applied on top of ctx so one wedged git cannot
// stall an unattended sweep for the whole pass.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	//nolint:gosec // G204: fixed "git" binary; args are fixed subcommands plus an operator-configured workspace path and worktree paths derived from a ReadDir of that workspace, passed as separate argv with no shell interpolation.
	cmd := exec.CommandContext(cctx, "git", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
