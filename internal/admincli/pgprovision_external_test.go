package admincli

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/dbschema"
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
	// A hardened database: no USAGE on public for anyone. The readiness
	// check must still work (it reads the catalogs; to_regclass on
	// 'public.…' would fail for the role).
	if h, err := sql.Open("pgx", withDBName(t, base, db)); err != nil {
		t.Fatal(err)
	} else {
		_, err = h.ExecContext(ctx, "REVOKE ALL ON SCHEMA public FROM PUBLIC")
		_ = h.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(withDBName(t, base, db))
	u.User = url.UserPassword(role, pw)
	roleDSN := u.String()

	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "pgprovision.sh")
	roleDB, err := sql.Open("pgx", roleDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer roleDB.Close()
	// readiness runs the shell gate and, at every step, the Go check that
	// dbschema.Ensure and `fleet validate-config` run (#2055 review): the
	// two must reach the same verdict and name the same findings.
	readiness := func() int {
		t.Helper()
		out, err := exec.Command("bash", "-c", `. "$1"; fleet_db_store_readiness "$2"`, "_", lib, roleDSN).Output()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		assertReadinessParity(t, roleDB, code, string(out))
		return code
	}
	can := func() bool { return readiness() == 0 }
	if got := readiness(); got != 1 {
		t.Fatalf("a role without CREATE on the database: readiness %d, want 1", got)
	}
	// bootstrap's own gate (the function every unprovisioned-DSN path calls,
	// --adopt-existing-db included) refuses it with an actionable message.
	gate := bootstrapFunc(t, "require_schema_privilege")
	runGate := func(what string) (string, error) {
		out, err := exec.Command("bash", "-c", `die() { echo "$*"; exit 1; }; . "$1"; eval "$2"; require_schema_privilege "$3" "$4"`, "_", lib, gate, roleDSN, what).CombinedOutput()
		return string(out), err
	}
	out0, err := runGate("the adopted DATABASE_URL")
	if err == nil || !strings.Contains(out0, "the adopted DATABASE_URL's role cannot run fleet's stores there") || strings.Contains(out0, pw) {
		t.Fatalf("bootstrap's privilege gate = %v: %s", err, out0)
	}
	var out []byte

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
	if out, err := runGate("x"); err != nil {
		t.Fatalf("bootstrap's privilege gate refused a granted role: %v: %s", err, out)
	}

	tdb, err := sql.Open("pgx", withDBName(t, base, db))
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close()
	superuser := mustQueryString(t, tdb, "SELECT current_user")
	// #2055 review: an existing store schema must be OWNED by the role — not
	// merely usable — even with CREATE on the database; the refusal names
	// the owner it found.
	mustExec(t, tdb, "CREATE SCHEMA chat; GRANT CREATE, USAGE ON SCHEMA chat TO "+role) // owned by the test superuser
	if got := readiness(); got != 4 {
		t.Fatalf("an existing chat schema owned by another role: readiness %d, want 4", got)
	}
	if out, err := runGate("DATABASE_URL"); err == nil || !strings.Contains(out, "schema chat is owned by "+superuser) || !strings.Contains(out, "must OWN") {
		t.Fatalf("the gate did not name the owner it found: %v: %s", err, out)
	}
	mustExec(t, tdb, "DROP SCHEMA chat")

	// Pre-created schemas OWNED by the role are the other accepted shape —
	// without CREATE on the database.
	mustExec(t, admin, "REVOKE CREATE ON DATABASE "+db+" FROM "+role)
	mustExec(t, tdb, "CREATE SCHEMA chat AUTHORIZATION "+role+"; CREATE SCHEMA sched AUTHORIZATION "+role)
	if !can() {
		t.Fatal("pre-created schemas owned by the role were not accepted")
	}
	// …but every table and sequence in them must be the role's too.
	mustExec(t, tdb, "CREATE TABLE sched.stray (id serial)") // owned by the test superuser
	if got := readiness(); got != 4 {
		t.Fatalf("a table in sched owned by another role: readiness %d, want 4", got)
	}
	if out, err := runGate("DATABASE_URL"); err == nil || !strings.Contains(out, "sched.stray is owned by "+superuser) || !strings.Contains(out, "sched.stray_id_seq is owned by "+superuser) {
		t.Fatalf("the gate did not name the table and sequence it found: %v: %s", err, out)
	}
	mustExec(t, tdb, "DROP TABLE sched.stray")
	checkManyFindings(t, tdb, readiness)
	if !can() {
		t.Fatal("the role's own schemas were refused once the stray table was gone")
	}

	// #2055 review: a database whose public holds a fleet store is refused,
	// pointing at consolidation, whatever the privileges.
	if _, err := tdb.ExecContext(ctx, "CREATE TABLE public.schema_migrations (version int)"); err != nil {
		t.Fatal(err)
	}
	if got := readiness(); got != 2 {
		t.Fatalf("a legacy store in public: readiness %d, want 2", got)
	}
	if out, err := runGate("DATABASE_URL"); err == nil || !strings.Contains(out, "fleet db consolidate") {
		t.Fatalf("the gate did not refuse a legacy store in public: %v: %s", err, out)
	}
}

// checkManyFindings: more than five findings, with names whose order differs
// between a linguistic and a byte-wise collation — the shell gate and the Go
// check show the same five.
func checkManyFindings(t *testing.T, tdb *sql.DB, readiness func() int) {
	t.Helper()
	mustExec(t, tdb, `CREATE TABLE sched.ab (i int); CREATE TABLE sched.a_b (i int); CREATE TABLE sched."A_c" (i int);
		CREATE TABLE sched.a1 (i int); CREATE TABLE sched.b (i int); CREATE TABLE sched.aa (i int)`)
	if got := readiness(); got != 4 {
		t.Fatalf("six tables in sched owned by another role: readiness %d, want 4", got)
	}
	mustExec(t, tdb, `DROP TABLE sched.ab, sched.a_b, sched."A_c", sched.a1, sched.b, sched.aa`)
}

// assertReadinessParity checks dbschema.CheckReadiness's verdict against the
// shell gate's exit code and printed findings.
func assertReadinessParity(t *testing.T, db *sql.DB, shellCode int, shellOut string) {
	t.Helper()
	r, err := dbschema.CheckReadiness(t.Context(), db, []string{"chat", "sched"})
	if err != nil {
		t.Fatalf("CheckReadiness: %v", err)
	}
	goCode := 0
	switch {
	case r.LegacyPublicStore:
		goCode = 2
	case len(r.NotOwned) > 0:
		goCode = 4
	case len(r.MissingCreate) > 0:
		goCode = 1
	}
	if goCode != shellCode {
		t.Fatalf("readiness parity: shell %d, Go %d (%+v)", shellCode, goCode, r)
	}
	if goCode == 4 {
		shown := r.NotOwned
		if len(shown) > 5 {
			shown = shown[:5]
		}
		if got, want := strings.TrimSpace(shellOut), strings.Join(shown, "; "); got != want {
			t.Fatalf("readiness parity: shell found %q, Go %q", got, want)
		}
		if !strings.Contains(r.Problem(), strings.Join(shown, "; ")) {
			t.Fatalf("Go's Problem() does not name its findings: %s", r.Problem())
		}
	}
}

// TestDSNRoleDB: fleet_dsn_role_db takes the role and database only from a
// URL DSN whose names are lower-case plain identifiers.
func TestDSNRoleDB(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "pgprovision.sh")
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
