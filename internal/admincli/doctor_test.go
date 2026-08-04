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
		"Scheduled backups",
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
		// Post-upgrade podman-info deferral: step 2's own stack upgrade must
		// not fail the box on the transient store lock it created (the step-6
		// restart clears it; step 7 re-verifies). Learned in production on
		// chat's doctor.
		"re-verifying after the service restart",
		"without sourcing it",
		"--network=none",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("doctor.sh must contain %q", want)
		}
	}
	// #966: the backup check must stay an ADVISORY when no timer is installed
	// (an operator who snapshots the volume is not misconfigured) and a FAILURE
	// when the timer's last run did not succeed.
	for _, want := range []string{
		`advise "no ${BACKUP_TIMER} + ${BACKUP_SERVICE} pair installed`,
		`fail "${BACKUP_SERVICE} last run FAILED`,
		`systemctl show -p Result --value "$BACKUP_SERVICE"`,
		// is-enabled reads the install symlink only: an enabled-but-stopped
		// timer fires nothing while its service's Result still says "success".
		`systemctl is-active --quiet "$BACKUP_TIMER"`,
		// The pair must ride the same unit-drift check as fleet.service.
		`for unit in "${SERVICE_NAME}.service" fleet-web.service "$BACKUP_SERVICE" "$BACKUP_TIMER"; do`,
		// …with the restart it can trigger scoped to the app units: reinstalling
		// a backup unit must not bounce the chat service.
		`*) restart_needed=1 ;;`,
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
