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
