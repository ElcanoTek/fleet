package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkWorktreeDir creates <root>/.fleet-worktrees/<name> and back-dates it so the
// age bound can be exercised without sleeping.
func mkWorktreeDir(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(root, Subdir, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	// A real worktree has contents; include one so RemoveAll has to recurse.
	if err := os.WriteFile(filepath.Join(path, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

func TestPruneStaleRemovesOnlyAgedDirs(t *testing.T) {
	root := t.TempDir()
	old := mkWorktreeDir(t, root, "task-old", 48*time.Hour)
	fresh := mkWorktreeDir(t, root, "task-fresh", time.Minute)

	res, err := PruneStale(context.Background(), root, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if res.Count() != 1 {
		t.Fatalf("removed %d dirs, want 1 (removed=%v)", res.Count(), res.Removed)
	}
	if res.Kept != 1 {
		t.Fatalf("kept %d, want 1", res.Kept)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("aged worktree %s survived the sweep (stat err %v)", old, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		// The live-run protection: a fresh dir may belong to a task still
		// running, and deleting it would destroy work in progress.
		t.Errorf("fresh worktree %s was removed; it may belong to a running task", fresh)
	}
}

func TestPruneStaleDryRunRemovesNothing(t *testing.T) {
	root := t.TempDir()
	old := mkWorktreeDir(t, root, "task-old", 48*time.Hour)

	res, err := PruneStale(context.Background(), root, 24*time.Hour, true)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if res.Count() != 1 {
		t.Fatalf("reported %d dirs, want 1", res.Count())
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("dry run deleted %s: %v", old, err)
	}
}

// A box that never enables worktree isolation has no .fleet-worktrees directory
// at all. That is the common case and must be a silent no-op, not an error the
// maintenance loop logs every hour.
func TestPruneStaleMissingDirIsNotAnError(t *testing.T) {
	res, err := PruneStale(context.Background(), t.TempDir(), 24*time.Hour, false)
	if err != nil {
		t.Fatalf("missing worktree dir should not error: %v", err)
	}
	if res.Count() != 0 || res.Kept != 0 {
		t.Fatalf("expected an empty result, got %+v", res)
	}
}

// A caller that passes a zero duration must not get "delete everything" — the
// age bound is the only thing standing between the sweep and a running task's
// checkout, so a zero is replaced with the default rather than honoured.
func TestPruneStaleZeroAgeFallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	fresh := mkWorktreeDir(t, root, "task-fresh", time.Minute)

	res, err := PruneStale(context.Background(), root, 0, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if res.Count() != 0 {
		t.Fatalf("zero age removed %d dir(s); it must fall back to DefaultPruneAge", res.Count())
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh worktree removed under a zero age bound: %v", err)
	}
}

func TestPruneStaleEmptyRootErrors(t *testing.T) {
	if _, err := PruneStale(context.Background(), "  ", 24*time.Hour, false); err == nil {
		t.Fatal("expected an error for an empty workspace root")
	}
}

// Non-directory entries alongside the worktrees are left alone: the sweep owns
// per-run directories, not whatever else lands in the tree.
func TestPruneStaleIgnoresFiles(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, Subdir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stray := filepath.Join(parent, "notes.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	when := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stray, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	res, err := PruneStale(context.Background(), root, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if res.Count() != 0 {
		t.Fatalf("removed %d entries; files must be ignored", res.Count())
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file was removed: %v", err)
	}
}

func TestResolveWorkspaceRootPrecedence(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", "/from/fleet")
	t.Setenv("CHAT_WORKSPACE_ROOT", "/from/chat")

	if got := ResolveWorkspaceRoot("  /explicit  "); got != "/explicit" {
		t.Errorf("explicit value should win and be trimmed; got %q", got)
	}
	if got := ResolveWorkspaceRoot(""); got != "/from/fleet" {
		t.Errorf("FLEET_WORKSPACE_ROOT should win over the legacy key; got %q", got)
	}

	t.Setenv("FLEET_WORKSPACE_ROOT", "")
	if got := ResolveWorkspaceRoot(""); got != "/from/chat" {
		t.Errorf("legacy CHAT_WORKSPACE_ROOT should apply when the canonical key is unset; got %q", got)
	}
}

// TestPruneStaleFloorsAgeAtMinPruneAge: an operator-supplied age below the
// task wall-clock ceiling must be raised to MinPruneAge, not honoured — a
// small --older-than would otherwise reclaim live checkouts.
func TestPruneStaleFloorsAgeAtMinPruneAge(t *testing.T) {
	root := t.TempDir()
	live := mkWorktreeDir(t, root, "task-live", 2*time.Hour) // older than 10m, younger than the floor
	old := mkWorktreeDir(t, root, "task-old", MinPruneAge+time.Hour)

	res, err := PruneStale(context.Background(), root, 10*time.Minute, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a 2h-old worktree was reclaimed under a 10m age; the floor did not apply: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("worktree older than the floor survived (stat err %v)", err)
	}
	if res.Kept != 1 || res.Count() != 1 {
		t.Errorf("kept=%d removed=%d, want 1/1", res.Kept, res.Count())
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "floor") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning that the age was floored: %v", res.Warnings)
	}
}

// gitRepo initialises a real repository at root with one commit so worktrees
// can be added to it. Skips when git is unavailable.
func gitRepo(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-q", "-m", "init")
}

func gitWorktreeAdd(t *testing.T, root, name string, age time.Duration, lock bool) string {
	t.Helper()
	path := filepath.Join(root, Subdir, name)
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "--detach", path).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if lock {
		if out, err := exec.Command("git", "-C", root, "worktree", "lock", "--reason", "run in progress", path).CombinedOutput(); err != nil {
			t.Fatalf("git worktree lock: %v: %s", err, out)
		}
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPruneStaleCrossChecksGit pins the git cross-check: an aged worktree git
// reports as LOCKED is kept regardless of age; an aged one git knows is
// removed through git (so its admin record goes with it); an aged directory
// git does not list is removed with the plain fallback.
func TestPruneStaleCrossChecksGit(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	lockedWT := gitWorktreeAdd(t, root, "task-locked", 48*time.Hour, true)
	staleWT := gitWorktreeAdd(t, root, "task-stale", 48*time.Hour, false)
	unknown := mkWorktreeDir(t, root, "task-unknown", 48*time.Hour)

	res, err := PruneStale(context.Background(), root, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if _, err := os.Stat(lockedWT); err != nil {
		t.Fatalf("locked worktree was reclaimed: %v", err)
	}
	if _, err := os.Stat(staleWT); !os.IsNotExist(err) {
		t.Errorf("stale git worktree survived (stat err %v)", err)
	}
	if _, err := os.Stat(unknown); !os.IsNotExist(err) {
		t.Errorf("directory unknown to git survived (stat err %v)", err)
	}
	if res.Count() != 2 || res.Kept != 1 {
		t.Errorf("removed=%v kept=%d, want 2 removed / 1 kept", res.Removed, res.Kept)
	}
	// git's own record of the stale worktree is gone, the locked one remains.
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "task-stale") {
		t.Errorf("git still lists the removed worktree:\n%s", out)
	}
	if !strings.Contains(string(out), "task-locked") || !strings.Contains(string(out), "locked") {
		t.Errorf("git lost the locked worktree:\n%s", out)
	}
}
