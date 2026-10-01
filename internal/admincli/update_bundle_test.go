package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// gitInit builds a throwaway repo with one commit and returns its path.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("mcp_servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "seed")
}

func capture(t *testing.T, fn func() bool) (string, bool) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	stale := fn()
	os.Stdout = orig
	_ = w.Close()
	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	_ = r.Close()
	return string(buf[:n]), stale
}

// TestClientBundleCheck covers the states an operator's box actually lands in.
// The one that motivated this is the last: a bundle with no upstream never
// fast-forwards, so `fleet update` leaves it behind while fleet itself advances
// — which surfaces in the UI as stale connector copy and nowhere else.
func TestClientBundleCheck(t *testing.T) {
	t.Run("no bundle configured", func(t *testing.T) {
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", "")
		t.Setenv("FLEET_STATE_DIR", t.TempDir())
		out, stale := capture(t, clientBundleCheck)
		if stale {
			t.Error("a generic install has no bundle to be stale")
		}
		if !strings.Contains(out, "none configured") {
			t.Errorf("want the generic-bundle note, got %q", out)
		}
	})

	t.Run("not a git checkout", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		out, stale := capture(t, clientBundleCheck)
		if stale {
			t.Error("a non-checkout cannot be 'behind'; it is a different problem")
		}
		if !strings.Contains(out, "not a git checkout") {
			t.Errorf("want the non-checkout note, got %q", out)
		}
	})

	t.Run("staged copy of the generic bundle", testStagedCopyCheck)

	// The shell updater compares resolved paths, so this check must too: a
	// relative FLEET_ROOT (repoRoot falls back to ".") or one reached through
	// a symlink names the same checkout the marker does, not a different one.
	t.Run("staged copy seen through a relative or symlinked root", func(t *testing.T) {
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(base, "src")
		if err := os.MkdirAll(filepath.Join(root, "config", "default"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "current")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".fleet-staged-from"), []byte(filepath.Join(root, "config", "default")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		t.Chdir(base)
		for _, r := range []string{link, "src", "./current"} {
			t.Setenv("FLEET_ROOT", r)
			if out, stale := capture(t, clientBundleCheck); stale || strings.Contains(out, "not of this checkout") {
				t.Errorf("FLEET_ROOT=%s: a copy of this very checkout read as stale: %q", r, out)
			}
		}
	})

	// A marker the service account replaced with a link, a FIFO or an empty
	// file is not one update.sh recognises, so the copy will not be refreshed:
	// stale, not a pass through the non-checkout branch.
	t.Run("an invalid marker is stale, never trusted", func(t *testing.T) {
		for name, plant := range map[string]func(string) error{
			"symlink": func(m string) error { return os.Symlink(filepath.Join(t.TempDir(), "secret"), m) },
			"empty":   func(m string) error { return os.WriteFile(m, nil, 0o644) },
			"fifo":    func(m string) error { return syscall.Mkfifo(m, 0o644) },
		} {
			dir := t.TempDir()
			if err := plant(filepath.Join(dir, ".fleet-staged-from")); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
			out, stale := capture(t, clientBundleCheck)
			if !stale || !strings.Contains(out, "not a readable regular file") || strings.Contains(out, "is the staged copy") {
				t.Errorf("%s marker: stale=%v %q", name, stale, out)
			}
		}
	})

	// The shipped unit keeps its state dir 0700, so a non-root --check cannot
	// read a copy staged there; it must say so and fail, not pass unlooked.
	t.Run("an unreadable staged copy is reported, not passed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads past a 0700 directory")
		}
		state := t.TempDir()
		dir := filepath.Join(state, "bundle")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".fleet-staged-from"), []byte("/x/config/default\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(state, 0o000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(state, 0o755) }()
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		out, stale := capture(t, clientBundleCheck)
		if !stale || !strings.Contains(out, "sudo fleet update --check") {
			t.Errorf("an unreadable staged copy: stale=%v %q", stale, out)
		}
	})

	t.Run("checkout with no upstream is reported stale", func(t *testing.T) {
		dir := t.TempDir()
		gitInit(t, dir)
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		out, stale := capture(t, clientBundleCheck)
		if !stale {
			t.Error("no upstream means fleet update will never advance it — that is the silent-stale case")
		}
		if !strings.Contains(out, "no upstream tracking branch") {
			t.Errorf("want the no-upstream diagnosis, got %q", out)
		}
	})

	// The state a real box was found in: parked on a closed PR's branch, fully
	// current with THAT branch (so `git pull --ff-only` succeeds and every
	// "behind upstream" check reports clean), while sitting 17 commits behind
	// main — which is where the connector copy it was missing had merged.
	t.Run("on a feature branch, current with it, behind the default", func(t *testing.T) {
		dir := seedBundleWithRemote(t)
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		out, stale := capture(t, clientBundleCheck)
		if !stale {
			t.Errorf("a bundle behind the DEFAULT branch is stale even when current with its own\n%s", out)
		}
		for _, want := range []string{"not main", "behind main", "will NOT fix this"} {
			if !strings.Contains(out, want) {
				t.Errorf("diagnosis missing %q\n--- output ---\n%s", want, out)
			}
		}
	})

	t.Run("bundle dir comes from the bootstrap state file", func(t *testing.T) {
		dir := t.TempDir()
		gitInit(t, dir)
		state := t.TempDir()
		if err := os.WriteFile(filepath.Join(state, "client-config.dir"), []byte(dir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", "")
		t.Setenv("FLEET_STATE_DIR", state)
		out, _ := capture(t, clientBundleCheck)
		if !strings.Contains(out, dir) {
			t.Errorf("state-file fallback should resolve the bundle dir, got %q", out)
		}
	})
}

// seedBundleWithRemote builds a bundle checkout on a feature branch that tracks
// its own remote branch and is behind the remote's default branch — the shape
// that reports "up to date" to every naive freshness check.
func seedBundleWithRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	git(remote, "init", "--quiet", "--bare", "-b", "main")

	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "init", "--quiet", "-b", "main")
	write(work, "manifest.yaml", "mcp_servers: []\n")
	git(work, "add", "-A")
	git(work, "commit", "--quiet", "-m", "seed")
	git(work, "remote", "add", "origin", remote)
	git(work, "push", "--quiet", "-u", "origin", "main")

	// A feature branch, pushed, then main moves on without it.
	git(work, "checkout", "--quiet", "-b", "fix/some-branch")
	write(work, "notes.md", "wip\n")
	git(work, "add", "-A")
	git(work, "commit", "--quiet", "-m", "wip")
	git(work, "push", "--quiet", "-u", "origin", "fix/some-branch")

	git(work, "checkout", "--quiet", "main")
	write(work, "manifest.yaml", "mcp_servers: []\n# copy landed here\n")
	git(work, "add", "-A")
	git(work, "commit", "--quiet", "-m", "connector copy")
	git(work, "push", "--quiet", "origin", "main")

	// Leave the checkout parked on the feature branch, fully current with it,
	// and teach it the remote's default so origin/HEAD resolves.
	git(work, "checkout", "--quiet", "fix/some-branch")
	git(work, "fetch", "--quiet", "origin")
	git(work, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return work
}

// testStagedCopyCheck: a staged copy of this checkout's config/default is
// current only while its content still matches the source; drift, an extra
// file, a symlink in place of a file, or a marker naming another checkout
// each make it stale.
func testStagedCopyCheck(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "config", "default")
	dir := t.TempDir()
	for _, base := range []string{src, dir} {
		if err := os.MkdirAll(filepath.Join(base, "personas"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"manifest.yaml": "servers: {}\n", "personas/default.md": "hello\n"} {
			if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".fleet-staged-from"), []byte(src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
	t.Setenv("FLEET_ROOT", root)
	out, stale := capture(t, clientBundleCheck)
	if stale {
		t.Errorf("a staged copy of this checkout is refreshed by update, not pulled; it is not stale by being a non-checkout: %q", out)
	}
	if !strings.Contains(out, "staged copy") || strings.Contains(out, "not a git checkout") {
		t.Errorf("want the staged-copy note, got %q", out)
	}
	// The source moved on outside `fleet update` (a hand fast-forward of
	// the checkout): the marker still names the same path, but the bytes
	// are old, so the copy is stale, whether a file changed or was added.
	persona := filepath.Join(src, "personas", "default.md")
	added := filepath.Join(src, "personas", "new.md")
	for _, step := range []struct {
		name           string
		apply, restore func() error
	}{
		{"changed", func() error { return os.WriteFile(persona, []byte("hello, world\n"), 0o644) }, func() error { return os.WriteFile(persona, []byte("hello\n"), 0o644) }},
		{"added", func() error { return os.WriteFile(added, []byte("x\n"), 0o644) }, func() error { return os.Remove(added) }},
	} {
		if err := step.apply(); err != nil {
			t.Fatal(err)
		}
		if out, stale := capture(t, clientBundleCheck); !stale || !strings.Contains(out, "no longer matches") {
			t.Errorf("%s in the source: the copy read as current (stale=%v): %q", step.name, stale, out)
		}
		if err := step.restore(); err != nil {
			t.Fatal(err)
		}
	}
	if _, stale := capture(t, clientBundleCheck); stale {
		t.Fatal("restored source: the copy should match again")
	}
	// Owner permission bits the refresh would restore (a directory gone
	// unreadable, a script that lost its executable bit) are drift too.
	personas := filepath.Join(dir, "personas")
	if err := os.Chmod(personas, 0o000); err != nil {
		t.Fatal(err)
	}
	if out, stale := capture(t, clientBundleCheck); !stale || !strings.Contains(out, "personas") {
		t.Errorf("a 000 directory in the copy read as current (stale=%v): %q", stale, out)
	}
	if err := os.Chmod(personas, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "manifest.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, stale := capture(t, clientBundleCheck); !stale || !strings.Contains(out, "manifest.yaml") {
		t.Errorf("a lost executable bit read as current (stale=%v): %q", stale, out)
	}
	if err := os.Chmod(filepath.Join(src, "manifest.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A copy holding a file its source dropped is stale too (rsync
	// --delete would remove it).
	if err := os.WriteFile(filepath.Join(dir, "extra.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, stale := capture(t, clientBundleCheck); !stale || !strings.Contains(out, "extra.md") {
		t.Errorf("an extra file in the copy read as current (stale=%v): %q", stale, out)
	}
	// A symlink in the copy in place of a file is a difference, never a
	// path the root-run comparison follows.
	if err := os.Remove(filepath.Join(dir, "extra.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "manifest.yaml"), filepath.Join(dir, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	if out, stale := capture(t, clientBundleCheck); !stale || !strings.Contains(out, "manifest.yaml") {
		t.Errorf("a symlink in the copy read as the file it names (stale=%v): %q", stale, out)
	}
	// The same copy seen from a checkout at another path (fleet re-cloned
	// elsewhere): update refreshes only a copy of its own config/default,
	// so the check must call this one stale rather than current.
	t.Setenv("FLEET_ROOT", "/root/fleet")
	out, stale = capture(t, clientBundleCheck)
	if !stale || !strings.Contains(out, "not of this checkout") {
		t.Errorf("a copy staged from another checkout read as current (stale=%v): %q", stale, out)
	}
}
