package admincli

import (
	"database/sql"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestBackupFollowsQueryOverrides (#2055 review round 6): pg_dump and
// pg_restore go where the store's pgx connection goes. The DSN's authority
// names a host and port nothing answers on, while its query overrides them
// (and the database) to the real server: pgx connects there, and so must the
// backup and the restore, or they would fail (or, worse, reach another
// cluster).
func TestBackupFollowsQueryOverrides(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	ctx := t.Context()
	base := testdb.MaintenanceDSN(t)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d", cm, sm)
	}
	srcDSN, dstDSN := testdb.Empty(t), testdb.Empty(t)
	dbName := func(dsn string) string {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimPrefix(u.Path, "/")
	}
	src, dst := dbName(srcDSN), dbName(dstDSN)
	srcDB, err := sql.Open("pgx", srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, srcDB, "CREATE TABLE sentinel (note text); INSERT INTO sentinel VALUES ('override-ok')")
	_ = srcDB.Close()

	// The authority says nowhere.invalid:1/decoy; the query says the server.
	override := func(db string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("host", u.Hostname())
		if p := u.Port(); p != "" {
			q.Set("port", p)
		}
		q.Set("dbname", db)
		q.Del("options")
		u.Host = "nowhere.invalid:1"
		u.Path = "/decoy"
		u.RawQuery = q.Encode()
		return u.String()
	}
	dump := filepath.Join(t.TempDir(), "override.dump")
	if err := runPgDump(ctx, override(src)+"&options=-csearch_path%3Dpublic", dump); err != nil {
		t.Fatalf("pg_dump did not follow the query override: %v", err)
	}
	if err := runPgRestore(ctx, override(dst)+"&options=-csearch_path%3Dpublic", dump); err != nil {
		t.Fatalf("pg_restore did not follow the query override: %v", err)
	}
	dstDB, err := sql.Open("pgx", dstDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer dstDB.Close()
	if got := mustQueryString(t, dstDB, "SELECT note FROM sentinel"); got != "override-ok" {
		t.Fatalf("restored %q", got)
	}
}
