package admincli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The stale-pause error podman prints, verbatim: the ONE signature that may
// trigger `podman system migrate` while fleet is live.
const stalePauseErr = `Error: invalid internal status, try resetting the pause process with "podman system migrate": could not find any running process: no such process`

// podmanMigrateLib runs one call into scripts/lib/podman-migrate.sh and returns
// its trimmed stdout. Arguments go through argv, never the script text, so an
// error string with quotes cannot change the snippet.
func podmanMigrateLib(t *testing.T, fn string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping podman-migrate.sh unit test")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh")
	cmd := exec.Command("bash", append([]string{"-c", `. "$0"; set -u; ` + fn + ` "$@"`, lib}, args...)...)
	cmd.Env = append(os.Environ(), "TERM=dumb")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %q: %v\n%s", fn, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPodmanMigratePlan — step 3 of doctor. migrate stops every running
// container of the service user, and fleet's --rm sandboxes are then gone
// while the live process keeps handing out their handles (the fleetdev
// outage). So healthy podman never migrates here (nothing to reset, and no
// check-then-act race with a supervisor doctor cannot reserve), and a stale
// pause migrates only a unit doctor controls: live and restartable
// (stop → migrate → start), or quiesced. Never under --no-restart on a live
// box, never for a supervisor doctor cannot stop, never for another failure.
func TestPodmanMigratePlan(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		infoOK, infoErr, live, fleetUp string
		canRestart, quiesced           string
		want                           string
	}{
		{"healthy, warm pool running", "1", "", "8", "1", "1", "0", "defer"},
		{"healthy, nothing live, unit quiesced", "1", "", "0", "0", "0", "1", "defer"},
		{"healthy, other supervisor between restarts", "1", "", "0", "0", "0", "0", "defer"},
		{"healthy, listing failed", "1", "", "unknown", "0", "1", "0", "defer"},
		{"stale pause, live, restartable", "0", stalePauseErr, "0", "1", "1", "0", "migrate-restart"},
		{"stale pause, unit quiesced", "0", stalePauseErr, "0", "0", "0", "1", "migrate"},
		{"stale pause, live, --no-restart", "0", stalePauseErr, "0", "1", "0", "0", "refuse"},
		// No loaded unit: an external supervisor may relaunch fleet between a
		// check and the migrate, so an instantaneous "no process" licenses nothing.
		{"stale pause, no unit, no process seen", "0", stalePauseErr, "0", "0", "0", "0", "refuse"},
		{"other podman failure, live", "0", "Error: cannot chdir to /root: Permission denied", "0", "1", "1", "0", "none"},
		{"other podman failure, quiesced", "0", "Error: no space left on device", "0", "0", "0", "1", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podmanMigrateLib(t, "podman_migrate_plan", tc.infoOK, tc.infoErr, tc.live, tc.fleetUp, tc.canRestart, tc.quiesced)
			if got != tc.want {
				t.Errorf("podman_migrate_plan = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSmokeRetryPlan — step 8 of doctor. After step 3 spared a live pool, a
// failed smoke may migrate + restart only on the stale-pause signature: a
// launch that failed for PID or disk exhaustion leaves the pool serving, and
// a kubernetes box's pool is pods that a local podman fault never touches.
func TestSmokeRetryPlan(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		deferred, canRestart, localPool, smoke string
		quiesced                               string
		want                                   string
	}{
		{"stale pause, live unit, pool in this store", "1", "1", "8", stalePauseErr, "0", "retry"},
		{"pids exhausted", "1", "1", "8", "Error: crun: pids limit reached", "0", "report"},
		{"disk full", "1", "1", "8", "Error: no space left on device", "0", "report"},
		{"no-restart", "1", "0", "8", stalePauseErr, "0", "report"},
		{"step 3 already migrated", "0", "1", "8", stalePauseErr, "0", "report"},
		// No fleet sandbox containers in this store (a kubernetes-backed
		// fleet, or no evidence at all): a local podman fault never restarts
		// the live control plane — whatever its config says.
		{"live unit, no local pool evidence", "1", "1", "0", stalePauseErr, "0", "report"},
		{"live unit, pool count unknown", "1", "1", "", stalePauseErr, "0", "report"},
		// Fleet stopped with doctor's unit quiesced: reset without a restart.
		{"stale pause, unit quiesced", "1", "0", "0", stalePauseErr, "1", "migrate"},
		{"pids exhausted, quiesced", "1", "0", "0", "Error: crun: pids limit reached", "1", "report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podmanMigrateLib(t, "smoke_retry_plan", tc.deferred, tc.canRestart, tc.localPool, tc.smoke, tc.quiesced)
			if got != tc.want {
				t.Errorf("smoke_retry_plan = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnitProvenStopped — only systemd's explicit inactive/failed is a stop.
// "activating" is the Restart=always auto-restart delay (systemd is about to
// start fleet again) and "deactivating" is still running: migrating during
// either deletes a pool that is, or is about to be, live.
func TestUnitProvenStopped(t *testing.T) {
	// unit_proven_stopped reports through its exit status; this wrapper turns
	// that into output, since podmanMigrateLib fails on a non-zero exit.
	const probe = `probe() { if unit_proven_stopped "$1"; then echo stopped; else echo live; fi; }; probe`
	for state, want := range map[string]string{
		"inactive": "stopped", "failed": "stopped",
		"active": "live", "activating": "live", "deactivating": "live", "reloading": "live", "": "live",
	} {
		if got := podmanMigrateLib(t, probe, state); got != want {
			t.Errorf("unit_proven_stopped(%q) => %s, want %s", state, got, want)
		}
	}
}

// TestFleetIsLive — step 3's liveness gate. The unit counts as live unless
// systemd reports it PROVEN stopped: the Restart=always auto-restart delay
// ("activating") must not look like "nothing live", or migrate runs just
// before systemd starts fleet again and deletes its new pool. A fleet process
// under another supervisor counts on its own.
func TestFleetIsLive(t *testing.T) {
	const stubs = `SERVICE_NAME=fleet SERVICE_USER=fleet
systemctl() { [[ "$1" == show ]] && echo "$UNIT_STATE"; }
pgrep() { [[ "$PROC" == 1 ]]; }
if fleet_is_live; then echo live; else echo idle; fi`
	for _, tc := range []struct{ state, proc, want string }{
		{"inactive", "0", "idle"},
		{"failed", "0", "idle"},
		{"active", "0", "live"},
		{"activating", "0", "live"}, // the auto-restart delay
		{"deactivating", "0", "live"},
		{"inactive", "1", "live"}, // another supervisor's fleet
	} {
		cmd := exec.Command("bash", "-c", `. "$0"; set -u; `+stubs, filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh"))
		cmd.Env = append(os.Environ(), "UNIT_STATE="+tc.state, "PROC="+tc.proc)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("fleet_is_live with ActiveState=%q, process=%s => %s, want %s", tc.state, tc.proc, got, tc.want)
		}
	}
}

// TestUnitRestartable — whether doctor may stop and start the unit to rebuild
// the pool. "activating" (the Restart=always delay — a unit stuck restarting
// on a stale store) must qualify, or that box can never be repaired. An
// installed but inactive unit must not, even with a live fleet process: that
// process is another supervisor's, and starting the unit runs a second fleet.
func TestUnitRestartable(t *testing.T) {
	const stubs = `SERVICE_NAME=fleet
systemctl() { case "$3" in LoadState) echo "$LOAD" ;; ActiveState) echo "$STATE" ;; esac; }
if unit_restartable; then echo yes; else echo no; fi`
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh")
	for _, tc := range []struct{ load, state, want string }{
		{"loaded", "active", "yes"},
		{"loaded", "activating", "yes"},
		{"loaded", "deactivating", "yes"},
		{"loaded", "inactive", "no"},
		{"loaded", "failed", "no"},
		{"not-found", "inactive", "no"},
		{"", "", "no"}, // no systemd
	} {
		cmd := exec.Command("bash", "-c", `. "$0"; set -u; `+stubs, lib)
		cmd.Env = append(os.Environ(), "LOAD="+tc.load, "STATE="+tc.state)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("unit_restartable with LoadState=%q ActiveState=%q => %s, want %s", tc.load, tc.state, got, tc.want)
		}
	}
}

// TestMigrateLiveServiceInterrupted — a SIGTERM (what `fleet doctor` now sends
// on Ctrl-C / cancellation instead of SIGKILL) while fleet is stopped for the
// migrate must still start fleet and fleet-web again before the script exits.
func TestMigrateLiveServiceInterrupted(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping podman-migrate.sh unit test")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh")
	// run_as_fleet signals the shell mid-"migrate", as an interrupt would.
	stubs := strings.Replace(migrateLiveServiceStubs,
		`run_as_fleet() { echo "run_as_fleet $*" >>"$LOG";`,
		`run_as_fleet() { echo "run_as_fleet $*" >>"$LOG"; kill -TERM $$;`, 1)
	stubs = strings.Replace(stubs, `migrate_live_service; echo "rc=$?" >>"$LOG"`,
		`trap 'cat "$LOG"' EXIT; migrate_live_service; echo "UNREACHED" >>"$LOG"`, 1)
	cmd := exec.Command("bash", "-c", `. "$0"; `+stubs, lib)
	cmd.Env = append(os.Environ(), "TERM=dumb", "STOP_RC=0", "STOP_STICKS=0", "PROC_LINGERS=0", "START_RC=0", "WEB=active")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("want exit 130 from the interrupt trap, got %v\n%s", err, out)
	}
	for _, w := range []string{"run_as_fleet podman system migrate\nstart fleet.service\nstart fleet-web.service\n"} {
		if !strings.Contains(string(out), w) {
			t.Errorf("want %q (fleet restored after the interrupt) in:\n%s", w, out)
		}
	}
	if strings.Contains(string(out), "UNREACHED") {
		t.Errorf("the interrupt must end the script, got:\n%s", out)
	}
}

// TestCountOwnedSandboxes — pool evidence counts only sandboxes owned by the
// live unit's current MainPID. Orphans from a crashed earlier podman-backed
// run (a later kubernetes boot does not prune them) must not make a
// kubernetes-backed fleet look like it owns a local pool.
func TestCountOwnedSandboxes(t *testing.T) {
	labels := "636304@1790274734\n636304@1790274734\n4242@1790000000\n\n636304x@1\n"
	for _, tc := range []struct{ pid, labels, want string }{
		{"636304", labels, "2"},
		{"4242", labels, "1"},
		{"999", labels, "0"}, // only orphans in the store
		{"0", labels, "0"},   // no live unit
		{"", labels, "0"},
		{"636304", "", "0"},
	} {
		if got := podmanMigrateLib(t, "count_owned_sandboxes", tc.pid, tc.labels); got != tc.want {
			t.Errorf("count_owned_sandboxes(%q) = %s, want %s", tc.pid, got, tc.want)
		}
	}
}

// TestFleetProcessAbsent — only pgrep's explicit no-match (exit 1) proves no
// fleet process. A host without pgrep, or a pgrep error, must count as
// present: treating "command not found" as absence would license a migrate
// under a live, externally supervised fleet.
func TestFleetProcessAbsent(t *testing.T) {
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh")
	for _, tc := range []struct{ name, stub, want string }{
		{"no match", `pgrep() { return 1; }`, "absent"},
		{"match", `pgrep() { return 0; }`, "present"},
		{"pgrep error", `pgrep() { return 2; }`, "present"},
		// No pgrep at all: an empty PATH, and no function to stand in.
		{"no pgrep on the host", `PATH=/nonexistent`, "present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `. "$0"; SERVICE_USER=fleet; ` + tc.stub + `; if fleet_process_absent; then echo absent; else echo present; fi`
			out, err := exec.Command("bash", "-c", script, lib).CombinedOutput()
			if err != nil {
				t.Fatalf("bash: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("fleet_process_absent => %s, want %s", got, tc.want)
			}
		})
	}
}

// TestUnitQuiesced — "nothing live" that licenses an unattended migrate:
// doctor's own unit loaded and proven stopped (systemd will not restart an
// inactive/failed unit on its own) and no fleet process. Without a loaded
// unit it is never quiesced: an external supervisor can relaunch fleet
// between the check and the migrate, and doctor cannot hold it off.
func TestUnitQuiesced(t *testing.T) {
	const stubs = `SERVICE_NAME=fleet SERVICE_USER=fleet
systemctl() { case "$3" in LoadState) echo "$LOAD" ;; ActiveState) echo "$STATE" ;; esac; }
pgrep() { [[ "$PROC" == 1 ]]; }
if unit_quiesced; then echo yes; else echo no; fi`
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh")
	for _, tc := range []struct{ load, state, proc, want string }{
		{"loaded", "inactive", "0", "yes"},
		{"loaded", "failed", "0", "yes"},
		{"loaded", "activating", "0", "no"}, // Restart=always delay
		{"loaded", "active", "0", "no"},
		{"loaded", "inactive", "1", "no"},    // another supervisor's fleet
		{"not-found", "inactive", "0", "no"}, // no unit doctor controls
		{"", "", "0", "no"},                  // no systemd
	} {
		cmd := exec.Command("bash", "-c", `. "$0"; set -u; `+stubs, lib)
		cmd.Env = append(os.Environ(), "LOAD="+tc.load, "STATE="+tc.state, "PROC="+tc.proc)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("unit_quiesced LoadState=%q ActiveState=%q process=%s => %s, want %s", tc.load, tc.state, tc.proc, got, tc.want)
		}
	}
}

// migrateLiveServiceStubs replaces systemctl, run_as_fleet and the reporters
// with shell functions that log every call (to a file: the helper silences
// run_as_fleet's output), so migrate_live_service's ordering
// can be asserted without systemd, podman or root. Unit state lives in shell
// variables the stub mutates: STOP_RC (the stop job's exit code) and
// STOP_STICKS (the unit stays active, or "deactivating") independently,
// PROC_LINGERS (a fleet process survives the unit), START_RC, and WEB
// (fleet-web's state before).
const migrateLiveServiceStubs = `
SERVICE_NAME=fleet SERVICE_USER=fleet
fleet=active; web="$WEB"
# A fleet process outlives the unit when PROC_LINGERS=1.
pgrep() { [[ "$PROC_LINGERS" == 1 ]]; }
LOG="$(mktemp)"
# The helper sends run_as_fleet's output to /dev/null, so log to a file.
# RuntimeDirectory= is gone after a stop; the helper must recreate it.
install() { echo "install $*" >>"$LOG"; }
run_as_fleet() { echo "run_as_fleet $*" >>"$LOG"; [[ "${MIGRATE_RC:-0}" == 0 ]] || { echo "Error: migrate broke" >&2; return 1; }; }
fixed() { echo "fixed: $*" >>"$LOG"; }
fail() { echo "fail: $*" >>"$LOG"; }
systemctl() {
  local unit="${*: -1}"
  case "$1" in
    is-active)
      if [[ "$unit" == fleet-web.service ]]; then [[ "$web" == active ]]; else [[ "$fleet" == active ]]; fi
      return ;;
    show) echo "$fleet"; return 0 ;; # show -p ActiveState --value fleet.service
    stop)
      echo "stop $unit" >>"$LOG"
      # STOP_STICKS=1: stays active; =deactivating: mid-stop, still running.
      case "$STOP_STICKS" in
        1) ;;
        deactivating) fleet=deactivating ;;
        *) fleet=inactive; web=inactive ;; # BindsTo: fleet-web goes down with it
      esac
      return "$STOP_RC" ;;
    start)
      echo "start $unit" >>"$LOG"
      if [[ "$unit" == fleet.service ]]; then [[ "$START_RC" == 0 ]] || return 1; fleet=active; else web=active; fi
      return 0 ;;
  esac
}
migrate_live_service; echo "rc=$?" >>"$LOG"
cat "$LOG"; rm -f "$LOG"
`

// TestMigrateLiveService — the one action that may migrate a live box's store.
// It must be a single stop → migrate → start, and the stop must be PROVEN (a
// successful job and the unit inactive) before the store is touched:
// migrating under a unit that is still running deletes the live pool, which is
// the outage this whole change exists to prevent. fleet-web (BindsTo) must be
// started again when it was running, since starting fleet does not return it.
func TestMigrateLiveService(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping podman-migrate.sh unit test")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "podman-migrate.sh")
	for _, tc := range []struct {
		name            string
		env             []string
		want, forbidden []string
	}{
		{
			name: "stop, migrate, start, fleet-web back",
			env:  []string{"STOP_RC=0", "STOP_STICKS=0", "PROC_LINGERS=0", "START_RC=0", "WEB=active"},
			want: []string{"stop fleet.service\ninstall -d -m 0700 -o fleet -g fleet /run/fleet\nrun_as_fleet podman system migrate\nstart fleet.service\nstart fleet-web.service\n", "fixed: fleet-web.service started again", "rc=0"},
		},
		{
			name:      "fleet-web was not running",
			env:       []string{"STOP_RC=0", "STOP_STICKS=0", "PROC_LINGERS=0", "START_RC=0", "WEB=inactive"},
			want:      []string{"run_as_fleet podman system migrate\nstart fleet.service\n", "rc=0"},
			forbidden: []string{"start fleet-web.service"},
		},
		{
			name: "stop job fails, unit still active",
			env:  []string{"STOP_RC=1", "STOP_STICKS=1", "PROC_LINGERS=0", "START_RC=0", "WEB=active"},
			// Aborted, but the already-issued stop must not leave fleet down.
			want:      []string{"fail: fleet.service did not stop (ActiveState=active) — podman system migrate NOT run", "rc=1", "start fleet.service"},
			forbidden: []string{"run_as_fleet podman system migrate"},
		},
		{
			name: "stop returns but the unit stays active",
			env:  []string{"STOP_RC=0", "STOP_STICKS=1", "PROC_LINGERS=0", "START_RC=0", "WEB=active"},
			// Aborted, but the already-issued stop must not leave fleet down.
			want:      []string{"fail: fleet.service did not stop (ActiveState=active) — podman system migrate NOT run", "rc=1", "start fleet.service"},
			forbidden: []string{"run_as_fleet podman system migrate"},
		},
		{
			// A stop that "failed" (timed out, then systemd killed it) but left
			// the unit inactive: the store is safe to touch, and fleet must be
			// started again rather than left down with nothing after it.
			name: "stop job fails but the unit went inactive",
			env:  []string{"STOP_RC=1", "STOP_STICKS=0", "PROC_LINGERS=0", "START_RC=0", "WEB=active"},
			want: []string{"stop fleet.service\ninstall -d -m 0700 -o fleet -g fleet /run/fleet\nrun_as_fleet podman system migrate\nstart fleet.service\nstart fleet-web.service\n", "rc=0"},
		},
		{
			// is-active is false for "deactivating", but the unit still runs.
			name: "stop interrupted mid-deactivation",
			env:  []string{"STOP_RC=1", "STOP_STICKS=deactivating", "PROC_LINGERS=0", "START_RC=0", "WEB=active"},
			// Aborted, but the already-issued stop must not leave fleet down.
			want:      []string{"fail: fleet.service did not stop (ActiveState=deactivating) — podman system migrate NOT run", "rc=1", "start fleet.service"},
			forbidden: []string{"run_as_fleet podman system migrate"},
		},
		{
			name: "unit inactive but a fleet process lingers",
			env:  []string{"STOP_RC=0", "STOP_STICKS=0", "PROC_LINGERS=1", "START_RC=0", "WEB=active"},
			// Aborted, but the already-issued stop must not leave fleet down.
			want:      []string{"podman system migrate NOT run", "rc=1", "start fleet.service"},
			forbidden: []string{"run_as_fleet podman system migrate"},
		},
		{
			// A failed migrate is reported with podman's own diagnostic, and
			// the service is still restored — never "fixed".
			name:      "migrate itself fails",
			env:       []string{"STOP_RC=0", "STOP_STICKS=0", "PROC_LINGERS=0", "START_RC=0", "WEB=active", "MIGRATE_RC=1"},
			want:      []string{"fail: podman system migrate failed as fleet: Error: migrate broke", "start fleet.service\nstart fleet-web.service\n", "rc=1"},
			forbidden: []string{"fixed: podman"},
		},
		{
			name: "fleet does not start again",
			env:  []string{"STOP_RC=0", "STOP_STICKS=0", "PROC_LINGERS=0", "START_RC=1", "WEB=inactive"},
			want: []string{"run_as_fleet podman system migrate", "fail: fleet.service did not start again", "rc=1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `. "$0"; set -u; `+migrateLiveServiceStubs, lib)
			cmd.Env = append(append(os.Environ(), "TERM=dumb"), tc.env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash: %v\n%s", err, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(out), w) {
					t.Errorf("want %q in:\n%s", w, out)
				}
			}
			for _, f := range tc.forbidden {
				if strings.Contains(string(out), f) {
					t.Errorf("must not contain %q:\n%s", f, out)
				}
			}
		})
	}
}
