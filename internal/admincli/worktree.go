package admincli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
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
	ws := fs.String("workspace", "", "workspace repo root (default: FLEET_WORKSPACE_ROOT from the environment or the server env file, else ./workspace, relative to the service home when it exists)")
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
	ws := fs.String("workspace", "", "workspace repo root (default: FLEET_WORKSPACE_ROOT from the environment or the server env file, else ./workspace, relative to the service home when it exists)")
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

// resolveWorkspaceRoot is the --workspace flag when given, else the root the
// server uses as far as an operator command can tell, so the CLI and the
// server's in-process sweep agree about which tree they act on. A login shell
// does not carry the service's environment or working directory, so
// FLEET_WORKSPACE_ROOT comes from this process, else from the deployment env
// file (envOrFile; an unreadable file is skipped), else it is ./workspace; a
// relative value is taken against the service's working directory
// (serviceWorkDir).
func resolveWorkspaceRoot(flagVal string) string {
	if strings.TrimSpace(flagVal) != "" {
		return worktree.ResolveWorkspaceRoot(flagVal)
	}
	root := strings.TrimSpace(envOrFile("FLEET_WORKSPACE_ROOT"))
	if root == "" {
		root = "workspace"
	}
	if filepath.IsAbs(root) {
		return filepath.Clean(root)
	}
	return filepath.Join(serviceWorkDir(), root)
}

// defaultServiceHome is the WorkingDirectory deploy/fleet.service gives the
// server.
const defaultServiceHome = "/var/lib/fleet"

// serviceWorkDir is the directory the server resolves a relative workspace
// root against: the service home (FLEET_SERVICE_HOME, else /var/lib/fleet)
// when it exists, else this process's working directory (a development
// server started from here).
func serviceWorkDir() string {
	home := strings.TrimSpace(envreg.Get("FLEET_SERVICE_HOME"))
	if home == "" {
		home = defaultServiceHome
	}
	if info, err := os.Stat(home); err == nil && info.IsDir() {
		return home
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}
