// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitTrustDirIsIdempotent — update.sh and doctor.sh trust their checkouts
// through scripts/lib/git.sh's git_trust_dir. The old bare
// `git config --global --add safe.directory` appended a line per run (a
// production box carried 261 copies of one path). The helper must add a
// missing entry once, never add a second, collapse duplicates left by older
// runs, match the path literally (a '.' in a path is not a regex wildcard), and
// leave every other entry and key alone.
func TestGitTrustDirIsIdempotent(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_NOSYSTEM=1")
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "git.sh")
	run := func(script string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c", ". "+lib+" && "+script)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash %q: %v\n%s", script, err, out)
		}
		return string(out)
	}
	entries := func() []string {
		t.Helper()
		return strings.Fields(run("git config --global --get-all safe.directory || true"))
	}
	count := func(dir string) int {
		n := 0
		for _, e := range entries() {
			if e == dir {
				n++
			}
		}
		return n
	}

	// A config shaped like a long-lived box: duplicates of two paths, a
	// look-alike that a regex match on "/opt/fleet.src" would also hit, and an
	// unrelated key.
	run(`git config --global user.name keep-me
for i in 1 2 3; do git config --global --add safe.directory /opt/fleet/client; done
for i in 1 2; do git config --global --add safe.directory /opt/fleet.src; done
git config --global --add safe.directory /opt/fleetXsrc`)

	run(`git_trust_dir /opt/fleet/client; git_trust_dir /opt/fleet.src; git_trust_dir /opt/fleet/client`)
	if n := count("/opt/fleet/client"); n != 1 {
		t.Errorf("/opt/fleet/client: %d entries after git_trust_dir, want 1 (%v)", n, entries())
	}
	if n := count("/opt/fleet.src"); n != 1 {
		t.Errorf("/opt/fleet.src: %d entries after git_trust_dir, want 1 (%v)", n, entries())
	}
	if n := count("/opt/fleetXsrc"); n != 1 {
		t.Errorf("/opt/fleetXsrc was touched by git_trust_dir /opt/fleet.src — the path must match literally (%v)", entries())
	}

	// A missing entry is added once, and repeat calls are no-ops.
	run(`git_trust_dir /srv/new; git_trust_dir /srv/new`)
	if n := count("/srv/new"); n != 1 {
		t.Errorf("/srv/new: %d entries, want 1 (%v)", n, entries())
	}
	// An empty value resets git's safe.directory list, so an entry that sits
	// only BEFORE a reset trusts nothing. The helper must leave DIR after the
	// last reset (the old bare --add always did, by appending), exactly once.
	run(`git config --global --add safe.directory /srv/reset-me
git config --global --add safe.directory ""
git_trust_dir /srv/reset-me; git_trust_dir /srv/reset-me`)
	if n := count("/srv/reset-me"); n != 1 {
		t.Errorf("/srv/reset-me: %d entries, want 1 (%v)", n, entries())
	}
	// -z: NUL-terminated values, so the empty reset entry survives parsing
	// (newline output cannot tell a trailing empty value from the final newline).
	raw := strings.Split(strings.TrimSuffix(run("git config --global -z --get-all safe.directory"), "\x00"), "\x00")
	lastReset, lastDir := -1, -1
	for i, e := range raw {
		switch e {
		case "":
			lastReset = i
		case "/srv/reset-me":
			lastDir = i
		}
	}
	if lastReset < 0 || lastDir < lastReset {
		t.Errorf("/srv/reset-me is not after the last empty safe.directory (reset at %d, entry at %d): %q", lastReset, lastDir, raw)
	}

	if got := strings.TrimSpace(run("git config --global user.name")); got != "keep-me" {
		t.Errorf("user.name = %q after git_trust_dir, want keep-me", got)
	}
}

// TestGitTrustDirWorksWithoutHome — `fleet update` launched with systemd-run
// has no HOME, so git can neither read nor write the global config that held
// the safe.directory line, and every git call on the root-read, fleet-owned
// client bundle died with "detected dubious ownership" (every box's bundle
// stopped advancing on 2026-11-18 while the update still exited 0).
// git_trust_dir must also trust DIR for the calling process through git's
// environment config, once, and re-add it when a later empty value reset the
// list. GIT_TEST_ASSUME_DIFFERENT_OWNER is git's own knob for exercising the
// ownership check without a second user.
func TestGitTrustDirWorksWithoutHome(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	setup := exec.Command("git", "-c", "init.defaultBranch=main", "init", "-q", repo)
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") || strings.HasPrefix(kv, "GIT_CONFIG_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "git.sh")
	run := func(script string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", "-c", "set -euo pipefail; . "+lib+"; "+script)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run(`git -C "` + repo + `" rev-parse --git-dir`); err == nil || !strings.Contains(out, "dubious ownership") {
		t.Fatalf("precondition: without HOME and trust, git must refuse the checkout (err=%v)\n%s", err, out)
	}
	out, err := run(`git_trust_dir "` + repo + `"; git_trust_dir "` + repo + `"
git -C "` + repo + `" rev-parse --git-dir >/dev/null
echo "count=$GIT_CONFIG_COUNT"`)
	if err != nil {
		t.Fatalf("git_trust_dir without HOME must leave the checkout readable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "count=1") {
		t.Errorf("two calls must add one environment entry, not two\n%s", out)
	}

	// An entry the caller already had, then an empty reset after it: DIR is
	// not trusted any more, so the helper appends it again, after the reset.
	out, err = run(`export GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0="` + repo + `" GIT_CONFIG_KEY_1=safe.directory GIT_CONFIG_VALUE_1=
git_trust_dir "` + repo + `"
git -C "` + repo + `" rev-parse --git-dir >/dev/null
echo "count=$GIT_CONFIG_COUNT last=$GIT_CONFIG_VALUE_2"`)
	if err != nil {
		t.Fatalf("an entry undone by a later reset must be re-added: %v\n%s", err, out)
	}
	if !strings.Contains(out, "count=3 last="+repo) {
		t.Errorf("want the entry re-added after the reset\n%s", out)
	}
}
