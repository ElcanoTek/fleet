package admincli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/migrations"
)

const migrateUsage = "usage: fleet migrate status [--database-url <dsn>] [--json]\n" +
	"       fleet migrate down --to <version> [--dry-run] [--no-confirm] [--database-url <dsn>]\n" +
	"       fleet migrate force <version> [--database-url <dsn>] [--no-confirm]"

// cmdMigrate implements `fleet migrate <subcommand>` over the one migration
// sequence (internal/migrations, ADR-0107). `status` is READ-ONLY. `down`
// undoes migrations from version 200 on; `force` repairs the ledger after a
// half-applied migration. Every store open migrates up, so there is no `up`.
func cmdMigrate(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 1
	}
	switch argv[0] {
	case "status":
		return cmdMigrateStatus(argv[1:])
	case "down":
		return cmdMigrateDown(argv[1:])
	case "force":
		return cmdMigrateForce(argv[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 0
	default:
		return errf(1, "unknown migrate subcommand %q (\"status\", \"down\" or \"force\")", argv[0])
	}
}

// openMigrateDB opens a small pool on the one database. Any store DSN
// reaches it; the runner names every schema itself.
func openMigrateDB(flagURL string) (*sql.DB, error) {
	dsn, err := chatDSN(flagURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

// cmdMigrateForce makes the ledger record every version up to <version> as
// applied and clean and nothing above it, running no migration: after a
// migration was left half-applied (a dirty row, and fleet refuses to start),
// the operator inspects the database, repairs or undoes what the migration
// did, and records the last version that is really applied (0: none). It
// writes, so it asks first unless --no-confirm, and refuses a non-TTY
// without it.
func cmdMigrateForce(argv []string) int {
	fs := flag.NewFlagSet("migrate force", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN of the one database (default DATABASE_URL)")
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive confirmation (for scripted repairs)")
	version, ok := parseForceArgs(fs, argv)
	if !ok {
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 1
	}
	if code := confirmBulkMutation(*noConfirm, fmt.Sprintf("this will record migration version %d (and every version below it) as applied and clean in %s, and forget every version above it, running nothing.", version, migrations.Table)); code != 0 {
		return code
	}
	db, err := openMigrateDB(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrations.Force(ctx, db, version); err != nil {
		return errf(1, "force %d: %v", version, err)
	}
	fmt.Printf("%s: version forced to %d (clean). Restart fleet to apply the remaining migrations.\n", migrations.Table, version)
	return 0
}

// parseForceArgs accepts the version before or after the flags. ok is false
// for a missing, extra, negative or non-integer version, or a bad flag.
func parseForceArgs(fs *flag.FlagSet, argv []string) (int, bool) {
	var positional, flags []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a != "" && a[0] != '-':
			positional = append(positional, a)
		case a == "--database-url" || a == "-database-url":
			flags = append(flags, a)
			if i+1 < len(argv) {
				i++
				flags = append(flags, argv[i])
			}
		default:
			flags = append(flags, a)
		}
	}
	if err := fs.Parse(flags); err != nil || fs.NArg() != 0 || len(positional) != 1 {
		return 0, false
	}
	v, err := strconv.Atoi(positional[0])
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// cmdMigrateDown undoes every applied migration above --to, newest first,
// each in a transaction with its ledger row (migrations.Down). It never goes
// below the last legacy version (177). It says to take a backup first, prints
// the plan, and asks unless --no-confirm (a non-TTY without it is refused);
// --dry-run prints the plan and changes nothing.
func cmdMigrateDown(argv []string) int {
	fs := flag.NewFlagSet("migrate down", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN of the one database (default DATABASE_URL)")
	to := fs.Int("to", -1, "the version to go down to (required; at least 177)")
	dryRun := fs.Bool("dry-run", false, "print what would be undone and change nothing")
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive confirmation (for scripted rollbacks)")
	if err := fs.Parse(argv); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 1
	}
	if *to < 0 {
		return errf(1, "--to <version> is required")
	}
	fmt.Fprintln(os.Stderr, "Before going down, take a backup of the whole database and keep it: fleet backup --db=all")
	fmt.Fprintln(os.Stderr, "Stop fleet first (a running server keeps using the newer schema), and start the release that ships the target version afterwards.")
	db, err := openMigrateDB(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plan, err := migrations.Down(ctx, db, *to, true)
	if err != nil {
		return errf(1, "down --to %d: %v", *to, err)
	}
	if len(plan.Steps) == 0 {
		fmt.Printf("nothing to undo: no applied migration above %d\n", *to)
		return 0
	}
	fmt.Printf("would undo, newest first:\n")
	for _, m := range plan.Steps {
		fmt.Printf("  %d  %s\n", m.Version, m.Name)
	}
	if *dryRun {
		return 0
	}
	if code := confirmBulkMutation(*noConfirm, fmt.Sprintf("this will run %d down migration(s) against the live database.", len(plan.Steps))); code != 0 {
		return code
	}
	done, err := migrations.Down(ctx, db, *to, false)
	if err != nil {
		return errf(5, "down --to %d: %v", *to, err)
	}
	fmt.Printf("undid %d migration(s); the database is at version %d\n", len(done.Steps), *to)
	return 0
}

// migrateStatusJSON is the --json shape: the one report's fields at the top
// level, plus "chat" and "sched" — the pre-M6 per-store reports, derived from
// the one sequence, kept for one release so a consumer of the old envelope
// keeps working (docs/MIGRATIONS.md). When the database cannot be read, every
// member carries "error" instead.
type migrateStatusJSON struct {
	*migrations.Report
	Error string          `json:"error,omitempty"`
	Chat  migrateViewJSON `json:"chat"`
	Sched migrateViewJSON `json:"sched"`
}

type migrateViewJSON struct {
	view  any
	Error string `json:"error,omitempty"`
}

// MarshalJSON flattens the view's fields into the member (or emits only the
// error).
func (v migrateViewJSON) MarshalJSON() ([]byte, error) {
	if v.view == nil {
		return json.Marshal(struct {
			Error string `json:"error,omitempty"`
		}{v.Error})
	}
	return json.Marshal(v.view)
}

func migrateStatusEnvelope(rep *migrations.Report, err error) migrateStatusJSON {
	if err != nil {
		return migrateStatusJSON{Error: err.Error(), Chat: migrateViewJSON{Error: err.Error()}, Sched: migrateViewJSON{Error: err.Error()}}
	}
	chat, sched := rep.Chat(), rep.Sched()
	return migrateStatusJSON{Report: rep, Chat: migrateViewJSON{view: chat}, Sched: migrateViewJSON{view: sched}}
}

// cmdMigrateStatus reports the one sequence's applied vs pending migrations,
// read-only. Exit 0 when the database was read, 1 otherwise; with --json the
// envelope is always emitted.
func cmdMigrateStatus(argv []string) int {
	fs := flag.NewFlagSet("migrate status", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN of the one database (default DATABASE_URL)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	rep, err := migrationStatus(*dbURL)
	if *asJSON {
		var r *migrations.Report
		if err == nil {
			r = &rep
		}
		if code := printJSON(migrateStatusEnvelope(r, err)); code != 0 {
			return code
		}
		if err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		return errf(1, "migration status: %v", err)
	}
	printMigrations(rep)
	return 0
}

func migrationStatus(flagURL string) (migrations.Report, error) {
	db, err := openMigrateDB(flagURL)
	if err != nil {
		return migrations.Report{}, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return migrations.Report{}, err
	}
	return migrations.Status(ctx, db)
}

func printMigrations(r migrations.Report) {
	cur := "none"
	if r.CurrentVersion != nil {
		cur = strconv.Itoa(*r.CurrentVersion)
	}
	fmt.Printf("fleet database (one migration sequence, ledger %s)\n", r.MigrationTable)
	if r.Adopted {
		fmt.Printf("  adopted: yes\n")
	} else {
		fmt.Printf("  adopted: not yet — the next fleet start moves the legacy ledgers into %s (%s)\n", r.MigrationTable, r.Legacy)
	}
	fmt.Printf("  current version: %s\n", cur)
	if r.Problem != "" {
		fmt.Printf("  REFUSED: %s\n", r.Problem)
	}
	for _, m := range r.Unknown {
		fmt.Printf("  REFUSED: version %d (%s) is not shipped by this build: a newer fleet migrated this database\n", m.Version, m.Name)
	}
	for _, m := range r.Mismatched {
		fmt.Printf("  REFUSED: version %d (%s) was recorded from a different file than this build ships (an edited migration; see docs/MIGRATIONS.md)\n", m.Version, m.Name)
	}
	if r.Dirty {
		fmt.Printf("  DIRTY: a migration was left half-applied — inspect the database, then record the last good version:\n")
		fmt.Printf("         fleet migrate force <version>\n")
		fmt.Printf("         and restart fleet (see docs/MIGRATIONS.md)\n")
	}
	fmt.Printf("  applied: %d   pending: %d\n", len(r.Applied), len(r.Pending))
	for _, m := range r.Applied {
		ts := ""
		if m.AppliedAt != nil {
			ts = time.Unix(*m.AppliedAt, 0).UTC().Format(time.RFC3339)
		}
		fmt.Printf("    [applied] %4d %-48s %s\n", m.Version, m.Name, ts)
	}
	for _, m := range r.Pending {
		fmt.Printf("    [PENDING] %4d %s\n", m.Version, m.Name)
	}
}
