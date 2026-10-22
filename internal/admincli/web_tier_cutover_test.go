// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cutoverBox is a scratch box for the web-tier cutover: the retired tier's
// unit in a scratch unit dir, and a stub systemctl that keeps real state
// (which units are enabled/active) so a restore is observable, with a switch
// that makes fleet fail to restart.
type cutoverBox struct {
	dir, unitDir, state string
	env                 []string
}

const statefulSystemctl = `#!/bin/bash
S="$FAKE_STATE"; echo "$*" >> "$S/log"
svc="${@: -1}"; svc="${svc%.service}"
case "$1" in
  cat) [[ "$svc" == fleet-web ]] && { [[ -f "$FLEET_WEB_TIER_UNIT_DIR/fleet-web.service" ]]; exit; }; exit 0 ;;
  is-enabled) [[ -f "$S/$svc.enabled" ]] ;;
  is-active) [[ -f "$S/$svc.active" ]] ;;
  disable) rm -f "$S/$svc.enabled"; [[ "$2" == --now ]] && rm -f "$S/$svc.active"; exit 0 ;;
  enable)
    [[ "$svc" == fleet-web && ! -f "$FLEET_WEB_TIER_UNIT_DIR/fleet-web.service" ]] && exit 1
    touch "$S/$svc.enabled"; [[ "$2" == --now ]] && touch "$S/$svc.active"; exit 0 ;;
  stop) rm -f "$S/$svc.active"; exit 0 ;;
  start|restart|try-restart)
    if [[ "$svc" == fleet && "${FAKE_FLEET_FAILS:-0}" == 1 ]]; then rm -f "$S/fleet.active"; exit 1; fi
    touch "$S/$svc.active"; exit 0 ;;
  *) exit 0 ;;
esac
`

func newCutoverBox(t *testing.T, webRunning, fleetFails bool) *cutoverBox {
	t.Helper()
	b := &cutoverBox{dir: t.TempDir()}
	b.unitDir = filepath.Join(b.dir, "units")
	b.state = filepath.Join(b.dir, "state")
	bin := filepath.Join(b.dir, "bin")
	for _, d := range []string{b.unitDir, b.state, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(bin, "systemctl"):               statefulSystemctl,
		filepath.Join(b.unitDir, "fleet-web.service"): "[Service]\n",
		filepath.Join(b.state, "fleet.enabled"):       "",
		filepath.Join(b.state, "fleet.active"):        "", // the OLD binary, still running
	}
	if webRunning {
		files[filepath.Join(b.state, "fleet-web.enabled")] = ""
		files[filepath.Join(b.state, "fleet-web.active")] = ""
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fails := "0"
	if fleetFails {
		fails = "1"
	}
	b.env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_STATE=" + b.state,
		"FAKE_FLEET_FAILS=" + fails,
		"FLEET_WEB_TIER_UNIT_DIR=" + b.unitDir,
		"FLEET_WEB_TIER_SHIM=" + filepath.Join(b.dir, "no-shim"),
		"FLEET_WEB_TIER_BUILD_DIR=" + filepath.Join(b.dir, "no-build"),
		"FLEET_WEB_TIER_READY_WAIT=1",
	}
	return b
}

func (b *cutoverBox) has(name string) bool {
	_, err := os.Stat(filepath.Join(b.state, name))
	return err == nil
}

func (b *cutoverBox) unitPresent() bool {
	_, err := os.Stat(filepath.Join(b.unitDir, "fleet-web.service"))
	return err == nil
}

// runBootstrapCutover runs bootstrap.sh's real cutover_web_tier_or_die, with
// web-tier.sh, the way both bootstrap call sites do — followed by a marker
// standing for everything bootstrap does next (more web-tier work, the
// success report), which a failed cutover must never reach.
func (b *cutoverBox) runBootstrapCutover(t *testing.T) (string, error) {
	t.Helper()
	root := repoRootFromTest(t)
	boot, err := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?ms)^cutover_web_tier_or_die\(\) \{.*?^\}`).Find(boot)
	if fn == nil {
		t.Fatal("bootstrap.sh has no cutover_web_tier_or_die")
	}
	script := "set -euo pipefail\n" +
		"ok() { echo \"OK $*\"; }; warn() { echo \"WARN $*\"; }; info() { :; }\n" +
		"die() { echo \"DIE $*\"; exit 1; }\n" +
		"SERVICE_NAME=fleet; REPO_ROOT=" + root + "\n" +
		". " + filepath.Join(root, "scripts", "lib", "web-tier.sh") + "\n" +
		string(fn) + "\n" +
		// bootstrap's enable-service section: stop before `enable --now`, then cut over.
		"fleet_stop_web_tier || true\nsystemctl enable --now fleet >/dev/null 2>&1 || true\n" +
		"if fleet_web_tier_present; then cutover_web_tier_or_die; fi\n" +
		"echo REACHED-THE-REST-OF-BOOTSTRAP\n"
	return runEnvShellWith(t, b.env, script)
}

func runEnvShellWith(t *testing.T, env []string, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Codex P1 on #1838: a bootstrap --enable-web re-run over a pre-ADR-0091 box
// whose new fleet fails to restart restored fleet-web, then went on into
// configure_web_tier, which stopped it again, deleted its unit and Next build,
// ignored the next restart failure and reported success — the UI stayed down.
// Now the restore ends the run: non-zero exit, the old tier running, its
// files intact, nothing after it executed.
func TestBootstrapRestartFailureKeepsTheOldWebTier(t *testing.T) {
	b := newCutoverBox(t, true, true)
	out, err := b.runBootstrapCutover(t)
	if err == nil {
		t.Fatalf("a failed restart must fail bootstrap\n%s", out)
	}
	if strings.Contains(out, "REACHED-THE-REST-OF-BOOTSTRAP") {
		t.Fatalf("bootstrap carried on after restoring the old tier:\n%s", out)
	}
	if !strings.Contains(out, "DIE fleet did not come up on the new binary") || !strings.Contains(out, "restarted, so the browser UI keeps working") {
		t.Fatalf("want a clear message naming the restored tier, got:\n%s", out)
	}
	if !b.unitPresent() {
		t.Fatal("the old tier's unit was deleted although fleet never came up")
	}
	if !b.has("fleet-web.active") || !b.has("fleet-web.enabled") {
		t.Fatalf("the old tier is not running again (state: active=%v enabled=%v)", b.has("fleet-web.active"), b.has("fleet-web.enabled"))
	}
}

// A restart that succeeds is what retires the old tier — after fleet is active.
func TestBootstrapRestartSuccessRetiresTheOldWebTier(t *testing.T) {
	b := newCutoverBox(t, true, false)
	out, err := b.runBootstrapCutover(t)
	if err != nil {
		t.Fatalf("bootstrap cutover failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "REACHED-THE-REST-OF-BOOTSTRAP") {
		t.Fatalf("a successful cutover must let bootstrap continue:\n%s", out)
	}
	if b.unitPresent() || b.has("fleet-web.active") {
		t.Fatalf("the old tier survived a successful cutover (unit=%v active=%v)", b.unitPresent(), b.has("fleet-web.active"))
	}
	log, _ := os.ReadFile(filepath.Join(b.state, "log"))
	calls := string(log)
	restart, active := strings.LastIndex(calls, "restart fleet"), strings.LastIndex(calls, "is-active --quiet fleet\n")
	removed := strings.LastIndex(calls, "reset-failed fleet-web.service")
	if restart < 0 || active < restart || removed < active {
		t.Fatalf("the old tier must be removed only after fleet restarted and reported active:\n%s", calls)
	}
}

// A tier that was installed but not running is left installed (and not
// started) when fleet fails; the run still fails.
func TestCutoverFailureLeavesAStoppedTierAlone(t *testing.T) {
	b := newCutoverBox(t, false, true)
	out, err := b.runBootstrapCutover(t)
	if err == nil || !strings.Contains(out, "was left installed") {
		t.Fatalf("want a failure naming the kept tier, got %v\n%s", err, out)
	}
	if !b.unitPresent() || b.has("fleet-web.active") {
		t.Fatalf("unit=%v active=%v, want the unit kept and not started", b.unitPresent(), b.has("fleet-web.active"))
	}
}

// Every path that retires the old tier removes it only after fleet is back
// up; nothing stops-and-removes in one go any more. bootstrap goes through
// cutover_web_tier_or_die at both sites; update.sh and doctor.sh remove only
// in their post-restart success branch; fleet-upgrade.sh after its readiness
// gate (TestFleetUpgrade* run it).
func TestWebTierIsRemovedOnlyAfterFleetIsUp(t *testing.T) {
	root := repoRootFromTest(t)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	lib := read("lib/web-tier.sh")
	if strings.Contains(lib, "fleet_retire_web_tier()") {
		t.Error("web-tier.sh still defines a stop-and-remove helper")
	}
	for _, name := range []string{"bootstrap.sh", "update.sh", "doctor.sh", "fleet-upgrade.sh"} {
		if strings.Contains(read(name), "fleet_retire_web_tier") {
			t.Errorf("%s still calls fleet_retire_web_tier", name)
		}
	}

	boot := read("bootstrap.sh")
	if strings.Contains(boot, "fleet_remove_web_tier") {
		t.Error("bootstrap.sh removes the old tier outside fleet_cutover_web_tier")
	}
	if n := strings.Count(boot, "cutover_web_tier_or_die\n"); n < 2 {
		t.Errorf("bootstrap.sh calls cutover_web_tier_or_die %d times, want both the service and the web-settings sites", n)
	}
	if regexp.MustCompile(`try-restart "\$SERVICE_NAME" >/dev/null 2>&1 \|\| true`).MatchString(boot) {
		t.Error("bootstrap.sh still ignores a failed restart")
	}

	for name, restart := range map[string]string{
		"update.sh": `systemctl restart "$SERVICE_NAME" && FLEET_WEB_TIER_READY_WAIT`,
		"doctor.sh": `systemctl restart "${SERVICE_NAME}.service" 2>/dev/null \` + "\n" + `       && { [[ "$web_tier_cutover" == "0" ]] || fleet_web_tier_wait_active`,
	} {
		body := read(name)
		r, rm := strings.Index(body, restart), strings.Index(body, "fleet_remove_web_tier")
		if r < 0 || rm < r {
			t.Errorf("%s: the old tier must be removed only after the gated restart (restart at %d, remove at %d)", name, r, rm)
		}
	}
}
