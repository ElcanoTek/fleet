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

// TestRestoreKeepsTheLedgerTrue (#2049 M6): a one-store restore of a dump
// whose ledger is at another version than the live one is refused (--db=all
// is not); a dump from before the one ledger drops the live ledger after the
// restore, writing the untouched store's legacy ledger first, so the next
// start adopts again; and such a dump is refused over a database that has
// run a migration from 200 on.
func TestRestoreKeepsTheLedgerTrue(t *testing.T) {
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
	open := func(dsn string) *sql.DB {
		t.Helper()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	migrated := func(through int) (string, *sql.DB) {
		t.Helper()
		dsn := testdb.Empty(t)
		db := open(dsn)
		if err := migrations.UpTo(ctx, db, through); err != nil {
			t.Fatal(err)
		}
		return dsn, db
	}
	dir := t.TempDir()

	src, _ := migrated(migrations.LegacyLatest)
	chatDump, schedDump, allDump := filepath.Join(dir, "chat.dump"), filepath.Join(dir, "sched.dump"), filepath.Join(dir, "all.dump")
	if err := runPgDump(ctx, config.WithSchema(src, config.ChatSchema), chatDump); err != nil {
		t.Fatal(err)
	}
	if err := runPgDump(ctx, config.WithSchema(src, config.SchedSchema), schedDump); err != nil {
		t.Fatal(err)
	}
	if err := runPgDumpAll(ctx, config.WithSchema(src, config.ChatSchema), allDump); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := dumpLedger(ctx, chatDump); err != nil || !ok || v != migrations.LegacyLatest {
		t.Fatalf("dumpLedger(chat dump) = %d, %v, %v", v, ok, err)
	}

	restoreAcrossVersions(t, migrated, chatDump, schedDump, allDump)
	restorePreM6Dump(t, open, migrated, dir)
}

// restoreAcrossVersions: a one-store restore at the live version goes ahead;
// at another it is refused, and --db=all is not.
func restoreAcrossVersions(t *testing.T, migrated func(int) (string, *sql.DB), chatDump, schedDump, allDump string) {
	t.Helper()
	ctx := context.Background()
	// Same version: a one-store restore goes ahead.
	same, sameDB := migrated(migrations.LegacyLatest)
	if err := runPgRestoreWith(ctx, config.WithSchema(same, config.ChatSchema), chatDump, restoreOpts{}); err != nil {
		t.Fatalf("restore chat at the same version: %v", err)
	}
	if v, ok, err := migrations.LedgerMax(ctx, sameDB); err != nil || !ok || v != migrations.LegacyLatest {
		t.Fatalf("ledger after the restore: %d, %v, %v", v, ok, err)
	}

	// Another version: one store is refused, the whole fleet is not. (The
	// live ledger is made to read 176 by hand: a restore over a schema that
	// differs is pg_restore --clean's own problem, not this check's.)
	behind, behindDB := migrated(migrations.LegacyLatest)
	if _, err := behindDB.ExecContext(ctx, `DELETE FROM core.schema_versions WHERE version = 177`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ dsn, dump string }{
		{config.WithSchema(behind, config.ChatSchema), chatDump},
		{config.WithSchema(behind, config.SchedSchema), schedDump},
	} {
		err := runPgRestoreWith(ctx, c.dsn, c.dump, restoreOpts{allowUnresolved: true})
		if err == nil || !strings.Contains(err.Error(), "--db=all") || !strings.Contains(err.Error(), "is at migration version 176") {
			t.Fatalf("one-store restore across versions = %v, want a refusal naming --db=all", err)
		}
	}
	if err := runPgRestoreAll(ctx, config.WithSchema(behind, config.ChatSchema), allDump); err != nil {
		t.Fatalf("restore --db=all across versions: %v", err)
	}
	if v, _, err := migrations.LedgerMax(ctx, behindDB); err != nil || v != migrations.LegacyLatest {
		t.Fatalf("after --db=all the ledger is the dump's: %d, %v", v, err)
	}
}

// restorePreM6Dump: a dump from before the one ledger drops the live ledger
// after the restore (writing the untouched store's legacy ledger first), the
// next start adopts again, and such a dump is refused over a database that
// ran a migration from 200 on.
func restorePreM6Dump(t *testing.T, open func(string) *sql.DB, migrated func(int) (string, *sql.DB), dir string) {
	t.Helper()
	ctx := context.Background()
	// A dump from before the one ledger: what a pre-M6 release dumped (its
	// schemas, its two legacy ledgers, no core.schema_versions).
	old := testdb.Empty(t)
	oldDB := open(old)
	if err := migrations.UpTo(ctx, oldDB, migrations.LegacyLatest); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE core.schema_versions`,
		`CREATE TABLE chat.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`,
		`INSERT INTO chat.schema_migrations SELECT g, 'm' || g, 1700000000 FROM generate_series(1, 99) g`,
		`CREATE TABLE sched.schema_migrations (version bigint not null primary key, dirty boolean not null)`,
		`INSERT INTO sched.schema_migrations VALUES (77, false)`,
	} {
		if _, err := oldDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	oldChat := filepath.Join(dir, "old-chat.dump")
	if err := runPgDump(ctx, config.WithSchema(old, config.ChatSchema), oldChat); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := dumpLedger(ctx, oldChat); err != nil || ok {
		t.Fatalf("a pre-M6 dump read as having a ledger (%v, %v)", ok, err)
	}
	live, liveDB := migrated(migrations.LegacyLatest)
	if err := runPgRestoreWith(ctx, config.WithSchema(live, config.ChatSchema), oldChat, restoreOpts{allowUnresolved: true}); err != nil {
		t.Fatalf("restore a pre-M6 chat dump: %v", err)
	}
	if _, ok, err := migrations.LedgerMax(ctx, liveDB); err != nil || ok {
		t.Fatalf("the live ledger survived a pre-M6 restore (%v, %v)", ok, err)
	}
	var chatRows, schedVersion int
	if err := liveDB.QueryRow(`SELECT (SELECT count(*) FROM chat.schema_migrations), (SELECT version FROM sched.schema_migrations)`).Scan(&chatRows, &schedVersion); err != nil || chatRows != 99 || schedVersion != 77 {
		t.Fatalf("legacy ledgers after the restore: chat %d rows, sched %d (%v)", chatRows, schedVersion, err)
	}
	// The next start adopts again.
	if err := migrations.Up(ctx, liveDB); err != nil {
		t.Fatalf("Up after a pre-M6 restore: %v", err)
	}
	if v, ok, err := migrations.LedgerMax(ctx, liveDB); err != nil || !ok || v != migrations.LegacyLatest {
		t.Fatalf("re-adopted ledger: %d, %v, %v", v, ok, err)
	}

	// Over a database that ran a migration from 200 on: refused, untouched.
	if _, err := liveDB.ExecContext(ctx, `INSERT INTO core.schema_versions (version, name, checksum) VALUES (200, '0200_future', '')`); err != nil {
		t.Fatal(err)
	}
	err := runPgRestoreWith(ctx, config.WithSchema(live, config.ChatSchema), oldChat, restoreOpts{allowUnresolved: true})
	if err == nil || !strings.Contains(err.Error(), "new, empty database") {
		t.Fatalf("pre-M6 restore over version 200 = %v", err)
	}
	if v, _, _ := migrations.LedgerMax(ctx, liveDB); v != 200 {
		t.Fatalf("the refused restore changed the ledger (max %d)", v)
	}
}
