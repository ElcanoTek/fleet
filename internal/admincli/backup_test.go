package admincli

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

func TestParsePGConn(t *testing.T) {
	c, err := parsePGConn("postgres://alice:s3cr3t@db.example:6543/fleet_chat?sslmode=require")
	if err != nil {
		t.Fatalf("parsePGConn: %v", err)
	}
	if c.host != "db.example" || c.port != "6543" || c.user != "alice" ||
		c.password != "s3cr3t" || c.dbname != "fleet_chat" || c.sslmode != "require" {
		t.Fatalf("parsed = %+v", c)
	}

	// Defaults: no port → 5432.
	c2, err := parsePGConn("postgresql://bob@localhost/sched")
	if err != nil {
		t.Fatalf("parsePGConn (defaults): %v", err)
	}
	if c2.port != "5432" || c2.dbname != "sched" || c2.password != "" {
		t.Fatalf("defaults wrong: %+v", c2)
	}

	for _, bad := range []string{
		"mysql://x/y",           // wrong scheme
		"postgres://localhost",  // no dbname
		"postgres://localhost/", // empty dbname
		"://nonsense",           // unparseable scheme
	} {
		if _, err := parsePGConn(bad); err == nil {
			t.Errorf("parsePGConn(%q) should error", bad)
		}
	}
}

func TestPGConnEnvHidesPasswordFromArgv(t *testing.T) {
	c, err := parsePGConn("postgres://alice:s3cr3t@h/db")
	if err != nil {
		t.Fatal(err)
	}
	env := c.env()
	var sawPassword bool
	for _, kv := range env {
		if kv == "PGPASSWORD=s3cr3t" {
			sawPassword = true
		}
	}
	if !sawPassword {
		t.Fatal("password must be passed via PGPASSWORD env")
	}
}

func TestSelectDBs(t *testing.T) {
	cases := map[string][]string{
		"chat":  {"chat"},
		"sched": {"sched"},
		"all":   {"chat", "sched"},
		"":      {"chat", "sched"},
	}
	for in, want := range cases {
		got, err := selectDBs(in)
		if err != nil {
			t.Fatalf("selectDBs(%q): %v", in, err)
		}
		if len(got) != len(want) {
			t.Fatalf("selectDBs(%q) = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("selectDBs(%q) = %v, want %v", in, got, want)
			}
		}
	}
	if _, err := selectDBs("bogus"); err == nil {
		t.Error("selectDBs(bogus) should error")
	}
}

func TestBackupFileName(t *testing.T) {
	ts := time.Date(2026, 6, 23, 14, 5, 6, 0, time.UTC)
	if got := backupFileName("chat", ts); got != "fleet-chat-20260623T140506Z.dump" {
		t.Fatalf("backupFileName = %q", got)
	}
}

// withDBName returns dsn with its database path swapped to db (preserving creds,
// host, and query like sslmode). Used to reach the maintenance DB and the scratch
// DBs the round-trip test provisions.
func withDBName(t *testing.T, dsn, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}

// TestBackupRestoreRoundTrip is the load-bearing test the issue's Note demands: a
// backup is only complete if its restore is verified. It provisions two scratch
// databases (so it never touches fleet_chat_test/fleet_sched_test), seeds a
// sentinel row in the source, runs the real pg_dump wrapper, restores into the
// fresh destination with the real pg_restore wrapper, and asserts the row arrives.
//
// Skips cleanly when DATABASE_URL is unset or pg_dump/pg_restore are absent, so a
// machine without the postgres client tools still passes `make test`.
func TestBackupRestoreRoundTrip(t *testing.T) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set, skipping pg_dump/pg_restore round-trip")
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not found, skipping round-trip")
	}
	if _, err := exec.LookPath("pg_restore"); err != nil {
		t.Skip("pg_restore not found, skipping round-trip")
	}
	ctx := context.Background()

	const srcDB = "fleet_backup_src_test"
	const dstDB = "fleet_backup_dst_test"
	adminDSN := withDBName(t, base, "postgres")
	srcDSN := withDBName(t, base, srcDB)
	dstDSN := withDBName(t, base, dstDB)

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin DB: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("admin DB unreachable, skipping: %v", err)
	}
	defer admin.Close()

	// pg_dump refuses to dump a server newer than itself ("aborting because of
	// server version mismatch"). That is an environment skew (e.g. a runner
	// shipping client 16 against a server-18 service), not a code defect, so skip
	// rather than fail when the major versions disagree.
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d; skipping round-trip (install a matching postgresql-client)", cm, sm)
	}

	// Fresh scratch DBs. DROP first in case a prior run left them behind.
	for _, db := range []string{srcDB, dstDB} {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db); err != nil {
			t.Fatalf("drop %s: %v", db, err)
		}
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+db); err != nil {
			if isPermissionError(err) {
				t.Skipf("cannot CREATE DATABASE (insufficient privilege), skipping: %v", err)
			}
			t.Fatalf("create %s: %v", db, err)
		}
	}
	defer func() {
		for _, db := range []string{srcDB, dstDB} {
			admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db)
		}
	}()

	// Seed a sentinel row in the source DB, then close the connection so the dump
	// (and later the destination restore) has no contention.
	const sentinel = "round-trip-ok"
	func() {
		src, err := sql.Open("pgx", srcDSN)
		if err != nil {
			t.Fatalf("open src: %v", err)
		}
		defer src.Close()
		if _, err := src.ExecContext(ctx, `CREATE TABLE sentinel (note text)`); err != nil {
			t.Fatalf("create sentinel table: %v", err)
		}
		if _, err := src.ExecContext(ctx, `INSERT INTO sentinel (note) VALUES ($1)`, sentinel); err != nil {
			t.Fatalf("insert sentinel: %v", err)
		}
	}()

	// Back up the source.
	dumpPath := filepath.Join(t.TempDir(), "src.dump")
	if err := runPgDump(ctx, srcDSN, dumpPath); err != nil {
		t.Fatalf("runPgDump: %v", err)
	}
	data, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if len(data) == 0 || !bytes.HasPrefix(data, []byte("PGDMP")) {
		t.Fatalf("dump is not a pg custom-format archive (len=%d, prefix=%q)", len(data), firstBytes(data, 8))
	}

	// Restore into the (empty) destination DB and verify the sentinel survived.
	if err := runPgRestore(ctx, dstDSN, dumpPath); err != nil {
		t.Fatalf("runPgRestore: %v", err)
	}
	dst, err := sql.Open("pgx", dstDSN)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	var got string
	if err := dst.QueryRowContext(ctx, `SELECT note FROM sentinel`).Scan(&got); err != nil {
		t.Fatalf("read restored sentinel: %v", err)
	}
	if got != sentinel {
		t.Fatalf("restored sentinel = %q, want %q", got, sentinel)
	}
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func isPermissionError(err error) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte("permission denied"))
}

// pgMajorVersions returns (pg_dump major, server major, ok). ok is false if
// either could not be determined (in which case the caller should not skip on a
// mismatch it cannot prove).
func pgMajorVersions(ctx context.Context, db *sql.DB) (clientMajor, serverMajor int, ok bool) {
	out, err := exec.CommandContext(ctx, "pg_dump", "--version").Output()
	if err != nil {
		return 0, 0, false
	}
	// e.g. "pg_dump (PostgreSQL) 16.14 (Ubuntu 16.14-1.pgdg24.04+1)"
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return 0, 0, false
	}
	cm := majorOf(fields[2])
	// server_version_num is e.g. 180004 → major 18.
	var num int
	if err := db.QueryRowContext(ctx, "SHOW server_version_num").Scan(&num); err != nil {
		return 0, 0, false
	}
	if cm == 0 || num == 0 {
		return 0, 0, false
	}
	return cm, num / 10000, true
}

// majorOf parses the leading integer of a version string like "16.14" → 16.
func majorOf(v string) int {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}
