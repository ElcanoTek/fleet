package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
)

// A restore keeps the migration ledgers true to the schemas it puts back
// (#2049 M6, ADR-0107). It is always a whole-database restore (--db=all),
// and pgRestoreAll recreates chat, core and sched inside the restore
// transaction before loading the archive, so the dump's schemas and ledgers
// replace the live ones whole:
//
//   - a dump taken since M6 carries core.schema_versions (and, below the
//     boundary, the two legacy ledgers): restored with everything else, while
//     any object or legacy ledger not in the dump is cleared with the schema
//     reset (the next start writes missing legacy ledgers again from the
//     restored ledger);
//   - a dump from before M6 (M4 or M5) has only the legacy ledgers. Below the
//     boundary they are authoritative and the next start adopts them into
//     core.schema_versions; past it (a version >= 200 ran, or the legacy
//     ledgers carry the sentinels) the restore is refused before anything
//     changes: those migrations' objects are not in the dump.
//
// Every check here is read-only and runs before anything changes.

// dumpTOC is the dump's table of contents (pg_restore --list, read-only).
func dumpTOC(ctx context.Context, path string) (string, error) {
	//nolint:gosec // G204: fixed "pg_restore" binary; path is an operator-supplied path passed as a separate argv. --list is read-only.
	out, err := exec.CommandContext(ctx, "pg_restore", "--list", path).Output()
	if err != nil {
		return "", fmt.Errorf("read the dump's table of contents (%s): %w", path, err)
	}
	return string(out), nil
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

// checkLedgerRestore checks a whole-database dump against the live database
// before the restore touches anything.
func checkLedgerRestore(ctx context.Context, dsn, inPath string) error {
	toc, err := dumpTOC(ctx, inPath)
	if err != nil {
		return err
	}
	if !tocHasTable(toc, config.IdentitySchema, "users") {
		return fmt.Errorf("%s is from before the one identity table (it has no %s.users: its accounts are unmerged), "+
			"which this release cannot run: restore a whole-database dump taken by fleet M4 or later (docs/BACKUP_RESTORE.md)", inPath, config.IdentitySchema)
	}
	if !tocHasTable(toc, config.IdentitySchema, "schema_versions") {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := migrations.CheckLegacyRestore(ctx, db); err != nil {
			return err
		}
	}
	return nil
}
