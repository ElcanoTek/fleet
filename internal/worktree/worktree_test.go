package worktree

import (
	"context"
	"os"
	"path/filepath"
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
