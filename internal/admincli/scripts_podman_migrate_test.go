package admincli

import (
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
		{"kubernetes backend, pool of pods", "kubernetes", "1", "", "0", "1", "1", "migrate"},
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podmanMigrateLib(t, "smoke_retry_plan", tc.deferred, tc.canRestart, tc.backend, tc.smoke)
			if got != tc.want {
				t.Errorf("smoke_retry_plan = %q, want %q", got, tc.want)
			}
		})
	}
}
