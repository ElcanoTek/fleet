package store

import (
	"context"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// chatTemplate is the chat store alone, migrated in schema chat — the
// production layout (ADR-0098). This package cannot use internal/testdb/fleetdb
// (it imports this package), so its tests clone a template of their own.
var chatTemplate = testdb.Spec{
	Name:        "chat",
	Fingerprint: MigrationFingerprint,
	Migrate: func(_ context.Context, dsn string) error {
		s, err := Open(config.WithSchema(dsn, config.ChatSchema), PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second})
		if err != nil {
			return err
		}
		return s.Close()
	},
}

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
