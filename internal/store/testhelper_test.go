package store

import (
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/onedb"
)

// chatTemplate is the one-database template (internal/testdb/onedb): the
// whole database migrated by the one runner — the chat store in schema chat,
// the scheduler in sched (the chat store reads and edits chat schedules,
// which are sched rows, ADR-0104), the identity table in core. fleetdb
// clones the same one.
var chatTemplate = onedb.Spec

// testPool is the small pool a test store opens with (testdb.PoolMaxOpen):
// many tests run at once.
func testPool() PoolConfig {
	p := DefaultPoolConfig()
	p.MaxOpenConns, p.MaxIdleConns = testdb.PoolMaxOpen, testdb.PoolMaxIdle
	return p
}

// newTestDSN returns the DSN (schema chat) of a fresh, fully migrated
// database of the test's own, dropped when the test ends. The test skips only
// when FLEET_TEST_DATABASE_URL is unset (internal/testdb).
func newTestDSN(t testing.TB) string {
	t.Helper()
	return config.WithSchema(testdb.New(t, chatTemplate), config.ChatSchema)
}

// emptyTestDSN returns the DSN (schema chat) of a fresh EMPTY database, for a
// test that drives the migration runner itself.
func emptyTestDSN(t testing.TB) string {
	t.Helper()
	return config.WithSchema(testdb.Empty(t), config.ChatSchema)
}

// newTestStore opens a Store on a database of the test's own (newTestDSN).
// Nothing is shared with any other test, so there is nothing to wipe.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	s, _ := newTestStoreDSN(t)
	return s
}

// newTestStoreDSN is newTestStore that also returns the DSN, for a test that
// reopens the same database.
func newTestStoreDSN(t testing.TB) (*Store, string) {
	t.Helper()
	dsn := newTestDSN(t)
	s, err := Open(dsn, testPool())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dsn
}
