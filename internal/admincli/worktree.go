package admincli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// fleet-admin worktree — operator hygiene for the per-run git worktrees that
// scheduled tasks create when worktree_config is enabled (#180).
//
//	fleet-admin worktree list   [--workspace DIR]
//	fleet-admin worktree prune  [--workspace DIR] [--older-than DUR] [--dry-run]
//
// Worktrees are created under <workspace>/.fleet-worktrees/<task>-<run>. A run
// that crashes between `git worktree add` and its cleanup leaves an orphan;
// prune reclaims orphans without needing manual host access. It runs host git
// directly (there is no DB/storage seam for worktrees), mirroring the
// bootstrap/status host-command pattern.

// cmdWorktree dispatches `fleet-admin worktree list|prune`.
func cmdWorktree(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin worktree list|prune [--workspace DIR] [--older-than DUR]")
	}
	switch argv[0] {
	case "list", "ls":
		return worktreeList(argv[1:])
	case "prune":
		return worktreePrune(argv[1:])
	default:
		return errf(1, "unknown worktree subcommand %q (want list|prune)", argv[0])
	}
}

// worktreeList prints `git worktree list --porcelain` for the configured
// workspace repo, so operators can see every registered worktree.
func worktreeList(argv []string) int {
	fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
	ws := fs.String("workspace", "", "workspace repo root (default: $FLEET_WORKSPACE_ROOT, else ./workspace)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	root := resolveWorkspaceRoot(*ws)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	out, err := gitOutput(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return errf(5, "git worktree list (workspace %s): %v\n%s", root, err, out)
	}
	fmt.Print(out)
	if !strings.HasSuffix(out, "\n") {
		fmt.Println()
	}
	return 0
}

// worktreePrune reclaims orphaned worktrees in two complementary steps that
// target DIFFERENT things: `git worktree prune` cleans git-side admin records
// for worktrees whose directory is already gone, while the filesystem sweep
// removes stale <workspace>/.fleet-worktrees/* DIRECTORIES (from a task that
// crashed before its cleanup) older than --older-than.
func worktreePrune(argv []string) int {
	fs := flag.NewFlagSet("worktree prune", flag.ContinueOnError)
	ws := fs.String("workspace", "", "workspace repo root (default: $FLEET_WORKSPACE_ROOT, else ./workspace)")
	olderThan := fs.Duration("older-than", 24*time.Hour, "only remove worktree dirs older than this (e.g. 24h; Go has no day unit — use hours)")
	dryRun := fs.Bool("dry-run", false, "list what would be removed without removing it")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	root := resolveWorkspaceRoot(*ws)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Step 1: git-side record prune (skipped on a dry run since it only removes
	// already-orphaned admin metadata, nothing the operator would want to inspect).
	if !*dryRun {
		if out, err := gitOutput(ctx, root, "worktree", "prune"); err != nil {
			// Non-fatal: the directory sweep below is the part that frees disk.
			fmt.Fprintf(os.Stderr, "warning: git worktree prune (workspace %s): %v\n%s\n", root, err, out)
		}
	}

	// Step 2: filesystem sweep of stale per-run worktree dirs.
	parent := filepath.Join(root, ".fleet-worktrees")
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("no worktree directory at %s; nothing to prune\n", parent)
			return 0
		}
		return errf(5, "read %s: %v", parent, err)
	}

	cutoff := time.Now().Add(-*olderThan)
	removed, kept := 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(parent, e.Name())
		info, statErr := e.Info()
		if statErr != nil {
			fmt.Fprintf(os.Stderr, "warning: stat %s: %v\n", path, statErr)
			continue
		}
		if info.ModTime().After(cutoff) {
			kept++
			continue
		}
		if *dryRun {
			fmt.Printf("would remove %s (mtime %s)\n", path, info.ModTime().Format(time.RFC3339))
			removed++
			continue
		}
		// Try a clean `git worktree remove --force` first so git's admin records
		// are updated too; fall back to a raw RemoveAll for a dir git no longer
		// knows about.
		if out, gErr := gitOutput(ctx, root, "worktree", "remove", "--force", path); gErr != nil {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				fmt.Fprintf(os.Stderr, "warning: remove %s: git: %v (%s); rmdir: %v\n", path, gErr, strings.TrimSpace(out), rmErr)
				continue
			}
		}
		fmt.Printf("removed %s\n", path)
		removed++
	}
	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	fmt.Printf("%s %d worktree dir(s); kept %d newer than %s\n", verb, removed, kept, olderThan.String())
	return 0
}

// resolveWorkspaceRoot mirrors how the running server resolves the workspace
// root (internal/agent/manager.go): an explicit flag wins, else
// FLEET_WORKSPACE_ROOT (legacy CHAT_WORKSPACE_ROOT), else ./workspace.
func resolveWorkspaceRoot(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
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

// gitOutput runs host git in dir and returns combined stdout+stderr.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	//nolint:gosec // G204: fixed "git" binary; args are fixed subcommands + an operator-supplied workspace path / worktree paths derived from a ReadDir of that workspace, passed as separate argv with no shell interpolation.
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
