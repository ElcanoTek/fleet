package admincli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
)

const migrateUsage = "usage: fleet migrate status [--database-url <dsn>] [--json]"

// cmdMigrate implements `fleet migrate <subcommand>`. Only the READ-ONLY
// `status` subcommand exists today: it reports applied vs pending migrations for
// both the chat and sched databases without touching either schema. Automated
// rollback and pre-migration backup are a documented follow-on (see
// docs/MIGRATIONS.md) — the chat DB deliberately ships no down-migrations, so a
// destructive rollback path warrants its own review.
func cmdMigrate(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 1
	}
	switch argv[0] {
	case "status":
		return cmdMigrateStatus(argv[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, migrateUsage)
		return 0
	default:
		return errf(1, "unknown migrate subcommand %q (only \"status\" is supported)", argv[0])
	}
}

// cmdMigrateStatus opens each database read-only and prints (or emits as JSON)
// its applied vs pending migrations. A database whose DSN is unset is skipped
// with a note rather than treated as an error; a DSN that IS set but fails to
// connect or query is a hard error. Exit 0 when at least one DB reported cleanly
// and none errored; 1 otherwise.
func cmdMigrateStatus(argv []string) int {
	fs := flag.NewFlagSet("migrate status", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN override for both DBs (default per-DB FLEET_*_DATABASE_URL / DATABASE_URL)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	ctx := context.Background()

	var (
		chat  *store.MigrationReport
		sched *scheddb.MigrationReport
		exit  int
	)

	if dsn, err := chatDSN(*dbURL); err != nil {
		fmt.Fprintf(os.Stderr, "chat DB: %v\n", err)
	} else if rep, err := chatMigrationStatus(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "chat DB: %v\n", err)
		exit = 1
	} else {
		chat = &rep
	}

	if dsn, err := schedDSN(*dbURL); err != nil {
		fmt.Fprintf(os.Stderr, "sched DB: %v\n", err)
	} else if rep, err := schedMigrationStatus(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "sched DB: %v\n", err)
		exit = 1
	} else {
		sched = &rep
	}

	if chat == nil && sched == nil {
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Chat  *store.MigrationReport   `json:"chat,omitempty"`
			Sched *scheddb.MigrationReport `json:"sched,omitempty"`
		}{Chat: chat, Sched: sched})
		return exit
	}
	if chat != nil {
		printChatMigrations(*chat)
	}
	if sched != nil {
		printSchedMigrations(*sched)
	}
	return exit
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
		fmt.Printf("  DIRTY: a prior migration failed mid-flight — run `migrate force <version>` and restart (see docs/MIGRATIONS.md)\n")
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
