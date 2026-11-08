package admincli

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

func TestTOCHasTable(t *testing.T) {
	t.Parallel()
	toc := `;
; Archive created at 2026-11-08
1; 2615 16390 SCHEMA - core fleet
212; 1259 16400 TABLE core schema_versions fleet
213; 1259 16401 TABLE core users fleet
3301; 0 16400 TABLE DATA core schema_versions fleet
`
	if !tocHasTable(toc, "core", "schema_versions") || tocHasTable(toc, "chat", "schema_versions") || tocHasTable(toc, "core", "identity_merge") {
		t.Fatal("tocHasTable misread the table of contents")
	}
}

// TestRestoreIsWholeDatabaseOnly (#2049 M6): `fleet restore --db=chat|sched`
// is refused with what to do instead.
func TestRestoreIsWholeDatabaseOnly(t *testing.T) {
	t.Parallel()
	for _, db := range []string{"chat", "sched"} {
		var code int
		out := captureStderr(t, func() { code = cmdRestore([]string{"--db=" + db, "--no-confirm", "x.dump"}) })
		if code != 1 || !strings.Contains(out, "single-store restores are not supported") || !strings.Contains(out, "--db=all") {
			t.Fatalf("restore --db=%s: exit %d, %q", db, code, out)
		}
	}
}

// TestRestoreKeepsTheLedgersTrue: a whole-database restore brings the dump's
// ledgers; a dump from before the one ledger (M4/M5) restores below the
// boundary — its legacy ledgers are authoritative and the next start
// reconciles — and is refused, changing nothing, past it; a dump from before
// the one identity table is refused; a dump without legacy ledgers leaves
// none stale behind.
func TestRestoreKeepsTheLedgersTrue(t *testing.T) {
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
	dir := t.TempDir()
	migrated := func() (string, *sql.DB) {
		t.Helper()
		dsn := testdb.Empty(t)
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := migrations.Up(ctx, db); err != nil {
			t.Fatal(err)
		}
		return dsn, db
	}
	dumpAll := func(dsn, name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := runPgDumpAll(ctx, config.WithSchema(dsn, config.ChatSchema), p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	restore := func(dsn, dump string) error {
		return runPgRestoreAll(ctx, config.WithSchema(dsn, config.ChatSchema), dump)
	}
	exec := func(db *sql.DB, q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// A dump taken since M6.
	src, _ := migrated()
	m6Dump := dumpAll(src, "m6.dump")
	dst, dstDB := migrated()
	if err := restore(dst, m6Dump); err != nil {
		t.Fatalf("restore an M6 dump: %v", err)
	}
	if v, ok, err := migrations.LedgerMax(ctx, dstDB); err != nil || !ok || v != migrations.LegacyLatest {
		t.Fatalf("ledger after the restore: %d, %v, %v", v, ok, err)
	}
	if err := migrations.Up(ctx, dstDB); err != nil {
		t.Fatalf("start after the restore: %v", err)
	}

	// A dump from M4/M5: legacy ledgers, no core.schema_versions.
	old, oldDB := migrated()
	exec(oldDB, `DROP TABLE core.schema_versions`)
	preM6 := dumpAll(old, "pre-m6.dump")
	if err := restore(dst, preM6); err != nil {
		t.Fatalf("restore a pre-M6 dump below the boundary: %v", err)
	}
	if err := migrations.Up(ctx, dstDB); err != nil {
		t.Fatalf("start after a pre-M6 restore (reconciles): %v", err)
	}

	// Past the boundary: refused, nothing changed.
	exec(dstDB, `INSERT INTO core.schema_versions (version, name, checksum) VALUES (200, '0200_from_a_newer_fleet', 'x')`)
	snap := func() string {
		t.Helper()
		var s string
		if err := dstDB.QueryRow(`SELECT (SELECT string_agg(version || ':' || checksum, ',' ORDER BY version) FROM core.schema_versions)
			|| '|' || (SELECT coalesce(string_agg(email || ':' || id, ',' ORDER BY email), '') FROM core.users)
			|| '|' || (SELECT count(*) FROM chat.schema_migrations)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snap()
	if err := restore(dst, preM6); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("pre-M6 restore past the boundary = %v, want a refusal", err)
	}
	if after := snap(); after != before {
		t.Fatalf("a refused restore changed the database:\nbefore %s\nafter  %s", before, after)
	}
	exec(dstDB, `DELETE FROM core.schema_versions WHERE version = 200`)

	// A dump without legacy ledgers: none stays behind stale; the next
	// start writes them again from the restored ledger.
	bare, bareDB := migrated()
	exec(bareDB, `DROP TABLE chat.schema_migrations; DROP TABLE sched.schema_migrations`)
	bareDump := dumpAll(bare, "bare.dump")
	if err := restore(dst, bareDump); err != nil {
		t.Fatal(err)
	}
	var legacy int
	if err := dstDB.QueryRow(`SELECT count(*) FROM pg_tables WHERE tablename = 'schema_migrations'`).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("legacy ledgers after restoring a dump without them: %d (%v)", legacy, err)
	}
	if err := migrations.Up(ctx, dstDB); err != nil {
		t.Fatal(err)
	}
	if err := dstDB.QueryRow(`SELECT count(*) FROM chat.schema_migrations`).Scan(&legacy); err != nil || legacy != 99 {
		t.Fatalf("legacy chat ledger rebuilt with %d rows (%v)", legacy, err)
	}

	// A dump from before the one identity table (no core.users).
	preM4 := testdb.Empty(t)
	preM4DB, err := sql.Open("pgx", preM4)
	if err != nil {
		t.Fatal(err)
	}
	defer preM4DB.Close()
	exec(preM4DB, `CREATE SCHEMA chat; CREATE SCHEMA core; CREATE SCHEMA sched;
		CREATE TABLE chat.users (email text); CREATE TABLE core.other (x int); CREATE TABLE sched.users (username text)`)
	if err := restore(dst, dumpAll(preM4, "pre-m4.dump")); err == nil || !strings.Contains(err.Error(), "before the one identity table") {
		t.Fatalf("pre-M4 restore = %v, want a refusal", err)
	}
}
