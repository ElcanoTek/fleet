package testdb

// Deprecated helpers kept only while the callers move to New / fleetdb.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
)

// SchedLockKey is the advisory lock every sched-DB test helper serializes on,
// so a test never wipes rows another package's test is using.
const SchedLockKey = 1

// ChatDSN returns the chat test DSN (FLEET_TEST_DATABASE_URL), skipping the
// test only when it is unset.
func ChatDSN(t testing.TB) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("FLEET_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set — skipping Postgres-backed test")
	}
	return dsn
}

// SchedDSN returns the sched test DSN (DATABASE_URL), skipping the test only
// when it is unset. Prefer SchedDB, which also serializes and cleans; use this
// only for a test that opens its own connection (a CLI verb under test).
func SchedDSN(t testing.TB) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("DATABASE_URL is not set — skipping sched-DB test")
	}
	return dsn
}

// SchedDB opens and migrates the sched test database (SchedDSN), takes
// pg_advisory_lock(SchedLockKey) on a dedicated connection for the life of the
// test, and runs `DELETE FROM <table>` for each of cleanTables, in order,
// before the test and again after it. Any open/migrate/lock/pre-clean error
// fails the test. Cleanup releases the lock and closes the database.
func SchedDB(t testing.TB, cleanTables ...string) *scheddb.Database {
	t.Helper()
	dsn := SchedDSN(t)
	database := scheddb.New()
	if err := database.Init(dsn, scheddb.DefaultPoolConfig()); err != nil {
		t.Fatalf("testdb: open sched DB: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		_ = database.Close()
		t.Fatalf("testdb: sched lock connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", SchedLockKey); err != nil {
		_ = conn.Close()
		_ = database.Close()
		t.Fatalf("testdb: sched test lock: %v", err)
	}
	clean := func() error { return deleteRows(ctx, database, cleanTables) }
	t.Cleanup(func() {
		// Best-effort after the test: a test may legitimately have closed the
		// database itself, and the rows are wiped again before the next one.
		if err := clean(); err != nil {
			t.Logf("testdb: post-test clean: %v", err)
		}
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", SchedLockKey)
		_ = conn.Close()
		_ = database.Close()
	})
	if err := clean(); err != nil {
		t.Fatalf("testdb: pre-test clean: %v", err)
	}
	return database
}

// DeleteRows runs `DELETE FROM <table>` for each table, in order, failing the
// test on any error — for a test that wipes mid-run (an export → wipe →
// import round trip).
func DeleteRows(t testing.TB, database *scheddb.Database, tables ...string) {
	t.Helper()
	if err := deleteRows(context.Background(), database, tables); err != nil {
		t.Fatalf("testdb: delete rows: %v", err)
	}
}

func deleteRows(ctx context.Context, database *scheddb.Database, tables []string) error {
	for _, table := range tables {
		// Table names come from test code, never from input.
		if _, err := database.Conn().ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("DELETE FROM %s: %w", table, err)
		}
	}
	return nil
}
