package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// The legacy ledgers below the boundary (ADR-0107).
//
// Until the database crosses FirstNew — no version >= 200 in
// core.schema_versions and no sentinel in either legacy ledger — a pre-M6
// release may still run against it (a rollback needs nothing else), and it
// reads and writes only chat.schema_migrations and sched.schema_migrations.
// So below the boundary those legacy ledgers are AUTHORITATIVE for the legacy
// versions, and core.schema_versions is reconciled to them on every Up (under
// both legacy locks): versions a pre-M6 binary applied are recorded, versions
// it rolled back are dropped (and applied again). Both legacy ledgers are
// kept present below the boundary — created from the one ledger when missing
// (a fresh install, a database restored without them) — and every legacy
// version the runner applies is recorded in them in its own transaction.
//
// Past the boundary the sentinels freeze the legacy ledgers and
// core.schema_versions alone is authoritative.

// isFrozen reports whether a legacy state carries a sentinel.
func isFrozen(st legacyState) bool {
	if _, ok := st.Chat[ChatSentinel]; ok {
		return true
	}
	return st.SchedVersion == SchedSentinel
}

// crossedLedger reports whether core.schema_versions holds a version at or
// past FirstNew.
func crossedLedger(ctx context.Context, q querier) (bool, error) {
	var crossed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core.schema_versions WHERE version >= $1)`, FirstNew).Scan(&crossed)
	return crossed, err
}

// Crossed reports whether the database is past the boundary: the one ledger
// holds a version >= FirstNew, or a legacy ledger carries a sentinel. A
// database without the one ledger is judged by the sentinels alone.
func Crossed(ctx context.Context, db *sql.DB) (bool, error) {
	exists, err := ledgerExists(ctx, db)
	if err != nil {
		return false, err
	}
	if exists {
		if c, err := crossedLedger(ctx, db); err != nil || c {
			return c, err
		}
	}
	st, err := readLegacy(ctx, db)
	if err != nil {
		return false, err
	}
	return isFrozen(st), nil
}

// reconcileLegacy brings core.schema_versions' legacy rows to what the legacy
// ledgers say, below the boundary, in one transaction. The caller holds the
// migration lock and both legacy locks. It refuses (AdoptionError, nothing
// changed) a legacy state this build cannot continue from: a dirty sched row,
// a version it does not ship.
func (r *runner) reconcileLegacy(ctx context.Context, conn *sql.Conn) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if crossed, err := crossedLedger(ctx, tx); err != nil || crossed {
		_ = tx.Rollback()
		return err
	}
	ledger, err := readLedger(ctx, tx)
	if err != nil {
		return err
	}
	st, err := readLegacy(ctx, tx)
	if err != nil {
		return err
	}
	if isFrozen(st) {
		// Past the boundary once (then `fleet migrate down` below it): the
		// legacy ledgers stay frozen and the one ledger is authoritative.
		return tx.Rollback()
	}
	if !st.ChatTable || !st.SchedTable {
		if err = r.writeLegacyLedgers(ctx, tx, ledger, !st.ChatTable, !st.SchedTable); err != nil {
			return fmt.Errorf("create the legacy ledgers: %w", err)
		}
		if st, err = readLegacy(ctx, tx); err != nil {
			return err
		}
	}
	want, err := r.plan(st)
	if err != nil {
		return err
	}
	wantSet := map[int]adoptionRow{}
	for _, row := range want {
		wantSet[row.Version] = row
	}
	var added, dropped []int
	for v := range ledger {
		if v >= FirstNew {
			continue
		}
		if _, ok := wantSet[v]; !ok {
			if _, err = tx.ExecContext(ctx, `DELETE FROM core.schema_versions WHERE version = $1`, v); err != nil {
				return err
			}
			dropped = append(dropped, v)
		}
	}
	for v, row := range wantSet {
		if _, ok := ledger[v]; ok {
			continue
		}
		var at any
		if row.AppliedAt != nil {
			at = *row.AppliedAt
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO core.schema_versions (version, name, checksum, applied_at, dirty)
			VALUES ($1, $2, $3, coalesce($4::timestamptz, now()), false)`, v, row.Name, row.Checksum, at); err != nil {
			return err
		}
		added = append(added, v)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(added)+len(dropped) > 0 {
		sort.Ints(added)
		sort.Ints(dropped)
		logging.From(ctx).Info("reconciled "+Table+" with the legacy ledgers (a release before the one runner ran on this database)",
			"recorded", joinInts(added), "dropped", joinInts(dropped))
	}
	return nil
}

// writeLegacyLedgers (re)creates the chat and/or sched legacy ledger from the
// one ledger's legacy rows, in their old shapes: chat one row per version
// (version, name, applied_at), sched golang-migrate's single row (the highest
// applied sched version, clean).
func (r *runner) writeLegacyLedgers(ctx context.Context, tx *sql.Tx, ledger map[int]ledgerRow, chat, sched bool) error {
	if chat {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS chat.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat.schema_migrations`); err != nil {
			return err
		}
		for v, row := range ledger {
			m, ok := r.set.Get(v)
			if !ok || m.Stream != StreamChat {
				continue
			}
			at := row.AppliedAt
			if at.IsZero() {
				at = time.Now()
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat.schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
				m.LegacyVersion(), m.LegacyName(), at.Unix()); err != nil {
				return err
			}
		}
	}
	if sched {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sched.schema_migrations (version bigint not null primary key, dirty boolean not null)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `TRUNCATE sched.schema_migrations`); err != nil {
			return err
		}
		top := -1
		for v := range ledger {
			if m, ok := r.set.Get(v); ok && m.Stream == StreamSched {
				top = max(top, m.LegacyVersion())
			}
		}
		if top >= 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sched.schema_migrations (version, dirty) VALUES ($1, false)`, top); err != nil {
				return err
			}
		}
	}
	return nil
}

// legacyCurrent is the fast path's check below the boundary: one read of both
// legacy ledgers, which must exist and describe exactly the legacy rows of
// the one ledger (versions are compared; names and checksums are this build's
// for every legacy row the fast path accepted). ok is false when anything
// differs or cannot be read; the locked path then reconciles or refuses.
func (r *runner) legacyCurrent(ctx context.Context, db *sql.DB, ledgerLegacy map[int]bool) bool {
	rows, err := db.QueryContext(ctx, `SELECT 'chat', version, false FROM chat.schema_migrations
		UNION ALL SELECT 'sched', version, dirty FROM sched.schema_migrations`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	st := legacyState{ChatTable: true, SchedTable: true, Chat: map[int]legacyChatRow{}, SchedVersion: -1}
	for rows.Next() {
		var stream string
		var v int
		var dirty bool
		if err := rows.Scan(&stream, &v, &dirty); err != nil {
			return false
		}
		if stream == StreamChat {
			st.Chat[v] = legacyChatRow{}
		} else {
			st.SchedRows++
			st.SchedVersion, st.SchedDirty = v, dirty
		}
	}
	if rows.Err() != nil {
		return false
	}
	if isFrozen(st) {
		return true
	}
	want, err := r.plan(st)
	if err != nil || len(want) != len(ledgerLegacy) {
		return false
	}
	for _, row := range want {
		if !ledgerLegacy[row.Version] {
			return false
		}
	}
	return true
}
