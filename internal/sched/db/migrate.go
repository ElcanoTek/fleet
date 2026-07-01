package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations runs all pending database migrations using golang-migrate with
// the embedded SQL files. golang-migrate's postgres driver works against the
// *sql.DB regardless of whether it was opened with lib/pq or pgx — it issues
// plain SQL over database/sql.
func RunMigrations(conn *sql.DB) error {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to create migration source: %w", err)
	}
	driver, err := postgres.WithInstance(conn, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("failed to create migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		var dirty migrate.ErrDirty
		if errors.As(err, &dirty) {
			return fmt.Errorf("sched migrations are DIRTY at version %d — a previous migration failed mid-run, "+
				"so the process refuses to start. Inspect the DB, then force the last-good version with "+
				"`migrate force <version>` and restart: %w", dirty.Version, err)
		}
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}

// MigrationInfo is one migration in a MigrationReport.
type MigrationInfo struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}

// MigrationReport is the read-only applied-vs-pending view of the orchestrator
// (sched) DB's migrations, served at GET /admin/migrations (#256) and printed by
// `fleet migrate status`. golang-migrate advances the schema linearly and tracks
// only a single {version, dirty} row, so CurrentVersion is that row's version
// (nil when nothing has been applied) and Applied is every embedded migration at
// or below it. Dirty is true when a prior migration failed mid-flight.
type MigrationReport struct {
	DB             string          `json:"db"`              // always "sched"
	Runner         string          `json:"runner"`          // always "golang-migrate"
	MigrationTable string          `json:"migration_table"` // always "schema_migrations"
	CurrentVersion *int            `json:"current_version"`
	Dirty          bool            `json:"dirty"`
	Applied        []MigrationInfo `json:"applied"`
	Pending        []MigrationInfo `json:"pending"`
}

// schedMigrationFilePattern matches golang-migrate up-migration files
// (NNN_name.up.sql); the down counterparts are the rollback path and are not
// enumerated here.
var schedMigrationFilePattern = regexp.MustCompile(`^(\d+)_(.+)\.up\.sql$`)

// availableMigrations enumerates the embedded up-migration files in ascending
// version order. It reads only the embedded FS (no DB), so it is the "what this
// build knows how to apply" half of the status report.
func availableMigrations() ([]MigrationInfo, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	out := make([]MigrationInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := schedMigrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("parse version in %s: %w", e.Name(), err)
		}
		out = append(out, MigrationInfo{Version: v, Name: strings.TrimSuffix(e.Name(), ".up.sql")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// MigrationStatus reports applied vs pending sched-DB migrations (#256). It is
// strictly READ-ONLY: it reads golang-migrate's schema_migrations tracking row
// directly (guarded by to_regclass) rather than via postgres.WithInstance, which
// would CREATE that table as a side effect — so it applies nothing and creates
// nothing, and is safe to call against a fresh database (every migration is then
// reported pending).
func MigrationStatus(conn *sql.DB) (MigrationReport, error) {
	report := MigrationReport{DB: "sched", Runner: "golang-migrate", MigrationTable: "schema_migrations"}
	available, err := availableMigrations()
	if err != nil {
		return report, err
	}

	var tbl sql.NullString
	if err := conn.QueryRow(`SELECT to_regclass('schema_migrations')`).Scan(&tbl); err != nil {
		return report, fmt.Errorf("probe schema_migrations: %w", err)
	}
	var current *int
	if tbl.Valid {
		var v int64
		var dirty bool
		switch err := conn.QueryRow(`SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&v, &dirty); {
		case errors.Is(err, sql.ErrNoRows):
			// Table exists but empty → nothing applied.
		case err != nil:
			return report, fmt.Errorf("read schema_migrations: %w", err)
		default:
			iv := int(v)
			current = &iv
			report.Dirty = dirty
		}
	}
	report.CurrentVersion = current

	for _, m := range available {
		if current != nil && m.Version <= *current {
			report.Applied = append(report.Applied, m)
		} else {
			report.Pending = append(report.Pending, m)
		}
	}
	return report, nil
}

// GetMigrationVersion returns the current migration version.
func GetMigrationVersion(conn *sql.DB) (uint, bool, error) {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return 0, false, fmt.Errorf("failed to create migration source: %w", err)
	}
	driver, err := postgres.WithInstance(conn, &postgres.Config{})
	if err != nil {
		return 0, false, fmt.Errorf("failed to create migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return 0, false, fmt.Errorf("failed to create migrate instance: %w", err)
	}
	return m.Version()
}
