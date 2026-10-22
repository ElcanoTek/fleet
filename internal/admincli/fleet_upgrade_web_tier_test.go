// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upgradeHarness runs scripts/fleet-upgrade.sh --no-build against a scratch
// box: a stub systemctl that records every call (and reports an installed,
// running fleet-web), a stub curl whose exit decides the readiness gate, a
// fake new binary, and the retired tier's unit under a scratch unit dir.
type upgradeHarness struct {
	dir, src, install, unitDir, log string
	env                             []string
}

func newUpgradeHarness(t *testing.T, ready, newHasBundle bool) *upgradeHarness {
	t.Helper()
	h := &upgradeHarness{dir: t.TempDir()}
	h.src = filepath.Join(h.dir, "src")
	h.install = filepath.Join(h.dir, "opt")
	h.unitDir = filepath.Join(h.dir, "units")
	h.log = filepath.Join(h.dir, "systemctl.log")
	bin := filepath.Join(h.dir, "bin")
	for _, d := range []string{h.src, h.install, h.unitDir, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	webExit := "0"
	if !newHasBundle {
		webExit = "1"
	}
	write(filepath.Join(h.src, "fleet"), "#!/bin/sh\n[ \"$1 $2\" = \"version --web\" ] && exit "+webExit+"\necho new\n", 0o755)
	write(filepath.Join(h.install, "fleet"), "#!/bin/sh\necho old\n", 0o755)
	write(filepath.Join(h.unitDir, "fleet-web.service"), "[Service]\n", 0o644)
	write(filepath.Join(bin, "systemctl"), "#!/bin/sh\necho \"$*\" >> "+h.log+"\ncase \"$1\" in show) exit 0 ;; esac\nexit 0\n", 0o755)
	curlExit := "1"
	if ready {
		curlExit = "0"
	}
	write(filepath.Join(bin, "curl"), "#!/bin/sh\nexit "+curlExit+"\n", 0o755)
	h.env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FLEET_WEB_TIER_UNIT_DIR=" + h.unitDir,
		"FLEET_WEB_TIER_SHIM=" + filepath.Join(h.dir, "no-shim"),
		"FLEET_WEB_TIER_BUILD_DIR=" + filepath.Join(h.dir, "no-build"),
		"FLEET_UPGRADE_YES=1",
	}
	return h
}

func (h *upgradeHarness) run(t *testing.T) (string, error) {
	t.Helper()
	return runScript(t, h.env, "fleet-upgrade.sh", "--no-build", "--src", h.src, "--install-dir", h.install,
		"--health-url", "http://127.0.0.1:1/readyz", "--health-timeout", "1")
}

func (h *upgradeHarness) calls(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(h.log)
	return string(b)
}

// A failed readiness gate rolls back to the previous binary — which may
// predate ADR-0091 and serve no UI — so the old web tier must come back with
// it: stopped reversibly before the restart, restarted on rollback, and its
// unit file never deleted.
func TestFleetUpgradeRollbackRestoresTheOldWebTier(t *testing.T) {
	h := newUpgradeHarness(t, false, true)
	out, err := h.run(t)
	if err == nil {
		t.Fatalf("a failed readiness gate must fail the upgrade\n%s", out)
	}
	calls := h.calls(t)
	stop := strings.Index(calls, "disable --now fleet-web.service")
	restart := strings.Index(calls, "restart fleet")
	restore := strings.LastIndex(calls, "enable --now fleet-web.service")
	if stop < 0 || restart < 0 || stop > restart {
		t.Fatalf("fleet-web must be stopped before fleet restarts:\n%s", calls)
	}
	if restore < restart {
		t.Fatalf("rollback did not restart fleet-web:\n%s\n--- output ---\n%s", calls, out)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, "fleet-web.service")); err != nil {
		t.Fatalf("the old tier's unit was deleted before readiness passed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(h.install, "fleet")); !strings.Contains(string(b), "old") {
		t.Fatalf("rollback did not restore the previous binary: %q", b)
	}
}

// A passing readiness gate is what makes the retirement final.
func TestFleetUpgradeRemovesTheOldWebTierAfterReadiness(t *testing.T) {
	h := newUpgradeHarness(t, true, true)
	if out, err := h.run(t); err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, "fleet-web.service")); !os.IsNotExist(err) {
		t.Fatalf("the old tier's unit survived a successful upgrade: %v", err)
	}
	if strings.Contains(h.calls(t), "enable --now fleet-web.service") {
		t.Fatalf("a successful upgrade restarted fleet-web:\n%s", h.calls(t))
	}
}

// --no-build must not install a binary that carries no web bundle.
func TestFleetUpgradeNoBuildRefusesABundlelessBinary(t *testing.T) {
	h := newUpgradeHarness(t, true, false)
	out, err := h.run(t)
	if err == nil || !strings.Contains(out, "carries no web bundle") {
		t.Fatalf("want a refusal naming the missing bundle, got %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(h.install, "fleet")); !strings.Contains(string(b), "old") {
		t.Fatalf("the live binary was replaced: %q", b)
	}
	if strings.Contains(h.calls(t), "restart") || strings.Contains(h.calls(t), "fleet-web") {
		t.Fatalf("nothing may be stopped or restarted:\n%s", h.calls(t))
	}
}
