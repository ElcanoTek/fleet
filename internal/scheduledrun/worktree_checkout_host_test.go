//go:build fleet_host_executor

package scheduledrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/worktree"
)

// TestCheckoutInSandboxFillsTheWorktree: the worktree the host creates holds
// no files; checkoutInSandbox fills it from the branch's commit through the
// sandbox (here the test-only host executor standing in for the container).
func TestCheckoutInSandboxFillsTheWorktree(t *testing.T) {
	repo := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "a.txt")
	gitOut(t, repo, "commit", "-q", "-m", "a")
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(wt.path, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("the host checked files out (stat err %v)", err)
	}
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	if err := checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base); err != nil {
		t.Fatalf("checkoutInSandbox: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(wt.path, "a.txt")); err != nil || string(got) != "hello\n" {
		t.Fatalf("a.txt = %q, %v; want the committed content", got, err)
	}
	if head := strings.TrimSpace(gitOut(t, wt.path, "symbolic-ref", "--short", "HEAD")); head != wt.branch {
		t.Fatalf("worktree HEAD = %q, want the run's branch %q (created in the sandbox)", head, wt.branch)
	}
}

// TestCheckoutInSandboxNeverResetsAnExistingBranch: a second checkout pass
// over the same worktree finds the run's branch already made and fails,
// naming it, instead of resetting the branch — the commit made on it stays.
func TestCheckoutInSandboxNeverResetsAnExistingBranch(t *testing.T) {
	repo := initGitRepo(t)
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	if err := checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	gitOut(t, wt.path, "commit", "--allow-empty", "-q", "-m", "work on the run's branch")
	before := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch))
	err = checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second pass: err = %v, want the existing branch reported", err)
	}
	if after := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch)); after != before {
		t.Fatalf("the run's branch moved from %s to %s", before, after)
	}
}

// TestCheckoutInSandboxSkipsRepositoryHooks: a repository hook that fails on
// checkout (git-lfs installs a post-checkout hook) does not fail fleet's
// checkout, and does not run during it.
func TestCheckoutInSandboxSkipsRepositoryHooks(t *testing.T) {
	repo := initGitRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	if err := checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base); err != nil {
		t.Fatalf("checkoutInSandbox: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the repository's post-checkout hook ran during fleet's checkout (stat err %v)", err)
	}
}

// TestCheckoutInSandboxRecordsNoUpstream: a remote-tracking base checks out,
// and the new branch gets no upstream entry in the repository's config (which
// a worktree run's sandbox mounts read-only).
func TestCheckoutInSandboxRecordsNoUpstream(t *testing.T) {
	upstream := initGitRepo(t)
	repo := filepath.Join(t.TempDir(), "clone")
	gitOut(t, filepath.Dir(repo), "clone", "-q", upstream, repo)
	worktree.PinAtStartup(repo)
	if err := worktree.PrepareRepoAtStartup(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true, BaseBranch: "origin/HEAD"}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	if err := checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base); err != nil {
		t.Fatalf("checkoutInSandbox from origin/HEAD: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), wt.branch) {
		t.Fatalf("the repository config names the run's branch:\n%s", config)
	}
}

// TestCheckoutInSandboxNamesABadBase: a base that does not resolve fails the
// run in the sandbox, with the base in the message.
func TestCheckoutInSandboxNamesABadBase(t *testing.T) {
	repo := initGitRepo(t)
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true, BaseBranch: "no-such-branch"}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	err = checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base)
	if err == nil || !strings.Contains(err.Error(), `base "no-such-branch" does not name a commit`) {
		t.Fatalf("err = %v, want a failure naming the base", err)
	}
}

// TestCheckoutInSandboxSaysTheRepositoryHasNoCommits: with the default base
// and a workspace repository whose HEAD names a branch with no commits, the
// run fails saying so.
func TestCheckoutInSandboxSaysTheRepositoryHasNoCommits(t *testing.T) {
	repo := t.TempDir()
	gitOut(t, repo, "init", "-q")
	worktree.PinAtStartup(repo)
	if err := worktree.PrepareRepoAtStartup(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	err = checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base)
	if err == nil || !strings.Contains(err.Error(), "the workspace repository has no commits") {
		t.Fatalf("err = %v, want the empty repository named", err)
	}
}

// TestCheckoutInSandboxNamesAnOldGit: a sandbox git older than
// minSandboxGit fails the run naming its version, before the base is
// looked at (git before 2.20 cannot resolve main-worktree/HEAD, which would
// otherwise read as an empty repository).
func TestCheckoutInSandboxNamesAnOldGit(t *testing.T) {
	repo := initGitRepo(t)
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "git"), []byte("#!/bin/sh\necho 'git version 2.19.1'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	sb := sandbox.NewHost(nil)
	defer sb.Close()
	err = checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base)
	if err == nil || !strings.Contains(err.Error(), "2.19.1") || !strings.Contains(err.Error(), "older than "+minSandboxGit) {
		t.Fatalf("err = %v, want the old version named", err)
	}
}

// TestCheckoutInSandboxBaseHEADIsTheRepositorys: base_branch HEAD (or "@")
// starts the run's branch at the repository's HEAD, not at the new
// worktree's own unborn HEAD.
func TestCheckoutInSandboxBaseHEADIsTheRepositorys(t *testing.T) {
	repo := initGitRepo(t)
	gitOut(t, repo, "commit", "--allow-empty", "-q", "-m", "second")
	head := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
	for i, base := range []string{"HEAD", "@"} {
		wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true, BaseBranch: base}), fmt.Sprintf("run%d", i))
		if err != nil {
			t.Fatalf("prepareWorktree: %v", err)
		}
		sb := sandbox.NewHost(nil)
		if err := checkoutInSandbox(context.Background(), sb, wt.path, wt.branch, wt.base); err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		sb.Close()
		if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch)); got != head {
			t.Errorf("base %q: branch at %s, want the repository's HEAD %s", base, got, head)
		}
		cleanup()
	}
}
