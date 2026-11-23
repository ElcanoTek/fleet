package boxdoctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/worktree"
)

func TestCheckWorktreeGit(t *testing.T) {
	ctx := context.Background()
	if c := CheckWorktreeGit(ctx, ""); c.Status != StatusSkip {
		t.Errorf("no root: %+v, want a skip", c)
	}
	plain := t.TempDir()
	if c := CheckWorktreeGit(ctx, plain); c.Status != StatusSkip {
		t.Errorf("not a repository: %+v, want a skip", c)
	}
	gitfile := t.TempDir()
	if err := os.WriteFile(filepath.Join(gitfile, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := CheckWorktreeGit(ctx, gitfile); c.Status != StatusWarn {
		t.Errorf("gitfile root: %+v, want a warning", c)
	}
	reftable := t.TempDir()
	if err := os.MkdirAll(filepath.Join(reftable, ".git", "reftable"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree.PinAtStartup(reftable)
	if c := CheckWorktreeGit(ctx, reftable); c.Status != StatusWarn {
		t.Errorf("reftable repository: %+v, want a warning", c)
	}
	// A layout the worktree check refuses is not reported as fine: a
	// recorded repository whose .git/worktrees is a symlink.
	linked := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", linked).CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	worktree.PinAtStartup(linked)
	if err := worktree.PrepareRepoAtStartup(ctx, linked); err != nil {
		t.Fatal(err)
	}
	if c := CheckWorktreeGit(ctx, linked); c.Status != StatusOK {
		t.Errorf("sound repository: %+v, want OK", c)
	}
	if err := os.Remove(filepath.Join(linked, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if c := CheckWorktreeGit(ctx, linked); c.Status != StatusWarn {
		t.Errorf("symlinked .git/worktrees: %+v, want a warning", c)
	}
}

// TestCheckWorktreeGitSkipsARootItCannotRead: a doctor run as a user who
// cannot search the root's parent (the standard layout's /var/lib/fleet is
// 0700) skips the check instead of warning that worktree tasks are refused.
func TestCheckWorktreeGitSkipsARootItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches a mode-000 directory")
	}
	home := filepath.Join(t.TempDir(), "home")
	ws := filepath.Join(home, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })
	worktree.PinAtStartup(ws)
	c := CheckWorktreeGit(context.Background(), ws)
	if c.Status != StatusSkip || !strings.Contains(c.Detail, "cannot read") {
		t.Errorf("status=%v detail=%q, want a skip saying the root cannot be read", c.Status, c.Detail)
	}
}

// TestCheckWorktreeGitAcceptsAFreshRepository: a freshly `git init`ed root,
// without the directories the server creates inside .git when it starts
// (worktrees/, logs/, …), is usable, not reported as refused.
func TestCheckWorktreeGitAcceptsAFreshRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	_ = os.RemoveAll(filepath.Join(root, ".git", "logs"))
	worktree.PinAtStartup(root)
	if c := CheckWorktreeGit(context.Background(), root); c.Status != StatusOK {
		t.Errorf("status=%v detail=%q, want OK", c.Status, c.Detail)
	}
	// A directory the server does not create is still required.
	if err := os.RemoveAll(filepath.Join(root, ".git", "refs")); err != nil {
		t.Fatal(err)
	}
	if c := CheckWorktreeGit(context.Background(), root); c.Status != StatusWarn {
		t.Errorf("without refs/: status=%v, want a warning", c.Status)
	}
}

// TestCheckWorktreeGitNotesAShallowClone: a usable shallow repository is
// ok, with the note that fetches changing its depth fail in worktree runs.
func TestCheckWorktreeGitNotesAShallowClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	worktree.PinAtStartup(root)
	if c := CheckWorktreeGit(context.Background(), root); c.Status != StatusOK || strings.Contains(c.Detail, "shallow") {
		t.Fatalf("full clone: status=%v detail=%q", c.Status, c.Detail)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := CheckWorktreeGit(context.Background(), root)
	if c.Status != StatusOK || !strings.HasSuffix(c.Detail, ShallowNote) {
		t.Errorf("shallow clone: status=%v detail=%q, want ok with the shallow note", c.Status, c.Detail)
	}
}
