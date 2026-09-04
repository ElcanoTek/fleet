package admincli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ElcanoTek/fleet/internal/worktree"
)

// fleet worktree — operator hygiene for the per-run git worktrees that
// scheduled tasks create when worktree_config is enabled (#180).
//
//	fleet worktree list   [--workspace DIR]
//	fleet worktree prune  [--workspace DIR] [--older-than DUR] [--dry-run]
//
// Worktrees are created under <workspace>/.fleet-worktrees/<task>-<run>. A run
// that crashes between `git worktree add` and its cleanup leaves an orphan;
// prune reclaims orphans without needing manual host access. It runs host git
// directly (there is no DB/storage seam for worktrees), mirroring the
// bootstrap/status host-command pattern.

// cmdWorktree dispatches `fleet worktree list|prune`.
func cmdWorktree(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet worktree list|prune [--workspace DIR] [--older-than DUR]")
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

// worktreePrune reclaims orphaned worktrees. The sweep itself lives in
// internal/worktree so the server's maintenance loop runs the SAME reclamation
// unattended; this command is the operator-facing surface over it (flags,
// human-readable output, an exit code).
func worktreePrune(argv []string) int {
	fs := flag.NewFlagSet("worktree prune", flag.ContinueOnError)
	ws := fs.String("workspace", "", "workspace repo root (default: $FLEET_WORKSPACE_ROOT, else ./workspace)")
	olderThan := fs.Duration("older-than", worktree.DefaultPruneAge, "only remove worktree dirs older than this (e.g. 24h; Go has no day unit — use hours)")
	dryRun := fs.Bool("dry-run", false, "list what would be removed without removing it")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	root := resolveWorkspaceRoot(*ws)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	res, err := worktree.PruneStale(ctx, root, *olderThan, *dryRun)
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if err != nil {
		return errf(5, "worktree prune: %v", err)
	}

	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	for _, path := range res.Removed {
		fmt.Printf("%s %s\n", verb, path)
	}
	fmt.Printf("%s %d worktree dir(s); kept %d newer than %s\n", verb, res.Count(), res.Kept, olderThan.String())
	return 0
}

// resolveWorkspaceRoot resolves the --workspace flag the way the running server
// resolves its workspace root, so the CLI and the server's in-process sweep can
// never disagree about which tree they are reclaiming.
func resolveWorkspaceRoot(flagVal string) string {
	return worktree.ResolveWorkspaceRoot(flagVal)
}

// gitOutput runs host git in dir and returns combined stdout+stderr.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	//nolint:gosec // G204: fixed "git" binary; args are fixed subcommands + an operator-supplied workspace path / worktree paths derived from a ReadDir of that workspace, passed as separate argv with no shell interpolation.
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
