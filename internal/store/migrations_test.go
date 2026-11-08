package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/migrations"
)

// The one migration runner's own tests live in internal/migrations; these
// check that the chat store's Open runs it.

// openRawDB opens a raw pgx connection, bypassing Open, on a fresh migrated
// database of the test's own (dsn), so a test can inspect the ledger
// directly or seed weird state.
func openRawDB(t *testing.T) (db *sql.DB, dsn string) {
	t.Helper()
	dsn = newTestDSN(t)
	return openRaw(t, dsn), dsn
}

func openRaw(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// maxAppliedVersion reads the highest version recorded in the one ledger,
// core.schema_versions. Zero means it is empty.
func maxAppliedVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v sql.NullInt64
	err := db.QueryRowContext(context.Background(),
		`SELECT MAX(version) FROM core.schema_versions`).Scan(&v)
	if err != nil {
		t.Fatalf("read core.schema_versions: %v", err)
	}
	if !v.Valid {
		return 0
	}
	return int(v.Int64)
}

func TestMigrations_FreshDBGetsEverything(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	// First open runs migrations; second should be a no-op.
	s1, dsn := newTestStoreDSN(t)
	v1 := maxAppliedVersion(t, s1.db)

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

func TestMigrations_SingleConnectionPool(t *testing.T) {
	t.Parallel()

	// An empty database: the whole migration set on one connection.
	pool := testPool()
	pool.MaxOpenConns, pool.MaxIdleConns = 1, 1
	start := time.Now()
	s, err := Open(emptyTestDSN(t), pool)
	if err != nil {
		t.Fatalf("Open with MaxOpenConns=1: %v", err)
	}
	_ = s.Close()
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("single-connection migration took %s", d)
	}
}

func TestMigrations_RefusesNewerDB(t *testing.T) {
	t.Parallel()

	// Poison the ledger with a version beyond what the binary knows about —
	// Open must refuse rather than silently run older-schema code against
	// newer data.
	raw, dsn := openRawDB(t)
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO core.schema_versions (version, name, checksum) VALUES ($1, $2, '')`,
		9999, "9999_ghost",
	); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if _, err := Open(dsn, testPool()); err == nil {
		t.Fatal("Open should reject a DB with a future schema version")
	}
}

func TestMigrations_PreservesExistingData(t *testing.T) {
	t.Parallel()

	// A real upgrade scenario: data exists; a future migration runs; data
	// survives. We use a single Store and verify data persists across
	// Close/Open cycles.
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "bob@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Reopen, to confirm Open itself is non-destructive.
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

func TestMigration046_NormalizesLegacyPositionTies(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "u@example.com", "", "assistant", "", false)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Recreate migration 045's non-unique index inside the transaction, then
	// seed the collision migration 046 must repair. Rolling the transaction back
	// leaves the test database's already-migrated schema untouched.
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS chat_input_queue_active_position`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS chat_input_queue_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS chat_input_queue_terminal_retention`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX chat_input_queue_pending ON chat_input_queue (conversation_id, position) WHERE state = 'queued'`); err != nil {
		t.Fatal(err)
	}
	for _, values := range []struct {
		id        string
		state     string
		position  int
		createdAt int
	}{
		{"legacy-b", "queued", 5, 100},
		{"legacy-a", "running", 5, 100},
		{"legacy-c", "injected", 7, 99},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_input_queue
			   (id, conversation_id, user_email, client_input_id, message, attachments,
			    mode, state, position, created_at, updated_at)
			 VALUES ($1, $2, 'u@example.com', $1, $1, '[]', 'queued', $3, $4, $5, $5)`,
			values.id, conv.ID, values.state, values.position, values.createdAt); err != nil {
			t.Fatal(err)
		}
	}

	migrationSQL, ok := migrations.Body(46)
	if !ok {
		t.Fatal("migration 046 not embedded")
	}
	if _, err := tx.ExecContext(ctx, migrationSQL); err != nil {
		t.Fatalf("apply migration 046: %v", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM chat_input_queue
		  WHERE conversation_id = $1 AND state IN ('queued','running','injected')
		  ORDER BY position`, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if want := "legacy-a,legacy-b,legacy-c"; strings.Join(got, ",") != want {
		t.Fatalf("normalized order = %v, want %s", got, want)
	}

	if _, err := tx.ExecContext(ctx, `SAVEPOINT duplicate_position`); err != nil {
		t.Fatal(err)
	}
	_, duplicateErr := tx.ExecContext(ctx,
		`INSERT INTO chat_input_queue
		   (id, conversation_id, user_email, client_input_id, message, attachments,
		    mode, state, position, created_at, updated_at)
		 VALUES ('duplicate-position', $1, 'u@example.com', 'duplicate-position', '', '[]',
		         'queued', 'queued', 1, 200, 200)`, conv.ID)
	if duplicateErr == nil {
		t.Fatal("migration 046 did not enforce unique queued positions")
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT duplicate_position`); err != nil {
		t.Fatal(err)
	}
	// The index is deliberately partial: terminal history may retain its old
	// position until the retention sweep removes it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat_input_queue
		   (id, conversation_id, user_email, client_input_id, message, attachments,
		    mode, state, position, created_at, updated_at)
		 VALUES ('terminal-position', $1, 'u@example.com', 'terminal-position', '', '[]',
		         'queued', 'completed', 1, 200, 200)`, conv.ID); err != nil {
		t.Fatalf("terminal duplicate position should remain allowed: %v", err)
	}
}
