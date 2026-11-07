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

func TestParseTOCSchemas(t *testing.T) {
	toc := `;
; Archive created at 2026-11-07 10:00:00 UTC
;     dbname: fleet
;
7; 2615 16390 SCHEMA - chat fleet
215; 1259 16391 TABLE chat users fleet
216; 1259 16400 SEQUENCE chat users_id_seq fleet
3340; 0 16391 TABLE DATA chat users fleet
217; 1259 16410 TABLE chat conversations fleet
`
	if got := parseTOCSchemas(toc); !reflect.DeepEqual(got, []string{"chat"}) {
		t.Errorf("schemas = %v, want [chat]", got)
	}
	mixed := "1; 1259 1 TABLE public a o\n2; 1259 2 TABLE sched b o\n"
	if got := parseTOCSchemas(mixed); !reflect.DeepEqual(got, []string{"public", "sched"}) {
		t.Errorf("schemas = %v, want [public sched]", got)
	}
	// An empty schema's dump: its SCHEMA entry alone names it (#2055 review).
	if got := parseTOCSchemas("8; 2615 5450663 SCHEMA - sched fleet\n"); !reflect.DeepEqual(got, []string{"sched"}) {
		t.Errorf("empty-schema dump: schemas = %v, want [sched]", got)
	}
	// Nothing to establish a schema from: restore refuses (runPgRestore).
	if got := parseTOCSchemas(";\n; Archive created\n3; 0 0 COMMENT - EXTENSION x\n"); len(got) != 0 {
		t.Errorf("schemas = %v, want none", got)
	}
}

// TestBackupRestoreSchemaMode: in the one-database layout a store's backup is
// its schema only, it restores into that store's schema of another database,
// and a dump of the other store is refused before anything is dropped.
func TestBackupRestoreSchemaMode(t *testing.T) {
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
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("admin DB unreachable: %v", err)
	}
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d", cm, sm)
	}
	const srcDB, dstDB = "fleet_backup_schema_src_test", "fleet_backup_schema_dst_test"
	for _, db := range []string{srcDB, dstDB} {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db)
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
	srcChat := config.WithSchema(withDBName(t, base, srcDB), config.ChatSchema)
	srcSched := config.WithSchema(withDBName(t, base, srcDB), config.SchedSchema)
	dstChat := config.WithSchema(withDBName(t, base, dstDB), config.ChatSchema)
	func() {
		src, err := sql.Open("pgx", withDBName(t, base, srcDB))
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		if _, err := src.ExecContext(ctx, `CREATE SCHEMA chat; CREATE SCHEMA sched;
			CREATE SCHEMA sched_empty; CREATE TABLE chat.sentinel (note text); INSERT INTO chat.sentinel VALUES ('chat-ok');
			CREATE TABLE sched.sentinel (note text); INSERT INTO sched.sentinel VALUES ('sched-ok')`); err != nil {
			t.Fatal(err)
		}
	}()

	chatDump := filepath.Join(t.TempDir(), "chat.dump")
	if err := runPgDump(ctx, srcChat, chatDump); err != nil {
		t.Fatalf("dump chat: %v", err)
	}
	if got, err := dumpSchemas(ctx, chatDump); err != nil || !reflect.DeepEqual(got, []string{"chat"}) {
		t.Fatalf("chat dump holds schemas %v (%v), want only chat", got, err)
	}
	schedDump := filepath.Join(t.TempDir(), "sched.dump")
	if err := runPgDump(ctx, srcSched, schedDump); err != nil {
		t.Fatalf("dump sched: %v", err)
	}

	// An empty schema's dump has no TABLE entry — only its SCHEMA entry —
	// and must still be recognised as that schema's (#2055 review).
	emptyDump := filepath.Join(t.TempDir(), "empty.dump")
	if err := runPgDump(ctx, config.WithSchema(withDBName(t, base, srcDB), "sched_empty"), emptyDump); err != nil {
		t.Fatalf("dump an empty schema: %v", err)
	}
	if got, err := dumpSchemas(ctx, emptyDump); err != nil || !reflect.DeepEqual(got, []string{"sched_empty"}) {
		t.Fatalf("empty-schema dump holds schemas %v (%v), want [sched_empty]", got, err)
	}

	// The destination is a live one-database install: both stores present,
	// the chat store with stale rows, the sched store with rows of its own
	// that a chat restore must not touch.
	dst, err := sql.Open("pgx", withDBName(t, base, dstDB))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := dst.ExecContext(ctx, `CREATE SCHEMA chat; CREATE SCHEMA sched;
		CREATE TABLE chat.sentinel (note text); INSERT INTO chat.sentinel VALUES ('stale');
		CREATE TABLE sched.sentinel (note text); INSERT INTO sched.sentinel VALUES ('sched-live')`); err != nil {
		t.Fatal(err)
	}

	for name, dump := range map[string]string{"the sched dump": schedDump, "an empty other-schema dump": emptyDump} {
		if err := runPgRestore(ctx, dstChat, dump); err == nil || !strings.Contains(err.Error(), `uses schema "chat"`) {
			t.Fatalf("restoring %s into chat = %v, want a refusal", name, err)
		}
	}
	if err := runPgRestore(ctx, dstChat, chatDump); err != nil {
		t.Fatalf("restore chat: %v", err)
	}
	var note string
	if err := dst.QueryRowContext(ctx, `SELECT string_agg(note, ',') FROM chat.sentinel`).Scan(&note); err != nil || note != "chat-ok" {
		t.Fatalf("restored chat sentinel = %q, %v; want only the dump's row", note, err)
	}
	if err := dst.QueryRowContext(ctx, `SELECT string_agg(note, ',') FROM sched.sentinel`).Scan(&note); err != nil || note != "sched-live" {
		t.Errorf("the chat restore touched the sched store: %q, %v", note, err)
	}

	// Into a fresh database, the restore creates the store's schema itself
	// and nothing else.
	if _, err := dst.ExecContext(ctx, `DROP SCHEMA chat CASCADE; DROP SCHEMA sched CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := runPgRestore(ctx, dstChat, chatDump); err != nil {
		t.Fatalf("restore chat into a fresh database: %v", err)
	}
	var n int
	if err := dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'sched'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the chat restore created a sched schema (%d, %v)", n, err)
	}
}
