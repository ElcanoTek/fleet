// Package fleetdb gives a test its own one-database fleet (#2049 M2): a fresh
// database cloned from a template that holds both stores, fully migrated, in
// the production layout (ADR-0098) — the chat store in schema chat, the
// scheduler in schema sched — and opens the stores on it.
//
// It imports both stores, so their own internal tests (package store,
// package db under internal/sched/db) cannot use it; they clone single-store
// templates through internal/testdb directly. Everything else uses this —
// internal/sched/storage too: fleetdb deliberately does not import it, so its
// tests can (wrap SchedDB with storage.SetDatabase).
//
// Every helper skips the test only when FLEET_TEST_DATABASE_URL is unset and
// fails it on any other error (internal/testdb). Each call to New is a new
// database, dropped when the test ends; the stores a helper opens are closed
// before that.
package fleetdb

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// spec is the one-database template: both stores migrated through their own
// runners, each on its schema's DSN, exactly as `fleet serve` does on a
// fresh DATABASE_URL.
var spec = testdb.Spec{
	Name: "fleet",
	Fingerprint: func() string {
		return "chat:" + store.MigrationFingerprint() + " sched:" + scheddb.MigrationFingerprint()
	},
	Migrate: func(_ context.Context, dsn string) error {
		chat, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second})
		if err != nil {
			return err
		}
		if err := chat.Close(); err != nil {
			return err
		}
		sched := scheddb.New()
		if err := sched.Init(config.WithSchema(dsn, config.SchedSchema), scheddb.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second}); err != nil {
			return err
		}
		return sched.Close()
	},
}

// DB is one test's database.
type DB struct {
	// DSN names the database with no schema: what DATABASE_URL holds on a
	// one-database install (config.ResolveDSNs adds each store's schema).
	DSN string
}

// New clones the template into a fresh database for t.
func New(t testing.TB) DB {
	t.Helper()
	return DB{DSN: testdb.New(t, spec)}
}

// ChatDSN is the chat store's DSN (schema chat), as config.ResolveDSNs
// derives it from DSN.
func (d DB) ChatDSN() string { return config.WithSchema(d.DSN, config.ChatSchema) }

// SchedDSN is the scheduler's DSN (schema sched).
func (d DB) SchedDSN() string { return config.WithSchema(d.DSN, config.SchedSchema) }

// ChatPool is the small pool the helpers open the chat store with
// (testdb.PoolMaxOpen).
func ChatPool() store.PoolConfig {
	p := store.DefaultPoolConfig()
	p.MaxOpenConns, p.MaxIdleConns = testdb.PoolMaxOpen, testdb.PoolMaxIdle
	return p
}

// SchedPool is the small pool the helpers open the scheduler with.
func SchedPool() scheddb.PoolConfig {
	p := scheddb.DefaultPoolConfig()
	p.MaxOpenConns, p.MaxIdleConns = testdb.PoolMaxOpen, testdb.PoolMaxIdle
	return p
}

// Chat opens the chat store on d, closed when the test ends.
func (d DB) Chat(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(d.ChatDSN(), ChatPool())
	if err != nil {
		t.Fatalf("fleetdb: open chat store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// SchedDB opens the scheduler database on d, closed when the test ends.
func (d DB) SchedDB(t testing.TB) *scheddb.Database {
	t.Helper()
	db := scheddb.New()
	if err := db.Init(d.SchedDSN(), SchedPool()); err != nil {
		t.Fatalf("fleetdb: open sched store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Chat is New(t).Chat(t): a chat store on a database of its own.
func Chat(t testing.TB) *store.Store {
	t.Helper()
	return New(t).Chat(t)
}

// SchedDB is New(t).SchedDB(t).
func SchedDB(t testing.TB) *scheddb.Database {
	t.Helper()
	return New(t).SchedDB(t)
}

// DeleteRows runs `DELETE FROM <table>` for each table, in order, failing the
// test on any error — for a test that wipes mid-run (an export → wipe →
// import round trip). Table names come from test code, never from input.
func DeleteRows(t testing.TB, database *scheddb.Database, tables ...string) {
	t.Helper()
	for _, table := range tables {
		if _, err := database.Conn().ExecContext(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatalf("fleetdb: DELETE FROM %s: %v", table, err)
		}
	}
}
