package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
// (one without core.schema_versions) over a database that has run a
// migration from FirstNew on: those migrations' objects are not in the dump
// and would survive the restore next to schemas that know nothing of them.
func CheckLegacyRestore(ctx context.Context, db *sql.DB) error {
	maxV, exists, err := LedgerMax(ctx, db)
	if err != nil || !exists {
		return err
	}
	if maxV >= FirstNew {
		return fmt.Errorf("this database has run migrations up to version %d, which a dump from before the one migration ledger (it has no %s) does not know: "+
			"restore it into a new, empty database instead (point DATABASE_URL at it), or restore a newer dump", maxV, Table)
	}
	return nil
}

// ForgetLedger drops core.schema_versions after a dump from before the one
// ledger was restored over part of the database, so the next start adopts
// again — from the legacy ledgers, which now describe the schemas: the
// restored store's comes from the dump, and for each store in rewrite (the
// ones the restore did not touch) the legacy ledger is written here from the
// one ledger first. In one transaction, under the migration locks. A
// database without the ledger is left alone.
func ForgetLedger(ctx context.Context, db *sql.DB, rewrite ...string) error {
	r, err := embeddedRunner()
	if err != nil {
		return err
	}
	return r.locked(ctx, db, true, func(conn *sql.Conn) (err error) {
		exists, err := ledgerExists(ctx, conn)
		if err != nil || !exists {
			return err
		}
		ledger, err := readLedger(ctx, conn)
		if err != nil {
			return err
		}
		for v := range ledger {
			if v >= FirstNew {
				return fmt.Errorf("refusing to drop %s: it records version %d, which the legacy ledgers cannot", Table, v)
			}
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
		for _, stream := range rewrite {
			switch stream {
			case StreamChat:
				if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS chat.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `DELETE FROM chat.schema_migrations`); err != nil {
					return err
				}
				for v, row := range ledger {
					m, ok := r.set.Get(v)
					if !ok || m.Stream != StreamChat {
						continue
					}
					if _, err = tx.ExecContext(ctx, `INSERT INTO chat.schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
						m.LegacyVersion(), m.LegacyName(), row.AppliedAt.Unix()); err != nil {
						return err
					}
				}
			case StreamSched:
				if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sched.schema_migrations (version bigint not null primary key, dirty boolean not null)`); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `TRUNCATE sched.schema_migrations`); err != nil {
					return err
				}
				top := -1
				for v := range ledger {
					if m, ok := r.set.Get(v); ok && m.Stream == StreamSched {
						top = max(top, m.LegacyVersion())
					}
				}
				if top >= 0 {
					if _, err = tx.ExecContext(ctx, `INSERT INTO sched.schema_migrations (version, dirty) VALUES ($1, false)`, top); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unknown stream %q", stream)
			}
		}
		if _, err = tx.ExecContext(ctx, `DROP TABLE core.schema_versions`); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// LedgerEntry is one row of core.schema_versions as a restore compares it.
type LedgerEntry struct {
	Version  int
	Name     string
	Checksum string
}

// Ledger returns every row of core.schema_versions, ascending, and whether
// the ledger exists.
func Ledger(ctx context.Context, db *sql.DB) ([]LedgerEntry, bool, error) {
	exists, err := ledgerExists(ctx, db)
	if err != nil || !exists {
		return nil, false, err
	}
	rows, err := readLedger(ctx, db)
	if err != nil {
		return nil, true, err
	}
	out := make([]LedgerEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, LedgerEntry{Version: r.Version, Name: r.Name, Checksum: r.Checksum})
	}
	sortEntries(out)
	return out, true, nil
}

func sortEntries(es []LedgerEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Version < es[j].Version })
}

// LedgerDiff describes the first difference between two ledgers ("" when
// they record the same versions, names and checksums).
func LedgerDiff(a, b []LedgerEntry) string {
	ma := map[int]LedgerEntry{}
	for _, e := range a {
		ma[e.Version] = e
	}
	mb := map[int]LedgerEntry{}
	for _, e := range b {
		mb[e.Version] = e
	}
	var versions []int
	for v := range ma {
		versions = append(versions, v)
	}
	for v := range mb {
		if _, ok := ma[v]; !ok {
			versions = append(versions, v)
		}
	}
	sort.Ints(versions)
	for _, v := range versions {
		ea, inA := ma[v]
		eb, inB := mb[v]
		switch {
		case !inA:
			return fmt.Sprintf("version %d (%s) only in the second", v, eb.Name)
		case !inB:
			return fmt.Sprintf("version %d (%s) only in the first", v, ea.Name)
		case ea.Name != eb.Name || ea.Checksum != eb.Checksum:
			return fmt.Sprintf("version %d differs (%s %.12s vs %s %.12s)", v, ea.Name, ea.Checksum, eb.Name, eb.Checksum)
		}
	}
	return ""
}
