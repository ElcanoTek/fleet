package admincli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapPerStoreKeysNamingOneDatabase (#2055 review): per-store keys
// that both name ONE database — compared by fleet_dsn_identity, as
// config.ResolveDSNs compares them, not as raw strings — are the one-database
// layout, converged from the key the sched store resolves from; keys naming
// two databases stay the legacy layout; one database under two roles is
// refused (bootstrap converges one role).
func TestBootstrapPerStoreKeysNamingOneDatabase(t *testing.T) {
	const pw = "placeholder-not-a-secret"
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	run := func(chat, sched string) (string, error) {
		t.Helper()
		body := "FLEET_CHAT_DATABASE_URL=" + chat + "\nFLEET_SCHED_DATABASE_URL=" + sched + "\n"
		if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local")
	}

	// The same database spelled differently: explicit default port vs none,
	// different search_path options, different query parameters.
	out, err := run("postgres://app:"+pw+"@127.0.0.1:5432/appdb?sslmode=disable&options=-csearch_path%3Dchat",
		"postgres://app:"+pw+"@127.0.0.1/appdb?options=-csearch_path%3Dsched")
	if err != nil {
		t.Fatalf("one database named per store: %v\n%s", err, out)
	}
	if strings.Contains(out, "legacy two-database layout") || !strings.Contains(out, "CREATE DATABASE appdb OWNER app") || strings.Contains(out, pw) {
		t.Errorf("per-store keys naming one database were not converged as one database\n--- output ---\n%s", out)
	}

	out, err = run("postgres://chat:"+pw+"@127.0.0.1:5432/chat", "postgres://sched:"+pw+"@127.0.0.1:5432/sched")
	if err != nil || !strings.Contains(out, "legacy two-database layout") {
		t.Errorf("two databases were not the legacy layout: %v\n%s", err, out)
	}

	out, err = run("postgres://one:"+pw+"@127.0.0.1:5432/appdb", "postgres://two:"+pw+"@127.0.0.1:5432/appdb")
	if err == nil || !strings.Contains(out, "different (or unparseable) roles") {
		t.Errorf("one database under two roles was not refused: %v\n%s", err, out)
	}
}
