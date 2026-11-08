package admincli

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
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
// it does, its rows (ascending) and highest version.
func dumpLedger(ctx context.Context, path string) ([]migrations.LedgerEntry, int, bool, error) {
	//nolint:gosec // G204: fixed "pg_restore" binary; path is an operator-supplied path passed as a separate argv. --list is read-only.
	out, err := exec.CommandContext(ctx, "pg_restore", "--list", path).Output()
	if err != nil {
		return nil, 0, false, fmt.Errorf("read the dump's table of contents (%s): %w", path, err)
	}
	if !tocHasTable(string(out), config.IdentitySchema, "schema_versions") {
		return nil, 0, false, nil
	}
	rows, err := dumpRows(ctx, path, config.IdentitySchema, "schema_versions", []string{"version", "name", "checksum"})
	if err != nil {
		return nil, 0, true, err
	}
	entries := make([]migrations.LedgerEntry, 0, len(rows))
	top := 0
	for _, r := range rows {
		n, err := strconv.Atoi(r[0])
		if err != nil {
			return nil, 0, true, fmt.Errorf("the dump's %s holds a non-numeric version %q", migrations.Table, r[0])
		}
		top = max(top, n)
		entries = append(entries, migrations.LedgerEntry{Version: n, Name: r[1], Checksum: r[2]})
	}
	return entries, top, true, nil
}

// dumpRows reads columns of one table out of a custom-format dump
// (pg_restore --data-only to stdout; nothing is connected to or written).
func dumpRows(ctx context.Context, path, schema, table string, columns []string) ([][]string, error) {
	//nolint:gosec // G204: fixed "pg_restore" binary; path/schema/table are separate argv (no shell).
	cmd := exec.CommandContext(ctx, "pg_restore", "--data-only", "-n", schema, "-t", table, "-f", "-", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	rows, perr := parseCopyRows(out, schema+"."+table, columns)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("read %s.%s from %s: %w", schema, table, path, err)
	}
	if perr != nil {
		return nil, fmt.Errorf("read %s.%s from %s: %w", schema, table, path, perr)
	}
	return rows, nil
}

// parseCopyRows collects the named columns of the COPY block for table in a
// pg_restore SQL script (COPY text format; \N reads as ""). A column the
// table does not have is an error.
func parseCopyRows(r io.Reader, table string, columns []string) ([][]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var idx []int
	var out [][]string
	in := false
	for sc.Scan() {
		ln := sc.Text()
		if !in {
			rest, ok := strings.CutPrefix(ln, "COPY "+table+" (")
			if !ok {
				continue
			}
			cols, _, ok := strings.Cut(rest, ") FROM stdin;")
			if !ok {
				return nil, fmt.Errorf("unexpected COPY header %q", ln)
			}
			have := strings.Split(cols, ", ")
			idx = idx[:0]
			for _, want := range columns {
				at := -1
				for i, c := range have {
					if strings.Trim(c, `"`) == want {
						at = i
					}
				}
				if at < 0 {
					_ = drain(sc)
					return nil, fmt.Errorf("%s has no column %s", table, want)
				}
				idx = append(idx, at)
			}
			in = true
			continue
		}
		if ln == `\.` {
			in = false
			continue
		}
		f := strings.Split(ln, "\t")
		row := make([]string, len(idx))
		for i, at := range idx {
			if at < len(f) && f[at] != `\N` {
				row[i] = f[at]
			}
		}
		out = append(out, row)
	}
	return out, sc.Err()
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
	dumpRows, _, dumpHas, err := dumpLedger(ctx, inPath)
	if err != nil || !dumpHas {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	liveRows, liveHas, err := migrations.Ledger(ctx, db)
	if err != nil || !liveHas || migrations.LedgerDiff(dumpRows, liveRows) == "" {
		return err
	}
	_, err = planLedgerRestore(ctx, dsn, inPath, target)
	return err
}

// planLedgerRestore checks the dump's ledger against the live one before a
// restore of target ("chat", "sched" or "all") touches anything.
func planLedgerRestore(ctx context.Context, dsn, inPath, target string) (ledgerRestore, error) {
	dumpEntries, dumpMax, dumpHas, err := dumpLedger(ctx, inPath)
	if err != nil {
		return ledgerRestore{}, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return ledgerRestore{}, err
	}
	defer db.Close()
	liveEntries, liveHas, err := migrations.Ledger(ctx, db)
	if err != nil {
		return ledgerRestore{}, err
	}
	liveMax := 0
	for _, e := range liveEntries {
		liveMax = max(liveMax, e.Version)
	}
	if dumpHas {
		// One store restored next to the other live: both ledgers must record
		// the same versions, names and checksums (the same schema), not just
		// reach the same version.
		diff := migrations.LedgerDiff(dumpEntries, liveEntries)
		if target == "all" || (liveHas && diff == "") {
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
		return ledgerRestore{}, fmt.Errorf("%s's migration ledger (at version %d) is not this database's (at migration version %d; "+
			"first difference: %s, dump vs database): restoring only the %s store would leave the two stores' schemas from different "+
			"migration histories under one ledger. Restore a whole-fleet dump with --db=all (fleet backup --db=all makes one), or "+
			"restore with the fleet release whose database matches the dump (docs/BACKUP_RESTORE.md)", inPath, dumpMax, liveMax, diff, target)
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
