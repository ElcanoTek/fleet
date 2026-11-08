package admincli

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestBackupAllIsOneSnapshot: --db=all is ONE dump of chat, core and sched
// (one pg_dump, one snapshot), restored with --db=all in one transaction; a
// per-store dump carries core too; --db=all refuses a dump that is not a
// whole-fleet one; and a single store still restores out of an all dump
// (Codex 4504695164 on #2083).
func TestBackupAllIsOneSnapshot(t *testing.T) {
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
	srcDB, dstDB := testdb.Empty(t), testdb.Empty(t)
	exec := func(dsn, q string) {
		t.Helper()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(srcDB, `CREATE SCHEMA chat; CREATE SCHEMA core; CREATE SCHEMA sched; CREATE SCHEMA other;
		CREATE TABLE core.users (email text PRIMARY KEY); INSERT INTO core.users VALUES ('ops@x.com');
		CREATE TABLE chat.sentinel (note text); INSERT INTO chat.sentinel VALUES ('chat-ok');
		CREATE TABLE sched.tasks (created_by text); INSERT INTO sched.tasks VALUES ('ops@x.com');
		CREATE TABLE other.keep (note text)`)

	allDump := filepath.Join(t.TempDir(), "fleet-all.dump")
	dsn, err := dsnFor("all", config.WithSchema(srcDB, config.ChatSchema), config.WithSchema(srcDB, config.SchedSchema))
	if err != nil {
		t.Fatalf("dsnFor all: %v", err)
	}
	if err := runPgDumpAll(ctx, dsn, allDump); err != nil {
		t.Fatalf("dump all: %v", err)
	}
	if got, err := dumpSchemas(ctx, allDump); err != nil || !reflect.DeepEqual(got, []string{"chat", "core", "sched"}) {
		t.Fatalf("all dump holds %v (%v), want [chat core sched]", got, err)
	}
	schedDump := filepath.Join(t.TempDir(), "fleet-sched.dump")
	if err := runPgDump(ctx, config.WithSchema(srcDB, config.SchedSchema), schedDump); err != nil {
		t.Fatalf("dump sched: %v", err)
	}
	if got, err := dumpSchemas(ctx, schedDump); err != nil || !reflect.DeepEqual(got, []string{"core", "sched"}) {
		t.Fatalf("sched dump holds %v (%v), want [core sched] (self-contained)", got, err)
	}

	// --db=all refuses a per-store dump before touching anything.
	exec(dstDB, `CREATE SCHEMA sched; CREATE TABLE sched.tasks (created_by text); INSERT INTO sched.tasks VALUES ('live')`)
	dstChat := config.WithSchema(dstDB, config.ChatSchema)
	if err := runPgRestoreAll(ctx, dstChat, schedDump); err == nil || !strings.Contains(err.Error(), "not a whole-fleet dump") {
		t.Fatalf("restore --db=all of a sched dump = %v, want a refusal", err)
	}
	read := func(q string) string {
		t.Helper()
		db, err := sql.Open("pgx", dstDB)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var s string
		if err := db.QueryRowContext(ctx, q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	if got := read(`SELECT string_agg(created_by, ',') FROM sched.tasks`); got != "live" {
		t.Fatalf("a refused restore changed sched: %q", got)
	}

	// One store out of the all dump: chat brings core, leaves sched alone.
	if err := runPgRestore(ctx, dstChat, allDump); err != nil {
		t.Fatalf("restore --db=chat from the all dump: %v", err)
	}
	if got := read(`SELECT note FROM chat.sentinel`) + "|" + read(`SELECT email FROM core.users`) + "|" + read(`SELECT created_by FROM sched.tasks`); got != "chat-ok|ops@x.com|live" {
		t.Fatalf("after --db=chat from the all dump: %q", got)
	}

	// The whole fleet, in one go.
	if err := runPgRestoreAll(ctx, dstChat, allDump); err != nil {
		t.Fatalf("restore --db=all: %v", err)
	}
	if got := read(`SELECT note FROM chat.sentinel`) + "|" + read(`SELECT email FROM core.users`) + "|" + read(`SELECT created_by FROM sched.tasks`); got != "chat-ok|ops@x.com|ops@x.com" {
		t.Fatalf("after --db=all: %q", got)
	}
	if got := read(`SELECT count(*)::text FROM pg_namespace WHERE nspname = 'other'`); got != "0" {
		t.Errorf("--db=all restored a schema that is not fleet's")
	}

	// --db=all needs both stores in one database.
	if _, err := dsnFor("all", config.WithSchema(srcDB, config.ChatSchema), config.WithSchema(dstDB, config.SchedSchema)); err == nil || !strings.Contains(err.Error(), "different databases") {
		t.Fatalf("dsnFor all across databases = %v, want a refusal", err)
	}
}
