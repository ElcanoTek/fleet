package admincli

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExternalSchemaPrivilegeCheck (#2055 review): external-mode bootstrap
// must refuse a DATABASE_URL whose role passes SELECT 1 but cannot create
// fleet's schemas, and the FLEET_DB_SUPERUSER_URL SQL must fix exactly that.
// Runs pgprovision.sh's functions against the test server.
func TestExternalSchemaPrivilegeCheck(t *testing.T) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set")
	}
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not installed")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", withDBName(t, base, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const db, role, pw = "fleet_extpriv_test", "fleet_extpriv_role", "placeholder-not-a-secret"
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		_, _ = admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+role)
	}
	cleanup()
	defer cleanup()
	if _, err := admin.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+pw+"'"); err != nil {
		t.Skipf("cannot create a role: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+db); err != nil { // owned by the test superuser
		t.Fatal(err)
	}
	u, _ := url.Parse(withDBName(t, base, db))
	u.User = url.UserPassword(role, pw)
	roleDSN := u.String()

	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "pgprovision.sh")
	can := func() bool {
		return exec.Command("bash", "-c", `. "$1"; fleet_db_can_create_schemas "$2"`, "_", lib, roleDSN).Run() == nil
	}
	if can() {
		t.Fatal("a role without CREATE on the database passed the schema check")
	}
	// bootstrap's own gate (the function every unprovisioned-DSN path calls,
	// --adopt-existing-db included) refuses it with an actionable message.
	gate := bootstrapFunc(t, "require_schema_privilege")
	out, err := exec.Command("bash", "-c", `die() { echo "$*"; exit 1; }; . "$1"; eval "$2"; require_schema_privilege "$3" "the adopted DATABASE_URL"`, "_", lib, gate, roleDSN).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "the adopted DATABASE_URL's role cannot create the chat and sched schemas") || strings.Contains(string(out), pw) {
		t.Fatalf("bootstrap's privilege gate = %v: %s", err, out)
	}

	// The superuser SQL bootstrap runs, built from the DSN itself.
	out, err = exec.Command("bash", "-c", `. "$1"; rd="$(fleet_dsn_role_db "$2")"; fleet_external_db_sql "${rd%% *}" "${rd#* }"`, "_", lib, roleDSN).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `GRANT CONNECT, CREATE ON DATABASE "`+db+`" TO "`+role+`"`) {
		t.Fatalf("superuser SQL:\n%s", out)
	}
	psql := exec.Command("psql", "-X", "-v", "ON_ERROR_STOP=1", "-q", withDBName(t, base, "postgres"))
	psql.Stdin = strings.NewReader(string(out))
	if o, err := psql.CombinedOutput(); err != nil {
		t.Fatalf("superuser SQL failed: %v\n%s", err, o)
	}
	if !can() {
		t.Fatal("the role still cannot create the schemas after the grant")
	}
	if out, err := exec.Command("bash", "-c", `die() { echo "$*"; exit 1; }; . "$1"; eval "$2"; require_schema_privilege "$3" x`, "_", lib, gate, roleDSN).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap's privilege gate refused a granted role: %v: %s", err, out)
	}

	// Pre-created schemas with CREATE + USAGE are the other accepted shape.
	if _, err := admin.ExecContext(ctx, "REVOKE CREATE ON DATABASE "+db+" FROM "+role); err != nil {
		t.Fatal(err)
	}
	tdb, err := sql.Open("pgx", withDBName(t, base, db))
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close()
	if _, err := tdb.ExecContext(ctx, "CREATE SCHEMA chat AUTHORIZATION "+role+"; CREATE SCHEMA sched AUTHORIZATION "+role); err != nil {
		t.Fatal(err)
	}
	if !can() {
		t.Fatal("pre-created schemas owned by the role were not accepted")
	}

	for dsn, ok := range map[string]bool{
		"postgres://app:x@h:5432/appdb?sslmode=require": true,
		"postgres://App:x@h/appdb":                      false,
		"postgres://app@h/app;db":                       false,
		"host=h dbname=appdb":                           false,
	} {
		err := exec.Command("bash", "-c", `. "$1"; fleet_dsn_role_db "$2"`, "_", lib, dsn).Run()
		if (err == nil) != ok {
			t.Errorf("fleet_dsn_role_db(%q) ok=%v, want %v", dsn, err == nil, ok)
		}
	}
}

// bootstrapFunc returns the source of one function defined in
// scripts/bootstrap.sh (from its `name() {` line to the first line that is
// just `}`), so a test can run the script's real code without running the
// whole script.
func bootstrapFunc(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	start := strings.Index(src, "\n"+name+"() {")
	if start < 0 {
		t.Fatalf("bootstrap.sh defines no %s()", name)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("cannot find the end of %s()", name)
	}
	return src[start : start+end+3]
}

// TestBootstrapAdoptAndExternalCheckSchemaPrivilege (#2055 review): both
// paths that accept a DSN bootstrap did not provision — --adopt-existing-db
// and external mode — run the privilege gate after SELECT 1 and before
// reporting success.
func TestBootstrapAdoptAndExternalCheckSchemaPrivilege(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, success := range []string{`ok "database adopted`, `ok "DATABASE_URL answered SELECT 1`} {
		end := strings.Index(src, success)
		if end < 0 {
			t.Fatalf("bootstrap.sh lost %q", success)
		}
		start := strings.LastIndex(src[:end], `-c "SELECT 1"`)
		if start < 0 || !strings.Contains(src[start:end], `require_schema_privilege "$DB_URL"`) {
			t.Errorf("the path ending in %q reports success without require_schema_privilege after its SELECT 1", success)
		}
	}
}
