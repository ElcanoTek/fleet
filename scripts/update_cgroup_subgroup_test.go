// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFleetServiceAndUpdateEnforceDelegateSubgroup pins both sides of the
// cgroup v2 sandbox-limit fix:
//  1. deploy/fleet.service ships `Delegate=yes` paired with
//     `DelegateSubgroup=supervisor` so `fleet` does not occupy
//     `fleet.service/cgroup.procs` and block subtree controller activation.
//  2. scripts/update.sh migrates an existing `/etc/systemd/system/fleet.service`
//     that has `Delegate=yes` without `DelegateSubgroup=` in place so
//     `fleet update --yes` brings already-provisioned boxes into parity.
func TestFleetServiceAndUpdateEnforceDelegateSubgroup(t *testing.T) {
	root := repoRoot(t)
	unit := readFile(t, root, "deploy/fleet.service")
	hasDelegate := false
	hasSubgroup := false
	for line := range strings.SplitSeq(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Delegate=yes" {
			hasDelegate = true
		}
		if trimmed == "DelegateSubgroup=supervisor" {
			hasSubgroup = true
		}
	}
	if !hasDelegate || !hasSubgroup {
		t.Fatalf("deploy/fleet.service must contain both Delegate=yes (%v) and DelegateSubgroup=supervisor (%v)", hasDelegate, hasSubgroup)
	}

	upd := readFile(t, root, "scripts/update.sh")
	if !strings.Contains(upd, "DelegateSubgroup=supervisor") {
		t.Fatal("scripts/update.sh missing in-place DelegateSubgroup=supervisor migration for existing boxes")
	}

	// Verify the sed migration expression idempotently upgrades a pre-subgroup
	// unit to match deploy/fleet.service's functional lines and leaves an
	// already-migrated or customized DelegateSubgroup= unit untouched.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.service")
	legacyContent := strings.Replace(unit, "Delegate=yes\nDelegateSubgroup=supervisor", "Delegate=yes", 1)
	if legacyContent == unit {
		t.Fatal("expected Delegate=yes followed by DelegateSubgroup=supervisor in deploy/fleet.service")
	}
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o600); err != nil {
		t.Fatal(err)
	}

	const snippet = `
set -euo pipefail
installed="$1"
if grep -q '^Delegate=yes$' "$installed" && ! grep -q '^DelegateSubgroup=' "$installed"; then
  sed -i 's/^Delegate=yes$/Delegate=yes\nDelegateSubgroup=supervisor/' "$installed"
fi
`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", snippet, "bash", legacyPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("migration snippet failed: %v\n%s", err, out)
	}
	migrated, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(migrated) != unit {
		t.Fatalf("migrated unit does not match deploy/fleet.service")
	}

	// Running a second time is a no-op.
	cmd2 := exec.CommandContext(t.Context(), "bash", "-c", snippet, "bash", legacyPath)
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("second migration run failed: %v\n%s", err, out)
	}
	migrated2, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(migrated2) != unit {
		t.Fatalf("second migration run mutated unit")
	}
}
