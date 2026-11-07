// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestUpdateRunsOneAtATime — a second `fleet update` while one holds the
// lock refuses before touching the checkout.
func TestUpdateRunsOneAtATime(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available")
	}
	_, env, sha := realignFixture(t, "v2026.11.01.1")
	state := t.TempDir()
	f, err := os.OpenFile(filepath.Join(state, "update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	before := sha("HEAD")
	out, err := runScript(t, append(env, "FLEET_STATE_DIR="+state), "update.sh", "--yes")
	if err == nil || !strings.Contains(out, "another fleet update is running") {
		t.Fatalf("want a refusal while another update holds the lock, got err=%v\n%s", err, out)
	}
	if sha("HEAD") != before {
		t.Fatal("the refused update moved the checkout")
	}
}
