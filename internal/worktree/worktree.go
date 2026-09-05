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

// MinPruneAge is the floor applied to any caller-supplied age. It is tied to
// the default per-run wall-clock ceiling (FLEET_TASK_WALL_TIMEOUT, 4h — see
// docs/MAINTENANCE.md): a worktree younger than the ceiling can belong to a
// run that is still inside its budget, so no age below it is honoured, however
// it was requested (`--older-than 10m`, a mistyped FLEET_WORKTREE_PRUNE_AGE).
// A box with a raised ceiling needs a raised prune age too; the floor cannot
// know the operator's override, which is why it stays at the default ceiling.
const MinPruneAge = 4 * time.Hour

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
// Before any deletion the aged candidates are cross-checked against
// `git worktree list --porcelain`: a worktree git reports as LOCKED is kept
// (a lock is the one explicit "in use" signal git offers, and `remove --force`
// refuses it anyway — the old code then fell through to RemoveAll and deleted
// it regardless); a worktree git knows is removed with
// `git worktree remove --force`, so git's records are updated in the same
// motion, and a failure there keeps the directory for the next sweep rather
// than deleting it behind git's back. Only a directory git does NOT list falls
// back to a plain RemoveAll — there is no record to keep consistent. When the
// listing itself fails (root is not a git repository, git wedged), every
// candidate is treated as unknown to git, which is the pre-cross-check
// behavior.
//
// dryRun reports what would be removed and skips both the git record prune
// (which only discards already-orphaned metadata, nothing an operator would
// want to inspect) and every deletion.
//
// A missing worktree directory is "nothing to prune", not an error: most boxes
// never enable worktree isolation. olderThan <= 0 is replaced with
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

	known, locked := listGitWorktrees(ctx, root)

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
		key := canonicalPath(path)
		if reason, isLocked := locked[key]; isLocked {
			// git's explicit in-use signal: never reclaim a locked worktree,
			// whatever its age.
			res.Kept++
			res.Warnings = append(res.Warnings, fmt.Sprintf("kept %s: locked by git (%s)", path, reason))
			continue
		}
		if dryRun {
			res.Removed = append(res.Removed, path)
			continue
		}
		if known[key] {
			if out, gErr := gitOutput(ctx, root, "worktree", "remove", "--force", path); gErr != nil {
				// git knows this worktree and refused: keep it for the next
				// sweep rather than deleting behind git's back.
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("remove %s: git worktree remove: %v (%s)", path, gErr, strings.TrimSpace(out)))
				continue
			}
			res.Removed = append(res.Removed, path)
			continue
		}
		// Unknown to git: no record to keep consistent, so a plain delete.
		if rmErr := os.RemoveAll(path); rmErr != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("remove %s: %v", path, rmErr))
			continue
		}
		res.Removed = append(res.Removed, path)
	}
	return res, nil
}

// listGitWorktrees parses `git worktree list --porcelain` for the repository
// at root into the set of worktree paths git knows (canonicalised) and the
// subset it reports as locked, keyed the same way, with the lock reason. On
// any failure both sets are empty: every candidate is then "unknown to git",
// the pre-cross-check behavior.
func listGitWorktrees(ctx context.Context, root string) (known map[string]bool, locked map[string]string) {
	known, locked = map[string]bool{}, map[string]string{}
	out, err := gitOutput(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return known, locked
	}
	current := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			current = canonicalPath(strings.TrimPrefix(line, "worktree "))
			known[current] = true
		case line == "locked" || strings.HasPrefix(line, "locked "):
			if current != "" {
				reason := strings.TrimSpace(strings.TrimPrefix(line, "locked"))
				if reason == "" {
					reason = "no reason given"
				}
				locked[current] = reason
			}
		case line == "":
			current = ""
		}
	}
	return known, locked
}

// canonicalPath normalises a path for set membership across the two sources
// (git's listing and our ReadDir): cleaned, absolute where possible, symlinks
// resolved when the path exists.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
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
