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

	// The superuser SQL bootstrap runs, built from the DSN itself.
	out, err := exec.Command("bash", "-c", `. "$1"; rd="$(fleet_dsn_role_db "$2")"; fleet_external_db_sql "${rd%% *}" "${rd#* }"`, "_", lib, roleDSN).Output()
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
