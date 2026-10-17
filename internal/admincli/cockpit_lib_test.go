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

// cockpitStubs writes fake systemctl + firewall-cmd binaries into a fresh dir
// and returns it. Each stub appends its argv to calls.log and answers from env:
//
//	STUB_COCKPIT  absent | live | masked   (what systemctl knows of cockpit.socket)
//	STUB_ACTIVE   1 → `systemctl is-active cockpit.socket` succeeds
//	STUB_FAIL     1 → every mutating systemctl call fails
//	STUB_FW       running | stopped        (firewall-cmd --state)
//	STUB_ZONES    space-separated zones; STUB_SVC_<zone> their permanent
//	              services, STUB_RT_<zone> their runtime ones
//
// Builtins only, so the PATH can hold nothing but the stubs.
func cockpitStubs(t *testing.T, withSystemctl, withFirewall bool) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!"+bash+"\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if withSystemctl {
		write("systemctl", `printf 'systemctl %s\n' "$*" >> "$STUB_LOG"
case "$1" in
  list-unit-files)
    case "$STUB_COCKPIT" in
      live)   printf 'cockpit.socket disabled disabled\n' ;;
      masked) printf 'cockpit.socket masked enabled\n' ;;
    esac ;;
  is-enabled) [[ "$STUB_COCKPIT" == masked ]] && { printf 'masked\n'; exit 1; }; printf 'disabled\n'; exit 1 ;;
  is-active) [[ "${STUB_ACTIVE:-0}" == 1 ]] ;;
  disable|mask|stop) [[ "${STUB_FAIL:-0}" != 1 ]] ;;
esac
`)
	}
	// The banner override needs systemd-tmpfiles (stubbed: it logs) plus the
	// real cmp and install. Linked only alongside systemctl, so the
	// no-systemd case keeps a PATH with nothing on it.
	if withSystemctl {
		write("systemd-tmpfiles", `printf 'systemd-tmpfiles %s\n' "$*" >> "$STUB_LOG"
`)
		for _, tool := range []string{"cmp", "install"} {
			resolved, err := exec.LookPath(tool)
			if err != nil {
				t.Skipf("%s not available", tool)
			}
			if err := os.Symlink(resolved, filepath.Join(dir, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if withFirewall {
		write("firewall-cmd", `printf 'firewall-cmd %s\n' "$*" >> "$STUB_LOG"
perm=0
for a in "$@"; do [[ "$a" == --permanent ]] && perm=1; done
for a in "$@"; do
  case "$a" in
    --state) [[ "$STUB_FW" == running ]] && { printf 'running\n'; exit 0; }; printf 'not running\n'; exit 252 ;;
    --get-zones) printf '%s\n' "$STUB_ZONES"; exit 0 ;;
    --zone=*) zone="${a#--zone=}" ;;
    --list-services) if [[ $perm == 1 ]]; then v="STUB_SVC_${zone}"; else v="STUB_RT_${zone}"; fi; printf '%s\n' "${!v}"; exit 0 ;;
  esac
done
exit 0
`)
	}
	return dir
}

// runCockpitKeepOff sources scripts/lib/cockpit.sh under the callers' shell
// options with bootstrap/update-shaped helpers, runs cockpit_keep_off, and
// returns its output plus every stub call made.
func runCockpitKeepOff(t *testing.T, stubDir string, env ...string) (out string, calls []string) {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "cockpit.sh")
	logPath := filepath.Join(t.TempDir(), "calls.log")
	script := `set -euo pipefail
. "$1"
DRY_RUN="${DRY_RUN:-0}"
ok()   { printf 'OK %s\n' "$*"; }
info() { printf 'INFO %s\n' "$*"; }
warn() { printf 'WARN %s\n' "$*"; }
run()  { if [[ "$DRY_RUN" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi; }
cockpit_keep_off
printf 'RC=%s\n' "$?"
`
	bash, _ := exec.LookPath("bash")
	cmd := exec.Command(bash, "-c", script, "cockpit", lib)
	// No cockpit-ws tmpfiles entry unless a case provides one: never the
	// host's real /usr/lib or /etc paths.
	scratch := t.TempDir()
	cmd.Env = append([]string{
		"PATH=" + stubDir, "STUB_LOG=" + logPath,
		"COCKPIT_PKG_TMPFILES=" + filepath.Join(scratch, "absent.conf"),
		"COCKPIT_ETC_TMPFILES=" + filepath.Join(scratch, "etc", "cockpit-ws.conf"),
	}, env...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cockpit_keep_off exited non-zero: %v\n%s", err, b)
	}
	if raw, err := os.ReadFile(logPath); err == nil {
		calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return string(b), calls
}

func mutating(calls []string) []string {
	var m []string
	for _, c := range calls {
		for _, verb := range []string{"systemctl disable", "systemctl mask", "systemctl stop", "--remove-service", "--reload", "systemd-tmpfiles --create"} {
			if strings.Contains(c, verb) {
				m = append(m, c)
				break
			}
		}
	}
	return m
}

// TestCockpitKeepOff pins scripts/lib/cockpit.sh, the step bootstrap.sh and
// update.sh run to keep the Cockpit web console (a root-capable login on
// :9090) off: disable+stop+mask a live socket and drop cockpit from every
// firewalld zone allowing it — and touch nothing when there is no systemd, no
// cockpit, or no running firewalld, when it is already masked, or under
// --dry-run. It never fails its caller.
func TestCockpitKeepOff(t *testing.T) {
	fwCockpit := []string{"STUB_FW=running", "STUB_ZONES=public trusted", "STUB_SVC_public=ssh dhcpv6-client cockpit", "STUB_SVC_trusted=ssh", "STUB_RT_public=ssh cockpit", "STUB_RT_trusted=ssh"}

	t.Run("no systemd, no firewalld", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, false, false))
		if len(calls) != 0 || !strings.Contains(out, "INFO cockpit is not installed") || !strings.Contains(out, "RC=0") {
			t.Errorf("want one info line and no calls; calls=%v\n%s", calls, out)
		}
	})

	t.Run("cockpit absent leaves the firewall alone", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, true, true), append([]string{"STUB_COCKPIT=absent"}, fwCockpit...)...)
		if m := mutating(calls); len(m) != 0 {
			t.Errorf("absent cockpit must change nothing, got %v\n%s", m, out)
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "firewall-cmd") {
				t.Errorf("absent cockpit must not even probe firewalld, got %q", c)
			}
		}
	})

	t.Run("live socket is masked and cockpit leaves the firewall", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, true, true), append([]string{"STUB_COCKPIT=live"}, fwCockpit...)...)
		want := []string{
			"systemctl disable --now cockpit.socket",
			"systemctl mask cockpit.socket",
			// Permanent and runtime edited directly: no --reload, which would
			// drop unrelated runtime-only rules.
			"firewall-cmd -q --permanent --zone=public --remove-service=cockpit",
			"firewall-cmd -q --zone=public --remove-service=cockpit",
		}
		if got := mutating(calls); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("mutations:\n got %v\nwant %v\n%s", got, want, out)
		}
		if !strings.Contains(out, "OK cockpit.socket disabled, stopped and masked") {
			t.Errorf("missing success line\n%s", out)
		}
	})

	t.Run("already masked is idempotent", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, true, true), "STUB_COCKPIT=masked", "STUB_FW=running", "STUB_ZONES=public", "STUB_SVC_public=ssh")
		if m := mutating(calls); len(m) != 0 {
			t.Errorf("masked socket + clean firewall must change nothing, got %v\n%s", m, out)
		}
		if !strings.Contains(out, "already masked") {
			t.Errorf("want an already-masked line\n%s", out)
		}
	})

	t.Run("a masked socket that is still listening is stopped", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, true, false), "STUB_COCKPIT=masked", "STUB_ACTIVE=1")
		if got := mutating(calls); strings.Join(got, "|") != "systemctl stop cockpit.socket" {
			t.Errorf("want only systemctl stop cockpit.socket, got %v\n%s", got, out)
		}
	})

	t.Run("firewalld not running is never touched", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, true, true), "STUB_COCKPIT=masked", "STUB_FW=stopped", "STUB_ZONES=public", "STUB_SVC_public=cockpit")
		if m := mutating(calls); len(m) != 0 {
			t.Errorf("stopped firewalld must not be changed, got %v\n%s", m, out)
		}
	})

	t.Run("dry run changes nothing", func(t *testing.T) {
		out, calls := runCockpitKeepOff(t, cockpitStubs(t, true, true), append([]string{"STUB_COCKPIT=live", "DRY_RUN=1"}, fwCockpit...)...)
		if m := mutating(calls); len(m) != 0 {
			t.Errorf("dry run made changes: %v\n%s", m, out)
		}
		for _, want := range []string{"[dry-run] systemctl disable --now cockpit.socket", "[dry-run] systemctl mask cockpit.socket", "[dry-run] firewall-cmd -q --permanent --zone=public --remove-service=cockpit"} {
			if !strings.Contains(out, want) {
				t.Errorf("dry-run plan missing %q\n%s", want, out)
			}
		}
	})

	t.Run("login-banner notice is blanked once", func(t *testing.T) {
		dir := t.TempDir()
		pkg := filepath.Join(dir, "usr-lib-cockpit-ws.conf")
		etc := filepath.Join(dir, "etc", "tmpfiles.d", "cockpit-ws.conf")
		if err := os.WriteFile(pkg, []byte("L /run/cockpit/issue - - - - active.issue\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stubs := cockpitStubs(t, true, false)
		env := []string{"STUB_COCKPIT=masked", "COCKPIT_PKG_TMPFILES=" + pkg, "COCKPIT_ETC_TMPFILES=" + etc}
		out, calls := runCockpitKeepOff(t, stubs, env...)
		if got := mutating(calls); len(got) != 1 || got[0] != "systemd-tmpfiles --create "+etc {
			t.Errorf("want one systemd-tmpfiles --create %s, got %v\n%s", etc, got, out)
		}
		installed, err := os.ReadFile(etc)
		if err != nil {
			t.Fatalf("override not installed: %v\n%s", err, out)
		}
		shipped, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "deploy", "cockpit-ws.tmpfiles.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if string(installed) != string(shipped) {
			t.Errorf("installed override differs from deploy/cockpit-ws.tmpfiles.conf")
		}
		// Idempotent: the second run finds it current and applies nothing.
		out, calls = runCockpitKeepOff(t, stubs, env...)
		if got := mutating(calls); len(got) != 0 || !strings.Contains(out, "already blank") {
			t.Errorf("second run should be a no-op, got %v\n%s", got, out)
		}
	})

	t.Run("a failure warns and never fails the caller", func(t *testing.T) {
		out, _ := runCockpitKeepOff(t, cockpitStubs(t, true, false), "STUB_COCKPIT=live", "STUB_FAIL=1")
		if !strings.Contains(out, "WARN could not turn cockpit off") || !strings.Contains(out, "RC=0") {
			t.Errorf("want a warning and rc 0\n%s", out)
		}
	})
}

// TestCockpitStepWiredIntoBootstrapAndUpdate — both installers must run the
// step: bootstrap for new boxes, every update so existing boxes stay healed.
func TestCockpitStepWiredIntoBootstrapAndUpdate(t *testing.T) {
	for _, tc := range []struct{ script, wantStep string }{
		{"bootstrap.sh", "Keeping the Cockpit web console off"},
		{"update.sh", "Host: keeping the Cockpit web console off"},
	} {
		src, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", tc.script))
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		if !strings.Contains(s, `. "$SCRIPT_DIR/lib/cockpit.sh"`) || !strings.Contains(s, "\ncockpit_keep_off\n") {
			t.Errorf("%s does not source lib/cockpit.sh and call cockpit_keep_off", tc.script)
		}
		if !strings.Contains(s, tc.wantStep) {
			t.Errorf("%s missing step %q", tc.script, tc.wantStep)
		}
	}
}
