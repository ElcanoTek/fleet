// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateWithoutAUnitSwapsButNeverClaimsARestart — a fleet run by a
// supervisor other than systemd still gets the verified binary (an atomic swap
// with --no-restart), and update never reports it as running: the operator is
// told to restart it.
func TestUpdateWithoutAUnitSwapsButNeverClaimsARestart(t *testing.T) {
	stub := t.TempDir()
	// A systemctl that knows no units (as on a box whose fleet is not one).
	if err := os.WriteFile(filepath.Join(stub, "systemctl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := runScript(t, env, "update.sh", "--dry-run", "--no-pull", "--tag", "v2026.11.07.3")
	_ = err // the tag is not in this checkout: a blocker, still exit 0 under --dry-run
	if !strings.Contains(out, "is not installed: would install") || !strings.Contains(out, "NO restart") {
		t.Errorf("the plan must swap without a restart when there is no unit\n%s", out)
	}

	root := repoRootFromTest(t)
	body, rerr := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	script := string(body)
	for _, want := range []string{
		`--install-dir "$INSTALL_DIR" --service "$SERVICE_NAME" --no-restart`,
		`INSTALL_STATE=swapped`,
		`INSTALL_STATE=restarted`,
		`"$INSTALL_STATE" != "restarted" ]]; then
  die`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
}
