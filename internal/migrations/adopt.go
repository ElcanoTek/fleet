package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver (UpDSN)
)

// legacyState is what the two pre-M6 ledgers record.
type legacyState struct {
	// chat.schema_migrations: version → (name, applied_at unix seconds).
	ChatTable bool
	Chat      map[int]legacyChatRow
	// sched.schema_migrations: golang-migrate's single {version, dirty} row.
	SchedTable   bool
	SchedVersion int // -1: no row (nothing applied)
	SchedDirty   bool
	SchedRows    int
}

type legacyChatRow struct {
	Name      string
	AppliedAt int64
}

// readLegacy reads both legacy ledgers; a missing one reads as empty.
func readLegacy(ctx context.Context, q querier) (legacyState, error) {
	st := legacyState{Chat: map[int]legacyChatRow{}, SchedVersion: -1}
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('chat.schema_migrations') IS NOT NULL,
		to_regclass('sched.schema_migrations') IS NOT NULL`).Scan(&st.ChatTable, &st.SchedTable); err != nil {
		return st, fmt.Errorf("probe the legacy ledgers: %w", err)
	}
	if st.ChatTable {
		rows, err := q.QueryContext(ctx, `SELECT version, name, applied_at FROM chat.schema_migrations`)
		if err != nil {
			return st, fmt.Errorf("read chat.schema_migrations: %w", err)
		}
		for rows.Next() {
			var v int
			var row legacyChatRow
			if err := rows.Scan(&v, &row.Name, &row.AppliedAt); err != nil {
				_ = rows.Close()
				return st, fmt.Errorf("read chat.schema_migrations: %w", err)
			}
			st.Chat[v] = row
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return st, fmt.Errorf("read chat.schema_migrations: %w", err)
		}
	}
	if st.SchedTable {
		rows, err := q.QueryContext(ctx, `SELECT version, dirty FROM sched.schema_migrations`)
		if err != nil {
			return st, fmt.Errorf("read sched.schema_migrations: %w", err)
		}
		for rows.Next() {
			var v int
			var dirty bool
			if err := rows.Scan(&v, &dirty); err != nil {
				_ = rows.Close()
				return st, fmt.Errorf("read sched.schema_migrations: %w", err)
			}
			st.SchedRows++
			st.SchedVersion, st.SchedDirty = v, dirty
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return st, fmt.Errorf("read sched.schema_migrations: %w", err)
		}
	}
	return st, nil
}

// AdoptionError is the refusal to adopt legacy ledgers this build cannot
// continue from. Nothing has been changed when it is returned.
type AdoptionError struct{ Problem string }

func (e AdoptionError) Error() string {
	return "cannot move this database to the one migration ledger (" + Table + "): " + e.Problem +
		". Nothing was changed. See docs/MIGRATIONS.md, \"Adoption\""
}

// adoptionRow is one row adoption writes.
type adoptionRow struct {
	Migration
	AppliedAt *time.Time // nil: now()
}

// plan validates the legacy ledgers against the set and returns the rows
// adoption writes, ascending. It changes nothing.
func (r *runner) plan(st legacyState) ([]adoptionRow, error) {
	if st.SchedRows > 1 {
		return nil, AdoptionError{fmt.Sprintf("sched.schema_migrations holds %d rows, but the previous runner kept exactly one", st.SchedRows)}
	}
	if st.SchedDirty {
		return nil, AdoptionError{fmt.Sprintf("sched.schema_migrations is DIRTY at version %d: a scheduler migration failed "+
			"mid-run under the previous release. Repair it with the previous release (inspect the database, then "+
			"`fleet migrate force <last good version>`), or by hand: UPDATE sched.schema_migrations SET version = "+
			"<last good version>, dirty = false", st.SchedVersion)}
	}
	if st.SchedVersion != -1 {
		if st.SchedVersion < 0 || SchedOffset+st.SchedVersion >= FirstNew {
			return nil, AdoptionError{fmt.Sprintf("sched.schema_migrations is at version %d, outside the legacy range "+
				"0–%d this build ships: a newer fleet migrated this database", st.SchedVersion, r.legacySchedLatest())}
		}
		if _, ok := r.set.Get(SchedOffset + st.SchedVersion); !ok && st.SchedVersion != 0 {
			return nil, AdoptionError{fmt.Sprintf("sched.schema_migrations is at version %d, which this build does not ship "+
				"(it ships 1–%d): a newer fleet migrated this database", st.SchedVersion, r.legacySchedLatest())}
		}
	}
	var unknown []int
	for v := range st.Chat {
		m, ok := r.set.Get(v)
		if !ok || m.Stream != StreamChat {
			unknown = append(unknown, v)
		}
	}
	if len(unknown) > 0 {
		sort.Ints(unknown)
		return nil, AdoptionError{fmt.Sprintf("chat.schema_migrations holds version(s) %s that this build does not ship "+
			"(it ships 1–%d): a newer fleet migrated this database", joinInts(unknown), r.legacyChatLatest())}
	}

	var out []adoptionRow
	for v, row := range st.Chat {
		m, _ := r.set.Get(v)
		at := time.Unix(row.AppliedAt, 0).UTC()
		out = append(out, adoptionRow{Migration: m, AppliedAt: &at})
	}
	for _, m := range r.set.list {
		if m.Stream == StreamSched && m.LegacyVersion() <= st.SchedVersion {
			out = append(out, adoptionRow{Migration: m})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (r *runner) legacyChatLatest() int {
	latest := 0
	for _, m := range r.set.list {
		if m.Stream == StreamChat {
			latest = m.Version
		}
	}
	return latest
}

func (r *runner) legacySchedLatest() int {
	latest := 0
	for _, m := range r.set.list {
		if m.Stream == StreamSched {
			latest = m.LegacyVersion()
		}
	}
	return latest
}

// adopt moves the database onto core.schema_versions the first time any M6
// build runs on it, and is a no-op (one probe) after that. In one
// transaction, under the migration lock and both legacy locks (the caller
// holds them):
//
//  1. if core.schema_versions exists, nothing happens;
//  2. both legacy ledgers are read and checked before anything changes: a
//     dirty sched ledger, a version this build does not ship in either, or a
//     sched version outside 0–77 is refused with an operator message;
//  3. the ledger is created, chat N recorded as version N (keeping when it
//     was applied) and sched 1..v as 101..100+v.
//
// The legacy ledgers are left as they are: a box behind the latest legacy
// release then applies its backlog, in the merged order, as pending
// versions. A fresh database (no legacy ledgers) adopts an empty ledger.
// It reports whether it adopted.
func (r *runner) adopt(ctx context.Context, conn *sql.Conn) (adopted bool, err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("adopt: begin: %w", err)
	}
	defer func() {
		if err != nil || !adopted {
			_ = tx.Rollback()
		}
	}()
	exists, err := ledgerExists(ctx, tx)
	if err != nil || exists {
		return false, err
	}
	st, err := readLegacy(ctx, tx)
	if err != nil {
		return false, err
	}
	rows, err := r.plan(st)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, createTableSQL); err != nil {
		return false, fmt.Errorf("adopt: create %s: %w", Table, err)
	}
	for _, row := range rows {
		var at any
		if row.AppliedAt != nil {
			at = *row.AppliedAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO core.schema_versions (version, name, checksum, applied_at, dirty)
			VALUES ($1, $2, $3, coalesce($4::timestamptz, now()), false)`, row.Version, row.Name, row.Checksum, at); err != nil {
			return false, fmt.Errorf("adopt: record version %d: %w", row.Version, err)
		}
	}
	if r.beforeAdoptCommit != nil {
		if err := r.beforeAdoptCommit(ctx, tx); err != nil {
			return false, fmt.Errorf("adopt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("adopt: commit: %w", err)
	}
	adopted = true
	if st.ChatTable || st.SchedTable {
		slog.Info("adopted the legacy migration ledgers into "+Table,
			"chat_versions", len(st.Chat), "sched_version", st.SchedVersion, "recorded", len(rows))
	}
	return true, nil
}

// describeLegacy renders a legacy state for messages ("chat 1–99, sched 77").
func describeLegacy(st legacyState) string {
	var parts []string
	if !st.ChatTable {
		parts = append(parts, "chat: no ledger")
	} else {
		maxV := 0
		for v := range st.Chat {
			maxV = max(maxV, v)
		}
		parts = append(parts, fmt.Sprintf("chat: %d version(s), highest %d", len(st.Chat), maxV))
	}
	switch {
	case !st.SchedTable:
		parts = append(parts, "sched: no ledger")
	case st.SchedVersion == -1:
		parts = append(parts, "sched: nothing applied")
	case st.SchedDirty:
		parts = append(parts, fmt.Sprintf("sched: %d (DIRTY)", st.SchedVersion))
	default:
		parts = append(parts, fmt.Sprintf("sched: %d", st.SchedVersion))
	}
	return strings.Join(parts, ", ")
}
