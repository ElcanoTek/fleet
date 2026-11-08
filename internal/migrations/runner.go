package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver (UpDSN)

	"github.com/ElcanoTek/fleet/internal/dbschema"
	"github.com/ElcanoTek/fleet/internal/logging"
)

// Table is the one migration ledger.
const Table = "core.schema_versions"

// createTableSQL is the ledger. A row is written in the same transaction as
// the migration it records, so a failed migration records nothing; dirty is
// for a future migration that cannot run in one transaction (ADR-0107) and
// for an operator who marks a row by hand.
const createTableSQL = `CREATE TABLE IF NOT EXISTS core.schema_versions (
	version    bigint PRIMARY KEY,
	name       text NOT NULL,
	checksum   text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now(),
	dirty      boolean NOT NULL DEFAULT false
)`

// defaultLockTimeout bounds how long a run waits for each lock. Generous: a
// slow migration on a large database legitimately holds the lock for
// minutes, but a boot that never returns (a wedged peer, a leaked session)
// must fail loudly instead of hanging.
const defaultLockTimeout = 5 * time.Minute

// runner applies one Set. Tests build their own over a test set and hooks.
type runner struct {
	set         *Set
	lockTimeout time.Duration
}

func newRunner(set *Set) *runner { return &runner{set: set, lockTimeout: defaultLockTimeout} }

func embeddedRunner() (*runner, error) {
	set, err := Embedded()
	if err != nil {
		return nil, err
	}
	return newRunner(set), nil
}

// Up brings the database db reaches to this build's latest version. Any pool
// on the database will do: the runner names every schema itself, whatever
// the pool's search_path. Both stores call it on open (store.Open,
// scheddb.Init), as do the admin verbs that open one.
//
// When the database is current it is one read of the ledger and nothing
// else. Otherwise, on one dedicated connection holding the migration lock
// (and both legacy runners' locks, so a pre-M6 binary booting at the same
// time waits), it
//
//  1. creates the schemas chat, core and sched that are missing, after the
//     readiness check (dbschema.Ensure);
//  2. adopts the legacy ledgers into core.schema_versions the first time
//     (Adopt, below), which a fresh database goes through too;
//  3. refuses a dirty row, or a version this build does not ship (a newer
//     fleet migrated the database: no downgrades);
//  4. applies every version the ledger lacks, ascending, each in its own
//     transaction together with its ledger row.
func Up(ctx context.Context, db *sql.DB) error {
	r, err := embeddedRunner()
	if err != nil {
		return err
	}
	return r.up(ctx, db)
}

// UpDSN opens a small pool on dsn, runs Up and closes it — for callers that
// hold a DSN rather than a pool (test template builders).
func UpDSN(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(2)
	return Up(ctx, db)
}

func (r *runner) up(ctx context.Context, db *sql.DB) error {
	if r.current(ctx, db) {
		return nil
	}
	return r.locked(ctx, db, true, func(conn *sql.Conn) error {
		if err := dbschema.Ensure(ctx, conn); err != nil {
			return err
		}
		if err := refuseUnadoptedPreM6(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, createTableSQL); err != nil {
			return fmt.Errorf("create %s: %w", Table, err)
		}
		ledger, err := readLedger(ctx, conn)
		if err != nil {
			return err
		}
		if err := r.checkLedger(ledger); err != nil {
			return err
		}
		applied := 0
		for _, m := range r.set.list {
			if _, ok := ledger[m.Version]; ok {
				continue
			}
			if err := r.apply(ctx, conn, m); err != nil {
				return err
			}
			applied++
		}
		if applied > 0 {
			logging.From(ctx).Info("migrated the database", "applied", applied, "version", r.set.Latest())
		}
		return nil
	})
}

// current is the fast path: one read of the ledger, no lock. Any error (the
// ledger does not exist yet, a role that cannot read it) is not current, and
// the locked path then does the work or reports the error properly.
func (r *runner) current(ctx context.Context, db *sql.DB) bool {
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum, dirty FROM core.schema_versions`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(&row.Version, &row.Name, &row.Checksum, &row.Dirty); err != nil {
			return false
		}
		m, known := r.set.Get(row.Version)
		if row.Dirty || !known || !matches(m, row) {
			return false
		}
		seen++
	}
	return rows.Err() == nil && seen == len(r.set.list)
}

// matches reports whether a ledger row records exactly migration m: the same
// name and the checksum of the same .up.sql bytes. A shipped migration is
// append-only; one edited after it ran would leave every database that ran
// it on the old SQL and every fresh install on the new.
func matches(m Migration, row ledgerRow) bool {
	return row.Name == m.Name && row.Checksum == m.Checksum
}

// ChecksumError is the refusal of ledger rows that do not record this
// build's migration of the same version: the file was edited (or renamed)
// after the database ran it.
type ChecksumError struct{ Rows []ChecksumMismatch }

// ChecksumMismatch is one row ChecksumError names.
type ChecksumMismatch struct {
	Version               int
	LedgerName, BuildName string
	LedgerSum, BuildSum   string
}

func (e ChecksumError) Error() string {
	parts := make([]string, 0, len(e.Rows))
	for _, r := range e.Rows {
		parts = append(parts, fmt.Sprintf("%d (ledger %s %.12s, build %s %.12s)", r.Version, r.LedgerName, r.LedgerSum, r.BuildName, r.BuildSum))
	}
	return fmt.Sprintf("%s records version(s) %s with a different name or checksum than this build ships: a shipped "+
		"migration was edited after this database ran it, so its schema is not what the file now says. Shipped migrations "+
		"are append-only — restore the original file and add a new migration instead. If the database has been brought "+
		"to what the edited file does by hand, record that with `fleet migrate force <version>` (docs/MIGRATIONS.md)",
		Table, strings.Join(parts, ", "))
}

// ledgerRow is one row of core.schema_versions.
type ledgerRow struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
	Dirty     bool
}

// querier is what the read helpers need: *sql.DB, *sql.Conn and *sql.Tx.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ledgerExists reports whether core.schema_versions exists.
func ledgerExists(ctx context.Context, q querier) (bool, error) {
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('core.schema_versions') IS NOT NULL`).Scan(&ok); err != nil {
		return false, fmt.Errorf("probe %s: %w", Table, err)
	}
	return ok, nil
}

// refuseUnadoptedPreM6 refuses a populated pre-M6 database that has no
// core.schema_versions yet, so a build after M6's adoption code was removed
// never creates an empty ledger and replays migration 1 over existing tables.
func refuseUnadoptedPreM6(ctx context.Context, q querier) error {
	var unadopted bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('core.schema_versions') IS NULL AND (
		to_regclass('chat.schema_migrations') IS NOT NULL OR
		to_regclass('sched.schema_migrations') IS NOT NULL OR
		to_regclass('chat.conversations') IS NOT NULL OR
		to_regclass('sched.tasks') IS NOT NULL
	)`).Scan(&unadopted); err != nil {
		return fmt.Errorf("probe pre-M6 schema state: %w", err)
	}
	if unadopted {
		return fmt.Errorf("%s is absent on a populated pre-M6 database: upgrade through the M6 release (v2026.11.08.12) first so it adopts the legacy ledgers into %s", Table, Table)
	}
	return nil
}

// readLedger reads every row of core.schema_versions.
func readLedger(ctx context.Context, q querier) (map[int]ledgerRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT version, name, checksum, applied_at, dirty FROM core.schema_versions`)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", Table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]ledgerRow{}
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(&row.Version, &row.Name, &row.Checksum, &row.AppliedAt, &row.Dirty); err != nil {
			return nil, fmt.Errorf("read %s: %w", Table, err)
		}
		out[row.Version] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", Table, err)
	}
	return out, nil
}

// DirtyError is the refusal of a ledger with a dirty row.
type DirtyError struct{ Versions []int }

func (e DirtyError) Error() string {
	return fmt.Sprintf("%s has dirty version(s) %s: a migration was left half-applied, so fleet refuses to start. "+
		"Inspect the database, repair or undo what the migration did, then record the last version that is "+
		"really applied with `fleet migrate force <version>` and restart (docs/MIGRATIONS.md)", Table, joinInts(e.Versions))
}

// UnknownVersionError is the refusal of a ledger holding versions this build
// does not ship: a newer fleet migrated the database.
type UnknownVersionError struct {
	Versions []int
	Latest   int
}

func (e UnknownVersionError) Error() string {
	return fmt.Sprintf("the database has migration version(s) %s that this build does not ship (it knows up to %d): "+
		"a newer fleet migrated it. Refusing to downgrade — run the newer release, or restore a backup taken "+
		"before it (docs/MIGRATIONS.md)", joinInts(e.Versions), e.Latest)
}

// checkLedger refuses dirty rows and versions the set does not have.
func (r *runner) checkLedger(ledger map[int]ledgerRow) error {
	var dirty, unknown []int
	for v, row := range ledger {
		if row.Dirty {
			dirty = append(dirty, v)
		}
		if _, ok := r.set.byVersion[v]; !ok {
			unknown = append(unknown, v)
		}
	}
	if len(unknown) > 0 {
		sort.Ints(unknown)
		return UnknownVersionError{Versions: unknown, Latest: r.set.Latest()}
	}
	var changed ChecksumError
	for v, row := range ledger {
		if m, _ := r.set.Get(v); !matches(m, row) {
			changed.Rows = append(changed.Rows, ChecksumMismatch{Version: v, LedgerName: row.Name, BuildName: m.Name, LedgerSum: row.Checksum, BuildSum: m.Checksum})
		}
	}
	if len(changed.Rows) > 0 {
		sort.Slice(changed.Rows, func(i, j int) bool { return changed.Rows[i].Version < changed.Rows[j].Version })
		return changed
	}
	if len(dirty) > 0 {
		sort.Ints(dirty)
		return DirtyError{Versions: dirty}
	}
	return nil
}

// apply runs one migration in a transaction on conn together with its
// ledger row. On any error the transaction rolls back and nothing is recorded.
func (r *runner) apply(ctx context.Context, conn *sql.Conn, m Migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %d (%s): begin: %w", m.Version, m.Name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// SearchPath is one of three constant identifiers.
	if _, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+m.SearchPath()); err != nil {
		return fmt.Errorf("migration %d (%s): set search_path: %w", m.Version, m.Name, err)
	}
	if strings.TrimSpace(m.Up) != "" {
		if _, err = tx.ExecContext(ctx, m.Up); err != nil {
			return migrationError(m, m.Up, err)
		}
	}
	if m.Version == LegacyLatest {
		// M4 renamed sched.users (created by legacy sched/001 and altered by
		// sched/072) to sched.users_premerge_m4 on every upgraded database; do
		// the same at the end of the legacy sequence so a fresh install and an
		// upgraded database end with identical schemas without editing frozen
		// legacy migration files.
		if _, err = tx.ExecContext(ctx, `DO $$ BEGIN IF to_regclass('sched.users') IS NOT NULL AND to_regclass('sched.users_premerge_m4') IS NULL THEN ALTER TABLE sched.users RENAME TO users_premerge_m4; END IF; END $$`); err != nil {
			return fmt.Errorf("migration %d (%s): rename legacy sched.users: %w", m.Version, m.Name, err)
		}
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO core.schema_versions (version, name, checksum, applied_at, dirty) VALUES ($1, $2, $3, now(), false)`,
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("migration %d (%s): record: %w", m.Version, m.Name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migration %d (%s): commit: %w", m.Version, m.Name, err)
	}
	logging.From(ctx).Debug("applied migration", "version", m.Version, "name", m.Name)
	return nil
}

// migrationError names the failing migration and, for a Postgres error, its
// message, line and column — not the migration's SQL.
func migrationError(m Migration, body string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		msg := pgErr.Message
		if line, col, ok := lineColumn(body, int(pgErr.Position)); ok && pgErr.Position > 0 {
			msg = fmt.Sprintf("%s (line %d, column %d)", msg, line, col)
		}
		if pgErr.Detail != "" {
			msg += ", " + pgErr.Detail
		}
		return fmt.Errorf("migration %d (%s) failed: %s: %w", m.Version, m.Name, msg, err)
	}
	return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Name, err)
}

// lineColumn converts Postgres's 1-based character position in s to a line
// and column.
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

func joinInts(vs []int) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}

// UpTo is Up stopped after version last: it applies the pending versions up
// to and including last and nothing above. For tests and upgrade rehearsals
// that need a database as an older release left it; fleet itself always runs
// Up.
func UpTo(ctx context.Context, db *sql.DB, last int) error {
	set, err := Embedded()
	if err != nil {
		return err
	}
	sub, err := set.through(last)
	if err != nil {
		return err
	}
	return newRunner(sub).up(ctx, db)
}

// through is the set's prefix up to and including version last.
func (s *Set) through(last int) (*Set, error) {
	if _, ok := s.byVersion[last]; !ok {
		return nil, fmt.Errorf("version %d is not a migration of this set", last)
	}
	out := &Set{byVersion: map[int]int{}, fsys: s.fsys}
	for _, m := range s.list {
		if m.Version > last {
			break
		}
		out.byVersion[m.Version] = len(out.list)
		out.list = append(out.list, m)
	}
	return out, nil
}
