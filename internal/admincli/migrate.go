package admincli

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
)

const migrateUsage = "usage: fleet migrate status [--database-url <dsn>] [--json]\n" +
	"       fleet migrate force <version> [--database-url <dsn>] [--no-confirm]   (sched DB only)"

// cmdMigrate implements `fleet migrate <subcommand>`. `status` is READ-ONLY: it
// reports applied vs pending migrations for both the chat and sched databases
// without touching either schema. `force` is the sched DB's dirty-row repair —
// what the external golang-migrate CLI's `migrate force` did before the runner
// was in-housed. Automated rollback and pre-migration backup are a documented
// follow-on (see docs/MIGRATIONS.md) — the chat DB deliberately ships no
// down-migrations, so a destructive rollback path warrants its own review.
func cmdMigrate(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 1
	}
	switch argv[0] {
	case "status":
		return cmdMigrateStatus(argv[1:])
	case "force":
		return cmdMigrateForce(argv[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 0
	default:
		return errf(1, "unknown migrate subcommand %q (\"status\" or \"force\")", argv[0])
	}
}

// cmdMigrateForce sets the sched DB's tracking row to <version>, clean, without
// running any migration: after a migration failed mid-run (the row is dirty and
// fleet refuses to start), the operator inspects the database, repairs or
// rolls back what the failed file did, and records the last version that is
// actually applied (-1: none). It writes, so it asks first unless
// --no-confirm, and refuses a non-TTY without it.
func cmdMigrateForce(argv []string) int {
	fs := flag.NewFlagSet("migrate force", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive confirmation (for scripted repairs)")
	version, ok := parseForceArgs(fs, argv)
	if !ok {
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 1
	}
	dsn, err := schedDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	if code := confirmBulkMutation(*noConfirm, fmt.Sprintf("this will record sched migration version %d as applied and clean, running nothing.", version)); code != 0 {
		return code
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errf(1, "sched DB: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := scheddb.ForceVersion(ctx, db, version); err != nil {
		return errf(1, "sched DB: force %d: %v", version, err)
	}
	fmt.Printf("sched database: version forced to %d (clean). Restart fleet to apply the remaining migrations.\n", version)
	return 0
}

// parseForceArgs accepts the version before or after the flags; "-1" is a
// version, not a flag. ok is false for a missing, extra or non-integer
// version, or a bad flag.
func parseForceArgs(fs *flag.FlagSet, argv []string) (int, bool) {
	var positional, flags []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-1" || (a != "" && a[0] != '-'):
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
	if err != nil || v < -1 {
		return 0, false
	}
	return v, true
}

// cmdMigrateStatus opens each database read-only and prints (or emits as JSON)
// its applied vs pending migrations. A database whose DSN is unset is skipped
// with a note rather than treated as an error; a DSN that IS set but fails to
// connect or query is a hard error. Exit 0 when at least one DB reported cleanly
// and none errored; 1 otherwise. With --json the envelope is ALWAYS emitted —
// {"chat": {...}, "sched": {...}} with a per-DB "error" member in place of the
// report fields when that DB could not be read — so a CI consumer parsing the
// output sees which side failed and why instead of an empty stdout or a
// silently missing key (both of which it used to get).
func cmdMigrateStatus(argv []string) int {
	fs := flag.NewFlagSet("migrate status", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN override for both DBs (default per-DB FLEET_*_DATABASE_URL / DATABASE_URL)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	ctx := context.Background()

	var (
		chat     *store.MigrationReport
		sched    *scheddb.MigrationReport
		chatErr  error
		schedErr error
		exit     int
	)

	if dsn, err := chatDSN(*dbURL); err != nil {
		chatErr = err
	} else if rep, err := chatMigrationStatus(ctx, dsn); err != nil {
		chatErr = err
		exit = 1
	} else {
		chat = &rep
	}

	if dsn, err := schedDSN(*dbURL); err != nil {
		schedErr = err
	} else if rep, err := schedMigrationStatus(ctx, dsn); err != nil {
		schedErr = err
		exit = 1
	} else {
		sched = &rep
	}

	if chat == nil && sched == nil {
		exit = 1
	}

	if *asJSON {
		if code := printJSON(migrateStatusEnvelope(chat, chatErr, sched, schedErr)); code != 0 {
			return code
		}
		return exit
	}
	if chatErr != nil {
		fmt.Fprintf(os.Stderr, "chat DB: %v\n", chatErr)
	}
	if schedErr != nil {
		fmt.Fprintf(os.Stderr, "sched DB: %v\n", schedErr)
	}
	if chat != nil {
		printChatMigrations(*chat)
	}
	if sched != nil {
		printSchedMigrations(*sched)
	}
	return exit
}

// migrateStatusJSON is the --json envelope. Each DB member is the report's
// fields flattened (the embedded pointer) plus an "error" string when the DB
// could not be read; a failed DB is therefore {"error": "..."} rather than
// absent, and the envelope is emitted even when both fail. Two concrete
// wrappers rather than one generic: Go cannot embed a type parameter, and the
// flattening is the point.
type migrateStatusJSON struct {
	Chat  migrateChatJSON  `json:"chat"`
	Sched migrateSchedJSON `json:"sched"`
}

type migrateChatJSON struct {
	*store.MigrationReport
	Error string `json:"error,omitempty"`
}

type migrateSchedJSON struct {
	*scheddb.MigrationReport
	Error string `json:"error,omitempty"`
}

func migrateStatusEnvelope(chat *store.MigrationReport, chatErr error, sched *scheddb.MigrationReport, schedErr error) migrateStatusJSON {
	env := migrateStatusJSON{
		Chat:  migrateChatJSON{MigrationReport: chat},
		Sched: migrateSchedJSON{MigrationReport: sched},
	}
	if chatErr != nil {
		env.Chat.Error = chatErr.Error()
	}
	if schedErr != nil {
		env.Sched.Error = schedErr.Error()
	}
	return env
}

func chatMigrationStatus(ctx context.Context, dsn string) (store.MigrationReport, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return store.MigrationReport{}, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return store.MigrationReport{}, err
	}
	return store.MigrationStatusDB(ctx, db)
}

func schedMigrationStatus(ctx context.Context, dsn string) (scheddb.MigrationReport, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return scheddb.MigrationReport{}, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return scheddb.MigrationReport{}, err
	}
	return scheddb.MigrationStatus(ctx, db)
}

func printChatMigrations(r store.MigrationReport) {
	fmt.Printf("chat database (%s runner, table %s)\n", r.Runner, r.MigrationTable)
	fmt.Printf("  applied: %d   pending: %d\n", len(r.Applied), len(r.Pending))
	for _, m := range r.Applied {
		ts := ""
		if m.AppliedAt != nil {
			ts = time.Unix(*m.AppliedAt, 0).UTC().Format(time.RFC3339)
		}
		fmt.Printf("    [applied] %-44s %s\n", m.Name, ts)
	}
	for _, m := range r.Pending {
		fmt.Printf("    [PENDING] %s\n", m.Name)
	}
	fmt.Println()
}

func printSchedMigrations(r scheddb.MigrationReport) {
	cur := "none"
	if r.CurrentVersion != nil {
		cur = fmt.Sprintf("%d", *r.CurrentVersion)
	}
	fmt.Printf("sched database (%s runner, table %s)\n", r.Runner, r.MigrationTable)
	fmt.Printf("  current version: %s\n", cur)
	if r.Dirty {
		fmt.Printf("  DIRTY: a prior migration failed mid-flight — inspect the DB, then force-set the last good version:\n")
		fmt.Printf("         fleet migrate force <version>\n")
		fmt.Printf("         and restart fleet (see docs/MIGRATIONS.md)\n")
	}
	fmt.Printf("  applied: %d   pending: %d\n", len(r.Applied), len(r.Pending))
	for _, m := range r.Applied {
		fmt.Printf("    [applied] %s\n", m.Name)
	}
	for _, m := range r.Pending {
		fmt.Printf("    [PENDING] %s\n", m.Name)
	}
	fmt.Println()
}
