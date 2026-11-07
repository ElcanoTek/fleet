// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReleaseHealthURLUsesTheDeployedAddress — `fleet release rollback` (and
// install) with no address flag must probe where the deployment listens, read
// from the server env file like every other verb, not a hardcoded default.
func TestReleaseHealthURLUsesTheDeployedAddress(t *testing.T) {
	env := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(env, []byte("FLEET_SERVER_ADDR=0.0.0.0:9191\nFLEET_TLS_MODE=manual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", env)
	t.Setenv("FLEET_SERVER_ADDR", "")
	t.Setenv("FLEET_TLS_MODE", "")
	resetEnvFileCache()
	t.Cleanup(resetEnvFileCache)
	var inf installFlags
	if got := inf.resolvedHealthURL(); got != "https://127.0.0.1:9191/readyz" {
		t.Errorf("health URL %q, want the env file's address over TLS", got)
	}
	inf.serverAddr = "127.0.0.1:7070"
	if got := inf.resolvedHealthURL(); got != "https://127.0.0.1:7070/readyz" {
		t.Errorf("--server-addr must win: %q", got)
	}
	inf.healthURL = "http://x/readyz"
	if got := inf.resolvedHealthURL(); got != "http://x/readyz" {
		t.Errorf("--health-url must win: %q", got)
	}
}
