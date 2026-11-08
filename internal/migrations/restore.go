package migrations

import (
	"context"
	"database/sql"
	"fmt"
)

// What `fleet restore` needs to keep the one ledger true to the schemas a
// restore leaves behind (docs/BACKUP_RESTORE.md).

// LedgerMax returns the highest version core.schema_versions records and
// whether the ledger exists (0, false before adoption).
func LedgerMax(ctx context.Context, db *sql.DB) (int, bool, error) {
	exists, err := ledgerExists(ctx, db)
	if err != nil || !exists {
		return 0, false, err
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM core.schema_versions`).Scan(&v); err != nil {
		return 0, true, fmt.Errorf("read %s: %w", Table, err)
	}
	return int(v.Int64), true, nil
}

// CheckLegacyRestore refuses to restore a dump from before the one ledger
// (one without core.schema_versions) over a database past the boundary: the
// objects of its migrations from FirstNew on are not in the dump and would
// survive next to schemas that know nothing of them. Below the boundary such
// a restore needs nothing else: the dump brings its store's legacy ledger,
// which is authoritative there, and the next start reconciles the one ledger
// to it.
func CheckLegacyRestore(ctx context.Context, db *sql.DB) error {
	crossed, err := Crossed(ctx, db)
	if err != nil || !crossed {
		return err
	}
	maxV, _, err := LedgerMax(ctx, db)
	if err != nil {
		return err
	}
	return fmt.Errorf("this database is past the one migration ledger's boundary (migrations up to version %d, or its legacy ledgers frozen), which a dump from before the one ledger (it has no %s) does not know: "+
		"restore it into a new, empty database instead (point DATABASE_URL at it), or restore a newer dump", maxV, Table)
}

// UpLegacy migrates the database through the legacy versions only when it is
// below the boundary (Up when it is past it): what a step that needs the
// legacy schema (the identity merge) runs without carrying the database
// across the boundary — a restore of a pre-M6 dump may follow.
func UpLegacy(ctx context.Context, db *sql.DB) error {
	crossed, err := Crossed(ctx, db)
	if err != nil {
		return err
	}
	if crossed {
		return Up(ctx, db)
	}
	return UpTo(ctx, db, LegacyLatest)
}

// DropLegacyLedgers drops the legacy ledgers named (StreamChat, StreamSched)
// under the migration locks: before a whole-fleet restore of a dump that does
// not carry them, so no stale ledger outlives the restore (the next start
// recreates them from the restored one ledger).
func DropLegacyLedgers(ctx context.Context, db *sql.DB, streams ...string) error {
	r, err := embeddedRunner()
	if err != nil {
		return err
	}
	return r.locked(ctx, db, true, func(conn *sql.Conn) error {
		for _, s := range streams {
			switch s {
			case StreamChat:
				if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS chat.schema_migrations`); err != nil {
					return err
				}
			case StreamSched:
				if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS sched.schema_migrations`); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown stream %q", s)
			}
		}
		return nil
	})
}
