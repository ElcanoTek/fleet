//go:build linux

package scheduledrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/worktree"
)

// TestPrepareWorktree_RepositoryProgramsDoNotRun pins that the host side of a
// worktree run — registering the worktree, removing it and deleting its
// branch, all without git — runs nothing the repository names. The repository at the
// workspace root is written from sandboxes, so anything its .git or tracked
// attributes name must not run on the host. Every case below configures one
// such program to append to a marker file outside the repository; after a
// full create + cleanup cycle no marker may exist. The include cases cover
// configuration only a worktree's own git process would load (includeIf on
// the run's branch or on a worktree gitdir).
func TestPrepareWorktree_RepositoryProgramsDoNotRun(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repo, script string)
	}{
		{"hooks directory", func(t *testing.T, repo, script string) {
			for _, hook := range []string{"post-checkout", "reference-transaction", "post-index-change"} {
				writeExecutable(t, filepath.Join(repo, ".git", "hooks", hook), "#!/bin/sh\nexec "+script+" "+hook+"\n")
			}
		}},
		{"core.hooksPath", func(t *testing.T, repo, script string) {
			dir := filepath.Join(repo, "myhooks")
			for _, hook := range []string{"post-checkout", "reference-transaction"} {
				writeExecutable(t, filepath.Join(dir, hook), "#!/bin/sh\nexec "+script+" "+hook+"\n")
			}
			gitOut(t, repo, "config", "core.hooksPath", dir)
		}},
		{"configured hook", func(t *testing.T, repo, script string) {
			gitOut(t, repo, "config", "hook.marker.command", script+" configured-hook")
			gitOut(t, repo, "config", "--add", "hook.marker.event", "post-checkout")
			gitOut(t, repo, "config", "--add", "hook.marker.event", "reference-transaction")
		}},
		{"included configuration", func(t *testing.T, repo, script string) {
			inc := filepath.Join(repo, "extra.cfg")
			body := "[hook \"inc\"]\n\tcommand = " + script + " included-hook\n\tevent = post-checkout\n[core]\n\tfsmonitor = " + script + "\n"
			if err := os.WriteFile(inc, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			gitOut(t, repo, "config", "include.path", inc)
		}},
		{"filter driver", func(t *testing.T, repo, script string) {
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=marker\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitOut(t, repo, "add", ".gitattributes", "a.txt")
			gitOut(t, repo, "commit", "-q", "-m", "attrs")
			gitOut(t, repo, "config", "filter.marker.smudge", script+" smudge")
			gitOut(t, repo, "config", "filter.marker.clean", script+" clean")
			gitOut(t, repo, "config", "filter.marker.required", "true")
		}},
		{"process filter", func(t *testing.T, repo, script string) {
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=proc\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitOut(t, repo, "add", ".gitattributes", "a.txt")
			gitOut(t, repo, "commit", "-q", "-m", "attrs")
			gitOut(t, repo, "config", "filter.proc.process", script+" process")
		}},
		{"fsmonitor", func(t *testing.T, repo, script string) {
			gitOut(t, repo, "config", "core.fsmonitor", script)
		}},
		{"filter via info/attributes behind includeIf onbranch", func(t *testing.T, repo, script string) {
			conditionalFilter(t, repo, script, "onbranch:fleet/**")
		}},
		{"filter via info/attributes behind includeIf gitdir", func(t *testing.T, repo, script string) {
			conditionalFilter(t, repo, script, "gitdir:**/worktrees/**")
		}},
		{"repository git-lfs program keys under a global lfs filter", func(t *testing.T, repo, script string) {
			if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=lfs\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitOut(t, repo, "add", ".gitattributes", "a.txt")
			gitOut(t, repo, "commit", "-q", "-m", "attrs")
			global := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(global, []byte("[filter \"lfs\"]\n\tprocess = "+script+" lfs-process\n\tsmudge = "+script+" lfs-smudge\n\trequired = true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			gitOut(t, repo, "config", "lfs.customtransfer.x.path", script)
			gitOut(t, repo, "config", "lfs.extension.x.smudge", script+" ext")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initGitRepo(t)
			marker := filepath.Join(t.TempDir(), "ran")
			script := filepath.Join(t.TempDir(), "mark.sh")
			// The script never reads stdin, so a long-running process filter
			// that git did start cannot hold the test open.
			writeExecutable(t, script, "#!/bin/sh\necho \"$*\" >> '"+marker+"'\nexit 0\n")
			tc.setup(t, repo, script)

			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			task := worktreeTask(&models.WorktreeConfig{Enabled: true})
			wt, cleanup, err := prepareWorktree(ctx, repo, task, "run1")
			if err == nil {
				if _, statErr := os.Stat(wt.path); statErr != nil {
					t.Fatalf("worktree not created: %v", statErr)
				}
				cleanup()
			}
			if data, readErr := os.ReadFile(marker); readErr == nil {
				t.Fatalf("a program named by the repository ran on the host during worktree setup: %q", data)
			}
			if err != nil {
				t.Fatalf("prepareWorktree: %v", err)
			}
		})
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareWorktree_RefusesASymlinkedWorktreeDir: a .fleet-worktrees that
// is a symlink is refused; no worktree is created through it.
func TestPrepareWorktree_RefusesASymlinkedWorktreeDir(t *testing.T) {
	repo := initGitRepo(t)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(repo, worktreeSubdir)); err != nil {
		t.Fatal(err)
	}
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	if _, _, err := prepareWorktree(context.Background(), repo, task, "run1"); err == nil {
		t.Fatal("want a refusal for a symlinked worktree directory")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("a worktree was created through the symlink: %v", entries)
	}
}

// TestEnsureWorktreeExcluded_StaysInsideTheWorkspace: an exclude file that is
// a symlink out of the workspace is not appended to.
func TestEnsureWorktreeExcluded_StaysInsideTheWorkspace(t *testing.T) {
	repo := initGitRepo(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(exclude)
	if err := os.Symlink(outside, exclude); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorktreeExcluded(context.Background(), repo); err == nil {
		t.Fatal("want an error for an exclude file that leads out of the workspace")
	}
	if got, _ := os.ReadFile(outside); string(got) != "original\n" {
		t.Fatalf("the file outside the workspace was changed: %q", got)
	}
}

// conditionalFilter makes every path select a filter defined only behind a
// conditional include: .git/info/attributes selects it, and the definition
// sits in a file included under cond.
func conditionalFilter(t *testing.T, repo, script, cond string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "a.txt")
	gitOut(t, repo, "commit", "-q", "-m", "a")
	writeExecutable(t, filepath.Join(repo, ".git", "info", "attributes"), "* filter=evil\n")
	inc := filepath.Join(t.TempDir(), "evil.cfg")
	body := "[filter \"evil\"]\n\tsmudge = " + script + " smudge\n\tprocess = " + script + " process\n" +
		"[hook \"inc\"]\n\tcommand = " + script + " hook\n\tevent = post-checkout\n\tevent = reference-transaction\n"
	if err := os.WriteFile(inc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("[includeIf \"" + cond + "\"]\n\tpath = " + inc + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareWorktree_RefusesARepositoryThatPointsElsewhere: a .git that
// names another repository (commondir) or borrows another's objects
// (objects/info/alternates) is refused before git runs, and nothing is
// written into the other repository.
func TestPrepareWorktree_RefusesARepositoryThatPointsElsewhere(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, repo, other string){
		"commondir": func(t *testing.T, repo, other string) {
			writeExecutable(t, filepath.Join(repo, ".git", "commondir"), filepath.Join(other, ".git")+"\n")
		},
		"alternates": func(t *testing.T, repo, other string) {
			writeExecutable(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.Join(other, ".git", "objects")+"\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := initGitRepo(t)
			other := initGitRepo(t)
			before := gitOut(t, other, "for-each-ref")
			setup(t, repo, other)
			task := worktreeTask(&models.WorktreeConfig{Enabled: true})
			if _, _, err := prepareWorktree(context.Background(), repo, task, "run1"); err == nil {
				t.Fatal("want a refusal")
			}
			if after := gitOut(t, other, "for-each-ref"); after != before {
				t.Fatalf("the other repository's refs changed:\n%s\n->\n%s", before, after)
			}
			if entries, _ := os.ReadDir(filepath.Join(other, ".git", "worktrees")); len(entries) != 0 {
				t.Fatalf("a worktree record was written into the other repository: %v", entries)
			}
		})
	}
}

// TestPrepareWorktree_ActsOnlyOnTheRepositoryRecordedAtStartup: a root whose
// .git appeared after fleet recorded it, whose .git was replaced since, or
// whose .git is a gitfile or a symlink, is refused.
func TestPrepareWorktree_ActsOnlyOnTheRepositoryRecordedAtStartup(t *testing.T) {
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	t.Run("never recorded", func(t *testing.T) {
		dir := t.TempDir()
		gitOut(t, dir, "init", "-q")
		gitOut(t, dir, "-c", "user.email=t@t.t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init")
		if _, _, err := prepareWorktree(context.Background(), dir, task, "run1"); err == nil || !strings.Contains(err.Error(), "not recorded") {
			t.Fatalf("err = %v, want a refusal for a root not recorded at startup", err)
		}
	})
	t.Run("appeared after startup", func(t *testing.T) {
		dir := t.TempDir()
		worktree.PinAtStartup(dir)
		gitOut(t, dir, "init", "-q")
		gitOut(t, dir, "-c", "user.email=t@t.t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init")
		if _, _, err := prepareWorktree(context.Background(), dir, task, "run1"); err == nil || !strings.Contains(err.Error(), "had no .git") {
			t.Fatalf("err = %v, want a refusal naming the missing startup repository", err)
		}
	})
	t.Run("replaced after startup", func(t *testing.T) {
		repo := initGitRepo(t)
		if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(t.TempDir(), "old.git")); err != nil {
			t.Fatal(err)
		}
		gitOut(t, repo, "init", "-q")
		gitOut(t, repo, "-c", "user.email=t@t.t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init")
		if _, _, err := prepareWorktree(context.Background(), repo, task, "run1"); err == nil || !strings.Contains(err.Error(), "replaced after startup") {
			t.Fatalf("err = %v, want a refusal naming the replaced .git", err)
		}
	})
	t.Run("gitfile root", func(t *testing.T) {
		main := initGitRepo(t)
		linked := filepath.Join(t.TempDir(), "linked")
		gitOut(t, main, "worktree", "add", "-q", "--detach", linked)
		worktree.PinAtStartup(linked)
		if _, _, err := prepareWorktree(context.Background(), linked, task, "run1"); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("err = %v, want a refusal for a gitfile root", err)
		}
	})
	t.Run("symlinked .git", func(t *testing.T) {
		other := initGitRepo(t)
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(other, ".git"), filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
		worktree.PinAtStartup(dir)
		if _, _, err := prepareWorktree(context.Background(), dir, task, "run1"); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v, want a refusal for a symlinked .git", err)
		}
	})
}

// TestHostWritesNothingOutsideTheWorkspaceWhileRefsSwap: a live worktree
// run's sandbox can write .git/refs and .git/logs, and here it keeps swapping
// refs/heads/fleet and logs/refs/heads/fleet between a real directory and a
// symlink leading out of the workspace while the host creates and cleans up
// other runs' worktrees and deletes their branches. The host writes no ref
// or reflog of its own (worktree add --detach), and deletes branches only
// through directories it opened without following a link, so nothing is
// written or removed outside the workspace.
func TestHostWritesNothingOutsideTheWorkspaceWhileRefsSwap(t *testing.T) {
	repo := initGitRepo(t)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	const runs = 60
	pairs := make([][2]string, 0, 2)
	for _, base := range []string{filepath.Join(repo, ".git", "refs", "heads"), filepath.Join(repo, ".git", "logs", "refs", "heads")} {
		if err := os.MkdirAll(filepath.Join(base, "fleet"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The refs and reflogs the runs' sandboxes would have created, so
		// the host's cleanup has something to delete.
		for i := 0; i < runs; i++ {
			name := fmt.Sprintf("task-%s-r%03d", task.ID, i)
			if err := os.WriteFile(filepath.Join(base, "fleet", name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(outside, filepath.Join(base, "fleet.alt")); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, [2]string{filepath.Join(base, "fleet"), filepath.Join(base, "fleet.alt")})
	}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			for _, p := range pairs {
				_ = unix.Renameat2(unix.AT_FDCWD, p[0], unix.AT_FDCWD, p[1], unix.RENAME_EXCHANGE)
			}
		}
	}()
	ok := 0
	for i := 0; i < runs; i++ {
		_, cleanup, err := prepareWorktree(context.Background(), repo, task, fmt.Sprintf("r%03d", i))
		if err != nil {
			continue
		}
		ok++
		cleanup()
	}
	stop.Store(true)
	<-done
	entries, _ := os.ReadDir(outside)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "keep" {
		t.Fatalf("the host wrote or removed files outside the workspace: %v (worktrees created: %d)", names, ok)
	}
	if ok == 0 {
		t.Fatal("no worktree was created while the refs were swapping")
	}
}

// TestPrepareWorktree_RefusesASymlinkedRecordsDir: a .git/worktrees planted
// as a symlink (while the root was not yet a repository, or before an
// upgrade) is refused before anything is written, so no record is written
// through it.
func TestPrepareWorktree_RefusesASymlinkedRecordsDir(t *testing.T) {
	repo := initGitRepo(t)
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(repo, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "r1")
	cleanup()
	if err == nil {
		t.Fatal("want a refusal for a symlinked .git/worktrees")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("worktree setup wrote through the symlink: %v", entries)
	}
}

// TestPrepareWorktree_ARecordWithAFIFODoesNotBlockSetup: a record a run
// left with a FIFO for its gitdir does not slow the next worktree's creation:
// the host runs no git that would read it.
func TestPrepareWorktree_ARecordWithAFIFODoesNotBlockSetup(t *testing.T) {
	repo := initGitRepo(t)
	task := worktreeTask(&models.WorktreeConfig{Enabled: true})
	first, _, err := prepareWorktree(context.Background(), repo, task, "r1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	gitdir := filepath.Join(first.adminDir, "gitdir")
	if err := os.Remove(gitdir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(gitdir, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	second, cleanup, err := prepareWorktree(ctx, repo, task, "r2")
	if err != nil {
		t.Fatalf("prepareWorktree after a FIFO record: %v (after %s)", err, time.Since(start))
	}
	defer cleanup()
	if time.Since(start) > 2*time.Second {
		t.Fatalf("creating the next worktree took %s", time.Since(start))
	}
	if second.adminDir == "" {
		t.Fatal("no record for the new worktree")
	}
}

// TestPrepareWorktree_CleanupRemovesItsRecordWithoutProof: a run that
// rewrote its own record (here: its gitdir names something else) still has
// that record removed by its cleanup, which knows the record's id.
func TestPrepareWorktree_CleanupRemovesItsRecordWithoutProof(t *testing.T) {
	repo := initGitRepo(t)
	wt, cleanup, err := prepareWorktree(context.Background(), repo, worktreeTask(&models.WorktreeConfig{Enabled: true}), "r1")
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.adminDir, "gitdir"), []byte("/elsewhere/.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Lstat(wt.adminDir); !os.IsNotExist(err) {
		t.Fatalf("the run's record survived its cleanup (lstat err %v)", err)
	}
	if _, err := os.Lstat(wt.path); !os.IsNotExist(err) {
		t.Fatalf("the run's checkout survived its cleanup (lstat err %v)", err)
	}
}

// TestWorktreeNotice: a worktree run's prompt says what its git can and
// cannot do, and that the packed-refs.lock line is expected; other runs'
// prompts are unchanged.
func TestWorktreeNotice(t *testing.T) {
	if got := withWorktreeNotice("do it", nil); got != "do it" {
		t.Fatalf("no worktree: %q", got)
	}
	got := withWorktreeNotice("do it", &runWorktree{path: "/ws/.fleet-worktrees/t-r", branch: "fleet/task-t-r"})
	for _, want := range []string{"fleet/task-t-r", "packed-refs.lock", "exit status", "--no-track", "git push -u", "submodules", "Deleting or renaming a branch", "do it"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q:\n%s", want, got)
		}
	}
}

// TestPrepareWorktree_ReadsNothingALiveRunCanWrite: a live worktree run can
// write .git/objects, .git/refs and its own record. None of what it can plant
// there — a FIFO pack index beside an empty pack, a FIFO base ref, its record's
// HEAD naming a FIFO ref, its record's commondir naming another directory, a
// base ref that is a link to /dev/zero — slows or blocks the next run's
// worktree creation, because the host runs no git there and reads none of it.
func TestPrepareWorktree_ReadsNothingALiveRunCanWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, repo, adminDir string)
	}{
		{"FIFO pack index", func(t *testing.T, repo, _ string) {
			p := filepath.Join(repo, ".git", "objects", "pack")
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(p, "pack-0000000000000000000000000000000000000000")
			if err := syscall.Mkfifo(base+".idx", 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(base+".pack", nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"FIFO base ref", func(t *testing.T, repo, _ string) {
			head, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			ref := filepath.Join(repo, ".git", strings.TrimSpace(strings.TrimPrefix(string(head), "ref: ")))
			if err := os.Remove(ref); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(ref, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"base ref linked to /dev/zero", func(t *testing.T, repo, _ string) {
			head, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			ref := filepath.Join(repo, ".git", strings.TrimSpace(strings.TrimPrefix(string(head), "ref: ")))
			if err := os.Remove(ref); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/dev/zero", ref); err != nil {
				t.Fatal(err)
			}
		}},
		{"own record HEAD names a FIFO ref", func(t *testing.T, repo, adminDir string) {
			if err := os.WriteFile(filepath.Join(adminDir, "HEAD"), []byte("ref: refs/heads/trap\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(repo, ".git", "refs", "heads", "trap"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"own record commondir names another directory", func(t *testing.T, repo, adminDir string) {
			elsewhere := filepath.Join(repo, "conv-1", "elsewhere")
			if err := os.MkdirAll(filepath.Join(elsewhere, "refs", "heads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(elsewhere, "refs", "heads", "x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(adminDir, "commondir"), []byte(elsewhere+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(adminDir, "HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initGitRepo(t)
			task := worktreeTask(&models.WorktreeConfig{Enabled: true})
			live, liveCleanup, err := prepareWorktree(context.Background(), repo, task, "live")
			if err != nil {
				t.Fatal(err)
			}
			tc.plant(t, repo, live.adminDir)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			start := time.Now()
			next, cleanup, err := prepareWorktree(ctx, repo, task, "next")
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("prepareWorktree after the plant: %v (after %s)", err, elapsed)
			}
			if elapsed > 2*time.Second {
				t.Fatalf("creating the next worktree took %s", elapsed)
			}
			if next.adminDir == "" {
				t.Fatal("no record for the next worktree")
			}
			done := make(chan struct{})
			go func() { cleanup(); liveCleanup(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("cleanup blocked")
			}
		})
	}
}
