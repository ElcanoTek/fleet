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

// TestTestDBSetupRedactsTheDSN: the preflight's connect-failure message names
// the server it tried, never the credentials in the DSN — a URL's userinfo
// (user and password) or a password= parameter. The DSNs point at a port
// nothing listens on, so psql fails without reaching any server.
func TestTestDBSetupRedactsTheDSN(t *testing.T) {
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not installed")
	}
	script := filepath.Join(repoRoot(t), "scripts", "test-db-setup.sh")
	const pw = "placeholder-password"
	for _, dsn := range []string{
		"postgres://someone:" + pw + "@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=2",
		"postgres://127.0.0.1:1/postgres?user=someone&password=" + pw + "&connect_timeout=2",
	} {
		cmd := exec.Command("bash", script)
		cmd.Env = append(os.Environ(), "FLEET_TEST_DATABASE_URL="+dsn, "PGPASSWORD=", "PGCONNECT_TIMEOUT=2")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("preflight against a dead port succeeded:\n%s", out)
		}
		text := string(out)
		if !strings.Contains(text, "cannot connect with") {
			t.Fatalf("no connect-failure message:\n%s", text)
		}
		if strings.Contains(text, pw) || strings.Contains(text, "someone:") {
			t.Errorf("the preflight printed credentials for %q:\n%s", dsn, text)
		}
		if !strings.Contains(text, "127.0.0.1:1/postgres") {
			t.Errorf("the message no longer names the server it tried:\n%s", text)
		}
	}
}
