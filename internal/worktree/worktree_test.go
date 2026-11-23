package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// On the majority of boxes the workspace root is not a git repository at all.
// The sweep must stay quiet there — the git-record prune has nothing to clean
// and used to log an hourly WARNING the operator could not act on — while the
// filesystem sweep still does its real work.
func TestPruneStaleNonGitRootIsQuiet(t *testing.T) {
	root := t.TempDir()
	old := mkWorktreeDir(t, root, "task-old", 48*time.Hour)

	res, err := PruneStale(context.Background(), root, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("non-git root must not warn: %v", res.Warnings)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("aged worktree %s survived the sweep (stat err %v)", old, err)
	}
}

// A root whose .git is a gitfile (a linked worktree, here one pointing
// nowhere) is not a repository fleet records at startup: its worktree records
// are left alone, and the sweep says so once, while the directory sweep still
// does its work.
func TestPruneStaleGitfileRootLeavesRecordsAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /nonexistent/gitdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mkWorktreeDir(t, root, "task-old", 48*time.Hour)

	res, err := PruneStale(context.Background(), root, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "left alone") {
			found = true
		}
	}
	if !found {
		t.Errorf("want one warning that the records are left alone, got: %v", res.Warnings)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("aged worktree %s survived the sweep (stat err %v)", old, err)
	}
	// Said once per process, not every pass.
	res, _ = PruneStale(context.Background(), root, 24*time.Hour, false)
	for _, w := range res.Warnings {
		if strings.Contains(w, "left alone") {
			t.Errorf("the warning repeated on the second pass: %v", res.Warnings)
		}
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
		t.Errorf("FLEET_WORKSPACE_ROOT should apply; got %q", got)
	}

	t.Setenv("FLEET_WORKSPACE_ROOT", "")
	if got := ResolveWorkspaceRoot(""); got == "/from/chat" {
		t.Errorf("the removed CHAT_WORKSPACE_ROOT (ADR-0106) must not apply; got %q", got)
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
	// What fleet serve does at startup: record the repository and create
	// the directories host code and sandboxes rely on.
	PinAtStartup(root)
	if err := PrepareRepoAtStartup(context.Background(), root); err != nil {
		t.Fatalf("PrepareRepoAtStartup: %v", err)
	}
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

// TestPruneStaleRefusesASymlinkedSubdir: a .fleet-worktrees that is a
// symlink is an error and nothing it points at is swept, however old.
func TestPruneStaleRefusesASymlinkedSubdir(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	old := filepath.Join(target, "keep-me")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, Subdir)); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneStale(context.Background(), root, DefaultPruneAge, false); err == nil {
		t.Fatal("want an error for a symlinked worktree directory")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the symlink's target was swept: %v", err)
	}
}

// TestPruneStaleDoesNotFollowLinksInside: an aged entry that is a symlink to
// a directory elsewhere is not a worktree directory and is left alone, and
// so is its target.
func TestPruneStaleDoesNotFollowLinksInside(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, Subdir)
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	marker := filepath.Join(target, "file")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(target, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneStale(context.Background(), root, DefaultPruneAge, false); err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a link's target was swept: %v", err)
	}
}

// TestPruneStaleDoesNotFollowARecordSymlink: an entry under .git/worktrees
// that is a symlink — here a relative one leading out of the workspace, which
// needs no knowledge of host paths — is skipped, and what it points at is
// untouched. Covered for a root that became a repository after startup (a
// sandbox ran `git init` there) and for one that was a repository at startup.
func TestPruneStaleDoesNotFollowARecordSymlink(t *testing.T) {
	for _, atStartup := range []bool{false, true} {
		name := "repository created after startup"
		if atStartup {
			name = "repository at startup"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "workspace")
			sibling := filepath.Join(base, "data", "important")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(sibling, 0o755); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(sibling, "db.txt")
			if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if atStartup {
				gitRepo(t, root) // records it at "startup"
			} else {
				PinAtStartup(root)
				if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
					t.Fatalf("git init: %v: %s", err, out)
				}
			}
			if err := os.MkdirAll(filepath.Join(root, ".git", "worktrees"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../../data", filepath.Join(root, ".git", "worktrees", "x")); err != nil {
				t.Fatal(err)
			}
			if _, err := PruneStale(context.Background(), root, 24*time.Hour, false); err != nil {
				t.Fatalf("PruneStale: %v", err)
			}
			if _, err := os.Stat(keep); err != nil {
				t.Fatalf("the sweep deleted a file outside the workspace: %v", err)
			}
		})
	}
}

// TestPruneStaleRemovesRecordsWithoutFollowingLinksInside: a stale fleet
// record holding a symlink out of the workspace is removed as a link, and
// the target survives.
func TestPruneStaleRemovesRecordsWithoutFollowingLinksInside(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	PinAtStartup(root)
	record := filepath.Join(root, ".git", "worktrees", "task-gone")
	if err := os.MkdirAll(record, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(root, Subdir, "task-gone", ".git")
	if err := os.WriteFile(filepath.Join(record, "gitdir"), []byte(gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(record, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneStale(context.Background(), root, 24*time.Hour, false); err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if _, err := os.Lstat(record); !os.IsNotExist(err) {
		t.Fatalf("the stale record survived (lstat err %v)", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a file behind a link inside the record was removed: %v", err)
	}
}

// TestRemoveWorktree removes a worktree's directory and the repository's
// record without git, refuses a locked one, and never follows a link.
func TestRemoveWorktree(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	PinAtStartup(root)
	wt := gitWorktreeAdd(t, root, "task-run", time.Minute, false)
	locked := gitWorktreeAdd(t, root, "task-locked", time.Minute, true)
	if err := RemoveWorktree(context.Background(), root, "task-run"); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir survived (stat err %v)", err)
	}
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "task-run") {
		t.Fatalf("git still lists the removed worktree:\n%s", out)
	}
	if err := RemoveWorktree(context.Background(), root, "task-locked"); err == nil {
		t.Fatal("a locked worktree was removed")
	}
	if _, err := os.Stat(locked); err != nil {
		t.Fatalf("locked worktree dir was removed: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if err := RemoveWorktree(context.Background(), root, bad); err == nil {
			t.Errorf("RemoveWorktree(%q): want a refusal", bad)
		}
	}
}

// TestCheckRefusesWhatPointsElsewhere: Check passes for the pinned repository
// and fails for a commondir, alternates, a replaced .git, a symlinked refs/.
func TestCheckRefusesWhatPointsElsewhere(t *testing.T) {
	ctx := context.Background()
	fresh := func(t *testing.T) (string, RepoPin) {
		root := t.TempDir()
		gitRepo(t, root)
		return root, PinAtStartup(root)
	}
	root, pin := fresh(t)
	if err := pin.Check(ctx, root); err != nil {
		t.Fatalf("Check on the pinned repository: %v", err)
	}
	cases := map[string]func(t *testing.T, root string){
		"commondir": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".git", "commondir"), []byte("/elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"alternates": func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, ".git", "objects", "info"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte("/elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"replaced .git": func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, ".git"), filepath.Join(t.TempDir(), "g")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked refs": func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, ".git", "refs"), filepath.Join(root, ".git", "refs-real")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("refs-real", filepath.Join(root, ".git", "refs")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root, pin := fresh(t)
			mutate(t, root)
			if err := pin.Check(ctx, root); err == nil {
				t.Fatal("Check passed")
			}
		})
	}
}

// TestRecordsMustNameEachOther: a live run can rewrite its own checkout's .git
// and its own record, but a record only counts as a worktree's when both name
// each other. A record claiming another run's checkout, with a lock, protects
// nothing and removes nothing of that run's.
func TestRecordsMustNameEachOther(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	victim := gitWorktreeAdd(t, root, "task-victim", 48*time.Hour, false)
	gitWorktreeAdd(t, root, "task-claimer", time.Minute, false)
	// The claimer's record now names the victim's checkout and is locked.
	claimRec := filepath.Join(root, ".git", "worktrees", "task-claimer")
	if err := os.WriteFile(filepath.Join(claimRec, "gitdir"), []byte(filepath.Join(victim, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimRec, "locked"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneStale(context.Background(), root, 24*time.Hour, false); err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("the aged victim was kept by a lock in a record that is not its own (stat err %v)", err)
	}
	if _, err := os.Stat(claimRec); err != nil {
		t.Fatalf("the claimer's own record was removed as the victim's: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "worktrees", "task-victim")); !os.IsNotExist(err) {
		t.Fatalf("the victim's real record survived (stat err %v)", err)
	}
}

// TestDeleteBranch removes a branch's loose ref, reflog and packed-refs entry
// without git, and never acts through a symlinked directory.
func TestDeleteBranch(t *testing.T) {
	ctx := context.Background()
	gitIn := func(t *testing.T, root string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	t.Run("loose, reflog and packed", func(t *testing.T) {
		root := t.TempDir()
		gitRepo(t, root)
		gitIn(t, root, "branch", "fleet/packed")
		gitIn(t, root, "pack-refs", "--all")
		gitIn(t, root, "-c", "core.logAllRefUpdates=always", "branch", "fleet/loose")
		for _, b := range []string{"fleet/loose", "fleet/packed"} {
			if err := DeleteBranch(ctx, root, b); err != nil {
				t.Fatalf("DeleteBranch(%s): %v", b, err)
			}
		}
		if out := gitIn(t, root, "branch", "--list", "fleet/*"); strings.TrimSpace(out) != "" {
			t.Fatalf("branches survived: %q", out)
		}
		if _, err := os.Stat(filepath.Join(root, ".git", "logs", "refs", "heads", "fleet", "loose")); !os.IsNotExist(err) {
			t.Fatalf("reflog survived (stat err %v)", err)
		}
		gitIn(t, root, "fsck", "--no-progress")
	})
	t.Run("symlinked ref directory", func(t *testing.T) {
		root := t.TempDir()
		gitRepo(t, root)
		outside := t.TempDir()
		keep := filepath.Join(outside, "task-1")
		if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, ".git", "refs", "heads", "fleet")); err != nil {
			t.Fatal(err)
		}
		if err := DeleteBranch(ctx, root, "fleet/task-1"); err == nil {
			t.Fatal("want a refusal for a symlinked ref directory")
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("a file outside the workspace was removed: %v", err)
		}
	})
}

// TestCheckObjectsInfo: a missing or symlinked objects/info is a refusal,
// never read as "no alternates"; PrepareRepoAtStartup creates it; a reftable
// repository is refused.
func TestCheckObjectsInfo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	info := filepath.Join(root, ".git", "objects", "info")
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}
	if err := Pin(root).Check(ctx, root); err == nil {
		t.Fatal("Check passed with no objects/info")
	}
	if err := os.Symlink(t.TempDir(), info); err != nil {
		t.Fatal(err)
	}
	if err := Pin(root).Check(ctx, root); err == nil {
		t.Fatal("Check passed with a symlinked objects/info")
	}
	if err := os.Remove(info); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRepoAtStartup(ctx, root); err != nil {
		t.Fatalf("PrepareRepoAtStartup: %v", err)
	}
	if err := Pin(root).Check(ctx, root); err != nil {
		t.Fatalf("Check after PrepareRepoAtStartup: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git", "reftable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Pin(root).Check(ctx, root); err == nil || !strings.Contains(err.Error(), "files ref storage") {
		t.Fatalf("reftable repository: err = %v, want a refusal naming the ref storage", err)
	}
}

// TestPinIsOnlyRecordedAtStartup: a root nothing recorded is not a
// repository to host-side worktree code, however it looks now.
func TestPinIsOnlyRecordedAtStartup(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if Pin(root).IsRepo() {
		t.Fatal("an unrecorded root was treated as a repository")
	}
	if err := Pin(root).Check(context.Background(), root); err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("err = %v, want a refusal for an unrecorded root", err)
	}
	if Pin(root).IsRepo() {
		t.Fatal("Pin recorded the root on use")
	}
}

// TestCheckRefusesALayoutTheHostWouldFollow: every path inside .git that the
// host's worktree code writes, or a worktree run's git reads, must be what it
// claims — a symlinked worktrees/, logs/ or info/, or a HEAD, config or
// packed-refs that is a link or a FIFO, is a refusal, so the host never acts
// through it.
func TestCheckRefusesALayoutTheHostWouldFollow(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(t *testing.T, gitDir string){
		"symlinked worktrees": func(t *testing.T, gitDir string) {
			if err := os.RemoveAll(filepath.Join(gitDir, "worktrees")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(gitDir, "worktrees")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked logs": func(t *testing.T, gitDir string) {
			if err := os.RemoveAll(filepath.Join(gitDir, "logs")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(gitDir, "logs")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked lfs": func(t *testing.T, gitDir string) {
			if err := os.RemoveAll(filepath.Join(gitDir, "lfs")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(gitDir, "lfs")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked info": func(t *testing.T, gitDir string) {
			if err := os.RemoveAll(filepath.Join(gitDir, "info")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(gitDir, "info")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked config": func(t *testing.T, gitDir string) {
			if err := os.Rename(filepath.Join(gitDir, "config"), filepath.Join(gitDir, "config.real")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("config.real", filepath.Join(gitDir, "config")); err != nil {
				t.Fatal(err)
			}
		},
		"FIFO HEAD": func(t *testing.T, gitDir string) {
			if err := os.Remove(filepath.Join(gitDir, "HEAD")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(gitDir, "HEAD"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"FIFO packed-refs": func(t *testing.T, gitDir string) {
			if err := syscall.Mkfifo(filepath.Join(gitDir, "packed-refs"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			gitRepo(t, root)
			mutate(t, filepath.Join(root, ".git"))
			if err := Pin(root).Check(ctx, root); err == nil {
				t.Fatal("Check passed")
			}
		})
	}
}

// TestPrepareRepoAtStartup creates the directories a repository may lack
// (worktrees/, logs/refs/heads/, objects/info/, info/, lfs/) and reports one
// that exists as a symlink instead of following it.
func TestPrepareRepoAtStartup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	for _, rel := range []string{"worktrees", "logs", filepath.Join("objects", "info"), "info", "lfs"} {
		if err := os.RemoveAll(filepath.Join(root, ".git", rel)); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareRepoAtStartup(ctx, root); err != nil {
		t.Fatalf("PrepareRepoAtStartup: %v", err)
	}
	for _, rel := range []string{"worktrees", filepath.Join("logs", "refs", "heads"), filepath.Join("objects", "info"), "info", "lfs"} {
		if fi, err := os.Lstat(filepath.Join(root, ".git", rel)); err != nil || !fi.IsDir() {
			t.Errorf("%s not created as a directory: %v", rel, err)
		}
	}
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(root, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRepoAtStartup(ctx, root); err == nil {
		t.Fatal("a symlinked .git/worktrees was accepted")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was created through the symlink: %v", entries)
	}
}

// TestRecordReadsDoNotBlockOnAFIFO: a run that swaps its record's gitdir
// between a regular file and a FIFO cannot make List, RemoveWorktree or
// the sweep block: every host read opens with O_NONBLOCK and requires a
// regular file.
func TestRecordReadsDoNotBlockOnAFIFO(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	gitWorktreeAdd(t, root, "swapper", 0, false)
	rec := filepath.Join(root, ".git", "worktrees", "swapper")
	orig, err := os.ReadFile(filepath.Join(rec, "gitdir"))
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg, fifo := filepath.Join(rec, "reg"), filepath.Join(rec, "fifo")
		for !stop.Load() {
			_ = os.WriteFile(reg, orig, 0o644)
			_ = os.Rename(reg, filepath.Join(rec, "gitdir"))
			_ = syscall.Mkfifo(fifo, 0o644)
			_ = os.Rename(fifo, filepath.Join(rec, "gitdir"))
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		finished := make(chan struct{})
		go func() {
			_ = RemoveWorktree(context.Background(), root, "not-there")
			_, _ = List(context.Background(), root)
			_, _ = PruneStale(context.Background(), root, DefaultPruneAge, true)
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			stop.Store(true)
			<-done
			t.Fatal("a host record read blocked on a FIFO swapped in for the record's gitdir")
		}
	}
	stop.Store(true)
	<-done
}

// TestDropPackedRefTakesTheLockFirst: while someone else holds
// packed-refs.lock, a branch deletion neither reads nor rewrites packed-refs
// (it waits, then reports the lock); concurrent deletions of different
// packed branches all land, and refs nobody deleted survive.
func TestDropPackedRefTakesTheLockFirst(t *testing.T) {
	ctx := context.Background()
	// The concurrent half below queues 8 writers on one lock; give them far
	// more than git's 1s so a slow (or -race) machine cannot turn queueing
	// into a failure.
	prevWait := packedRefsLockWait
	packedRefsLockWait = time.Minute
	t.Cleanup(func() { packedRefsLockWait = prevWait })
	root := t.TempDir()
	gitRepo(t, root)
	gitIn := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	var branches []string
	for i := 0; i < 8; i++ {
		b := fmt.Sprintf("fleet/b%d", i)
		gitIn("branch", b)
		branches = append(branches, b)
	}
	gitIn("pack-refs", "--all")
	packed := filepath.Join(root, ".git", "packed-refs")
	before, err := os.ReadFile(packed)
	if err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(root, ".git", "packed-refs.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	packedRefsLockWait = 200 * time.Millisecond
	err = DeleteBranch(ctx, root, branches[0])
	packedRefsLockWait = time.Minute
	if err == nil || !strings.Contains(err.Error(), "lock packed-refs") {
		t.Fatalf("with the lock held: err = %v, want a lock refusal", err)
	}
	if after, _ := os.ReadFile(packed); string(after) != string(before) {
		t.Fatal("packed-refs changed while another writer held the lock")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(branches))
	for i, b := range branches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = DeleteBranch(ctx, root, b)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("DeleteBranch(%s): %v", branches[i], err)
		}
	}
	if out := gitIn("branch", "--list", "fleet/*"); strings.TrimSpace(out) != "" {
		t.Fatalf("branches came back or survived: %q", out)
	}
	if out := gitIn("rev-parse", "--verify", "main"); strings.TrimSpace(out) == "" {
		t.Fatal("main was lost")
	}
}

// TestListSanitizesWhatARunWrote: a lock reason a run wrote is shown with
// its control characters replaced.
func TestListSanitizesWhatARunWrote(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	gitWorktreeAdd(t, root, "task-run", time.Minute, false)
	if err := os.WriteFile(filepath.Join(root, ".git", "worktrees", "task-run", "locked"), []byte("\x1b[2Jcleared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := List(context.Background(), root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %+v, %v", entries, err)
	}
	if got := SanitizeForTerminal(entries[0].Locked); strings.ContainsRune(got, 0x1b) || !strings.Contains(got, "cleared") {
		t.Fatalf("sanitized lock reason = %q", got)
	}
}

// TestCreateWorktree writes the record and the checkout's .git the way
// `git worktree add --no-checkout` does, without running git: git then sees
// the worktree, and `git switch --create <branch> <base>` in it creates
// the branch and fills the checkout. An existing name, a bad branch name and
// a repository not recorded at startup are refused.
func TestCreateWorktree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	if err := os.Mkdir(filepath.Join(root, Subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := CreateWorktree(ctx, root, "task-run", "fleet/task-run")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if want := filepath.Join(root, ".git", "worktrees", "task-run"); record != want {
		t.Fatalf("record = %q, want %q", record, want)
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	checkout := filepath.Join(root, Subdir, "task-run")
	for path, want := range map[string]string{
		filepath.Join(record, "gitdir"):    filepath.Join(checkout, ".git") + "\n",
		filepath.Join(record, "commondir"): "../..\n",
		filepath.Join(record, "HEAD"):      "ref: refs/heads/fleet/task-run\n",
		filepath.Join(checkout, ".git"):    "gitdir: " + record + "\n",
	} {
		if got := read(path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(record, "locked")); !os.IsNotExist(err) {
		t.Errorf("the initialization lock was left behind (lstat err %v)", err)
	}
	gitIn := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	if !strings.Contains(gitIn(root, "worktree", "list", "--porcelain"), checkout) {
		t.Fatal("git does not list the worktree")
	}
	gitIn(checkout, "switch", "--quiet", "--create", "fleet/task-run", "main-worktree/HEAD")
	if _, err := os.Stat(filepath.Join(checkout, "README")); err != nil {
		t.Fatalf("the checkout was not filled: %v", err)
	}
	if head := strings.TrimSpace(gitIn(checkout, "symbolic-ref", "--short", "HEAD")); head != "fleet/task-run" {
		t.Fatalf("HEAD = %q", head)
	}

	if _, err := CreateWorktree(ctx, root, "task-run", "fleet/other"); err == nil {
		t.Error("an existing worktree name was accepted")
	}
	// The existing worktree is left as it was.
	if got := read(filepath.Join(record, "HEAD")); got != "ref: refs/heads/fleet/task-run\n" {
		t.Errorf("the existing record's HEAD = %q", got)
	}
	if _, err := os.Stat(filepath.Join(checkout, "README")); err != nil {
		t.Errorf("the existing checkout lost its files: %v", err)
	}
	// A checkout directory that exists without a record: the call fails,
	// leaves the directory alone and removes the record it had started.
	stray := filepath.Join(root, Subdir, "task-stray")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateWorktree(ctx, root, "task-stray", "fleet/task-stray"); err == nil {
		t.Error("an existing checkout directory was accepted")
	}
	if got := read(filepath.Join(stray, "keep.txt")); got != "keep\n" {
		t.Errorf("the existing checkout directory changed: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(root, ".git", "worktrees", "task-stray")); !os.IsNotExist(err) {
		t.Errorf("the record the failed call started is still there (lstat err %v)", err)
	}
	for _, bad := range []string{"", "-x", "/x", "a..b", "a\nb", "a b"} {
		if _, err := CreateWorktree(ctx, root, "task-bad", bad); err == nil {
			t.Errorf("branch %q was accepted", bad)
		}
	}
	unrecorded := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", unrecorded).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if _, err := CreateWorktree(ctx, unrecorded, "task-run", "fleet/task-run"); err == nil {
		t.Error("a repository not recorded at startup was accepted")
	}
}

// TestReadsRefuseALinkInsideTheDirectory: os.Root follows a link that stays
// inside its directory even with O_NOFOLLOW, so record reads Lstat first:
// a record gitdir that is a link to another file in .git/worktrees is not
// read (the record is not proven), and neither is a lock that is a link.
func TestReadsRefuseALinkInsideTheDirectory(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root)
	gitWorktreeAdd(t, root, "task-run", time.Minute, false)
	rec := filepath.Join(root, ".git", "worktrees", "task-run")
	if err := os.Rename(filepath.Join(rec, "gitdir"), filepath.Join(rec, "gitdir.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gitdir.real", filepath.Join(rec, "gitdir")); err != nil {
		t.Fatal(err)
	}
	entries, err := List(context.Background(), root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %+v, %v", entries, err)
	}
	if entries[0].RecordID != "" {
		t.Fatalf("a record whose gitdir is a link was taken as proven: %+v", entries[0])
	}
}

// TestPruneReclaimsWhatAStoppedCreateLeft: a server stopped part-way through
// CreateWorktree leaves either a record holding only its "initializing" lock
// (stopped before gitdir) or a complete pair whose lock was never removed.
// Past the prune age the sweep removes both; before it, both are kept.
func TestPruneReclaimsWhatAStoppedCreateLeft(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	if err := os.Mkdir(filepath.Join(root, Subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * DefaultPruneAge)
	records := filepath.Join(root, ".git", "worktrees")
	half := func(name string, at time.Time) string {
		t.Helper()
		dir := filepath.Join(records, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(dir, "locked")
		if err := os.WriteFile(lock, []byte("initializing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lock, at, at); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	oldHalf := half("old-half", old)
	freshHalf := half("fresh-half", time.Now())
	// A record with no gitdir but more than the lock is not one CreateWorktree
	// left: it stays.
	otherHalf := half("other-half", old)
	if err := os.WriteFile(filepath.Join(otherHalf, "HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pair := func(name, lock string) (string, string) {
		t.Helper()
		record, err := CreateWorktree(ctx, root, name, "fleet/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(record, "locked"), []byte(lock+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		checkout := filepath.Join(root, Subdir, name)
		if err := os.Chtimes(checkout, old, old); err != nil {
			t.Fatal(err)
		}
		return record, checkout
	}
	initRecord, initCheckout := pair("stopped", "initializing")
	keptRecord, keptCheckout := pair("held", "in use by the operator")

	if _, err := PruneStale(ctx, root, DefaultPruneAge, false); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{oldHalf, initRecord, initCheckout} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s was kept (lstat err %v)", gone, err)
		}
	}
	for _, kept := range []string{freshHalf, otherHalf, keptRecord, keptCheckout} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

// TestDropPackedRefKeepsTheFileMode: rewriting packed-refs to drop a run's
// branch keeps the file's permissions.
func TestDropPackedRefKeepsTheFileMode(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	out, err := exec.Command("git", "-C", root, "branch", "fleet/task-run").CombinedOutput()
	if err != nil {
		t.Fatalf("git branch: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "pack-refs", "--all").CombinedOutput(); err != nil {
		t.Fatalf("git pack-refs: %v %s", err, out)
	}
	packed := filepath.Join(root, ".git", "packed-refs")
	if err := os.Chmod(packed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteBranch(ctx, root, "fleet/task-run"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	info, err := os.Stat(packed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("packed-refs mode = %o, want 600", info.Mode().Perm())
	}
	if b, _ := os.ReadFile(packed); strings.Contains(string(b), "fleet/task-run") {
		t.Errorf("packed-refs still names the branch:\n%s", b)
	}
}

// TestUnfinishedRecordReadsAtMostTwoEntries: a record directory with more
// than its initializing lock (a run's own record can hold any number of
// entries) is not an unfinished record, and is judged from the first two
// entries only.
func TestUnfinishedRecordReadsAtMostTwoEntries(t *testing.T) {
	admin, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.Mkdir("rec", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := admin.WriteFile(filepath.Join("rec", "locked"), []byte("initializing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := unfinishedRecord(admin, "rec"); !ok {
		t.Fatal("a record holding only its initializing lock is unfinished")
	}
	for i := 0; i < 2000; i++ {
		if err := admin.WriteFile(filepath.Join("rec", fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := unfinishedRecord(admin, "rec"); ok {
		t.Fatal("a record with more entries counted as unfinished")
	}
}

// TestRemoveWorktreeRefusesALockedOne: RemoveWorktree reports ErrLocked for
// a worktree whose proven record is locked, and removes nothing.
func TestRemoveWorktreeRefusesALockedOne(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	if err := os.Mkdir(filepath.Join(root, Subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := CreateWorktree(ctx, root, "task-run", "fleet/task-run")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record, "locked"), []byte("keep \x1b[31mred\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = RemoveWorktree(ctx, root, "task-run")
	if !errors.Is(err, ErrLocked) || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("err = %q, want ErrLocked with the reason's control characters replaced", err)
	}
	for _, p := range []string{record, filepath.Join(root, Subdir, "task-run", ".git")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

// TestRemoveWorktreeKeepsTheRecordOfACheckoutItCannotRemove: the record goes
// only once the checkout is gone; a checkout with a directory this process
// cannot search keeps its record, and the error is not ErrRecordNotRemoved.
func TestRemoveWorktreeKeepsTheRecordOfACheckoutItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches a mode-000 directory")
	}
	ctx := context.Background()
	root := t.TempDir()
	gitRepo(t, root)
	if err := os.Mkdir(filepath.Join(root, Subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := CreateWorktree(ctx, root, "task-run", "fleet/task-run")
	if err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(root, Subdir, "task-run", "stuck")
	if err := os.MkdirAll(filepath.Join(stuck, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })
	err = RemoveWorktree(ctx, root, "task-run")
	if err == nil || errors.Is(err, ErrRecordNotRemoved) {
		t.Fatalf("err = %v, want a checkout-removal error", err)
	}
	if _, err := os.Lstat(record); err != nil {
		t.Errorf("the record went while the checkout survived: %v", err)
	}
}
