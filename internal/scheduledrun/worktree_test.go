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
	"github.com/ElcanoTek/fleet/internal/worktree"
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
	// What fleet serve does at startup: record the workspace repository and
	// create the directories host code and sandboxes rely on.
	worktree.PinAtStartup(dir)
	if err := worktree.PrepareRepoAtStartup(context.Background(), dir); err != nil {
		t.Fatalf("PrepareRepoAtStartup: %v", err)
	}
	return dir
}

// sandboxCheckout does in wt what checkoutInSandbox has the run's sandbox do:
// create the run's branch from its base and fill the worktree.
func sandboxCheckout(t *testing.T, run *runWorktree) {
	t.Helper()
	gitOut(t, run.path, "-c", "core.hooksPath=/dev/null", "switch", "--quiet", "--no-track", "--create", run.branch, run.base)
	run.checkedOut.Store(true)
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
	wt, cleanup, err := prepareWorktree(ctx, t.TempDir(), worktreeTask(nil), "run1")
	if err != nil {
		t.Fatalf("nil config: unexpected error: %v", err)
	}
	if wt != nil || wt.dir() != "" {
		t.Fatalf("nil config: want no worktree, got %+v", wt)
	}
	cleanup() // must be a safe no-op

	// explicitly disabled
	wt, cleanup, err = prepareWorktree(ctx, t.TempDir(), worktreeTask(&models.WorktreeConfig{Enabled: false}), "run1")
	if err != nil || wt != nil {
		t.Fatalf("disabled: want no worktree/no error, got %+v / %v", wt, err)
	}
	cleanup()
}

func TestPrepareWorktree_NonRepoRejected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir() // not a git repo
	worktree.PinAtStartup(dir)
	_, _, err := prepareWorktree(context.Background(), dir, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err == nil {
		t.Fatal("expected error for non-git workspace, got nil")
	}
	if !strings.Contains(err.Error(), "had no .git") {
		t.Fatalf("error should say the workspace had no .git, got: %v", err)
	}
}

func TestPrepareWorktree_SubdirOfRepoRejected(t *testing.T) {
	repo := initGitRepo(t)
	sub := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	worktree.PinAtStartup(sub)
	_, _, err := prepareWorktree(context.Background(), sub, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err == nil {
		t.Fatal("expected error for non-root subdir, got nil")
	}
	// A subdirectory of a repository has no .git of its own: it is refused
	// before git is asked anything.
	if !strings.Contains(err.Error(), "had no .git") {
		t.Fatalf("error should say the workspace has no .git of its own, got: %v", err)
	}
}

func TestPrepareWorktree_CreatesIsolatedWorktree(t *testing.T) {
	repo := initGitRepo(t)
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	run, cleanup, err := prepareWorktree(context.Background(), repo, task, "abc123")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	wt, branch := run.path, run.branch
	// The host only registers the worktree; the run's sandbox creates the
	// branch and fills it (checkoutInSandbox). Do here what the sandbox does.
	if entries, _ := os.ReadDir(wt); len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("host-created worktree should hold only its .git file, got %v", entries)
	}
	if refs := gitOut(t, repo, "for-each-ref", "refs/heads/"+branch); strings.TrimSpace(refs) != "" {
		t.Fatalf("the host created the branch: %q", refs)
	}
	if run.base != "main-worktree/HEAD" {
		t.Fatalf("default base = %q, want the repository's HEAD (main-worktree/HEAD)", run.base)
	}
	sandboxCheckout(t, run)
	if want := filepath.Join(repo, ".git", "worktrees", filepath.Base(wt)); run.adminDir != want {
		t.Fatalf("adminDir = %q, want %q", run.adminDir, want)
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
	run, cleanup, err := prepareWorktree(context.Background(), repo, task, "run9")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	wt, branch := run.path, run.branch
	sandboxCheckout(t, run)

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
		if err := ensureWorktreeExcluded(context.Background(), repo); err != nil {
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

// TestPrepareWorktree_ACollisionLeavesTheExistingWorktree: when a run's
// worktree name is already taken, prepareWorktree fails and leaves the
// worktree, record and branch already there exactly as they were.
func TestPrepareWorktree_ACollisionLeavesTheExistingWorktree(t *testing.T) {
	repo := initGitRepo(t)
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	first, cleanup, err := prepareWorktree(context.Background(), repo, task, "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	defer cleanup()
	sandboxCheckout(t, first)
	gitOut(t, first.path, "commit", "--allow-empty", "-q", "-m", "work")
	tip := strings.TrimSpace(gitOut(t, repo, "rev-parse", first.branch))

	if _, again, err := prepareWorktree(context.Background(), repo, task, "run1"); err == nil {
		again()
		t.Fatal("a second worktree under the same name was created")
	}
	for _, p := range []string{filepath.Join(first.path, ".git"), filepath.Join(first.adminDir, "gitdir")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the existing worktree lost %s: %v", p, err)
		}
	}
	if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", first.branch)); got != tip {
		t.Errorf("the existing branch is at %s, want %s", got, tip)
	}
}

// TestPrepareWorktree_CleanupDeletesOnlyTheRunsOwnBranch: cleanup deletes the
// run's branch once the run's sandbox created it, and leaves a branch of that
// name alone when the checkout never happened (the branch was there before,
// or the run failed first).
func TestPrepareWorktree_CleanupDeletesOnlyTheRunsOwnBranch(t *testing.T) {
	repo := initGitRepo(t)
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	wt, cleanup, err := prepareWorktree(context.Background(), repo, task, "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	gitOut(t, repo, "branch", wt.branch)
	cleanup()
	if out := gitOut(t, repo, "branch", "--list", wt.branch); !strings.Contains(out, wt.branch) {
		t.Fatalf("cleanup deleted a branch the run did not create")
	}
	gitOut(t, repo, "branch", "-D", wt.branch)

	wt, cleanup, err = prepareWorktree(context.Background(), repo, task, "run2")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	sandboxCheckout(t, wt)
	cleanup()
	if out := gitOut(t, repo, "branch", "--list", wt.branch); strings.Contains(out, wt.branch) {
		t.Fatalf("cleanup kept the branch the run created")
	}
}

// TestWorktreeBase: an empty base, HEAD, "@" and per-worktree pseudo-refs
// mean the repository's, read through main-worktree/; other bases are kept.
func TestWorktreeBase(t *testing.T) {
	for in, want := range map[string]string{
		"":             "main-worktree/HEAD",
		"  ":           "main-worktree/HEAD",
		"@":            "main-worktree/HEAD",
		"HEAD":         "main-worktree/HEAD",
		"FETCH_HEAD":   "main-worktree/FETCH_HEAD",
		"ORIG_HEAD":    "main-worktree/ORIG_HEAD",
		"main":         "main",
		"origin/main":  "origin/main",
		"HEADquarters": "HEADquarters",
		"release_HEAD": "release_HEAD",
	} {
		if got := worktreeBase(in); got != want {
			t.Errorf("worktreeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPrepareWorktree_CleanupKeepsALockedWorktree: when the run locked its
// worktree, cleanup removes nothing — checkout, record and branch stay, so
// the run's commit keeps its ref.
func TestPrepareWorktree_CleanupKeepsALockedWorktree(t *testing.T) {
	repo := initGitRepo(t)
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	sandboxCheckout(t, wt)
	gitOut(t, wt.path, "commit", "--allow-empty", "-q", "-m", "work")
	gitOut(t, wt.path, "worktree", "lock", "--reason", "keep", ".")
	tip := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch))
	cleanup()
	for _, p := range []string{filepath.Join(wt.path, ".git"), wt.adminDir, filepath.Join(wt.adminDir, "locked")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("cleanup removed %s from a locked worktree: %v", p, err)
		}
	}
	if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch)); got != tip {
		t.Errorf("the locked worktree's branch is at %q, want %q", got, tip)
	}
}

// TestPrepareWorktree_CleanupKeepsAWorktreeItCannotRemove: when the run's
// checkout cannot be removed completely (a directory inside it the service
// user cannot search), cleanup keeps the record and the branch, so the
// run's commit keeps its ref; once the checkout can go, a later cleanup
// finishes.
func TestPrepareWorktree_CleanupKeepsAWorktreeItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches a mode-000 directory")
	}
	repo := initGitRepo(t)
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "run1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	sandboxCheckout(t, wt)
	gitOut(t, wt.path, "commit", "--allow-empty", "-q", "-m", "work")
	tip := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch))
	stuck := filepath.Join(wt.path, "out", "stuck")
	if err := os.MkdirAll(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })

	cleanup()
	if _, err := os.Lstat(wt.adminDir); err != nil {
		t.Errorf("cleanup removed the record of a checkout it could not remove: %v", err)
	}
	if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", wt.branch)); got != tip {
		t.Errorf("the branch is at %q, want %q", got, tip)
	}

	if err := os.Chmod(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanup()
	for _, p := range []string{wt.path, wt.adminDir} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("a later cleanup left %s (lstat err %v)", p, err)
		}
	}
	if out := gitOut(t, repo, "branch", "--list", wt.branch); strings.Contains(out, wt.branch) {
		t.Errorf("a later cleanup kept the branch %s", wt.branch)
	}
}

// TestWorktreeNoticeShallow: the notice says what fails in a shallow clone
// only when the repository is one.
func TestWorktreeNoticeShallow(t *testing.T) {
	plain := withWorktreeNotice("do it", &runWorktree{path: "/ws/.fleet-worktrees/t-r", branch: "fleet/task-t-r"})
	if strings.Contains(plain, "shallow") {
		t.Errorf("notice for a full clone mentions shallow clones:\n%s", plain)
	}
	shallow := withWorktreeNotice("do it", &runWorktree{path: "/ws/.fleet-worktrees/t-r", branch: "fleet/task-t-r", shallow: true})
	for _, want := range []string{"shallow clone", "ordinary fetches work", "--deepen", "--unshallow", "shallow.lock", "do it"} {
		if !strings.Contains(shallow, want) {
			t.Errorf("shallow notice lacks %q:\n%s", want, shallow)
		}
	}
}
