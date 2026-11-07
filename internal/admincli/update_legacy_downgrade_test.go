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

// TestUpdateLegacyPathRefusesADowngrade — on a box whose installed fleet
// predates `fleet release` (the shell-verified first artifact update), an
// older release is refused before anything is downloaded, exactly as
// `fleet release fetch` refuses it, unless --allow-downgrade.
func TestUpdateLegacyPathRefusesADowngrade(t *testing.T) {
	clone, env, _ := realignFixture(t)
	// Put the checkout AT the (older) release, as --no-pull requires.
	cmd := exec.Command("git", "checkout", "-q", "--detach", fixtureTag)
	cmd.Dir, cmd.Env = clone, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	install := t.TempDir()
	// An installed fleet from before `fleet release`, NEWER than the tag.
	fake := "#!/bin/sh\ncase \"$1\" in version) echo 'fleet 2026.12.01.1 (abcdef012345)';; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(install, "fleet"), []byte(fake), 0o755); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	env = append(env, "FLEET_INSTALL_DIR="+install, "FLEET_RELEASE_CACHE_DIR="+t.TempDir())
	out, err := runScript(t, env, "update.sh", "--no-pull", "--tag", fixtureTag)
	if err == nil || !strings.Contains(out, "refusing a downgrade") {
		t.Fatalf("want a downgrade refusal, got err=%v\n%s", err, out)
	}
	if strings.Contains(out, "could not download") {
		t.Errorf("the downgrade guard must run before any download\n%s", out)
	}
	out, _ = runScript(t, env, "update.sh", "--no-pull", "--tag", fixtureTag, "--allow-downgrade")
	if strings.Contains(out, "refusing a downgrade") {
		t.Errorf("--allow-downgrade must pass the guard\n%s", out)
	}
}
