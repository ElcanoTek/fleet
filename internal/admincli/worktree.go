package admincli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
// that crashes between registering its worktree and its cleanup leaves an orphan;
// prune reclaims orphans without needing manual host access. Neither runs git
// (see internal/worktree, ADR-0118).

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

// worktreeList prints the per-run worktrees under the workspace's
// .fleet-worktrees and what fleet can prove about their records, without
// running git (worktree.List). Text a run could write (a lock reason) is
// printed through worktree.SanitizeForTerminal.
func worktreeList(argv []string) int {
	fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
	ws := fs.String("workspace", "", "workspace repo root (default: $FLEET_WORKSPACE_ROOT, else ./workspace)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	root := resolveWorkspaceRoot(*ws)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Like `fleet worktree prune`, this one-off process records the
	// workspace repository as it finds it now (see worktree.Pin).
	worktree.PinAtStartup(root)
	entries, err := worktree.List(ctx, root)
	if err != nil {
		return errf(5, "worktree list (workspace %s): %v", root, err)
	}
	if len(entries) == 0 {
		fmt.Printf("no per-run worktrees under %s\n", worktree.SanitizeForTerminal(root))
		return 0
	}
	for _, e := range entries {
		record := "no proven record"
		if e.RecordID != "" {
			record = "record " + worktree.SanitizeForTerminal(e.RecordID)
		}
		line := worktree.SanitizeForTerminal(e.Path) + "  " + record
		if e.Locked != "" {
			line += "  locked: " + worktree.SanitizeForTerminal(e.Locked)
		}
		fmt.Println(line)
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

	// This one-off process records the workspace repository as it finds it
	// now: the sweep then treats it the way the server treats the repository
	// it recorded at its own start (see worktree.Pin).
	worktree.PinAtStartup(root)
	res, err := worktree.PruneStale(ctx, root, *olderThan, *dryRun)
	// Warnings and paths can carry text a run wrote (a lock reason, a
	// directory name): printed with control characters replaced.
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", worktree.SanitizeForTerminal(w))
	}
	if err != nil {
		return errf(5, "worktree prune: %s", worktree.SanitizeForTerminal(err.Error()))
	}

	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	for _, path := range res.Removed {
		fmt.Printf("%s %s\n", verb, worktree.SanitizeForTerminal(path))
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
