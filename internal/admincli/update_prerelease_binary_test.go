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

// TestUpdateRefusesAFleetThatPredatesReleaseVerbs — update.sh used to verify
// the first artifact with the shell verifier when the installed fleet predated
// `fleet release` (a from-source install). That fallback went with the on-box
// build (ADR-0101): such a box is told to re-run bootstrap, before anything is
// downloaded, and nothing is installed.
func TestUpdateRefusesAFleetThatPredatesReleaseVerbs(t *testing.T) {
	clone, env, _ := realignFixture(t)
	// Put the checkout AT the release, as --no-pull requires.
	cmd := exec.Command("git", "checkout", "-q", "--detach", fixtureTag)
	cmd.Dir, cmd.Env = clone, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	install := t.TempDir()
	// An installed fleet from before `fleet release`: `release --help` fails.
	fake := "#!/bin/sh\ncase \"$1\" in version) echo 'fleet 2026.10.01.1 (abcdef012345)';; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(install, "fleet"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	env = append(env, "FLEET_INSTALL_DIR="+install, "FLEET_RELEASE_CACHE_DIR="+t.TempDir())
	out, err := runScript(t, env, "update.sh", "--no-pull", "--tag", fixtureTag)
	if err == nil || !strings.Contains(out, "predates `fleet release`") || !strings.Contains(out, "bootstrap.sh --enable-service") {
		t.Fatalf("want a refusal pointing at bootstrap, got err=%v\n%s", err, out)
	}
	if strings.Contains(out, "could not download") || strings.Contains(out, "verifying this first artifact") {
		t.Errorf("a pre-`fleet release` box must be refused before any download, with no shell-verifier fallback\n%s", out)
	}
	if !strings.Contains(out, "Nothing was installed") {
		t.Errorf("the refusal must say nothing was installed\n%s", out)
	}
}
