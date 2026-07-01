package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migrations live as `NNN_name.sql` files under migrations/. They run in
// numeric order, each inside its own transaction. The applied version +
// name + timestamp is recorded in the schema_migrations table so re-runs
// skip migrations that are already in place.
//
// To add a migration:
//   1. Create migrations/NNN_description.sql where NNN is the next
//      integer (zero-padded to 3 digits for sort stability).
//   2. Write the SQL. Use plain DDL; the runner guarantees each file
//      runs exactly once per database.
//   3. Never rename or edit a shipped migration — add a new one that
//      corrects the previous state.
//
// The scheme is deliberately simple: no down-migrations, no separate
// CLI tool. For this scale a single hand-rolled runner beats carrying
// golang-migrate as a dependency.

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationFilenamePattern — two groups: the zero-padded number and the
// human-readable suffix. Anything else in migrations/ is ignored so we
// can drop README.md etc. in there without breaking the scanner.
var migrationFilenamePattern = regexp.MustCompile(`^(\d{3})_[A-Za-z0-9_]+\.sql$`)

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads every NNN_*.sql file from the embedded FS and
// returns them in ascending-version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("parse version in %s: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d (%s and %s)", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		out = append(out, migration{
			version: version,
			name:    strings.TrimSuffix(e.Name(), ".sql"),
			sql:     string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrationLockKey is the pg_advisory_lock key used to serialize
// applyMigrations across concurrent chat-server boots (and across
// test packages sharing a database). Fixed arbitrary int64; has to
// be the same everywhere the migration runner runs.
const migrationLockKey int64 = 0x01A7B00B_CAFE01

// applyMigrations runs every migration whose version is newer than the
// highest version in schema_migrations. Each migration executes inside
// its own transaction; on failure the transaction rolls back and the
// database is left at the last successfully-applied version.
//
// Grabs a session-level pg_advisory_lock first so two processes that
// race through Open() can't both try to CREATE TABLE at once — Postgres'
// catalog locking can surface as pg_type unique-constraint violations
// under concurrent DDL otherwise.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	// Advisory lock on a dedicated connection so we hold it for the
	// full migration run. Using the pool would risk releasing early.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for migration lock: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("pg_advisory_lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if err := ensureSchemaMigrationsTable(ctx, db); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("no migrations found (migrations/*.sql must be embedded)")
	}

	applied, err := loadAppliedVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("load applied versions: %w", err)
	}

	// Safety check: if the DB has a higher version than we know about,
	// someone downgraded the binary onto a newer schema. Refuse rather
	// than silently running older-schema code against newer data.
	latestKnown := migrations[len(migrations)-1].version
	var highestApplied int
	for v := range applied {
		if v > highestApplied {
			highestApplied = v
		}
	}
	if highestApplied > latestKnown {
		return fmt.Errorf("database is at schema version %d, but this build only knows up to %d — refusing to downgrade",
			highestApplied, latestKnown)
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// ensureSchemaMigrationsTable creates the tracking table on first run.
// IF NOT EXISTS keeps subsequent runs a no-op.
func ensureSchemaMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at BIGINT NOT NULL
		)`)
	return err
}

// loadAppliedVersions returns the set of already-applied migration
// versions.
func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// MigrationInfo is one migration in a MigrationReport. AppliedAt is nil for a
// pending (not-yet-run) migration and the unix-second timestamp recorded in
// schema_migrations for an applied one.
type MigrationInfo struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	AppliedAt *int64 `json:"applied_at,omitempty"`
}

// MigrationReport is the read-only applied-vs-pending view of the chat DB's
// migrations, served at GET /admin/migrations (#256) and printed by
// `fleet migrate status`.
type MigrationReport struct {
	DB             string          `json:"db"`              // always "chat"
	Runner         string          `json:"runner"`          // always "hand-rolled"
	MigrationTable string          `json:"migration_table"` // always "schema_migrations"
	Applied        []MigrationInfo `json:"applied"`
	Pending        []MigrationInfo `json:"pending"`
}

// MigrationStatusDB reports applied vs pending chat-DB migrations by comparing
// the embedded migration set against the schema_migrations tracking table
// (#256). It is strictly READ-ONLY: it never applies a migration and never
// creates a table, so it is safe to call at any time — including against a fresh
// database where the tracking table does not exist yet (every migration is then
// reported as pending). Applied migrations the running binary does not know
// about (a DB ahead of the build) are still surfaced under Applied so a
// downgrade is visible rather than hidden.
func MigrationStatusDB(ctx context.Context, db *sql.DB) (MigrationReport, error) {
	report := MigrationReport{DB: "chat", Runner: "hand-rolled", MigrationTable: "schema_migrations"}
	embedded, err := loadMigrations()
	if err != nil {
		return report, err
	}

	type appliedRow struct {
		name string
		at   int64
	}
	applied := map[int]appliedRow{}

	// to_regclass returns NULL (not an error) when the relation is absent, so we
	// can probe for the tracking table without a failing query or error-string
	// matching, and stay read-only against a not-yet-migrated database.
	var tbl sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations')`).Scan(&tbl); err != nil {
		return report, fmt.Errorf("probe schema_migrations: %w", err)
	}
	if tbl.Valid {
		rows, err := db.QueryContext(ctx, `SELECT version, name, applied_at FROM schema_migrations`)
		if err != nil {
			return report, fmt.Errorf("read schema_migrations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			var n string
			var at int64
			if err := rows.Scan(&v, &n, &at); err != nil {
				return report, err
			}
			applied[v] = appliedRow{name: n, at: at}
		}
		if err := rows.Err(); err != nil {
			return report, err
		}
	}

	seen := map[int]bool{}
	for _, m := range embedded {
		seen[m.version] = true
		if a, ok := applied[m.version]; ok {
			at := a.at
			report.Applied = append(report.Applied, MigrationInfo{Version: m.version, Name: m.name, AppliedAt: &at})
		} else {
			report.Pending = append(report.Pending, MigrationInfo{Version: m.version, Name: m.name})
		}
	}
	for v, a := range applied {
		if !seen[v] {
			at := a.at
			report.Applied = append(report.Applied, MigrationInfo{Version: v, Name: a.name, AppliedAt: &at})
		}
	}
	sort.Slice(report.Applied, func(i, j int) bool { return report.Applied[i].Version < report.Applied[j].Version })
	// Pending inherits loadMigrations' ascending order.
	return report, nil
}

// MigrationStatus is the Store-scoped wrapper around MigrationStatusDB, so the
// HTTP layer can report migration status through the narrow chatStore interface
// without exposing the underlying *sql.DB.
func (s *Store) MigrationStatus(ctx context.Context) (MigrationReport, error) {
	return MigrationStatusDB(ctx, s.db)
}

// applyMigration runs one migration inside a transaction and records
// the applied version.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
		m.version, m.name, time.Now().Unix(),
	); err != nil {
		return err
	}
	return tx.Commit()
}
