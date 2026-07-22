// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorDryRunSmoke — `doctor.sh --dry-run` must succeed on any host (no
// root, no dnf, no systemd) and its checklist must name the load-bearing
// passes: package currency, the rootless-podman prerequisites, unit drift,
// the env files, the health probes, and the sandbox smoke.
func TestDoctorDryRunSmoke(t *testing.T) {
	out := runScriptDryRun(t, "doctor.sh", "--dry-run")
	for _, want := range []string{
		"Toolchain",
		"Package currency",
		"Rootless podman",
		"subuid/subgid",
		"functional drift",
		"OPENROUTER_API_KEY",
		"/healthz + /readyz",
		"Sandbox smoke",
		"Source freshness",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor --dry-run checklist missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestDoctorHelp — --help must work unprivileged and document the three modes.
func TestDoctorHelp(t *testing.T) {
	out, err := runScript(t, nil, "doctor.sh", "--help")
	if err != nil {
		t.Fatalf("doctor --help exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	for _, want := range []string{"--check", "--no-restart", "--dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor --help missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestDoctorRejectsUnknownFlag — a typo must die loudly, not run repairs with
// a silently-ignored flag (e.g. `--chek` running fixes the operator wanted to
// preview).
func TestDoctorRejectsUnknownFlag(t *testing.T) {
	out, err := runScript(t, nil, "doctor.sh", "--chek")
	if err == nil {
		t.Fatalf("doctor accepted an unknown flag\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "unknown argument") {
		t.Errorf("expected the unknown-argument error, got:\n%s", out)
	}
}

// TestDoctorLoadBearingStrings — the repair semantics that took production
// debugging to learn must stay in the script: the dnf skip_if_unavailable
// guard, `dnf upgrade` (NOT install) for currency, the bootstrap-matching
// subuid range and containers.conf body, root-owned-0600 env-file enforcement
// (fleet's env file is root's, unlike chat's app-user-owned .env.local), the
// report-only source-freshness rule, and never sourcing the secrets file.
func TestDoctorLoadBearingStrings(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		"skip_if_unavailable=1",
		"upgrade -y --quiet nodejs",
		":100000:65536",
		`cgroup_manager = "cgroupfs"`,
		"podman system migrate",
		"600 root",
		"Report-only in every mode",
		"without sourcing it",
		"--network=none",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("doctor.sh must contain %q", want)
		}
	}
	// Doctor must never source the secrets env file (a tampered box would get
	// code execution as root the moment the operator runs doctor).
	for _, forbid := range []string{"source \"$ENV_FILE\"", ". \"$ENV_FILE\""} {
		if strings.Contains(script, forbid) {
			t.Errorf("doctor.sh must not source the env file (%q present)", forbid)
		}
	}
}
