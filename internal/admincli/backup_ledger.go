package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
)

// A restore keeps the one migration ledger, core.schema_versions (#2049 M6,
// ADR-0107), true to the schemas it leaves behind:
//
//   - a dump taken since M6 carries the ledger (in core). A one-store restore
//     (--db=chat or --db=sched) puts back one store's schema while the other
//     stays live, so it is refused unless the dump's ledger is at exactly the
//     live one's version; a whole-fleet restore (--db=all) replaces all three
//     schemas, ledger included, and needs no check.
//   - a dump from before M6 has no ledger. Restoring it drops the live one,
//     so the next start adopts again from the legacy ledgers: the restored
//     store's comes from the dump, and the untouched store's is written from
//     the live ledger first. Over a database that has run a migration from
//     version 200 on, such a restore is refused (restore into a new database).

// ledgerRestore is what planLedgerRestore decided.
type ledgerRestore struct {
	// forget: the dump has no ledger; drop the live one after the restore,
	// writing the legacy ledgers of rewrite first.
	forget  bool
	rewrite []string
}

// dumpLedger reports whether the dump holds core.schema_versions and, when
// it does, the highest version it records.
func dumpLedger(ctx context.Context, path string) (int, bool, error) {
	//nolint:gosec // G204: fixed "pg_restore" binary; path is an operator-supplied path passed as a separate argv. --list is read-only.
	out, err := exec.CommandContext(ctx, "pg_restore", "--list", path).Output()
	if err != nil {
		return 0, false, fmt.Errorf("read the dump's table of contents (%s): %w", path, err)
	}
	if !tocHasTable(string(out), config.IdentitySchema, "schema_versions") {
		return 0, false, nil
	}
	versions, err := dumpColumn(ctx, path, config.IdentitySchema, "schema_versions", "version")
	if err != nil {
		return 0, true, err
	}
	top := 0
	for v := range versions {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, true, fmt.Errorf("the dump's %s holds a non-numeric version %q", migrations.Table, v)
		}
		top = max(top, n)
	}
	return top, true, nil
}

// tocHasTable reports whether a pg_restore --list output has a TABLE entry
// for schema.table.
func tocHasTable(toc, schema, table string) bool {
	for _, ln := range strings.Split(toc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), ";") {
			continue
		}
		_, rest, ok := strings.Cut(ln, "; ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 5 && f[2] == "TABLE" && f[3] == schema && f[4] == table {
			return true
		}
	}
	return false
}

// checkLedgerVersions refuses a one-store restore of a dump whose ledger is
// at another version than the live one — the case that needs no other
// check's context, so it runs before anything can change the database.
func checkLedgerVersions(ctx context.Context, dsn, inPath, target string) error {
	dumpMax, dumpHas, err := dumpLedger(ctx, inPath)
	if err != nil || !dumpHas {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	liveMax, liveHas, err := migrations.LedgerMax(ctx, db)
	if err != nil || !liveHas || liveMax == dumpMax {
		return err
	}
	_, err = planLedgerRestore(ctx, dsn, inPath, target)
	return err
}

// planLedgerRestore checks the dump's ledger against the live one before a
// restore of target ("chat", "sched" or "all") touches anything.
func planLedgerRestore(ctx context.Context, dsn, inPath, target string) (ledgerRestore, error) {
	dumpMax, dumpHas, err := dumpLedger(ctx, inPath)
	if err != nil {
		return ledgerRestore{}, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return ledgerRestore{}, err
	}
	defer db.Close()
	liveMax, liveHas, err := migrations.LedgerMax(ctx, db)
	if err != nil {
		return ledgerRestore{}, err
	}
	if dumpHas {
		if target == "all" || (liveHas && liveMax == dumpMax) {
			return ledgerRestore{}, nil
		}
		if !liveHas {
			// No ledger here: an empty database, or one no M6 release has
			// started on yet. A chat restore brings the dump's ledger with
			// core, which is right only when the scheduler's schema is empty
			// too; a sched restore brings none (core is chat's).
			if target == config.ChatSchema {
				busy, err := schemaHasTables(ctx, db, config.SchedSchema)
				if err != nil || !busy {
					return ledgerRestore{}, err
				}
			}
			return ledgerRestore{}, fmt.Errorf("%s is at migration version %d but this database has no %s yet: restore the "+
				"chat dump first into an empty database (it carries core and the ledger), restore a whole-fleet dump with --db=all "+
				"(fleet backup --db=all makes one), or start this fleet release once on the database first (docs/BACKUP_RESTORE.md)",
				inPath, dumpMax, migrations.Table)
		}
		return ledgerRestore{}, fmt.Errorf("%s is at migration version %d but this database is at migration version %d: restoring only "+
			"the %s store would leave the two stores' schemas at different versions under one ledger. Restore a whole-fleet dump "+
			"with --db=all (fleet backup --db=all makes one), or restore with the fleet release whose database matches the dump "+
			"(docs/BACKUP_RESTORE.md)", inPath, dumpMax, liveMax, target)
	}
	if !liveHas {
		return ledgerRestore{}, nil
	}
	if err := migrations.CheckLegacyRestore(ctx, db); err != nil {
		return ledgerRestore{}, err
	}
	plan := ledgerRestore{forget: true}
	switch target {
	case config.ChatSchema:
		plan.rewrite = []string{migrations.StreamSched}
	case config.SchedSchema:
		plan.rewrite = []string{migrations.StreamChat}
	}
	return plan, nil
}

// finish runs after a successful restore.
func (p ledgerRestore) finish(ctx context.Context, dsn string, opts restoreOpts) error {
	if !p.forget {
		return nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrations.ForgetLedger(ctx, db, p.rewrite...); err != nil {
		return fmt.Errorf("the restore succeeded, but dropping %s failed (drop it by hand before the next start: DROP TABLE %s): %w", migrations.Table, migrations.Table, err)
	}
	opts.notef("the dump predates the one migration ledger: dropped %s, so the next fleet start adopts the legacy ledgers again", migrations.Table)
	return nil
}

// schemaHasTables reports whether schema holds any table.
func schemaHasTables(ctx context.Context, db *sql.DB, schema string) (bool, error) {
	var ok bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_tables WHERE schemaname = $1)`, schema).Scan(&ok); err != nil {
		return false, fmt.Errorf("probe schema %s: %w", schema, err)
	}
	return ok, nil
}
