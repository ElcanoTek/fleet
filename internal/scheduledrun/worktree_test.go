package scheduledrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// initGitRepo creates a git repo with one commit in a fresh temp dir and returns
// its path. Skips the test if git is unavailable.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runs := [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t.t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	}
	for _, args := range runs {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func worktreeTask(wc *models.WorktreeConfig) *models.Task {
	return &models.Task{ID: uuid.New(), WorktreeConfig: wc}
}

func TestPrepareWorktree_DisabledReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	// nil config
	wt, branch, cleanup, err := prepareWorktree(ctx, t.TempDir(), worktreeTask(nil), "run1")
	if err != nil {
		t.Fatalf("nil config: unexpected error: %v", err)
	}
	if wt != "" || branch != "" {
		t.Fatalf("nil config: want empty wtPath/branch, got %q/%q", wt, branch)
	}
	cleanup() // must be a safe no-op

	// explicitly disabled
	wt, _, cleanup, err = prepareWorktree(ctx, t.TempDir(), worktreeTask(&models.WorktreeConfig{Enabled: false}), "run1")
	if err != nil || wt != "" {
		t.Fatalf("disabled: want empty/no-error, got %q / %v", wt, err)
	}
	cleanup()
}

func TestPrepareWorktree_NonRepoRejected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir() // not a git repo
	_, _, _, err := prepareWorktree(context.Background(), dir, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err == nil {
		t.Fatal("expected error for non-git workspace, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error should mention non-git repo, got: %v", err)
	}
}

func TestPrepareWorktree_SubdirOfRepoRejected(t *testing.T) {
	repo := initGitRepo(t)
	sub := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := prepareWorktree(context.Background(), sub, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err == nil {
		t.Fatal("expected error for non-root subdir, got nil")
	}
	if !strings.Contains(err.Error(), "not its root") {
		t.Fatalf("error should mention non-root, got: %v", err)
	}
}

func TestPrepareWorktree_CreatesIsolatedWorktree(t *testing.T) {
	repo := initGitRepo(t)
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	wt, branch, cleanup, err := prepareWorktree(context.Background(), repo, task, "abc123")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}

	// Worktree path is under <repo>/.fleet-worktrees/<task>-<run>.
	wantParent := filepath.Join(repo, worktreeSubdir)
	if !strings.HasPrefix(wt, wantParent) {
		t.Fatalf("worktree %q not under %q", wt, wantParent)
	}
	if fi, statErr := os.Stat(wt); statErr != nil || !fi.IsDir() {
		t.Fatalf("worktree dir missing: %v", statErr)
	}

	// Branch name uses the default prefix.
	wantBranch := models.DefaultWorktreeBranchPrefix + task.ID.String() + "-abc123"
	if branch != wantBranch {
		t.Fatalf("branch = %q, want %q", branch, wantBranch)
	}

	// The worktree is checked out on that branch.
	if got := strings.TrimSpace(gitOut(t, wt, "rev-parse", "--abbrev-ref", "HEAD")); got != branch {
		t.Fatalf("worktree HEAD branch = %q, want %q", got, branch)
	}

	// git operations work inside the worktree (the .git linkage resolves because
	// the worktree is a subdir of the repo).
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, wt, "add", "-A")
	gitOut(t, wt, "commit", "-q", "-m", "wt change")

	// The main working tree's status is clean — the worktree subdir is excluded.
	if status := strings.TrimSpace(gitOut(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("main tree status should be clean (worktree excluded), got:\n%s", status)
	}

	// The branch is registered before cleanup.
	if !strings.Contains(gitOut(t, repo, "worktree", "list", "--porcelain"), wt) {
		t.Fatal("worktree not registered before cleanup")
	}

	cleanup()

	// After cleanup the worktree dir and the branch are gone.
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("worktree dir should be removed, stat err = %v", statErr)
	}
	if strings.Contains(gitOut(t, repo, "worktree", "list", "--porcelain"), wt) {
		t.Fatal("worktree still registered after cleanup")
	}
	branches := gitOut(t, repo, "branch", "--list", branch)
	if strings.TrimSpace(branches) != "" {
		t.Fatalf("branch %q should be deleted, still present: %q", branch, branches)
	}
}

func TestPrepareWorktree_CustomPrefixAndBase(t *testing.T) {
	repo := initGitRepo(t)
	initBranch := strings.TrimSpace(gitOut(t, repo, "symbolic-ref", "--short", "HEAD"))
	// A second commit on a named base branch to branch from.
	gitOut(t, repo, "checkout", "-q", "-b", "develop")
	gitOut(t, repo, "commit", "--allow-empty", "-q", "-m", "second")
	developHead := strings.TrimSpace(gitOut(t, repo, "rev-parse", "develop"))
	gitOut(t, repo, "checkout", "-q", initBranch)

	task := worktreeTask(&models.WorktreeConfig{Enabled: true, BranchPrefix: "iso/", BaseBranch: "develop"})
	wt, branch, cleanup, err := prepareWorktree(context.Background(), repo, task, "run9")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(branch, "iso/") {
		t.Fatalf("branch %q should use custom prefix iso/", branch)
	}
	// The worktree branched from develop's HEAD.
	if got := strings.TrimSpace(gitOut(t, wt, "rev-parse", "HEAD")); got != developHead {
		t.Fatalf("worktree HEAD = %q, want develop head %q", got, developHead)
	}
}

func TestEnsureWorktreeExcluded_Idempotent(t *testing.T) {
	repo := initGitRepo(t)
	for i := 0; i < 3; i++ {
		if err := ensureWorktreeExcluded(repo); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), worktreeSubdir+"/"); n != 1 {
		t.Fatalf("exclude line should appear exactly once, found %d:\n%s", n, data)
	}
}
