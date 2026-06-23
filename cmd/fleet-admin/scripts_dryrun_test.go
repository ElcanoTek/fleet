// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRootFromTest walks up from the package dir to the repo root (the dir that
// holds scripts/bootstrap.sh) so the script smoke tests run regardless of cwd.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "bootstrap.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (no scripts/bootstrap.sh above %s)", dir)
		}
		dir = parent
	}
}

// runScriptDryRun executes a repo script with --dry-run and returns combined
// output. It fails the test on a non-zero exit (a dry-run must never touch the
// box, so it should always succeed). Skips when bash is unavailable.
func runScriptDryRun(t *testing.T, script string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping operator-script dry-run smoke test")
	}
	root := repoRootFromTest(t)
	full := append([]string{filepath.Join(root, "scripts", script)}, args...)
	cmd := exec.Command("bash", full...)
	cmd.Dir = root
	// A dry-run reads the bundle manifest; point at the in-repo generic bundle so
	// the test is self-contained and never depends on an external checkout.
	cmd.Env = append(os.Environ(),
		"FLEET_CLIENT_CONFIG_DIR="+filepath.Join(root, "config", "default"),
		"TERM=dumb",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v exited non-zero: %v\n--- output ---\n%s", script, args, err, out)
	}
	return string(out)
}

// TestBootstrapDryRunSmoke is the regression guard for #91 (operator-script
// coverage): `bootstrap.sh --dry-run` must succeed and its plan must still
// include the steps the readiness audit fixed — the pg_hba scram rewrite (#78)
// and, with --enable-service, the build+install of the fleet binary (#71).
func TestBootstrapDryRunSmoke(t *testing.T) {
	out := runScriptDryRun(t, "bootstrap.sh", "--dry-run", "--postgres=local", "--enable-service")
	for _, want := range []string{
		// The toolchain-install STEP must be in the plan. We assert only the
		// step header (always printed), NOT the dnf package line — that line only
		// renders on a dnf host, and CI runs on a non-dnf (apt) runner where the
		// step prints the "install these yourself" warning instead.
		"Installing system dependencies",
		"client bundle manifest found",
		"pg_hba",                                 // the scram-sha-256 loopback rewrite step (#78)
		"Building + installing the fleet binary", // the binary build+install step (#71)
		"would install fleet + fleet-admin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestUpdateDryRunSmoke is the regression guard for #91: `update.sh
// --dry-run --no-pull` must succeed and its plan must include the binary
// build + the install-to-deploy-path step (#71 — without which an update is a
// silent no-op against the live binary).
func TestUpdateDryRunSmoke(t *testing.T) {
	out := runScriptDryRun(t, "update.sh", "--dry-run", "--no-pull")
	for _, want := range []string{
		"make build",
		"would install fleet + fleet-admin", // the install-to-ExecStart step (#71)
		"Restarting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("update --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}
