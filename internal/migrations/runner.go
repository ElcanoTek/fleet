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

	"github.com/ElcanoTek/fleet/internal/dbschema"
	"github.com/ElcanoTek/fleet/internal/logging"
)

// Table is the one migration ledger.
const Table = "core.schema_versions"

// createTableSQL is the ledger. A row is written in the same transaction as
// the migration it records, so a failed migration records nothing; dirty is
// for a future migration that cannot run in one transaction (ADR-0107) and
// for an operator who marks a row by hand.
const createTableSQL = `CREATE TABLE core.schema_versions (
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
	// beforeAdoptCommit, when set, runs inside the adoption transaction just
	// before it commits (a test makes adoption fail there).
	beforeAdoptCommit func(context.Context, *sql.Tx) error
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
		if _, err := r.adopt(ctx, conn); err != nil {
			return err
		}
		// Below the boundary the legacy ledgers are authoritative (a pre-M6
		// release may have run since): bring the one ledger to them first.
		if err := r.reconcileLegacy(ctx, conn); err != nil {
			return err
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
		// Whenever the ledger has crossed FirstNew the sentinels must be there
		// (whoever wrote the ledger: a restore, an older force); idempotent.
		return r.ensureSentinels(ctx, conn)
	})
}

// ensureSentinels writes the legacy ledgers' sentinels when the ledger holds a
// version at or past FirstNew. The caller holds both legacy locks.
func (r *runner) ensureSentinels(ctx context.Context, conn *sql.Conn) (err error) {
	var crossed bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core.schema_versions WHERE version >= $1)`, FirstNew).Scan(&crossed); err != nil || !crossed {
		return err
	}
	if ok, err := sentinelsPresent(ctx, conn); err != nil || ok {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = writeSentinels(ctx, tx); err != nil {
		return fmt.Errorf("sentinels: %w", err)
	}
	return tx.Commit()
}

// sentinelsPresent reports whether both legacy ledgers carry their sentinel.
func sentinelsPresent(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT to_regclass('chat.schema_migrations') IS NOT NULL AND to_regclass('sched.schema_migrations') IS NOT NULL`).Scan(&ok)
	if err != nil || !ok {
		return false, err
	}
	err = q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM chat.schema_migrations WHERE version = $1)
		AND coalesce((SELECT max(version) FROM sched.schema_migrations), -1) = $2`, ChatSentinel, SchedSentinel).Scan(&ok)
	return ok, err
}

// current is the fast path: one read of the ledger and one of the legacy
// ledgers (below the boundary) or the sentinels (past it), no lock. Any error (the
// ledger does not exist yet, a role that cannot read it) is not current, and
// the locked path then does the work or reports the error properly.
func (r *runner) current(ctx context.Context, db *sql.DB) bool {
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum, dirty FROM core.schema_versions`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	seen, crossed := 0, false
	legacy := map[int]bool{}
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(&row.Version, &row.Name, &row.Checksum, &row.Dirty); err != nil {
			return false
		}
		m, known := r.set.Get(row.Version)
		if row.Dirty || !known || !matches(m, row) {
			return false
		}
		crossed = crossed || row.Version >= FirstNew
		if row.Version < FirstNew {
			legacy[row.Version] = true
		}
		seen++
	}
	if rows.Err() != nil || seen != len(r.set.list) {
		return false
	}
	if crossed {
		// Past the boundary the sentinels must be in place (one more read).
		ok, err := sentinelsPresent(ctx, db)
		return err == nil && ok
	}
	// Below it the legacy ledgers are authoritative: one more read, which
	// notices what a pre-M6 release applied or rolled back since.
	return r.legacyCurrent(ctx, db, legacy)
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
// ledger row (and, for a legacy one, its old ledger's bookkeeping; for the
// first new one, the sentinels). On any error the transaction rolls back and
// nothing is recorded.
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
	if err = mirrorLegacy(ctx, tx, m); err != nil {
		return fmt.Errorf("migration %d (%s): legacy ledger: %w", m.Version, m.Name, err)
	}
	if m.Version >= FirstNew {
		if err = writeSentinels(ctx, tx); err != nil {
			return fmt.Errorf("migration %d (%s): sentinels: %w", m.Version, m.Name, err)
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

// mirrorLegacy keeps the old ledger of a legacy migration's store in step
// when that ledger exists (a database adopted from a pre-M6 release), so the
// box can still go back to that release with the rollback in
// docs/MIGRATIONS.md until a migration >= FirstNew has run. A database that
// never had the old ledgers (a fresh install) gets none.
func mirrorLegacy(ctx context.Context, tx *sql.Tx, m Migration) error {
	switch m.Stream {
	case StreamChat:
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass('chat.schema_migrations') IS NOT NULL`).Scan(&ok); err != nil || !ok {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO chat.schema_migrations (version, name, applied_at)
			VALUES ($1, $2, extract(epoch FROM now())::bigint) ON CONFLICT (version) DO NOTHING`, m.LegacyVersion(), m.LegacyName())
		return err
	case StreamSched:
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass('sched.schema_migrations') IS NOT NULL`).Scan(&ok); err != nil || !ok {
			return err
		}
		var at sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM sched.schema_migrations`).Scan(&at); err != nil {
			return err
		}
		if at.Valid && at.Int64 == SchedSentinel {
			return nil // never step back below the sentinel
		}
		if _, err := tx.ExecContext(ctx, `TRUNCATE sched.schema_migrations`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO sched.schema_migrations (version, dirty) VALUES ($1, false)`, m.LegacyVersion())
		return err
	}
	return nil
}

// writeSentinels marks both legacy ledgers so a pre-M6 binary refuses the
// database once it holds a migration that binary cannot know about. The
// ledgers are created (in their old shapes) when missing, so a database
// installed fresh by an M6 release refuses an old binary too. Idempotent.
func writeSentinels(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS chat.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sched.schema_migrations (version bigint not null primary key, dirty boolean not null)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat.schema_migrations (version, name, applied_at)
		VALUES ($1, $2, extract(epoch FROM now())::bigint) ON CONFLICT (version) DO NOTHING`, ChatSentinel, ChatSentinelName); err != nil {
		return err
	}
	var at sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM sched.schema_migrations`).Scan(&at); err != nil {
		return err
	}
	if at.Valid && at.Int64 == SchedSentinel {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `TRUNCATE sched.schema_migrations`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sched.schema_migrations (version, dirty) VALUES ($1, false)`, SchedSentinel)
	return err
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
