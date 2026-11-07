package admincli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// identityParityDSNs are the DSN shapes the shell fallback and
// config.DSNIdentity (pgconn.ParseConfig) must agree on.
var identityParityDSNs = []string{
	"postgres://u@h/db", "postgres://v:pw@H:5432/db?sslmode=require", "postgresql://u@h:5433/db",
	"postgres://u@localhost/db", "postgres://u@127.0.0.1:5432/db", "postgres://u@[::1]/db", "postgres://u@[2001:db8::1]:6000/db",
	"postgres://u@/db", "postgres://u@/db?host=%2Fvar%2Frun%2Fpostgresql&port=5444", "postgres://u@/db?host=/tmp",
	"postgres://fleet@h", "postgres://fleet@h/", "postgres://h/db+x", "postgres://u@h/d%20b", "postgres://u@h?dbname=q",
	"host=h dbname=db", "host=h port=5432 dbname=db user=x", "host=/tmp dbname=db", "hostaddr=127.0.0.1 dbname=db",
	"host=h user=fleet", "host=h", "postgres://", "not a dsn",
	// #2055 review round 6: a query parameter overrides the URL's own part,
	// as pgx (and libpq) read it — the last occurrence winning.
	"postgres://u@a.example:5432/db?host=b.example",
	"postgres://u@a.example:5432/db?port=6000",
	"postgres://u@a.example/db?dbname=other",
	"postgres://u@a.example/db?database=other",
	"postgres://u@a.example?user=svc",
	"postgres://u:pw@a.example:5432/db?host=b.example&port=6000&dbname=x&user=y",
	"postgres://u@a.example/db?host=b.example&host=c.example",
	"postgres://u@127.0.0.1:5432/db?host=%2Frun%2Fpostgresql",
	"postgres://u@db.example/db?host=localhost",
	"postgres://u@h/db?port=05432",
	"postgres://u@h/db?dbname=a+b",
	"postgres://u@h/db?user=a%2Bb",
	"host=a.example dbname=db host=b.example",
	"host=h dbname=a database=b",
	// What pgx refuses (and so has no identity).
	"postgres://a,b/db", "host=a,b dbname=db", "postgres://u@h/db?port=notaport", "postgres://u@h/db?port=70000",
	"postgres://u@h/db?flag", "postgres://u@h/db?a=b=c", "postgres://u@h/d%ZZ", "postgres://u@h/db?service=x",
	// The userinfo ends at the first '@' before any '/'.
	"postgres://u@h@x/db",
}

func shellIdentity(t *testing.T, lib, dsn string, env ...string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; fleet_dsn_identity "$2"`, "_", lib, dsn)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fleet_dsn_identity %q: %v\n%s", dsn, err, out)
	}
	return string(out)
}

// TestIdleLibIdentityParity pins the shell fallback, fleet_dsn_identity_sh,
// to config.DSNIdentity — pgconn.ParseConfig's reading — across DSN shapes,
// query overrides and the PG* environment (#2055 review rounds 3 and 6).
func TestIdleLibIdentityParity(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, k := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(k, "")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "idle.sh")
	check := func(dsn string) {
		t.Helper()
		if got, want := shellIdentity(t, lib, dsn, "FLEET_IDENTITY_BIN="), config.DSNIdentity(dsn); got != want {
			t.Errorf("fleet_dsn_identity_sh(%q) = %q, want config.DSNIdentity's %q", dsn, got, want)
		}
	}
	for _, dsn := range identityParityDSNs {
		check(dsn)
	}
	// What a DSN leaves out comes from the PG* environment, for both.
	t.Setenv("PGHOST", "env.example")
	t.Setenv("PGPORT", "6001")
	t.Setenv("PGDATABASE", "envdb")
	t.Setenv("PGUSER", "envuser")
	for _, dsn := range []string{"postgres://", "postgres://u@/", "postgres://u@h/db", "host=h", "dbname=x", "postgres://h?host=", "user=z"} {
		check(dsn)
	}
}

// TestIdleLibIdentityAsksTheBinary (#2055 review round 6): with a fleet
// binary at hand, fleet_dsn_identity takes the Go answer from
// `fleet db dsn-identity` (the DSN on stdin), and falls back to the shell
// mirror only when the binary cannot answer.
func TestIdleLibIdentityAsksTheBinary(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "idle.sh")
	dir := t.TempDir()
	answering := filepath.Join(dir, "fleet-new")
	// Records argv and stdin, so the test can see the DSN never rode argv.
	script := "#!/bin/sh\necho \"$*\" > \"$0.argv\"\ncat > \"$0.stdin\"\necho from-the-binary\n"
	if err := os.WriteFile(answering, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "fleet-old")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho 'unknown db subcommand' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const dsn = "postgres://u:placeholder-not-a-secret@a.example/db?host=b.example"
	if got := shellIdentity(t, lib, dsn, "FLEET_IDENTITY_BIN="+answering); got != "from-the-binary" {
		t.Errorf("with a binary: %q", got)
	}
	argv, _ := os.ReadFile(answering + ".argv")
	stdin, _ := os.ReadFile(answering + ".stdin")
	if strings.TrimSpace(string(argv)) != "db dsn-identity" || string(stdin) != dsn {
		t.Errorf("the binary got argv %q, stdin %q", argv, stdin)
	}
	if got, want := shellIdentity(t, lib, dsn, "FLEET_IDENTITY_BIN="+old), config.DSNIdentity(dsn); got != want || want != "b.example:5432/db" {
		t.Errorf("an old binary: %q, want the shell fallback's %q", got, want)
	}
	// Auto-discovery: FLEET_INSTALL_DIR/fleet.
	if err := os.Rename(answering, filepath.Join(dir, "fleet")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `unset FLEET_IDENTITY_BIN; . "$1"; fleet_dsn_identity "$2"`, "_", lib, dsn)
	cmd.Env = append(os.Environ(), "FLEET_INSTALL_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "from-the-binary" {
		t.Errorf("auto-discovery: %v %q", err, out)
	}
}

// TestDBDSNIdentityVerb: the hidden verb prints config.DSNIdentity of the DSN
// on stdin, and refuses a DSN given as an argument.
func TestDBDSNIdentityVerb(t *testing.T) {
	t.Parallel()

	for _, dsn := range identityParityDSNs {
		var out bytes.Buffer
		if code := cmdDBDSNIdentity(nil, strings.NewReader(dsn), &out); code != 0 {
			t.Fatalf("exit %d for %q", code, dsn)
		}
		if got, want := strings.TrimSuffix(out.String(), "\n"), config.DSNIdentity(dsn); got != want {
			t.Errorf("dsn-identity %q = %q, want %q", dsn, got, want)
		}
	}
	if code := cmdDBDSNIdentity([]string{"postgres://u@h/db"}, strings.NewReader(""), &bytes.Buffer{}); code == 0 {
		t.Error("a DSN on argv was accepted")
	}
}
