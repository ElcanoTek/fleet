package admincli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapProvisionsOneDatabase: a fresh install provisions ONE role and
// ONE database and writes DATABASE_URL only (#2049); a re-run reuses the
// password recorded in DATABASE_URL and never prints it.
func TestBootstrapProvisionsOneDatabase(t *testing.T) {
	const livePassword = "placeholder-not-a-secret"
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("DATABASE_URL=postgres://fleet:"+livePassword+"@127.0.0.1:5432/fleet?sslmode=disable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local")
	if err != nil {
		t.Fatalf("bootstrap --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if strings.Contains(out, livePassword) {
		t.Errorf("the plan printed the live password\n--- output ---\n%s", out)
	}
	for _, want := range []string{
		"CREATE ROLE fleet LOGIN PASSWORD ''<db password>''",
		"ALTER ROLE fleet WITH LOGIN PASSWORD '<db password>'",
		"CREATE DATABASE fleet OWNER fleet",
		"would set DATABASE_URL=… in " + envFile,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q\n--- output ---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"CREATE DATABASE chat", "CREATE DATABASE sched", "FLEET_CHAT_DATABASE_URL=…", "FLEET_SCHED_DATABASE_URL=…", "legacy two-database layout"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a one-database plan contains %q\n--- output ---\n%s", unwanted, out)
		}
	}
}

// TestBootstrapOneDatabaseNameFlags: --db-name/--db-user override the
// defaults; the legacy per-store flags select the legacy layout.
func TestBootstrapOneDatabaseNameFlags(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local", "--db-name", "fleet_prod", "--db-user", "fleet_owner")
	if err != nil {
		t.Fatalf("exit: %v\n%s", err, out)
	}
	if !strings.Contains(out, "CREATE DATABASE fleet_prod OWNER fleet_owner") {
		t.Errorf("plan ignores --db-name/--db-user\n--- output ---\n%s", out)
	}
	out, err = runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local", "--db-name", "bad-name;drop")
	if err == nil || !strings.Contains(out, "must be a lower-case plain identifier") {
		t.Errorf("an unsafe --db-name was accepted\n--- output ---\n%s", out)
	}
}

// TestBootstrapOneDatabaseRejectsMixedCaseNames (#2055 review): the names go
// unquoted into SQL, where Postgres folds them to lower case, so a mixed-case
// --db-name/--db-user would provision one name and write another into the
// DSN.
func TestBootstrapOneDatabaseRejectsMixedCaseNames(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	for _, args := range [][]string{{"--db-name", "Fleet"}, {"--db-user", "FleetOwner"}} {
		out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", append([]string{"--dry-run", "--postgres=local"}, args...)...)
		if err == nil || !strings.Contains(out, "lower-case plain identifier") {
			t.Errorf("%v was accepted\n--- output ---\n%s", args, out)
		}
	}
}

// TestBootstrapExternalOneDatabaseRerun (#2055 review): an external-mode
// re-run reads DATABASE_URL from the env file when the shell does not set
// it, and FLEET_DB_SUPERUSER_URL grants the DSN's role CREATE on the DSN's
// own database.
func TestBootstrapExternalOneDatabaseRerun(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("DATABASE_URL=postgres://app:placeholder-not-a-secret@db.example.com:5432/appdb?sslmode=require\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile, "DATABASE_URL="}, "bootstrap.sh", "--dry-run", "--postgres=external")
	if err != nil {
		t.Fatalf("external re-run with DATABASE_URL only in the env file failed: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "would run: psql '<dsn>' -c 'SELECT 1'") || strings.Contains(out, "placeholder-not-a-secret") {
		t.Errorf("unexpected plan\n--- output ---\n%s", out)
	}
	out, err = runScript(t, []string{"FLEET_ENV_FILE=" + envFile, "DATABASE_URL=", "FLEET_DB_SUPERUSER_URL=postgres://admin@db.example.com/postgres"}, "bootstrap.sh", "--dry-run", "--postgres=external")
	if err != nil {
		t.Fatalf("exit: %v\n%s", err, out)
	}
	for _, want := range []string{`CREATE DATABASE "appdb"`, `GRANT CONNECT, CREATE ON DATABASE "appdb" TO "app";`} {
		if !strings.Contains(out, want) {
			t.Errorf("superuser plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// LEGACY-DB-LAYOUT: remove after consolidation — the test below.

// TestBootstrapKeepsALegacyInstallLegacy: re-running bootstrap on a box whose
// env file records the per-store DSNs converges the two databases as they are
// (an upgrade alone must never move or strand data) and points at
// `fleet db consolidate`; the one-database flags are refused there.
func TestBootstrapKeepsALegacyInstallLegacy(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	legacy := "FLEET_CHAT_DATABASE_URL=postgres://chat:pwA1@127.0.0.1:5432/chat?sslmode=disable\n" +
		"FLEET_SCHED_DATABASE_URL=postgres://sched:pwB2@127.0.0.1:5432/sched?sslmode=disable\n"
	if err := os.WriteFile(envFile, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local")
	if err != nil {
		t.Fatalf("exit: %v\n%s", err, out)
	}
	for _, want := range []string{"legacy two-database layout", "fleet db consolidate", "CREATE DATABASE chat OWNER chat", "would set FLEET_SCHED_DATABASE_URL=…"} {
		if !strings.Contains(out, want) {
			t.Errorf("legacy re-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "would set DATABASE_URL=…") {
		t.Errorf("a legacy re-run would write DATABASE_URL\n--- output ---\n%s", out)
	}
	out, err = runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local", "--db-name", "fleet")
	if err == nil || !strings.Contains(out, "fleet db consolidate") {
		t.Errorf("--db-name on a legacy install was not refused\n--- output ---\n%s", out)
	}
}
