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

// TestCockpitUninstall pins scripts/lib/cockpit.sh: bootstrap removes
// Cockpit when cockpit-ws is installed, skips with FLEET_KEEP_COCKPIT=1, is
// silent without rpm or cockpit, prints instead of running under --dry-run,
// and only warns when dnf fails.
func TestCockpitUninstall(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "cockpit.sh")
	run := func(t *testing.T, withRPM bool, env ...string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		log := filepath.Join(dir, "calls.log")
		stub := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("#!"+bash+"\nprintf '"+name+" %s\\n' \"$*\" >> '"+log+"'\n"+body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if withRPM {
			stub("rpm", `[[ "${STUB_INSTALLED:-0}" == 1 ]]`+"\n")
		}
		stub("dnf", `[[ "${STUB_FAIL:-0}" != 1 ]]`+"\n")
		script := `set -euo pipefail
. "$1"
DRY_RUN="${DRY_RUN:-0}"
info() { printf 'INFO %s\n' "$*"; }
ok()   { printf 'OK %s\n' "$*"; }
warn() { printf 'WARN %s\n' "$*"; }
run()  { if [[ "$DRY_RUN" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi; }
cockpit_uninstall
printf 'RC=%s\n' "$?"
`
		cmd := exec.Command(bash, "-c", script, "cockpit", lib)
		cmd.Env = append([]string{"PATH=" + dir}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "RC=0") {
			t.Fatalf("cockpit_uninstall failed: %v\n%s", err, out)
		}
		calls, _ := os.ReadFile(log)
		return string(out), string(calls)
	}

	if out, calls := run(t, true, "STUB_INSTALLED=1"); !strings.Contains(calls, "dnf -y remove cockpit*") || !strings.Contains(out, "OK removed Cockpit") {
		t.Errorf("installed: want dnf -y remove cockpit*\ncalls:\n%s\n%s", calls, out)
	}
	for name, tc := range map[string]struct {
		rpm bool
		env []string
	}{
		"not installed": {true, nil},
		"no rpm":        {false, []string{"STUB_INSTALLED=1"}},
		"kept":          {true, []string{"STUB_INSTALLED=1", "FLEET_KEEP_COCKPIT=1"}},
		"dry run":       {true, []string{"STUB_INSTALLED=1", "DRY_RUN=1"}},
	} {
		out, calls := run(t, tc.rpm, tc.env...)
		if strings.Contains(calls, "dnf") {
			t.Errorf("%s: dnf ran\n%s", name, out)
		}
		if name == "dry run" && !strings.Contains(out, "[dry-run] dnf -y remove cockpit*") {
			t.Errorf("dry run: plan missing\n%s", out)
		}
		if (name == "not installed" || name == "no rpm") && strings.TrimSpace(out) != "RC=0" {
			t.Errorf("%s: want silence\n%s", name, out)
		}
	}
	if out, _ := run(t, true, "STUB_INSTALLED=1", "STUB_FAIL=1"); !strings.Contains(out, "WARN could not remove Cockpit") {
		t.Errorf("failure: want a warning\n%s", out)
	}
}

// TestCockpitUninstallIsBootstrapOnly — update never touches Cockpit.
func TestCockpitUninstallIsBootstrapOnly(t *testing.T) {
	root := repoRootFromTest(t)
	boot, _ := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if !strings.Contains(string(boot), "\ncockpit_uninstall\n") {
		t.Error("bootstrap.sh does not call cockpit_uninstall")
	}
	upd, _ := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if strings.Contains(string(upd), "cockpit") {
		t.Error("update.sh must not touch Cockpit")
	}
}
