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
	for _, unwanted := range []string{"CREATE DATABASE chat", "CREATE DATABASE sched", "FLEET_CHAT_DATABASE_URL=…", "FLEET_SCHED_DATABASE_URL=…"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a one-database plan contains %q\n--- output ---\n%s", unwanted, out)
		}
	}
}

// TestBootstrapOneDatabaseNameFlags: --db-name/--db-user override the
// defaults; an unsafe name is refused.
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

// TestBootstrapRerunKeepsTheRecordedDatabase (#2055 review): a re-run takes
// the role and database from the DATABASE_URL a previous run recorded —
// never the defaults — and refuses flags or a DSN it cannot honour.
func TestBootstrapRerunKeepsTheRecordedDatabase(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	write := func(dsn string) {
		t.Helper()
		if err := os.WriteFile(envFile, []byte("DATABASE_URL="+dsn+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, error) {
		return runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", append([]string{"--dry-run", "--postgres=local"}, args...)...)
	}

	write("postgres://myapp:placeholder-not-a-secret@127.0.0.1:5432/mydb?sslmode=disable")
	for _, args := range [][]string{nil, {"--db-name", "mydb", "--db-user", "myapp"}} {
		out, err := run(args...)
		if err != nil || !strings.Contains(out, "CREATE DATABASE mydb OWNER myapp") || strings.Contains(out, "CREATE DATABASE fleet") {
			t.Errorf("re-run %v did not keep mydb/myapp: %v\n%s", args, err, out)
		}
		if strings.Contains(out, "placeholder-not-a-secret") {
			t.Errorf("re-run printed the password\n%s", out)
		}
	}
	for flag, want := range map[string]string{"--db-name": "conflicts with database 'mydb'", "--db-user": "conflicts with role 'myapp'"} {
		if out, err := run(flag, "other"); err == nil || !strings.Contains(out, want) {
			t.Errorf("%s other on a box recorded as mydb/myapp: %v\n%s", flag, err, out)
		}
	}

	write("postgres://myapp:x@127.0.0.1:5433/mydb")
	if out, err := run(); err == nil || !strings.Contains(out, "does not name the local cluster on port 5432") {
		t.Errorf("a recorded non-default port was not refused: %v\n%s", err, out)
	}
	write("postgres://MyApp:x@127.0.0.1/mydb")
	if out, err := run(); err == nil || !strings.Contains(out, "cannot tell the role and database") {
		t.Errorf("an unparseable recorded DSN was not refused: %v\n%s", err, out)
	}
	// --adopt-existing-db keeps any DSN as it is.
	write("postgres://myapp:x@127.0.0.1:5433/mydb")
	if out, err := run("--adopt-existing-db"); err != nil {
		t.Errorf("--adopt-existing-db refused a recorded DSN: %v\n%s", err, out)
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

// TestBootstrapRefusesRetiredDSNKeys: an env file that still records the
// per-store DSNs of the removed two-database layout is refused, naming
// DATABASE_URL (#2049 phase 2) — fleet would refuse to boot with it — and the
// per-store flags are gone.
func TestBootstrapRefusesRetiredDSNKeys(t *testing.T) {
	for _, key := range []string{"FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL"} {
		envFile := filepath.Join(t.TempDir(), "fleet.env")
		if err := os.WriteFile(envFile, []byte(key+"=postgres://chat:pwA1@127.0.0.1:5432/chat?sslmode=disable\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local")
		if err == nil || !strings.Contains(out, key+" is no longer supported") || !strings.Contains(out, "set DATABASE_URL") {
			t.Errorf("%s in the env file was not refused\n--- output ---\n%s", key, out)
		}
		if strings.Contains(out, "pwA1") {
			t.Errorf("the refusal printed the DSN's password\n--- output ---\n%s", out)
		}
	}
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local", "--chat-db-name", "chat")
	if err == nil {
		t.Errorf("the removed --chat-db-name flag was accepted\n--- output ---\n%s", out)
	}
}
