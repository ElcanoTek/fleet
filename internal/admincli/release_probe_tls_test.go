// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withEnvFile(t *testing.T, body string) {
	t.Helper()
	env := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", env)
	for _, k := range []string{"FLEET_SERVER_ADDR", "FLEET_TLS_MODE", "FLEET_TLS_DOMAIN", "FLEET_TLS_CERT_FILE"} {
		t.Setenv(k, "")
	}
	resetEnvFileCache()
	t.Cleanup(resetEnvFileCache)
}

// TestReleaseProbeTLSUsesTheDeploymentsDomain — a TLS chat listener is probed
// on loopback with ServerName = FLEET_TLS_DOMAIN and full verification.
func TestReleaseProbeTLSUsesTheDeploymentsDomain(t *testing.T) {
	withEnvFile(t, "FLEET_SERVER_ADDR=:8443\nFLEET_TLS_MODE=auto\nFLEET_TLS_DOMAIN=fleet.example.test\n")
	var inf installFlags
	o, err := inf.options(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if o.HealthURL != "https://127.0.0.1:8443/readyz" {
		t.Fatalf("health URL %q", o.HealthURL)
	}
	cfg, err := inf.probeTLS(o.HealthURL)
	if err != nil || cfg == nil || cfg.ServerName != "fleet.example.test" || cfg.InsecureSkipVerify {
		t.Fatalf("probe TLS %+v, %v", cfg, err)
	}
	inf.probeServerName = "flag.example.test"
	if cfg, _ := inf.probeTLS(o.HealthURL); cfg.ServerName != "flag.example.test" {
		t.Errorf("--probe-server-name must win, got %q", cfg.ServerName)
	}
}

// TestReleaseProbeTLSWithoutADomainFailsClearly — never a skipped check.
func TestReleaseProbeTLSWithoutADomainFailsClearly(t *testing.T) {
	withEnvFile(t, "FLEET_SERVER_ADDR=:8443\nFLEET_TLS_MODE=manual\n")
	var inf installFlags
	_, err := inf.options(os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "FLEET_TLS_DOMAIN") || !strings.Contains(err.Error(), "--probe-server-name") {
		t.Fatalf("want a clear error naming FLEET_TLS_DOMAIN and --probe-server-name, got %v", err)
	}
	// --no-restart probes nothing, so it needs no domain.
	inf.noRestart = true
	if _, err := inf.options(os.Stderr); err != nil {
		t.Fatalf("--no-restart: %v", err)
	}
}
