package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// ErrNotAdopted is returned by Down and Force on a database that has no
// core.schema_versions yet: start this release once first (it adopts).
var ErrNotAdopted = errors.New(Table + " does not exist yet: start this fleet release once (it adopts the legacy ledgers), then retry")

// DownPlan lists the migrations Down undoes, newest first.
type DownPlan struct {
	To    int
	Steps []Migration
}

// Down undoes every applied migration above version to, newest first, each
// in its own transaction that runs the .down.sql and deletes its ledger row,
// under the migration lock. to may not be below LegacyLatest: the legacy
// migrations are not undone (the sched ones keep their .down.sql files as
// history; the chat ones never had any). With dryRun nothing changes and
// the plan is returned.
func Down(ctx context.Context, db *sql.DB, to int, dryRun bool) (DownPlan, error) {
	r, err := embeddedRunner()
	if err != nil {
		return DownPlan{To: to}, err
	}
	return r.down(ctx, db, to, dryRun)
}

func (r *runner) down(ctx context.Context, db *sql.DB, to int, dryRun bool) (plan DownPlan, err error) {
	plan.To = to
	if to < LegacyLatest {
		return plan, fmt.Errorf("refusing to go down to %d: the legacy migrations (up to %d) are not undone; the lowest target is %d", to, LegacyLatest, LegacyLatest)
	}
	if _, ok := r.set.Get(to); !ok {
		return plan, fmt.Errorf("version %d is not a migration this build ships", to)
	}
	err = r.locked(ctx, db, false, func(conn *sql.Conn) error {
		exists, err := ledgerExists(ctx, conn)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotAdopted
		}
		ledger, err := readLedger(ctx, conn)
		if err != nil {
			return err
		}
		if err := r.checkLedger(ledger); err != nil {
			return err
		}
		var versions []int
		for v := range ledger {
			if v > to {
				versions = append(versions, v)
			}
		}
		sort.Sort(sort.Reverse(sort.IntSlice(versions)))
		for _, v := range versions {
			m, _ := r.set.Get(v)
			if m.Down == "" {
				return fmt.Errorf("migration %d (%s) has no down migration", v, m.Name)
			}
			plan.Steps = append(plan.Steps, m)
		}
		if dryRun {
			return nil
		}
		for _, m := range plan.Steps {
			if err := r.undo(ctx, conn, m); err != nil {
				return err
			}
		}
		return nil
	})
	return plan, err
}

// undo runs one down migration in a transaction with its ledger row's delete.
func (r *runner) undo(ctx context.Context, conn *sql.Conn, m Migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("down %d (%s): begin: %w", m.Version, m.Name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+m.SearchPath()); err != nil {
		return fmt.Errorf("down %d (%s): set search_path: %w", m.Version, m.Name, err)
	}
	if _, err = tx.ExecContext(ctx, m.Down); err != nil {
		return migrationError(m, m.Down, fmt.Errorf("down: %w", err))
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM core.schema_versions WHERE version = $1`, m.Version); err != nil {
		return fmt.Errorf("down %d (%s): delete ledger row: %w", m.Version, m.Name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("down %d (%s): commit: %w", m.Version, m.Name, err)
	}
	logging.From(ctx).Info("reverted migration", "version", m.Version, "name", m.Name)
	return nil
}

// Force makes the ledger say exactly "every version this build ships up to
// version is applied, cleanly, as the files this build ships, and nothing
// above it", running no migration —
// the repair after a migration was left half-applied (a dirty row), once the
// operator has inspected and fixed the database by hand. version 0 records
// nothing applied. Rows above version are deleted, missing rows at or below
// it are added, and every row is marked clean.
func Force(ctx context.Context, db *sql.DB, version int) error {
	r, err := embeddedRunner()
	if err != nil {
		return err
	}
	return r.force(ctx, db, version)
}

func (r *runner) force(ctx context.Context, db *sql.DB, version int) error {
	if _, ok := r.set.Get(version); !ok && version != 0 {
		return fmt.Errorf("version %d is not a migration this build ships (use 0 for none)", version)
	}
	// Under both legacy locks too, like Up: a force at or past FirstNew writes
	// the legacy ledgers' sentinels.
	return r.locked(ctx, db, true, func(conn *sql.Conn) (err error) {
		exists, err := ledgerExists(ctx, conn)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotAdopted
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
		// Exactly the build's versions up to version: rows above it, and rows
		// at or below it this build does not ship, go.
		shipped := make([]int64, 0, len(r.set.list))
		for _, m := range r.set.list {
			if m.Version <= version {
				shipped = append(shipped, int64(m.Version))
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM core.schema_versions WHERE version > $1 OR NOT (version = ANY($2))`, version, shipped); err != nil {
			return fmt.Errorf("force: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE core.schema_versions SET dirty = false WHERE dirty`); err != nil {
			return fmt.Errorf("force: %w", err)
		}
		// Every row at or below version is recorded as this build's file
		// (name and checksum included): force is also how an operator accepts
		// a database brought by hand to what an edited migration now says.
		for _, m := range r.set.list {
			if m.Version > version {
				break
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO core.schema_versions (version, name, checksum, applied_at, dirty)
				VALUES ($1, $2, $3, now(), false)
				ON CONFLICT (version) DO UPDATE SET name = EXCLUDED.name, checksum = EXCLUDED.checksum`, m.Version, m.Name, m.Checksum); err != nil {
				return fmt.Errorf("force: record %d: %w", m.Version, err)
			}
		}
		return tx.Commit()
	})
}
