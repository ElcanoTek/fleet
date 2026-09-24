package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// outage). So it may run only when nothing is live, or on podman's own
// stale-pause error with a restart to follow — never under --no-restart or
// an unrestartable supervisor, and never for an unrelated podman failure.
func TestPodmanMigratePlan(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		backend, infoOK, infoErr, live, fleetUp string
		canRestart                              string
		want                                    string
	}{
		{"healthy, nothing live", "podman", "1", "", "0", "0", "1", "migrate"},
		{"healthy, warm pool running", "podman", "1", "", "8", "1", "1", "defer"},
		{"healthy, containers but no unit (other supervisor)", "podman", "1", "", "3", "0", "0", "defer"},
		{"healthy, fleet up with an empty pool", "podman", "1", "", "0", "1", "1", "defer"},
		{"healthy, listing failed", "podman", "1", "", "unknown", "0", "1", "defer"},
		{"stale pause, fleet down", "podman", "0", stalePauseErr, "0", "0", "0", "migrate"},
		{"stale pause, live, restartable", "podman", "0", stalePauseErr, "0", "1", "1", "migrate-restart"},
		{"stale pause, live, --no-restart", "podman", "0", stalePauseErr, "0", "1", "0", "refuse"},
		{"other podman failure, live", "podman", "0", "Error: cannot chdir to /root: Permission denied", "0", "1", "1", "none"},
		{"other podman failure, fleet down", "podman", "0", "Error: no space left on device", "0", "0", "1", "none"},
		// The backend never licenses a migrate: a live kubernetes box is gated
		// like any other, so a misread backend cannot delete a podman pool.
		{"kubernetes backend, fleet live", "kubernetes", "1", "", "0", "1", "1", "defer"},
		{"kubernetes backend, stale pause, --no-restart", "kubernetes", "0", stalePauseErr, "0", "1", "0", "refuse"},
		{"kubernetes backend, nothing live", "kubernetes", "1", "", "0", "0", "1", "migrate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podmanMigrateLib(t, "podman_migrate_plan", tc.backend, tc.infoOK, tc.infoErr, tc.live, tc.fleetUp, tc.canRestart)
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
		name                                 string
		deferred, canRestart, backend, smoke string
		want                                 string
	}{
		{"stale pause after a skip", "1", "1", "podman", stalePauseErr, "retry"},
		{"pids exhausted", "1", "1", "podman", "Error: crun: pids limit reached", "report"},
		{"disk full", "1", "1", "podman", "Error: no space left on device", "report"},
		{"no-restart", "1", "0", "podman", stalePauseErr, "report"},
		{"step 3 already migrated", "0", "1", "podman", stalePauseErr, "report"},
		{"kubernetes backend", "1", "1", "kubernetes", stalePauseErr, "report"},
		// Fail-closed: only a backend resolved as exactly podman may restart.
		// An expression doctor could not interpolate (nested braces) or an
		// unknown value restricts rather than being taken for podman.
		{"uninterpolated manifest expression", "1", "1", "${RUNNER_BACKEND:-pre {inner}}", stalePauseErr, "report"},
		{"unrecognized backend", "1", "1", "docker", stalePauseErr, "report"},
		{"manifest backend doctor could not parse", "1", "1", "unparsed", stalePauseErr, "report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podmanMigrateLib(t, "smoke_retry_plan", tc.deferred, tc.canRestart, tc.backend, tc.smoke)
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

// TestResolveSandboxBackend — doctor must see the backend the daemon runs
// (sandbox.ResolveBackend: env, else the bundle's sandbox.backend, else
// podman). Reading only the env file made a manifest-selected kubernetes box
// look like podman, so a local podman fault could restart its control plane.
func TestResolveSandboxBackend(t *testing.T) {
	for _, tc := range []struct{ env, manifest, want string }{
		{"", "", "podman"},
		{"kubernetes", "", "kubernetes"},
		{"", "kubernetes", "kubernetes"},
		{"  Kubernetes ", "", "kubernetes"},
		{"", " KUBERNETES", "kubernetes"},
		{"podman", "kubernetes", "podman"},
		{"kubernetes", "podman", "kubernetes"},
	} {
		if got := podmanMigrateLib(t, "resolve_sandbox_backend", tc.env, tc.manifest); got != tc.want {
			t.Errorf("resolve_sandbox_backend(%q, %q) = %q, want %q", tc.env, tc.manifest, got, tc.want)
		}
	}
}

// TestDoctorResolvesBackendLikeTheDaemon — the real doctor.sh, not the
// library. The backend (and the bundle, and any ${VAR} the manifest selects
// it through) must resolve the way the running daemon's does: its live
// process env first (clientconfig's "process env wins", where a key present
// but empty still wins — a unit Environment= line or an external supervisor
// can set values no file holds), then the deployment env file it folds in
// (#1123), then the manifest default. Each case runs a real process with a
// controlled env and points doctor at it, so the /proc read is exercised.
func TestDoctorResolvesBackendLikeTheDaemon(t *testing.T) {
	dir := t.TempDir()
	// Two bundles: "kube" selects kubernetes outright; "var" selects via ${...}.
	writeBundle := func(name, backend string) string {
		b := filepath.Join(dir, name)
		if err := os.MkdirAll(b, 0o755); err != nil {
			t.Fatal(err)
		}
		m := "app_name: Test\nsandbox:\n  tag: localhost/test:latest\n  backend: " + backend + "\n"
		if err := os.WriteFile(filepath.Join(b, "manifest.yaml"), []byte(m), 0o600); err != nil {
			t.Fatal(err)
		}
		return b
	}
	varBundle := writeBundle("var", "${RUNNER_BACKEND:-podman}")
	reqBundle := writeBundle("req", "${RUNNER_BACKEND:?set RUNNER_BACKEND}")
	kubeBundle := writeBundle("kube", "kubernetes")
	// Valid YAML the block reader must not misread as "no backend" (= podman).
	commented := filepath.Join(dir, "commented")
	inline := filepath.Join(dir, "inline")
	for b, m := range map[string]string{
		commented: "app_name: Test\nsandbox: # runner settings\n  backend: kubernetes\n",
		inline:    "app_name: Test\nsandbox: {tag: localhost/test:latest, backend: kubernetes}\n",
	} {
		if err := os.MkdirAll(b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b, "manifest.yaml"), []byte(m), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	envFile := filepath.Join(dir, "fleet.env")
	for _, tc := range []struct {
		name, envBody string
		daemonEnv     []string
		want          string
	}{
		{"env file ${VAR}", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\nRUNNER_BACKEND=kubernetes\n", nil, "kubernetes"},
		{"manifest default", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\n", nil, "podman"},
		{"env file backend beats manifest", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\nRUNNER_BACKEND=kubernetes\nFLEET_SANDBOX_BACKEND=podman\n", nil, "podman"},
		{"daemon env backend beats env file", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\nFLEET_SANDBOX_BACKEND=podman\n", []string{"FLEET_SANDBOX_BACKEND=kubernetes"}, "kubernetes"},
		{"daemon env ${VAR}", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\n", []string{"RUNNER_BACKEND=kubernetes"}, "kubernetes"},
		// Present-but-empty in the daemon's env still beats the env file.
		{"daemon env empty backend beats env file", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\nFLEET_SANDBOX_BACKEND=kubernetes\n", []string{"FLEET_SANDBOX_BACKEND="}, "podman"},
		{"manifest ${VAR:?msg}", "FLEET_CLIENT_CONFIG_DIR=" + reqBundle + "\n", []string{"RUNNER_BACKEND=kubernetes"}, "kubernetes"},
		{"sandbox block line with a comment", "FLEET_CLIENT_CONFIG_DIR=" + commented + "\n", nil, "kubernetes"},
		// Unreadable to the block parser: "unparsed", which is not podman, so
		// step 8 restricts (TestSmokeRetryPlan pins that any non-podman does).
		{"inline sandbox mapping", "FLEET_CLIENT_CONFIG_DIR=" + inline + "\n", nil, "unparsed"},
		// The daemon loads the bundle ITS env names, not the env file's.
		{"daemon bundle dir beats env file", "FLEET_CLIENT_CONFIG_DIR=" + varBundle + "\n", []string{"FLEET_CLIENT_CONFIG_DIR=" + kubeBundle}, "kubernetes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(envFile, []byte(tc.envBody), 0o600); err != nil {
				t.Fatal(err)
			}
			daemon := exec.Command("sleep", "60")
			daemon.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tc.daemonEnv...)
			if err := daemon.Start(); err != nil {
				t.Skipf("cannot start a stand-in daemon process: %v", err)
			}
			t.Cleanup(func() { _ = daemon.Process.Kill(); _ = daemon.Wait() })
			// Empty shell vars: the daemon's env and the env file must win.
			out, err := runScript(t, []string{
				"FLEET_ENV_FILE=" + envFile,
				"FLEET_DOCTOR_DAEMON_PID=" + strconv.Itoa(daemon.Process.Pid),
				"RUNNER_BACKEND=", "FLEET_SANDBOX_BACKEND=",
			}, "doctor.sh", "--dry-run")
			if err != nil {
				t.Fatalf("doctor --dry-run: %v\n%s", err, out)
			}
			if want := "sandbox backend: " + tc.want + " "; !strings.Contains(out, want) {
				t.Errorf("want %q in the dry-run, got:\n%s", want, out)
			}
		})
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
