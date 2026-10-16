// The runner below re-implements the part of github.com/golang-migrate/migrate
// v4.20.1 fleet used (Up and Force over the pgx/v5 driver and an embedded
// source); a few small pieces — the lock-key formula, the file-name pattern,
// the tracking-table DDL and quoteIdentifier — are adapted from its source.
// Copyright (c) 2016 Matthias Kadenbach and contributors; see
// LICENSE.golang-migrate. SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// The sched DB's migration runner. It replaces golang-migrate v4 (whose pgx/v5
// driver was the only reason lib/pq and pgerrcode were in go.mod) with the
// subset fleet used — Up over an embedded directory — and is deliberately
// state-compatible with it, so a database migrated by any earlier fleet
// release continues from where it stopped, and a mixed-version rollout still
// serializes on the same lock:
//
//   - the tracking table is golang-migrate's: one row in
//     <current_schema>.schema_migrations (version bigint primary key, dirty
//     boolean), created if missing;
//   - the lock is the session-level advisory lock on golang-migrate's
//     GenerateAdvisoryLockId key (crc32 of schema, table and database name,
//     times its salt), held on one dedicated connection for the whole run and
//     acquired within 15 s;
//   - each migration is version-set dirty, run as ONE multi-statement Exec
//     (no explicit transaction: Postgres runs a multi-statement simple query
//     as one implicit transaction, exactly as golang-migrate's driver did),
//     then version-set clean — so a failure leaves the row dirty and every
//     later boot refuses until an operator runs `fleet migrate force`;
//   - files are NNN_name.up.sql / NNN_name.down.sql; a version whose .up.sql
//     is missing is recorded without running anything, as upstream did.
//
// docs/IN-HOUSED-DEPENDENCIES.md records what was kept and what was dropped.

const (
	// migrationsTable is golang-migrate's default table name.
	migrationsTable = "schema_migrations"
	// advisoryLockIDSalt is golang-migrate's database.advisoryLockIDSalt.
	advisoryLockIDSalt uint32 = 1486364155
	// nilVersion is golang-migrate's database.NilVersion: nothing applied.
	nilVersion = -1
)

// migrationLockTimeout is golang-migrate's DefaultLockTimeout. A var so a test
// can shorten it.
var migrationLockTimeout = 15 * time.Second

// ErrNoChange reports that there was nothing to apply.
var ErrNoChange = errors.New("no change")

// DirtyError reports that a previous migration failed mid-run, leaving the
// tracking row dirty at Version. Its text is golang-migrate's.
type DirtyError struct {
	Version int
}

func (e DirtyError) Error() string {
	return fmt.Sprintf("Dirty database version %v. Fix and force version.", e.Version)
}

// advisoryLockID is golang-migrate's database.GenerateAdvisoryLockId(dbName,
// schema, table): the names joined with NUL in the order schema, table,
// database, CRC-32 (IEEE), multiplied by the salt in uint32 arithmetic.
// Upstream passes the decimal string as the bigint argument; the value is the
// same.
func advisoryLockID(databaseName, schemaName, tableName string) int64 {
	joined := strings.Join([]string{schemaName, tableName, databaseName}, "\x00")
	return int64(crc32.ChecksumIEEE([]byte(joined)) * advisoryLockIDSalt)
}

// schedMigration is one embedded version: its name and up-migration body.
type schedMigration struct {
	Version int
	Name    string
	Up      string // empty: no .up.sql for this version
}

// migrationFilePattern is golang-migrate's source.Regex: version, name,
// direction, extension.
var migrationFilePattern = regexp.MustCompile(`^([0-9]+)_(.*)\.(down|up)\.(.*)$`)

// loadMigrations reads every migration in dir, ascending by version. A
// duplicate version for one direction is an error, as in golang-migrate's
// iofs source.
func loadMigrations(fsys fs.FS, dir string) ([]schedMigration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	byVersion := map[int]*schedMigration{}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 63)
		if err != nil {
			return nil, fmt.Errorf("parse version in %s: %w", e.Name(), err)
		}
		version := int(v)
		key := strconv.Itoa(version) + "/" + m[3]
		if seen[key] {
			return nil, fmt.Errorf("duplicate migration file: %s", e.Name())
		}
		seen[key] = true
		mig := byVersion[version]
		if mig == nil {
			mig = &schedMigration{Version: version, Name: m[1] + "_" + m[2]}
			byVersion[version] = mig
		}
		if m[3] == "up" {
			body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", e.Name(), err)
			}
			mig.Up = string(body)
			mig.Name = m[1] + "_" + m[2]
		}
	}
	out := make([]schedMigration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// RunMigrations applies every pending embedded sched migration. Nothing to
// apply is success.
func RunMigrations(conn *sql.DB) error {
	err := migrateUp(context.Background(), conn, migrationsFS, "migrations")
	if err == nil || errors.Is(err, ErrNoChange) {
		return nil
	}
	var dirty DirtyError
	if errors.As(err, &dirty) {
		return fmt.Errorf("sched migrations are DIRTY at version %d — a previous migration failed mid-run, "+
			"so the process refuses to start. Inspect the DB, then force the last-good version with "+
			"`fleet migrate force <version>` and restart: %w", dirty.Version, err)
	}
	return fmt.Errorf("failed to run migrations: %w", err)
}

// ForceVersion sets the tracking row to version, clean, applying nothing —
// golang-migrate's Force, behind `fleet migrate force`. version must be one
// this build embeds, or -1 to record that nothing is applied.
func ForceVersion(ctx context.Context, conn *sql.DB, version int) error {
	return forceVersion(ctx, conn, migrationsFS, "migrations", version)
}

func forceVersion(ctx context.Context, conn *sql.DB, fsys fs.FS, dir string, version int) error {
	migs, err := loadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	if version != nilVersion && indexOfVersion(migs, version) < 0 {
		return fmt.Errorf("version %d is not a migration this build knows (use -1 for none)", version)
	}
	return withMigrationLock(ctx, conn, func(r *migrationRun) error {
		return r.setVersion(ctx, version, false)
	})
}

func indexOfVersion(migs []schedMigration, version int) int {
	for i, m := range migs {
		if m.Version == version {
			return i
		}
	}
	return -1
}

// migrateUp is golang-migrate's Migrate.Up: lock, refuse a dirty row, apply
// each version after the recorded one, unlock.
func migrateUp(ctx context.Context, conn *sql.DB, fsys fs.FS, dir string) error {
	migs, err := loadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	return withMigrationLock(ctx, conn, func(r *migrationRun) error {
		current, dirty, err := r.version(ctx)
		if err != nil {
			return err
		}
		if dirty {
			return DirtyError{Version: current}
		}
		next := 0
		if current != nilVersion {
			i := indexOfVersion(migs, current)
			if i < 0 {
				// A database ahead of this build (or edited by hand): upstream
				// refused it the same way rather than guess.
				return fmt.Errorf("no migration found for version %d", current)
			}
			next = i + 1
		}
		if next >= len(migs) {
			return ErrNoChange
		}
		for _, m := range migs[next:] {
			if err := r.setVersion(ctx, m.Version, true); err != nil {
				return err
			}
			if strings.TrimSpace(m.Up) != "" {
				if _, err := r.conn.ExecContext(ctx, m.Up); err != nil {
					return migrationError(m, err)
				}
			}
			if err := r.setVersion(ctx, m.Version, false); err != nil {
				return err
			}
		}
		return nil
	})
}

// migrationError names the failing file and, for a Postgres error, its
// message, line and column — not the migration's SQL, which golang-migrate
// appended whole.
func migrationError(m schedMigration, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		msg := pgErr.Message
		if line, col, ok := lineColumn(m.Up, int(pgErr.Position)); ok && pgErr.Position > 0 {
			msg = fmt.Sprintf("%s (line %d, column %d)", msg, line, col)
		}
		if pgErr.Detail != "" {
			msg += ", " + pgErr.Detail
		}
		return fmt.Errorf("migration %s failed: %s: %w", m.Name, msg, err)
	}
	return fmt.Errorf("migration %s failed: %w", m.Name, err)
}

// lineColumn converts Postgres's 1-based character position in s to a line and
// column.
func lineColumn(s string, pos int) (line, col int, ok bool) {
	runes := []rune(strings.ReplaceAll(s, "\r\n", "\n"))
	if pos <= 0 || pos > len(runes) {
		return 0, 0, false
	}
	line, col = 1, 1
	for _, r := range runes[:pos-1] {
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col, true
}

// acquireLock takes the session-level advisory lock within
// migrationLockTimeout. It polls pg_try_advisory_lock rather than blocking in
// pg_advisory_lock under a deadline: cancelling a blocked lock call races the
// grant, and a lock granted to a connection that then goes back to the pool
// would wedge every later boot. Same key, same exclusion as golang-migrate.
func acquireLock(ctx context.Context, conn *sql.Conn, lockID int64) error {
	deadline := time.Now().Add(migrationLockTimeout)
	for {
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&got); err != nil {
			return fmt.Errorf("try lock failed: %w", err)
		}
		if got {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("timeout: can't acquire database lock")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("try lock failed: %w", ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// lockPollInterval is how often acquireLock retries a held lock.
const lockPollInterval = 100 * time.Millisecond

// migrationRun is one locked session on a dedicated connection.
type migrationRun struct {
	conn  *sql.Conn
	table string // quoted schema.table
}

// withMigrationLock takes a dedicated connection, ensures the tracking table
// exists, and runs fn holding the advisory lock on that connection.
func withMigrationLock(ctx context.Context, db *sql.DB, fn func(*migrationRun) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var dbName, schema sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT CURRENT_DATABASE(), CURRENT_SCHEMA()`).Scan(&dbName, &schema); err != nil {
		return fmt.Errorf("resolve database and schema: %w", err)
	}
	if dbName.String == "" {
		return errors.New("no database name")
	}
	if schema.String == "" {
		return errors.New("no schema")
	}
	lockID := advisoryLockID(dbName.String, schema.String, migrationsTable)

	if err := acquireLock(ctx, conn, lockID); err != nil {
		return err
	}
	defer func() {
		// A background context: the unlock must run even when ctx is done.
		// Closing the connection would release the lock anyway.
		if _, uerr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unlock: %w", uerr))
		}
	}()

	r := &migrationRun{conn: conn, table: quoteIdentifier(schema.String) + "." + quoteIdentifier(migrationsTable)}
	if err := r.ensureVersionTable(ctx, schema.String); err != nil {
		return err
	}
	return fn(r)
}

// ensureVersionTable is golang-migrate's: probe information_schema first, so a
// role without CREATE can still read a table that exists.
func (r *migrationRun) ensureVersionTable(ctx context.Context, schema string) error {
	var count int
	if err := r.conn.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2 LIMIT 1`,
		schema, migrationsTable).Scan(&count); err != nil {
		return fmt.Errorf("probe %s: %w", migrationsTable, err)
	}
	if count == 1 {
		return nil
	}
	if _, err := r.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+r.table+` (version bigint not null primary key, dirty boolean not null)`); err != nil {
		return fmt.Errorf("create %s: %w", migrationsTable, err)
	}
	return nil
}

// version reads the tracking row; no row is nilVersion.
func (r *migrationRun) version(ctx context.Context) (int, bool, error) {
	var v int64
	var dirty bool
	err := r.conn.QueryRowContext(ctx, `SELECT version, dirty FROM `+r.table+` LIMIT 1`).Scan(&v, &dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nilVersion, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read %s: %w", migrationsTable, err)
	}
	return int(v), dirty, nil
}

// setVersion replaces the tracking row in one transaction (TRUNCATE + INSERT),
// as golang-migrate's SetVersion does; nilVersion clean leaves it empty.
func (r *migrationRun) setVersion(ctx context.Context, version int, dirty bool) (err error) {
	tx, err := r.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set version: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `TRUNCATE `+r.table); err != nil {
		return fmt.Errorf("set version: truncate: %w", err)
	}
	if version >= 0 || (version == nilVersion && dirty) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+r.table+` (version, dirty) VALUES ($1, $2)`, version, dirty); err != nil { //nolint:gosec // G202: r.table is quoteIdentifier'd current_schema() and a constant; the values are bind parameters
			return fmt.Errorf("set version: insert: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("set version: commit: %w", err)
	}
	return nil
}

// quoteIdentifier is golang-migrate's (itself lib/pq's): cut at NUL, double
// embedded quotes, wrap in quotes.
func quoteIdentifier(name string) string {
	if end := strings.IndexRune(name, 0); end > -1 {
		name = name[:end]
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// MigrationInfo is one migration in a MigrationReport.
type MigrationInfo struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}

// MigrationReport is the read-only applied-vs-pending view of the orchestrator
// (sched) DB's migrations, served at GET /admin/migrations (#256) and printed by
// `fleet migrate status`. The runner (golang-migrate's format) advances the
// schema linearly and tracks only a single {version, dirty} row, so CurrentVersion is that row's version
// (nil when nothing has been applied) and Applied is every embedded migration at
// or below it. Dirty is true when a prior migration failed mid-flight.
type MigrationReport struct {
	DB             string          `json:"db"`              // always "sched"
	Runner         string          `json:"runner"`          // always "golang-migrate": the tracking format, kept for consumers
	MigrationTable string          `json:"migration_table"` // always "schema_migrations"
	CurrentVersion *int            `json:"current_version"`
	Dirty          bool            `json:"dirty"`
	Applied        []MigrationInfo `json:"applied"`
	Pending        []MigrationInfo `json:"pending"`
}

// schedMigrationFilePattern matches up-migration files
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
// strictly READ-ONLY: it reads the schema_migrations tracking row directly
// (guarded by to_regclass) rather than through the runner, which would CREATE
// that table as a side effect — so it applies
// nothing and creates nothing, and is safe to call against a fresh database
// (every migration is then reported pending). The context bounds both queries so
// a hung DB (or a disconnected HTTP client) can't block indefinitely.
func MigrationStatus(ctx context.Context, conn *sql.DB) (MigrationReport, error) {
	report := MigrationReport{DB: "sched", Runner: "golang-migrate", MigrationTable: "schema_migrations"}
	available, err := availableMigrations()
	if err != nil {
		return report, err
	}

	var tbl sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations')`).Scan(&tbl); err != nil {
		return report, fmt.Errorf("probe schema_migrations: %w", err)
	}
	var current *int
	if tbl.Valid {
		var v int64
		var dirty bool
		switch err := conn.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&v, &dirty); {
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
