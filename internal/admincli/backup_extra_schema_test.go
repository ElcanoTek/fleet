package admincli

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestBackupRestoreWithExtraSchemas (#2055 review): the database may hold
// schemas besides the store's. A store's backup is its schema only, an
// archive holding the store's schema among others (a whole-database dump)
// restores the store's schema and nothing else, and a dump of the removed
// two-database layout (its store in public) is refused (#2049 phase 2).
func TestBackupRestoreWithExtraSchemas(t *testing.T) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set")
	}
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not found", b)
		}
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", withDBName(t, base, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d", cm, sm)
	}
	const srcDB, dstDB = "fleet_backup_extra_src_test", "fleet_backup_extra_dst_test"
	for _, db := range []string{srcDB, dstDB} {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+db); err != nil {
			if isPermissionError(err) {
				t.Skipf("cannot CREATE DATABASE: %v", err)
			}
			t.Fatal(err)
		}
	}
	defer func() {
		for _, db := range []string{srcDB, dstDB} {
			_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		}
	}()
	srcDSN := config.WithSchema(withDBName(t, base, srcDB), config.ChatSchema)
	dstDSN := config.WithSchema(withDBName(t, base, dstDB), config.ChatSchema)
	src, err := sql.Open("pgx", srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.ExecContext(ctx, `CREATE SCHEMA chat; CREATE TABLE chat.sentinel (note text); INSERT INTO chat.sentinel VALUES ('store-ok');
		CREATE SCHEMA reporting; CREATE TABLE reporting.extra (x int)`); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	// fleet backup of a store: its schema only.
	dump := filepath.Join(t.TempDir(), "store.dump")
	if err := runPgDump(ctx, srcDSN, dump); err != nil {
		t.Fatalf("dump: %v", err)
	}
	if got, err := dumpSchemas(ctx, dump); err != nil || !reflect.DeepEqual(got, []string{"chat"}) {
		t.Fatalf("store backup holds %v (%v), want only chat", got, err)
	}

	// A whole-database archive (made by hand, extra schema included) still
	// restores into the store: its schema only.
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
	if got, _ := dumpSchemas(ctx, whole); !reflect.DeepEqual(got, []string{"chat", "public", "reporting"}) && !reflect.DeepEqual(got, []string{"chat", "reporting"}) {
		t.Fatalf("whole-database archive holds %v, want chat and reporting", got)
	}
	if err := runPgRestore(ctx, dstDSN, whole); err != nil {
		t.Fatalf("restore a whole-database archive into the store: %v", err)
	}
	dst, err := sql.Open("pgx", dstDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var note string
	if err := dst.QueryRowContext(ctx, `SELECT note FROM chat.sentinel`).Scan(&note); err != nil || note != "store-ok" {
		t.Fatalf("restored sentinel = %q, %v", note, err)
	}
	var n int
	if err := dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'reporting'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the restore brought in the archive's other schema (%d, %v)", n, err)
	}

	// A dump of the removed two-database layout (the store in public) is
	// refused before anything is dropped.
	legacySrc, err := sql.Open("pgx", withDBName(t, base, srcDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacySrc.ExecContext(ctx, `DROP SCHEMA chat CASCADE; DROP SCHEMA reporting CASCADE; CREATE TABLE public.old (x int)`); err != nil {
		t.Fatal(err)
	}
	_ = legacySrc.Close()
	legacyDump := filepath.Join(t.TempDir(), "legacy.dump")
	if err := runPgDump(ctx, config.WithSchema(withDBName(t, base, srcDB), config.PublicSchema), legacyDump); err != nil {
		t.Fatalf("dump public: %v", err)
	}
	if err := runPgRestore(ctx, dstDSN, legacyDump); err == nil || !strings.Contains(err.Error(), "removed two-database layout") {
		t.Fatalf("restore of a public-schema dump = %v, want the two-database refusal", err)
	}
	if err := dst.QueryRowContext(ctx, `SELECT note FROM chat.sentinel`).Scan(&note); err != nil || note != "store-ok" {
		t.Fatalf("the refused restore touched the store: %q, %v", note, err)
	}
}
