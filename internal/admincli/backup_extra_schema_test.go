package admincli

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestBackupRestoreWithExtraSchemas (#2055 review): a legacy store's
// database may hold schemas of its own. Its backup is public only, and an
// archive holding the store's schema among others (a whole-database dump)
// restores the store's schema and nothing else.
func TestBackupRestoreWithExtraSchemas(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not found", b)
		}
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", testdb.MaintenanceDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d", cm, sm)
	}
	srcDSN, dstDSN := testdb.Empty(t), testdb.Empty(t)
	src, err := sql.Open("pgx", srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.ExecContext(ctx, `CREATE TABLE public.sentinel (note text); INSERT INTO public.sentinel VALUES ('legacy-ok');
		CREATE SCHEMA reporting; CREATE TABLE reporting.extra (x int)`); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	// fleet backup of a legacy store: public only.
	dump := filepath.Join(t.TempDir(), "legacy.dump")
	if err := runPgDump(ctx, srcDSN, dump); err != nil {
		t.Fatalf("dump: %v", err)
	}
	if got, err := dumpSchemas(ctx, dump); err != nil || !reflect.DeepEqual(got, []string{"public"}) {
		t.Fatalf("legacy backup holds %v (%v), want only public", got, err)
	}

	// A whole-database archive (made by hand, extra schema included) still
	// restores into the legacy store: public only.
	whole := filepath.Join(t.TempDir(), "whole.dump")
	c, err := parsePGConn(srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "pg_dump", "-Fc", "-f", whole)
	cmd.Env = c.env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}
	if got, _ := dumpSchemas(ctx, whole); !reflect.DeepEqual(got, []string{"public", "reporting"}) {
		t.Fatalf("whole-database archive holds %v, want [public reporting]", got)
	}
	if err := runPgRestore(ctx, dstDSN, whole); err != nil {
		t.Fatalf("restore a whole-database archive into the legacy store: %v", err)
	}
	dst, err := sql.Open("pgx", dstDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var note string
	if err := dst.QueryRowContext(ctx, `SELECT note FROM public.sentinel`).Scan(&note); err != nil || note != "legacy-ok" {
		t.Fatalf("restored sentinel = %q, %v", note, err)
	}
	var n int
	if err := dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'reporting'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the restore brought in the archive's other schema (%d, %v)", n, err)
	}
}
