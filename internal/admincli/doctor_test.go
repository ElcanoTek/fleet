// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// TestDoctorNeverRunsPodmanMigrate — `podman system migrate` stops every
// running container of the service user, and fleet's sandboxes run --rm, so
// running it under a live fleet deletes the whole warm pool while the process
// keeps handing out the dead handles: "no such container" on every chat turn
// and task until a restart (learned in production on fleetdev, where a
// repair-mode doctor did exactly that). Doctor must never run it; it detects a
// stale pause process and prints the repair for an operator or agent to judge.
func TestDoctorNeverRunsPodmanMigrate(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(body), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") {
			continue
		}
		// Printed guidance lives inside double-quoted strings; an executed
		// call would be a bare command such as `run_as_fleet podman system migrate`.
		if strings.Contains(code, "podman system migrate") && !strings.Contains(code, `"`) && !strings.Contains(code, `'`) {
			t.Errorf("doctor.sh:%d executes podman system migrate: %s", i+1, code)
		}
		if strings.Contains(code, "run_as_fleet podman system migrate") {
			t.Errorf("doctor.sh:%d executes podman system migrate: %s", i+1, code)
		}
	}
}

// TestDoctorStalePauseHelpers — the detection and the printed repair, run
// straight out of doctor.sh. Only podman's own stale-pause error is matched
// (a disk or PID failure is not migrate's to fix), and the repair — one
// &&-chain gated on a proven stop — names the
// CONFIGURED unit, user and home, recreates /run/<user> (the stop removes it)
// and brings fleet-web back (the stop takes it down through BindsTo=).
func TestDoctorStalePauseHelpers(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fns := make([]string, 0, 2)
	for _, name := range []string{"is_stale_pause_error", "stale_pause_fix"} {
		start := strings.Index(string(body), "\n"+name+"() {")
		if start < 0 {
			t.Fatalf("doctor.sh has no %s()", name)
		}
		end := strings.Index(string(body)[start:], "\n}\n")
		fns = append(fns, string(body)[start:start+end+3])
	}
	const stale = `Error: invalid internal status, try resetting the pause process with "podman system migrate": could not find any running process: no such process`
	script := strings.Join(fns, "\n") + `
SERVICE_NAME=fleet-prod SERVICE_USER=svc SERVICE_HOME=/srv/svc
for e in "$STALE" "Error: crun: pids limit reached" "Error: no space left on device"; do
  if is_stale_pause_error "$e"; then echo "match: $e"; else echo "no: $e"; fi
done
stale_pause_fix`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "STALE="+stale)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	for _, want := range []string{
		"match: " + stale,
		"no: Error: crun: pids limit reached",
		"no: Error: no space left on device",
		"fleet sched task list --status running",
		// One &&-chain: migrate runs only after a successful stop AND with
		// no fleet process left (another supervisor is not stopped by
		// systemctl), so a pasted repair can never migrate a live pool.
		"sudo systemctl stop fleet-prod && { pgrep -u svc -x fleet >/dev/null; [ $? -eq 1 ]; } && sudo install -d -m 0700 -o svc -g svc /run/svc && (cd /srv/svc && sudo -u svc HOME=/srv/svc XDG_RUNTIME_DIR=/run/svc podman system migrate) && sudo systemctl start fleet-prod",
		"sudo systemctl start fleet-web",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
}

// TestDoctorStalePauseRepairChainIsGated — the printed repair, EXECUTED with
// stubbed sudo/pgrep. Pasted as one line, it must reach `podman system
// migrate` only after a successful stop AND pgrep's explicit "no fleet
// process" (exit 1): not when the stop fails, not while fleet is alive (a
// supervisor systemctl cannot stop), and not when pgrep is missing (127) —
// any of those would migrate a live pool, the outage this PR fixes.
func TestDoctorStalePauseRepairChainIsGated(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(body), "\nstale_pause_fix() {")
	end := strings.Index(string(body)[start:], "\n}\n")
	fn := string(body)[start : start+end+3]
	render := exec.Command("bash", "-c", fn+`
SERVICE_NAME=fleet SERVICE_USER=fleet SERVICE_HOME=/tmp
fix="$(stale_pause_fix)"; chain="${fix#*run as one line: }"; printf '%s' "${chain%% — then*}"`)
	chain, err := render.Output()
	if err != nil || !strings.HasPrefix(string(chain), "sudo systemctl stop fleet && ") {
		t.Fatalf("could not extract the repair chain (%v): %q", err, chain)
	}
	for _, tc := range []struct {
		name            string
		stopRC, pgrepRC int
		wantMigrate     bool
	}{
		{"clean stop, no fleet process", 0, 1, true},
		{"stop fails", 1, 1, false},
		{"fleet still alive (another supervisor)", 0, 0, false},
		{"pgrep missing", 0, 127, false},
		{"pgrep error", 0, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubs := fmt.Sprintf(`sudo() { if [[ $1 == systemctl && $2 == stop ]]; then return %d; fi; echo "RAN: $*"; }
pgrep() { return %d; }
`, tc.stopRC, tc.pgrepRC)
			out, _ := exec.Command("bash", "-c", stubs+string(chain)).CombinedOutput()
			if got := strings.Contains(string(out), "podman system migrate"); got != tc.wantMigrate {
				t.Errorf("migrate ran = %v, want %v\n%s", got, tc.wantMigrate, out)
			}
		})
	}
}
