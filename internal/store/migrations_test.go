package store

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openRawDB opens a raw pgx connection bypassing applyMigrations so a
// test can inspect schema_migrations directly or seed weird state.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL is not set — skipping Postgres-backed test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// maxAppliedVersion reads the highest version recorded in
// schema_migrations. Zero means the table is empty or absent.
func maxAppliedVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v sql.NullInt64
	err := db.QueryRowContext(context.Background(),
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !v.Valid {
		return 0
	}
	return int(v.Int64)
}

func TestMigrations_FreshDBGetsEverything(t *testing.T) {
	s := newTestStore(t)

	// Store is usable — pick a table from 001_initial and write to it.
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if v := maxAppliedVersion(t, s.db); v < 1 {
		t.Errorf("max applied version: got %d, want >= 1", v)
	}
}

func TestMigrations_IdempotentReopen(t *testing.T) {
	// First open runs migrations; second should be a no-op.
	s1 := newTestStore(t)
	v1 := maxAppliedVersion(t, s1.db)

	dsn := testDSN()
	s2, err := Open(dsn, DefaultPoolConfig())
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()

	v2 := maxAppliedVersion(t, s2.db)
	if v1 != v2 {
		t.Errorf("version changed across reopens: %d → %d", v1, v2)
	}
}

func TestMigrations_RefusesNewerDB(t *testing.T) {
	// Poison schema_migrations with a version beyond what the binary knows
	// about — Open must refuse rather than silently run older-schema code
	// against newer data.
	raw := openRawDB(t)
	ctx := context.Background()
	if err := ensureSchemaMigrationsTable(ctx, raw); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)
		 ON CONFLICT (version) DO NOTHING`,
		999, "ghost", 0,
	); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	t.Cleanup(func() {
		// Clean up the poisoned row so subsequent tests can Open normally.
		_, _ = raw.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 999`)
	})

	if _, err := Open(testDSN(), DefaultPoolConfig()); err == nil {
		t.Fatal("Open should reject a DB with a future schema version")
	}
}

func TestMigrations_PreservesExistingData(t *testing.T) {
	// A real upgrade scenario: data exists; a future migration runs; data
	// survives. Since our helper truncates on open, we use a single Store
	// and verify data persists across Close/Open cycles.
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "bob@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Reopen WITHOUT truncating, to confirm Open itself is non-destructive.
	dsn := testDSN()
	s2, err := Open(dsn, DefaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	users, err := s2.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Email != "bob@example.com" {
		t.Errorf("data didn't survive: %+v", users)
	}
}

func TestMigrationStatusDB(t *testing.T) {
	s := newTestStore(t) // opens + runs every migration
	ctx := context.Background()

	rep, err := MigrationStatusDB(ctx, s.db)
	if err != nil {
		t.Fatalf("MigrationStatusDB: %v", err)
	}
	if rep.DB != "chat" || rep.Runner != "hand-rolled" || rep.MigrationTable != "schema_migrations" {
		t.Fatalf("unexpected metadata: %+v", rep)
	}
	embedded, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(rep.Pending) != 0 {
		t.Errorf("a fully-migrated DB should report 0 pending, got %d: %+v", len(rep.Pending), rep.Pending)
	}
	if len(rep.Applied) != len(embedded) {
		t.Errorf("applied=%d, want=%d (every embedded migration)", len(rep.Applied), len(embedded))
	}
	for i, m := range rep.Applied {
		if m.AppliedAt == nil {
			t.Errorf("applied migration %q is missing applied_at", m.Name)
		}
		if i > 0 && rep.Applied[i-1].Version >= m.Version {
			t.Errorf("Applied not strictly ascending at index %d (%d then %d)", i, rep.Applied[i-1].Version, m.Version)
		}
	}

	// A schema_migrations row the running binary does not know about (a DB ahead
	// of the build) must still surface under Applied so a downgrade is visible.
	const future = 100000
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
		future, "100000_from_a_newer_build", int64(1700000000)); err != nil {
		t.Fatalf("seed future migration row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE version = $1`, future)
	})
	rep2, err := MigrationStatusDB(ctx, s.db)
	if err != nil {
		t.Fatalf("MigrationStatusDB (ahead): %v", err)
	}
	if len(rep2.Applied) != len(embedded)+1 {
		t.Errorf("applied with a future row = %d, want %d", len(rep2.Applied), len(embedded)+1)
	}
	var found bool
	for _, m := range rep2.Applied {
		if m.Version == future {
			found = true
		}
	}
	if !found {
		t.Errorf("a schema_migrations row ahead of the embedded set must appear in Applied")
	}
}

func TestMigrations_LoadOrderAndDedup(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("expected at least one migration")
	}
	// Sorted ascending + no duplicate versions.
	for i := 1; i < len(ms); i++ {
		if ms[i].version <= ms[i-1].version {
			t.Errorf("migrations not strictly ascending: [%d]=%d, [%d]=%d",
				i-1, ms[i-1].version, i, ms[i].version)
		}
	}
}
