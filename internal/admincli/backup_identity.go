package admincli

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config"
)

// A per-store restore must not leave the scheduler's task and dataset
// creators pointing at accounts that are not there (#2049 M4: the accounts
// live in core.users, next to both stores). This file holds the checks
// runPgRestoreWith runs before it touches anything.

// creatorColumns are the scheduler columns that name an account by its
// core.users id.
var creatorColumns = []struct{ table, column string }{
	{"tasks", "created_by"},
	{"datasets", "created_by_user_id"},
}

// restoreOpts are the restore's operator choices.
type restoreOpts struct {
	// allowUnresolved restores even when some task or dataset creators that
	// resolve now (or in the dump) would name no account afterwards.
	allowUnresolved bool
	// notes receives what the restore decided (which schemas, why).
	notes io.Writer
}

func (o restoreOpts) notef(format string, a ...any) {
	if o.notes != nil {
		fmt.Fprintf(o.notes, "note: "+format+"\n", a...)
	}
}

// liveIdentity is what the target database holds of the identity schema.
type liveIdentity struct {
	usersTable bool            // core.users exists
	ids        map[string]bool // its account ids
	merged     bool            // core.identity_merge records the sched merge
}

func readLiveIdentity(ctx context.Context, dsn string) (liveIdentity, error) {
	var li liveIdentity
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return li, err
	}
	defer db.Close()
	var hasMerge bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('core.users') IS NOT NULL, to_regclass('core.identity_merge') IS NOT NULL`).Scan(&li.usersTable, &hasMerge); err != nil {
		return li, fmt.Errorf("probe the identity tables: %w", err)
	}
	li.ids = map[string]bool{}
	if li.usersTable {
		if err := collect(ctx, db, `SELECT id::text FROM core.users`, li.ids); err != nil {
			return li, fmt.Errorf("read the accounts: %w", err)
		}
	}
	if hasMerge {
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core.identity_merge WHERE step = 'm4_sched_users')`).Scan(&li.merged); err != nil {
			return li, fmt.Errorf("read the identity merge marker: %w", err)
		}
	}
	return li, nil
}

// liveCreators is every non-null creator id in the target's sched store.
func liveCreators(ctx context.Context, dsn string) (map[string]bool, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]bool{}
	for _, c := range creatorColumns {
		var ok bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, config.SchedSchema+"."+c.table).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// Identifiers are the fixed creatorColumns, not input.
		q := `SELECT DISTINCT ` + c.column + `::text FROM ` + config.SchedSchema + `.` + c.table + ` WHERE ` + c.column + ` IS NOT NULL` //nolint:gosec // G202: fixed identifiers
		if err := collect(ctx, db, q, out); err != nil {
			return nil, fmt.Errorf("read %s.%s: %w", c.table, c.column, err)
		}
	}
	return out, nil
}

func collect(ctx context.Context, db *sql.DB, q string, into map[string]bool) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		into[v] = true
	}
	return rows.Err()
}

// dumpColumn reads one column of one table out of a custom-format dump
// (pg_restore --data-only to stdout; nothing is connected to or written).
// Nulls are skipped. A table the dump does not hold gives an empty set.
func dumpColumn(ctx context.Context, path, schema, table, column string) (map[string]bool, error) {
	//nolint:gosec // G204: fixed "pg_restore" binary; path/schema/table are separate argv (no shell).
	cmd := exec.CommandContext(ctx, "pg_restore", "--data-only", "-n", schema, "-t", table, "-f", "-", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	vals, perr := parseCopyColumn(out, schema+"."+table, column)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("read %s.%s from %s: %w", schema, table, path, err)
	}
	if perr != nil {
		return nil, fmt.Errorf("read %s.%s from %s: %w", schema, table, path, perr)
	}
	return vals, nil
}

// parseCopyColumn collects one column of the COPY block for table in a
// pg_restore SQL script: `COPY <table> (<cols>) FROM stdin;`, tab-separated
// rows in COPY text format, ended by `\.`. \N (null) is skipped.
func parseCopyColumn(r io.Reader, table, column string) (map[string]bool, error) {
	out := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	idx := -1
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
			for i, c := range strings.Split(cols, ", ") {
				if strings.Trim(c, `"`) == column {
					idx = i
				}
			}
			if idx < 0 {
				// An older schema without the column: nothing to resolve.
				return out, drain(sc)
			}
			in = true
			continue
		}
		if ln == `\.` {
			in = false
			continue
		}
		f := strings.Split(ln, "\t")
		if idx < len(f) && f[idx] != `\N` {
			out[f[idx]] = true
		}
	}
	return out, sc.Err()
}

func drain(sc *bufio.Scanner) error {
	for sc.Scan() { //nolint:revive // drain the pipe so pg_restore exits
	}
	return sc.Err()
}

// dumpCreators is every non-null creator id the dump's sched store holds.
func dumpCreators(ctx context.Context, path string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range creatorColumns {
		vals, err := dumpColumn(ctx, path, config.SchedSchema, c.table, c.column)
		if err != nil {
			return nil, err
		}
		for v := range vals {
			out[v] = true
		}
	}
	return out, nil
}

// orphaned lists the creators in want that resolve in before but not in after.
func orphaned(creators, before, after map[string]bool) []string {
	var out []string
	for id := range creators {
		if before[id] && !after[id] {
			out = append(out, id)
		}
	}
	return out
}

func orphanErr(n int, what string) error {
	return fmt.Errorf("%s would leave %d task/dataset creator(s) naming no account. Restore the whole fleet from a fleet-all dump with --db=all, "+
		"or restore chat and sched from the same backup; pass --allow-unresolved-creators to restore anyway (those rows then show no creator)", what, n)
}

// planSchedRestore decides what a --db=sched restore restores, and refuses
// the cases that would orphan creators. It restores schema sched only, in
// every case: core belongs to the chat store's migrations (099 creates it by
// moving chat.users), so a core restored ahead of chat into a fresh database
// would stop the chat store from ever migrating. The dump's own core is read
// only to tell which creators resolved when it was taken.
//
//   - A per-store or fleet-all dump (it holds core): refused when a creator
//     that resolves in the dump's accounts is not an account of this
//     database — including a database with no accounts yet, where chat
//     must be restored first (its dump carries core) or the whole fleet
//     with --db=all.
//   - A sched dump from before the one identity table (no core; its accounts
//     in sched.users): fine while the database has not merged its accounts
//     yet (the scheduler's next start merges the restored sched.users);
//     refused once it has, since nothing would fold them in again.
func planSchedRestore(ctx context.Context, dsn, inPath string, dumpHasCore bool, opts restoreOpts) error {
	live, err := readLiveIdentity(ctx, dsn)
	if err != nil {
		return err
	}
	switch {
	case !dumpHasCore && live.merged:
		return fmt.Errorf("%s is a sched dump from before the one identity table (its accounts are in sched.users), and this database has already merged its accounts into core.users: restore it into an empty database, or with the release that made it", inPath)
	case !dumpHasCore:
		opts.notef("this sched dump predates the one identity table; the scheduler's next start merges its accounts (sched.users) into core.users")
		return nil
	}
	opts.notef("restoring sched only; identities are kept from this database (core.users, %d account(s))", len(live.ids))
	if opts.allowUnresolved {
		return nil
	}
	creators, err := dumpCreators(ctx, inPath)
	if err != nil {
		return err
	}
	dumpIDs, err := dumpColumn(ctx, inPath, config.IdentitySchema, "users", "id")
	if err != nil {
		return err
	}
	lost := orphaned(creators, dumpIDs, live.ids)
	switch {
	case len(lost) == 0:
		return nil
	case !live.usersTable || len(live.ids) == 0:
		return fmt.Errorf("this database has no accounts yet, so restoring sched alone would leave %d task/dataset creator(s) naming no account: "+
			"restore chat from the same backup first (its dump carries the accounts, schema core), or the whole fleet from a fleet-all dump with --db=all; "+
			"pass --allow-unresolved-creators to restore anyway", len(lost))
	default:
		return orphanErr(len(lost), "restoring sched only, against this database's accounts,")
	}
}

// checkChatRestoreKeepsCreators refuses a --db=chat restore whose accounts
// (the dump's core) would leave live sched creators that resolve now naming
// no account.
func checkChatRestoreKeepsCreators(ctx context.Context, dsn, inPath string) error {
	live, err := readLiveIdentity(ctx, dsn)
	if err != nil || !live.usersTable {
		return err
	}
	creators, err := liveCreators(ctx, dsn)
	if err != nil || len(creators) == 0 {
		return err
	}
	dumpIDs, err := dumpColumn(ctx, inPath, config.IdentitySchema, "users", "id")
	if err != nil {
		return err
	}
	if lost := orphaned(creators, live.ids, dumpIDs); len(lost) > 0 {
		return orphanErr(len(lost), "restoring chat (and its accounts, core) under the live sched store")
	}
	return nil
}
