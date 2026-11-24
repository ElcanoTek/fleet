package scheduledrun

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/worktree"
)

// liveSandboxImage is the sandbox image the live test runs: the locally
// built default-bundle image, or FLEET_SANDBOX_TEST_IMAGE.
func liveSandboxImage() string {
	if img := os.Getenv("FLEET_SANDBOX_TEST_IMAGE"); img != "" {
		return img
	}
	return "localhost/fleet-sandbox:latest"
}

// TestWorktreeRunsLiveContainer runs worktree tasks end to end in real
// sandboxes, against a workspace repository cloned from an upstream (so
// origin/main exists) whose post-checkout hook fails, as a git-lfs hook does
// without git-lfs installed. The records are fleet's own (prepareWorktree →
// worktree.CreateWorktree) and the checkout is checkoutInSandbox's.
//
//   - A run on the default base and a run on base origin/main both check out
//     in their sandboxes; the hook neither runs nor fails them, and the
//     repository's config gets no upstream entry.
//   - git commits and branches inside the run's worktree; a second checkout
//     pass reports the existing branch and leaves the commit in place.
//   - In the run's sandbox the config, hooks, info/, commondir, alternates,
//     packed-refs, HEAD, another run's record and another run's checkout stay
//     read-only, and the Git LFS store (.git/lfs) is writable; in a chat
//     sandbox nothing under .git or .fleet-worktrees can be written and
//     neither directory can be renamed.
//   - The sandbox image's git is new enough for the checkout (minSandboxGit).
func TestWorktreeRunsLiveContainer(t *testing.T) {
	image := liveSandboxOrSkip(t)

	base := t.TempDir()
	upstream := filepath.Join(base, "upstream")
	root := filepath.Join(base, "workspace")
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t")
	hostGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if err := os.Mkdir(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	hostGit(upstream, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostGit(upstream, "add", "a.txt")
	hostGit(upstream, "commit", "-q", "-m", "init")
	hostGit(base, "clone", "-q", upstream, root)
	hostGit(root, "pack-refs", "--all")
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.WriteFile(filepath.Join(gitDir, "hooks", "post-checkout"), []byte("#!/bin/sh\necho hook ran >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	// What fleet serve does at startup (agent.wholeRootGitDirs).
	worktree.PinAtStartup(root)
	if err := worktree.PrepareRepoAtStartup(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	wtDir := filepath.Join(root, worktree.Subdir)
	if err := os.Mkdir(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanups := map[*runWorktree]func(){}
	prepare := func(runID, baseBranch string) *runWorktree {
		t.Helper()
		wt, cleanup, err := prepareWorktree(context.Background(), root, worktreeTask(&models.WorktreeConfig{Enabled: true, BaseBranch: baseBranch}), runID)
		if err != nil {
			t.Fatalf("prepareWorktree %s: %v", runID, err)
		}
		t.Cleanup(cleanup)
		cleanups[wt] = cleanup
		return wt
	}
	run := prepare("run", "")
	tracking := prepare("tracking", "origin/main")
	other := prepare("other", "")
	conv := filepath.Join(root, "conv-1")
	if err := os.Mkdir(conv, 0o755); err != nil {
		t.Fatal(err)
	}

	var started []*sandbox.Sandbox
	start := func(wt *runWorktree) *sandbox.Sandbox {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sb, err := sandbox.NewContainer(ctx, sandbox.ContainerConfig{
			Image: image, WorkspaceHostDir: root, BridgeScript: []byte("# unused\n"),
			NoNetwork: true, StatsInterval: -1,
			WholeRootGitDirs: []string{gitDir, wtDir}, Worktree: wt.mount(),
		})
		if err != nil {
			t.Fatalf("NewContainer: %v", err)
		}
		t.Cleanup(sb.Close)
		started = append(started, sb)
		return sb
	}
	bash := func(sb *sandbox.Sandbox, cmd string) (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		res, err := sb.RunBash(ctx, sandbox.BashRequest{Command: cmd, Timeout: 50 * time.Second})
		if err != nil {
			t.Fatalf("RunBash %q: %v", cmd, err)
		}
		return res.ExitCode, string(res.Stdout) + string(res.Stderr)
	}
	checkout := func(sb *sandbox.Sandbox, wt *runWorktree) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return checkoutInSandbox(ctx, sb, wt.path, wt.branch, wt.base)
	}
	q := shellQuote
	refused := func(t *testing.T, sb *sandbox.Sandbox, cmds []string) {
		t.Helper()
		for _, cmd := range cmds {
			if code, out := bash(sb, cmd); code == 0 {
				t.Errorf("%q succeeded; want it refused (output %q)", cmd, out)
			}
		}
	}
	readOnlyEverywhere := []string{
		"echo x > " + q(filepath.Join(gitDir, "hooks", "post-checkout")),
		"echo '[core]' >> " + q(filepath.Join(gitDir, "config")),
		"echo x > " + q(filepath.Join(gitDir, "commondir")),
		"echo x > " + q(filepath.Join(gitDir, "info", "attributes")),
		"echo /x > " + q(filepath.Join(gitDir, "objects", "info", "alternates")),
		"echo x >> " + q(filepath.Join(gitDir, "packed-refs")),
		"echo x > " + q(filepath.Join(gitDir, "HEAD")),
		"mkdir " + q(filepath.Join(gitDir, "worktrees", "planted")),
		"echo x > " + q(filepath.Join(other.adminDir, "gitdir")),
		"touch " + q(filepath.Join(other.path, "planted")),
		"mv " + q(gitDir) + " " + q(filepath.Join(root, "moved")),
		"mv " + q(wtDir) + " " + q(filepath.Join(root, "moved-wt")),
		"ln -s /tmp " + q(filepath.Join(wtDir, "planted-link")),
	}

	t.Run("chat sandbox", func(t *testing.T) {
		sb := start(nil)
		refused(t, sb, append([]string{
			"touch " + q(filepath.Join(run.path, "f.txt")),
			"echo x > " + q(filepath.Join(gitDir, "refs", "heads", "planted")),
			"touch " + q(filepath.Join(gitDir, "objects", "planted")),
			"touch " + q(filepath.Join(gitDir, "lfs", "planted")),
		}, readOnlyEverywhere...))
		if code, out := bash(sb, "git -C "+q(root)+" log --oneline -1 && touch "+q(filepath.Join(conv, "ok"))); code != 0 {
			t.Errorf("read of the repository / write to a conversation dir failed: %d %q", code, out)
		}
	})

	t.Run("worktree run sandbox", func(t *testing.T) {
		sb := start(run)
		if err := checkout(sb, run); err != nil {
			t.Fatalf("checkout on the default base: %v", err)
		}
		if code, out := bash(sb, "cat "+q(filepath.Join(run.path, "a.txt"))); code != 0 || !strings.Contains(out, "hello") {
			t.Fatalf("the checkout is not filled: exit %d, output %q", code, out)
		}
		commit := "cd " + q(run.path) + " && echo hi > f.txt && git add f.txt && git -c user.email=s@s.s -c user.name=s commit -q -m 'from the sandbox' && git log --oneline -1 && git branch -q extra"
		if code, out := bash(sb, commit); code != 0 || !strings.Contains(out, "from the sandbox") {
			t.Fatalf("git commit and branch inside the run's worktree: exit %d, output %q", code, out)
		}
		if err := checkout(sb, run); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("a second checkout pass: err = %v, want the existing branch reported", err)
		}
		// The Git LFS store is writable, as git-lfs needs it to store the
		// objects it downloads (the default image has no git-lfs; the write is
		// what git-lfs would do).
		lfsObject := filepath.Join(gitDir, "lfs", "objects", "ab", "cd", "abcd-test")
		if code, out := bash(sb, "mkdir -p "+q(filepath.Dir(lfsObject))+" && echo lfs > "+q(lfsObject)); code != 0 {
			t.Errorf("writing under .git/lfs in the worktree run: exit %d, output %q", code, out)
		}
		refused(t, sb, readOnlyEverywhere)
		_, version := bash(sb, "git version")
		if !gitAtLeast(version, minSandboxGit) {
			t.Errorf("the sandbox image's %q is older than %s", strings.TrimSpace(version), minSandboxGit)
		}
	})

	t.Run("remote-tracking base", func(t *testing.T) {
		sb := start(tracking)
		if err := checkout(sb, tracking); err != nil {
			t.Fatalf("checkout on base origin/main: %v", err)
		}
		if code, out := bash(sb, "cat "+q(filepath.Join(tracking.path, "a.txt"))); code != 0 || !strings.Contains(out, "hello") {
			t.Fatalf("the checkout is not filled: exit %d, output %q", code, out)
		}
	})

	if configAfter, err := os.ReadFile(filepath.Join(gitDir, "config")); err != nil || string(configAfter) != string(configBefore) {
		t.Errorf("the repository config changed (%v):\n%s", err, configAfter)
	}
	// The commit landed in the root repository, on the run's branch.
	if out := hostGit(root, "log", "--oneline", "-1", run.branch); !strings.Contains(out, "from the sandbox") {
		t.Fatalf("the run's branch on the host: %q", out)
	}

	for _, sb := range started {
		sb.Close()
	}
	checkLiveCleanup(t, liveCleanup{base: base, root: root, gitDir: gitDir, hostGit: hostGit,
		run: run, tracking: tracking, other: other, cleanups: cleanups})
}

// liveCleanup is what checkLiveCleanup needs from TestWorktreeRunsLiveContainer.
type liveCleanup struct {
	base, root, gitDir   string
	hostGit              func(dir string, args ...string) string
	run, tracking, other *runWorktree
	cleanups             map[*runWorktree]func()
}

// checkLiveCleanup runs the cleanup of the live test's runs, as runWorker
// leaves it (the sandboxes closed, checkedOut set): the checkout, the record
// and the branch go — a loose ref the sandbox wrote (run), and one packed on
// the host afterwards (tracking) — while main and the other run stay, and
// nothing outside the workspace changes.
func checkLiveCleanup(t *testing.T, c liveCleanup) {
	t.Helper()
	base, root, gitDir, hostGit := c.base, c.root, c.gitDir, c.hostGit
	run, tracking, other, cleanups := c.run, c.tracking, c.other, c.cleanups
	outsideBefore := listTree(t, base, root)
	// Pack every ref but the run's, whose loose file (the one its sandbox
	// wrote) is set aside meanwhile and put back.
	loose := filepath.Join(gitDir, "refs", "heads", run.branch)
	aside := filepath.Join(t.TempDir(), "loose-ref")
	if err := os.Rename(loose, aside); err != nil {
		t.Fatalf("the run's branch is not a loose ref the sandbox wrote: %v", err)
	}
	hostGit(root, "pack-refs", "--all")
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil { // pack-refs prunes empty ref dirs
		t.Fatal(err)
	}
	if err := os.Rename(aside, loose); err != nil {
		t.Fatal(err)
	}
	if packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs")); err != nil || !strings.Contains(string(packed), tracking.branch) || strings.Contains(string(packed), run.branch) {
		t.Fatalf("want the tracking run's branch packed and the run's loose (%v):\n%s", err, packed)
	}
	for _, wt := range []*runWorktree{run, tracking} {
		wt.checkedOut.Store(true)
		cleanups[wt]()
		for _, p := range []string{wt.path, wt.adminDir, filepath.Join(gitDir, "refs", "heads", wt.branch), filepath.Join(gitDir, "logs", "refs", "heads", wt.branch)} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("cleanup left %s (lstat err %v)", p, err)
			}
		}
		if out := hostGit(root, "for-each-ref", "refs/heads/"+wt.branch); strings.TrimSpace(out) != "" {
			t.Errorf("cleanup left the branch %s: %q", wt.branch, out)
		}
	}
	if packed, _ := os.ReadFile(filepath.Join(gitDir, "packed-refs")); strings.Contains(string(packed), tracking.branch) {
		t.Errorf("packed-refs still names %s:\n%s", tracking.branch, packed)
	}
	hostGit(root, "rev-parse", "--verify", "main")
	if _, err := os.Lstat(other.adminDir); err != nil {
		t.Errorf("another run's record went: %v", err)
	}
	if after := listTree(t, base, root); after != outsideBefore {
		t.Errorf("cleanup changed files outside the workspace:\nbefore:\n%s\nafter:\n%s", outsideBefore, after)
	}
}

// listTree lists every path under dir except the subtree skip, with its
// size and modification time, one per line.
func listTree(t *testing.T, dir, skip string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == skip {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// liveSandboxOrSkip skips the test unless the real sandbox can run here
// (linux, rootless, podman, git, the sandbox image present), and returns the
// image.
func liveSandboxOrSkip(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if os.Geteuid() == 0 {
		t.Skip("keep-id mapping is exercised rootless only")
	}
	for _, bin := range []string{"podman", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	image := liveSandboxImage()
	if err := exec.Command("podman", "image", "exists", image).Run(); err != nil {
		t.Skipf("sandbox image %s not present", image)
	}
	return image
}

// gitAtLeast reports whether `git version` output names a version at least
// floor ("major.minor").
func gitAtLeast(versionOutput, floor string) bool {
	fields := strings.Fields(versionOutput)
	if len(fields) < 3 {
		return false
	}
	parse := func(v string) (int, int, bool) {
		parts := strings.SplitN(v, ".", 3)
		if len(parts) < 2 {
			return 0, 0, false
		}
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		return major, minor, err1 == nil && err2 == nil
	}
	gotMajor, gotMinor, ok1 := parse(fields[2])
	wantMajor, wantMinor, ok2 := parse(floor)
	return ok1 && ok2 && (gotMajor > wantMajor || gotMajor == wantMajor && gotMinor >= wantMinor)
}
