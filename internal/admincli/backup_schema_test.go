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

func TestParseTOCTableSchemas(t *testing.T) {
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
	if got := parseTOCTableSchemas(toc); !reflect.DeepEqual(got, []string{"chat"}) {
		t.Errorf("schemas = %v, want [chat]", got)
	}
	mixed := "1; 1259 1 TABLE public a o\n2; 1259 2 TABLE sched b o\n"
	if got := parseTOCTableSchemas(mixed); !reflect.DeepEqual(got, []string{"public", "sched"}) {
		t.Errorf("schemas = %v, want [public sched]", got)
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
			CREATE TABLE chat.sentinel (note text); INSERT INTO chat.sentinel VALUES ('chat-ok');
			CREATE TABLE sched.sentinel (note text); INSERT INTO sched.sentinel VALUES ('sched-ok')`); err != nil {
			t.Fatal(err)
		}
	}()

	chatDump := filepath.Join(t.TempDir(), "chat.dump")
	if err := runPgDump(ctx, srcChat, chatDump); err != nil {
		t.Fatalf("dump chat: %v", err)
	}
	if got, err := dumpTableSchemas(ctx, chatDump); err != nil || !reflect.DeepEqual(got, []string{"chat"}) {
		t.Fatalf("chat dump holds schemas %v (%v), want only chat", got, err)
	}
	schedDump := filepath.Join(t.TempDir(), "sched.dump")
	if err := runPgDump(ctx, srcSched, schedDump); err != nil {
		t.Fatalf("dump sched: %v", err)
	}

	if err := runPgRestore(ctx, dstChat, schedDump); err == nil || !strings.Contains(err.Error(), `uses schema "chat"`) {
		t.Fatalf("restoring the sched dump into chat = %v, want a refusal", err)
	}
	if err := runPgRestore(ctx, dstChat, chatDump); err != nil {
		t.Fatalf("restore chat: %v", err)
	}
	dst, err := sql.Open("pgx", dstChat)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var note string
	if err := dst.QueryRowContext(ctx, `SELECT note FROM sentinel`).Scan(&note); err != nil || note != "chat-ok" {
		t.Fatalf("restored chat sentinel = %q, %v", note, err)
	}
	var n int
	if err := dst.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'sched'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the chat restore created a sched schema (%d, %v)", n, err)
	}
}
