// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDoctorDryRunSmoke — `doctor.sh --dry-run` must succeed on any host (no
// root, no dnf, no systemd) and its checklist must name the load-bearing
// passes: package currency, the rootless-podman prerequisites, unit drift,
// the env files, the health probes, the scheduled timers + disk headroom, and
// the sandbox smoke.
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
		"Scheduled maintenance",
		"fleet-backup.timer",
		// The maintenance timer is what keeps stale sandbox image layers from
		// filling the disk, and the disk check is what notices when nothing
		// has. Both are new enough that a silent regression is plausible.
		"fleet-maintenance.timer",
		"free space on the data dir",
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
		// migrate stops every running container of the user; on a live box
		// that deleted the service's whole --rm warm pool while the process
		// kept handing out dead handles. It must stay gated on the service
		// being down (or podman already broken, with a restart after).
		// Learned in production on fleetdev.
		"migrate would stop its live sandbox containers",
		"stopped for it and started again, rebuilding its sandbox pool",
		// The decisions themselves live in scripts/lib/podman-migrate.sh
		// (TestPodmanMigratePlan pins every branch); doctor must source it.
		"lib/podman-migrate.sh",
		// The manual recovery names the CONFIGURED unit: a literal "fleet"
		// under FLEET_SERVICE_NAME would leave the real service running while
		// migrate deletes its sandboxes.
		"or by hand: stop ${SERVICE_NAME},",
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
		// The absent-pair advisories hand the operator the one-command verb
		// (which installs from deploy/, reloads and enables), not a copy-paste
		// install/daemon-reload/enable chain.
		"sudo fleet timers install --backup",
		"sudo fleet timers install --maintenance",
		`fail "${BACKUP_SERVICE} last run FAILED`,
		`systemctl show -p Result --value "$BACKUP_SERVICE"`,
		// is-enabled reads the install symlink only: an enabled-but-stopped
		// timer fires nothing while its service's Result still says "success".
		`systemctl is-active --quiet "$BACKUP_TIMER"`,
		// Both timer pairs must ride the same unit-drift check as fleet.service.
		`for unit in "${SERVICE_NAME}.service" fleet-web.service "$BACKUP_SERVICE" "$BACKUP_TIMER" "$MAINT_SERVICE" "$MAINT_TIMER"; do`,
		// …with the restart it can trigger scoped to the app units: reinstalling
		// a backup unit must not bounce the chat service.
		`*) restart_needed=1 ;;`,
		// Host maintenance: same advisory-if-absent / fail-if-last-run-failed
		// posture as backups, because the layers it prunes accumulate silently.
		`advise "no ${MAINT_TIMER} + ${MAINT_SERVICE} pair installed`,
		`fail "${MAINT_SERVICE} last run FAILED`,
		// Disk headroom must name the configured floor, not just "getting
		// full": below the floor the process has already stopped claiming
		// scheduled work, which is a different statement to an operator.
		"FLEET_DISK_MIN_FREE_PERCENT",
		"HOLDING BACK scheduled tasks",
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

// TestCmdDoctorCancelsWithSIGTERM — `fleet doctor` must hand an interrupt to
// the script as a trappable SIGTERM, not exec's default SIGKILL: while doctor
// has fleet stopped for a podman store reset (migrate_live_service), the
// script's trap is what starts fleet again, and bash cannot trap SIGKILL.
// The fixture script traps TERM, takes 3s in the trap (a restore is not
// instant, so no forced-kill deadline may follow the SIGTERM) and exits 7;
// the test signals its own process the way an operator's Ctrl-C / SIGTERM
// reaches `fleet doctor`.
func TestCmdDoctorCancelsWithSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The trap takes a few seconds, as a real restore (a service start) does:
	// a forced kill deadline after the SIGTERM would cut it short.
	script := `trap 'kill "$sleeper" 2>/dev/null; sleep 3; echo trapped >"$DOCTOR_OUT"; exit 7' TERM
sleep 30 & sleeper=$!
: >"$DOCTOR_READY"
wait
`
	if err := os.WriteFile(filepath.Join(root, "scripts", "doctor.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, ready := filepath.Join(root, "out"), filepath.Join(root, "ready")
	t.Setenv("FLEET_ROOT", root)
	t.Setenv("DOCTOR_OUT", out)
	t.Setenv("DOCTOR_READY", ready)
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(ready); err == nil {
				// cmdDoctor's signal.NotifyContext is registered before the
				// script starts, so this cancels the run rather than the test.
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	if code := cmdDoctor(nil); code != 7 {
		t.Fatalf("cmdDoctor exit = %d, want 7 (the script's TERM trap); a SIGKILL cancel gives -1", code)
	}
	if b, err := os.ReadFile(out); err != nil || strings.TrimSpace(string(b)) != "trapped" {
		t.Fatalf("the script's TERM trap did not run: %q, %v", b, err)
	}
}
