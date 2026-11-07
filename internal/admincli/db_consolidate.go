// LEGACY-DB-LAYOUT: remove after consolidation — this whole file, its
// _linux/_other helpers and its tests (docs/ONE-DATABASE.md, "Removing the
// legacy layout").

package admincli

// `fleet db consolidate` moves a legacy two-database install (chat and sched
// in two databases, ADR-0005) into ONE database with a chat and a sched
// schema (ADR-0098, docs/ONE-DATABASE.md). It is self-contained on purpose:
// once every box is consolidated it is deleted with the rest of the legacy
// layout.
//
// What it guarantees:
//
//   - Dry run by default: the full plan, the preflight verdicts and the exact
//     per-table row counts it will copy. Nothing changes without --apply.
//   - The source databases are never modified. Each is copied with CREATE
//     DATABASE … TEMPLATE into a scratch database, whose public schema is
//     renamed to the store's schema, dumped (pg_dump -Fc -n) and restored
//     into the target in one transaction (pg_restore --single-transaction
//     --no-owner --no-privileges --role=<owner>). The scratch copy is dropped.
//   - Verification before anything points at the target: exact per-table
//     row counts, sequence values and schema_migrations rows (source vs
//     target, and the sources recounted after the copy to prove nothing
//     wrote to them meanwhile), object ownership, then both stores opened
//     through the real store code in schema mode (their migrations must
//     already be applied) with a trivial read.
//   - The env file is rewritten only after verification passed, in one
//     atomic write, after a timestamped backup. Interrupt it anywhere before
//     that and the box is exactly as it was (plus, at worst, a partly filled
//     target, which a re-run refuses until --force-clean).
//   - No secret is printed: superuser work runs as the postgres OS user
//     (runuser -u postgres -- psql/pg_dump/pg_restore, peer auth, no
//     password), DSNs are shown redacted, and the target reuses the sched
//     DSN's role and password — no new secret is created.

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/systemd"
)

const dbUsage = `usage: fleet db consolidate [--apply] [--target-db fleet] [--force-clean] [--env-file <path>]
       fleet db layout [--env-file <path>]

  consolidate  move a legacy two-database install (chat + sched) into ONE database with
               schemas chat and sched. Dry run unless --apply. Run as root on the box with
               fleet stopped (systemctl stop fleet fleet-backup.timer fleet-maintenance.timer).
  layout       print the env file's database layout: "schemas" (one database) or "legacy"`

// cmdDB dispatches `fleet db <verb>`.
func cmdDB(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, dbUsage)
		return 1
	}
	switch argv[0] {
	case "consolidate":
		return cmdDBConsolidate(argv[1:])
	case "layout":
		return cmdDBLayout(argv[1:])
	case "-h", "--help", "help":
		fmt.Println(dbUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown db subcommand %q\n%s\n", argv[0], dbUsage)
		return 1
	}
}

// cmdDBLayout prints the layout the env file resolves to (the file, not the
// shell: it is what the service reads). update.sh and doctor.sh use it to
// recommend consolidation.
func cmdDBLayout(argv []string) int {
	fs := flag.NewFlagSet("db layout", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "env file (default: the server env file)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	vals, err := creds.ReadEnvValues(serverEnvFile(*envFile))
	if err != nil {
		return errf(5, "read env file: %v", err)
	}
	fmt.Println(string(config.ResolveDSNs(func(k string) string { return vals[k] }).Layout))
	return 0
}

// ── options, dependencies ───────────────────────────────────────────────────

type consolidateOptions struct {
	envFile    string
	targetDB   string
	apply      bool
	forceClean bool
}

// pgSuper is the superuser side of the move: psql, pg_dump and pg_restore as
// the postgres OS user. An interface so the plan and preflight logic can be
// tested with a scripted fake.
type pgSuper interface {
	// Check verifies the client binaries are present and runnable.
	Check(ctx context.Context) error
	// ClientMajor is pg_dump's major version.
	ClientMajor(ctx context.Context) (int, error)
	// Query runs one statement in db and returns its rows.
	Query(ctx context.Context, db, sql string) ([][]string, error)
	// Exec runs SQL (one or more statements) in db.
	Exec(ctx context.Context, db, sql string) error
	// DumpRestore copies schema from srcDB into dstDB in one transaction,
	// every object created as role.
	DumpRestore(ctx context.Context, srcDB, schema, dstDB, role string) error
}

type consolidateDeps struct {
	pg pgSuper
	// unitState is `systemctl is-active`'s word for a unit ("" without
	// systemd).
	unitState func(unit string) string
	// freeBytes is the free space of the filesystem holding path.
	freeBytes func(path string) (uint64, error)
	// verifyStores opens both stores through the real code against the
	// target and reads from each.
	verifyStores func(ctx context.Context, chatDSN, schedDSN string) error
	now          func() time.Time
	out          io.Writer
}

func cmdDBConsolidate(argv []string) int {
	fs := flag.NewFlagSet("db consolidate", flag.ContinueOnError)
	var o consolidateOptions
	fs.StringVar(&o.envFile, "env-file", "", "env file to read and, with --apply, rewrite (default: the server env file)")
	fs.StringVar(&o.targetDB, "target-db", "fleet", "target database name")
	fs.BoolVar(&o.apply, "apply", false, "perform the move (default: dry run — print the plan and change nothing)")
	fs.BoolVar(&o.forceClean, "force-clean", false, "drop the chat and sched schemas of a partly filled target left by an earlier failed run")
	fs.Usage = func() { fmt.Fprintln(fs.Output(), dbUsage); fs.PrintDefaults() }
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected argument %q\n%s", fs.Arg(0), dbUsage)
	}
	o.envFile = serverEnvFile(o.envFile)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	deps := consolidateDeps{
		pg: &execPG{prefix: []string{"runuser", "-u", "postgres", "--"}, needRoot: true},
		unitState: func(unit string) string {
			if _, err := exec.LookPath("systemctl"); err != nil {
				return ""
			}
			return systemd.ActiveState(context.Background(), unit)
		},
		freeBytes:    freeBytesAt,
		verifyStores: verifyStoresOpen,
		now:          time.Now,
		out:          os.Stdout,
	}
	return runConsolidate(ctx, o, deps)
}

// ── the plan ────────────────────────────────────────────────────────────────

// pgSource is one parsed source DSN.
type pgSource struct {
	label    string // "chat" / "sched"
	schema   string // the target schema it moves into
	dsn      string
	from     string // the env key that named it
	host     string // host as written ("" for a socket in the query)
	port     string
	database string
	role     string
}

type tableCount struct {
	table string
	rows  int64
}

// inventory is what one store holds: exact row counts, sequence values and
// the schema_migrations rows.
type inventory struct {
	tables     []tableCount
	sequences  map[string]string
	migrations string
}

type consolidatePlan struct {
	envFile        string
	chat, sched    pgSource
	target         string
	owner          string
	targetDSN      string
	targetExists   bool
	targetNonEmpty bool
	leftoverTmp    []string
	sourceSize     int64
	inv            map[string]inventory // keyed by label
	passwords      []string             // for redaction only
}

func (p *consolidatePlan) tmpDB(s pgSource) string { return p.target + "_consolidate_" + s.label }

// legacyEnvKeys are the per-store keys consolidation retires.
var legacyEnvKeys = []string{"FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL"}

var dbPartKeys = []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE"}

// check is one preflight verdict.
type check struct {
	ok     bool
	warn   bool
	label  string
	detail string
}

func runConsolidate(ctx context.Context, o consolidateOptions, d consolidateDeps) int {
	out := d.out
	mode := "DRY RUN — nothing will be changed; re-run with --apply to perform it"
	if o.apply {
		mode = "APPLY"
	}
	fmt.Fprintf(out, "fleet db consolidate — %s\n\n", mode)

	plan, code, err := buildPlan(o, d)
	if err != nil {
		return errf(code, "%v", err)
	}
	if plan == nil { // already one database
		return 0
	}
	printSources(out, plan)

	checks := preflight(ctx, o, d, plan)
	fmt.Fprintln(out, "\nPreflight:")
	failed := 0
	for _, c := range checks {
		mark := "✓"
		switch {
		case !c.ok:
			mark, failed = "✗", failed+1
		case c.warn:
			mark = "!"
		}
		fmt.Fprintf(out, "  %s %s", mark, c.label)
		if c.detail != "" {
			detail := c.detail
			if !c.ok || c.warn {
				// Failures carry tool and driver output; scrub it. (Our own
				// verdicts name databases and roles, never a password.)
				detail = plan.redact(detail)
			}
			fmt.Fprintf(out, " — %s", detail)
		}
		fmt.Fprintln(out)
	}
	if plan.inv != nil {
		printInventory(out, plan)
	}
	printSteps(out, o, plan)
	if failed > 0 {
		fmt.Fprintf(out, "\nRefusing: %d preflight check(s) failed. Nothing was changed.\n", failed)
		return 1
	}
	if !o.apply {
		fmt.Fprintln(out, "\nDry run complete: preflight passed, nothing was changed. Re-run with --apply to perform it.")
		return 0
	}
	if err := applyConsolidation(ctx, o, d, plan); err != nil {
		fmt.Fprintf(out, "\nFAILED: %s\n", plan.redact(err.Error()))
		fmt.Fprintln(out, "The env file was NOT changed and the source databases were not touched: the box still runs on")
		fmt.Fprintln(out, "the legacy layout. Start fleet again as it was, fix the cause, and re-run (a partly filled")
		fmt.Fprintf(out, "target is refused until you pass --force-clean, which drops only its chat and sched schemas).\n")
		return 5
	}
	return 0
}

// buildPlan resolves the env file's DSNs and the target. A nil plan with a
// nil error means the box already runs one database.
func buildPlan(o consolidateOptions, d consolidateDeps) (*consolidatePlan, int, error) {
	if _, err := os.Stat(o.envFile); err != nil {
		return nil, 1, fmt.Errorf("env file %s: %w", o.envFile, err)
	}
	vals, err := creds.ReadEnvValues(o.envFile)
	if err != nil {
		return nil, 5, fmt.Errorf("read env file %s: %w", o.envFile, err)
	}
	for _, k := range append(append([]string{"DATABASE_URL"}, legacyEnvKeys...), dbPartKeys...) {
		if os.Getenv(k) != "" {
			fmt.Fprintf(d.out, "note: %s is set in this shell; ignored — consolidation reads only %s, which is what the service reads\n", k, o.envFile)
		}
	}
	dsns := config.ResolveDSNs(func(k string) string { return vals[k] })
	if dsns.Layout != config.LayoutLegacy {
		fmt.Fprintf(d.out, "%s already configures ONE database (schemas %s and %s). Nothing to do.\n", o.envFile, config.ChatSchema, config.SchedSchema)
		return nil, 0, nil
	}
	if !dsns.ChatConfigured() || !dsns.SchedConfigured() {
		return nil, 1, fmt.Errorf("%s configures no database (the bare localhost defaults would be used); set the per-store DSNs it really uses first", o.envFile)
	}
	chat, err := parseSource("chat", config.ChatSchema, dsns.Chat, dsns.ChatFrom)
	if err != nil {
		return nil, 1, err
	}
	sched, err := parseSource("sched", config.SchedSchema, dsns.Sched, dsns.SchedFrom)
	if err != nil {
		return nil, 1, err
	}
	if chat.host != sched.host || chat.port != sched.port {
		return nil, 1, fmt.Errorf("chat (%s:%s) and sched (%s:%s) are on different servers; consolidation moves two databases of ONE local server", chat.host, chat.port, sched.host, sched.port)
	}
	if !localHost(sched.host) {
		return nil, 1, fmt.Errorf("the databases are on %q, not this box; consolidation runs on the box that hosts Postgres", sched.host)
	}
	if !config.PlainIdentifier(o.targetDB) {
		return nil, 1, fmt.Errorf("--target-db %q: use a lower-case name of letters, digits and _", o.targetDB)
	}
	for _, s := range []pgSource{chat, sched} {
		if !config.PlainIdentifier(s.database) {
			return nil, 1, fmt.Errorf("%s database name %q is not a plain lower-case identifier; consolidate it by hand (docs/ONE-DATABASE.md)", s.label, s.database)
		}
		if s.database == o.targetDB {
			return nil, 1, fmt.Errorf("the target database %q is the %s source; choose another with --target-db", o.targetDB, s.label)
		}
	}
	if sched.role == "" {
		return nil, 1, errors.New("the sched DSN names no user; the target is owned by the sched DSN's role, so it must name one")
	}
	p := &consolidatePlan{
		envFile: o.envFile, chat: chat, sched: sched, target: o.targetDB, owner: sched.role,
	}
	p.targetDSN, err = targetDSN(sched.dsn, o.targetDB)
	if err != nil {
		return nil, 1, err
	}
	for _, s := range []string{chat.dsn, sched.dsn} {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				p.passwords = append(p.passwords, pw)
			}
		}
	}
	return p, 0, nil
}

// parseSource parses one legacy source DSN (URL form only: the on-box
// installs bootstrap.sh writes are all URLs).
func parseSource(label, schema, dsn, from string) (pgSource, error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return pgSource{}, fmt.Errorf("the %s DSN (%s) is not a postgres:// URL; consolidate supports URL DSNs only", label, from)
	}
	s := pgSource{label: label, schema: schema, dsn: dsn, from: from}
	s.database = strings.TrimPrefix(u.Path, "/")
	if s.database == "" {
		return pgSource{}, fmt.Errorf("the %s DSN (%s) names no database", label, from)
	}
	if u.User != nil {
		s.role = u.User.Username()
	}
	s.host, s.port = u.Hostname(), u.Port()
	if s.host == "" {
		s.host, s.port = u.Query().Get("host"), u.Query().Get("port")
	}
	if s.port == "" {
		s.port = "5432"
	}
	return s, nil
}

func localHost(h string) bool {
	if h == "" || strings.HasPrefix(h, "/") || h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// targetDSN is the sched DSN pointed at the target database: same role,
// password, host, port and options.
func targetDSN(schedDSN, target string) (string, error) {
	u, err := url.Parse(schedDSN)
	if err != nil {
		return "", err
	}
	u.Path = "/" + target
	u.RawPath = ""
	return u.String(), nil
}

func (p *consolidatePlan) redact(s string) string {
	for _, pw := range p.passwords {
		s = strings.ReplaceAll(s, pw, "<password>")
		s = strings.ReplaceAll(s, url.QueryEscape(pw), "<password>")
		s = strings.ReplaceAll(s, url.PathEscape(pw), "<password>")
	}
	return s
}

// ── preflight ───────────────────────────────────────────────────────────────

func preflight(ctx context.Context, o consolidateOptions, d consolidateDeps, p *consolidatePlan) []check {
	var cs []check
	add := func(ok bool, label, detail string) { cs = append(cs, check{ok: ok, label: label, detail: detail}) }
	warn := func(label, detail string) { cs = append(cs, check{ok: true, warn: true, label: label, detail: detail}) }

	// fleet and the units that touch the databases must be stopped.
	units := []string{serviceName("") + ".service", "fleet-backup.service", "fleet-maintenance.service", "fleet-backup.timer", "fleet-maintenance.timer"}
	var running []string
	systemdSeen := false
	for _, u := range units {
		st := d.unitState(u)
		if st != "" {
			systemdSeen = true
		}
		switch st {
		case "active", "activating", "reloading", "deactivating":
			running = append(running, u+" ("+st+")")
		}
	}
	switch {
	case !systemdSeen:
		warn("fleet services stopped", "systemd not available; relying on the connection check below")
	case len(running) > 0:
		add(false, "fleet services stopped", "still running: "+strings.Join(running, ", ")+" — stop them: systemctl stop "+strings.Join(stripStates(running), " "))
	default:
		add(true, "fleet services stopped", strings.Join(units, ", ")+" inactive")
	}

	if err := d.pg.Check(ctx); err != nil {
		add(false, "postgres client tools", err.Error())
		return cs // nothing below can run
	}
	add(true, "postgres client tools", "psql, pg_dump, pg_restore run as the postgres user")

	q := func(db, sql string) ([][]string, error) { return d.pg.Query(ctx, db, sql) }
	// q1 is q for a query that must return exactly one row of cols columns.
	q1 := func(db, sql string, cols int) ([]string, error) {
		rows, err := q(db, sql)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || len(rows[0]) != cols {
			return nil, fmt.Errorf("unexpected result from %s: %d row(s)", db, len(rows))
		}
		return rows[0], nil
	}

	// The server is reachable and pg_dump is new enough for it.
	rows, err := q("postgres", `SHOW server_version_num`)
	if err != nil || len(rows) != 1 {
		add(false, "server reachable as superuser", fmt.Sprintf("%v", err))
		return cs
	}
	serverNum, _ := strconv.Atoi(rows[0][0])
	clientMajor, err := d.pg.ClientMajor(ctx)
	switch {
	case err != nil:
		add(false, "pg_dump version", err.Error())
	case clientMajor < serverNum/10000:
		add(false, "pg_dump version", fmt.Sprintf("pg_dump %d is older than the server (%d); install the matching client", clientMajor, serverNum/10000))
	default:
		add(true, "pg_dump version", fmt.Sprintf("client %d, server %d", clientMajor, serverNum/10000))
	}

	// The databases and the role.
	names := []string{p.chat.database, p.sched.database, p.target, p.tmpDB(p.chat), p.tmpDB(p.sched)}
	rows, err = q("postgres", `SELECT datname, pg_get_userbyid(datdba) FROM pg_database WHERE datname IN (`+sqlList(names)+`)`)
	if err != nil {
		add(false, "list databases", err.Error())
		return cs
	}
	exists := map[string]string{}
	for _, r := range rows {
		exists[r[0]] = r[1]
	}
	srcOK := true
	for _, s := range []pgSource{p.chat, p.sched} {
		if _, ok := exists[s.database]; !ok {
			add(false, s.label+" source exists", fmt.Sprintf("database %q not found", s.database))
			srcOK = false
		}
	}
	rows, err = q("postgres", `SELECT 1 FROM pg_roles WHERE rolname = `+sqlLit(p.owner))
	add(err == nil && len(rows) == 1, "target owner role exists", fmt.Sprintf("role %q (the sched DSN's user)", p.owner))

	// No client connections: not to the sources (CREATE DATABASE … TEMPLATE
	// needs that, and a writer would make the copy stale), not to the target.
	rows, err = q("postgres", `SELECT datname, usename, coalesce(application_name, ''), count(*) FROM pg_stat_activity
		WHERE datname IN (`+sqlList([]string{p.chat.database, p.sched.database, p.target})+`) AND pid <> pg_backend_pid()
		GROUP BY 1, 2, 3 ORDER BY 1`)
	if err != nil {
		add(false, "no client connections", err.Error())
	} else if len(rows) > 0 {
		var who []string
		for _, r := range rows {
			who = append(who, fmt.Sprintf("%s: %s connection(s) as %s (%s)", r[0], r[3], r[1], r[2]))
		}
		add(false, "no client connections", strings.Join(who, "; ")+" — stop whatever holds them")
	} else {
		add(true, "no client connections", "none to "+p.chat.database+", "+p.sched.database+" or "+p.target)
	}

	// The sources are legacy fleet stores, not already in schema mode, and
	// the sched one is not dirty.
	if srcOK {
		for _, s := range []pgSource{p.chat, p.sched} {
			row, err := q1(s.database, `SELECT to_regclass('public.schema_migrations') IS NOT NULL,
				(SELECT count(*) FROM pg_namespace WHERE nspname IN ('chat', 'sched'))`, 2)
			switch {
			case err != nil:
				add(false, s.label+" source is a legacy store", err.Error())
			case row[0] != "t":
				add(false, s.label+" source is a legacy store", fmt.Sprintf("%q has no public.schema_migrations — not a fleet store", s.database))
			case row[1] != "0":
				add(false, s.label+" source is a legacy store", fmt.Sprintf("%q already has a chat or sched schema", s.database))
			default:
				add(true, s.label+" source is a legacy store", fmt.Sprintf("%q, tables in public", s.database))
			}
		}
		rows, err := q(p.sched.database, `SELECT coalesce(bool_or(dirty), false) FROM public.schema_migrations`)
		if err == nil && len(rows) == 1 && rows[0][0] == "t" {
			add(false, "sched migrations clean", "the sched database is DIRTY (a migration failed); fix it with `fleet migrate force` first")
		}
	}

	// The target is free.
	if owner, ok := exists[p.target]; ok {
		p.targetExists = true
		if owner != p.owner {
			add(false, "target database is usable", fmt.Sprintf("%q exists but is owned by %q, not %q", p.target, owner, p.owner))
		} else {
			row, err := q1(p.target, `SELECT
				(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('chat', 'sched'))
				  + (SELECT count(*) FROM pg_namespace WHERE nspname IN ('chat', 'sched')),
				to_regclass('public.schema_migrations') IS NOT NULL,
				(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p'))`, 3)
			switch {
			case err != nil:
				add(false, "target database is free", err.Error())
			case row[1] == "t" || row[2] != "0":
				add(false, "target database is free", fmt.Sprintf("%q has tables in public (a fleet store or something else); choose another --target-db", p.target))
			case row[0] != "0" && !o.forceClean:
				p.targetNonEmpty = true
				add(false, "target database is free", fmt.Sprintf("%q already has chat/sched schemas (%s objects) — a partly filled target from an earlier run? Re-run with --force-clean to drop those two schemas (the env file still names the sources, so nothing uses them)", p.target, row[0]))
			case row[0] != "0":
				p.targetNonEmpty = true
				warn("target database is free", fmt.Sprintf("%q has chat/sched schemas (%s objects); --force-clean will drop those two schemas first", p.target, row[0]))
			default:
				add(true, "target database is free", fmt.Sprintf("%q exists, owned by %q, empty", p.target, p.owner))
			}
		}
	} else {
		add(true, "target database is free", fmt.Sprintf("%q does not exist; it will be created owned by %q", p.target, p.owner))
	}
	for _, s := range []pgSource{p.chat, p.sched} {
		if _, ok := exists[p.tmpDB(s)]; ok {
			p.leftoverTmp = append(p.leftoverTmp, p.tmpDB(s))
		}
	}
	if len(p.leftoverTmp) > 0 {
		warn("scratch databases", "left over from an interrupted run, will be dropped first: "+strings.Join(p.leftoverTmp, ", "))
	}

	// Disk: the scratch copy of the larger source plus the target need about
	// the sources' size twice over; ask for that plus a margin.
	if srcOK {
		rows, err := q("postgres", `SELECT pg_database_size(`+sqlLit(p.chat.database)+`) + pg_database_size(`+sqlLit(p.sched.database)+`), current_setting('data_directory')`)
		if err != nil || len(rows) != 1 {
			warn("disk space", fmt.Sprintf("could not measure: %v", err))
		} else {
			p.sourceSize, _ = strconv.ParseInt(rows[0][0], 10, 64)
			need := uint64(p.sourceSize)*2 + uint64(p.sourceSize)/5 + 64<<20
			free, ferr := d.freeBytes(rows[0][1])
			switch {
			case ferr != nil:
				warn("disk space", fmt.Sprintf("could not measure free space at %s: %v (need about %s)", rows[0][1], ferr, consolidateBytes(need)))
			case free < need:
				add(false, "disk space", fmt.Sprintf("%s free at %s, need about %s", consolidateBytes(free), rows[0][1], consolidateBytes(need)))
			default:
				add(true, "disk space", fmt.Sprintf("%s free, need about %s", consolidateBytes(free), consolidateBytes(need)))
			}
		}
		// The inventory the dry run shows and the copy is verified against.
		p.inv = map[string]inventory{}
		for _, s := range []pgSource{p.chat, p.sched} {
			inv, err := takeInventory(ctx, d.pg, s.database, config.PublicSchema)
			if err != nil {
				add(false, s.label+" inventory", err.Error())
				p.inv = nil
				break
			}
			p.inv[s.label] = inv
		}
	}
	return cs
}

func stripStates(units []string) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i], _, _ = strings.Cut(u, " ")
	}
	return out
}

// takeInventory reads exact row counts, sequence values and the
// schema_migrations rows of schema in db.
func takeInventory(ctx context.Context, pg pgSuper, db, schema string) (inventory, error) {
	inv := inventory{sequences: map[string]string{}}
	rows, err := pg.Query(ctx, db, `SELECT c.relname,
		(xpath('/row/n/text()', query_to_xml(format('SELECT count(*) AS n FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = `+sqlLit(schema)+` AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		return inv, fmt.Errorf("count rows in %s.%s: %w", db, schema, err)
	}
	for _, r := range rows {
		if len(r) != 2 {
			return inv, fmt.Errorf("count rows in %s.%s: unexpected result %q", db, schema, r)
		}
		n, err := strconv.ParseInt(r[1], 10, 64)
		if err != nil {
			return inv, fmt.Errorf("count rows in %s.%s.%s: %q", db, schema, r[0], r[1])
		}
		inv.tables = append(inv.tables, tableCount{table: r[0], rows: n})
	}
	rows, err = pg.Query(ctx, db, `SELECT sequencename, coalesce(last_value::text, 'unused') FROM pg_sequences WHERE schemaname = `+sqlLit(schema)+` ORDER BY 1`)
	if err != nil {
		return inv, fmt.Errorf("read sequences in %s.%s: %w", db, schema, err)
	}
	for _, r := range rows {
		if len(r) != 2 {
			return inv, fmt.Errorf("read sequences in %s.%s: unexpected result %q", db, schema, r)
		}
		inv.sequences[r[0]] = r[1]
	}
	rows, err = pg.Query(ctx, db, `SELECT coalesce(string_agg(t::text, ',' ORDER BY t::text), '') FROM `+quoteIdent(schema)+`.schema_migrations t`)
	if err != nil {
		return inv, fmt.Errorf("read %s.%s.schema_migrations: %w", db, schema, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return inv, fmt.Errorf("read %s.%s.schema_migrations: unexpected result", db, schema)
	}
	inv.migrations = rows[0][0]
	return inv, nil
}

// diffInventory lists every difference between want (a source) and got (its
// copy).
func diffInventory(label string, want, got inventory) []string {
	var diffs []string
	gotRows := map[string]int64{}
	for _, t := range got.tables {
		gotRows[t.table] = t.rows
	}
	for _, t := range want.tables {
		n, ok := gotRows[t.table]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s.%s missing", label, t.table))
		case n != t.rows:
			diffs = append(diffs, fmt.Sprintf("%s.%s has %d rows, source %d", label, t.table, n, t.rows))
		}
		delete(gotRows, t.table)
	}
	for t := range gotRows {
		diffs = append(diffs, fmt.Sprintf("%s.%s is not in the source", label, t))
	}
	for name, v := range want.sequences {
		if g, ok := got.sequences[name]; !ok || g != v {
			diffs = append(diffs, fmt.Sprintf("%s sequence %s is %q, source %q", label, name, g, v))
		}
	}
	if len(got.sequences) != len(want.sequences) {
		diffs = append(diffs, fmt.Sprintf("%s has %d sequences, source %d", label, len(got.sequences), len(want.sequences)))
	}
	if want.migrations != got.migrations {
		diffs = append(diffs, label+".schema_migrations differs from the source")
	}
	sort.Strings(diffs)
	return diffs
}

// ── output ──────────────────────────────────────────────────────────────────

func printSources(out io.Writer, p *consolidatePlan) {
	fmt.Fprintf(out, "Env file:  %s\n", p.envFile)
	fmt.Fprintln(out, "Sources (never modified; left in place afterwards):")
	for _, s := range []pgSource{p.chat, p.sched} {
		fmt.Fprintf(out, "  %-5s database %q, role %q, from %s  %s\n", s.label, s.database, s.role, s.from, redactDSN(s.dsn))
	}
	fmt.Fprintln(out, "Target:")
	fmt.Fprintf(out, "  database %q on the same server, owned by role %q (the sched DSN's role and password: no new secret)\n", p.target, p.owner)
	fmt.Fprintf(out, "  chat -> schema %q, sched -> schema %q\n", config.ChatSchema, config.SchedSchema)
	fmt.Fprintf(out, "  DATABASE_URL=%s\n", redactDSN(p.targetDSN))
}

func printInventory(out io.Writer, p *consolidatePlan) {
	fmt.Fprintln(out, "\nRows to copy (exact counts; verified against the target after the copy):")
	for _, s := range []pgSource{p.chat, p.sched} {
		inv := p.inv[s.label]
		var total int64
		for _, t := range inv.tables {
			total += t.rows
		}
		fmt.Fprintf(out, "  %s: %d tables, %d rows, %d sequences\n", s.label, len(inv.tables), total, len(inv.sequences))
		for _, t := range inv.tables {
			fmt.Fprintf(out, "    %-40s %12d\n", s.schema+"."+t.table, t.rows)
		}
	}
	if p.sourceSize > 0 {
		fmt.Fprintf(out, "  on disk: %s\n", consolidateBytes(uint64(p.sourceSize)))
	}
}

func printSteps(out io.Writer, o consolidateOptions, p *consolidatePlan) {
	fmt.Fprintln(out, "\nSteps (--apply):")
	n := 0
	step := func(f string, a ...any) { n++; fmt.Fprintf(out, "  %2d. %s\n", n, fmt.Sprintf(f, a...)) }
	if len(p.leftoverTmp) > 0 {
		step("DROP DATABASE %s (scratch copies left by an interrupted run)", strings.Join(p.leftoverTmp, ", "))
	}
	if p.targetExists {
		if p.targetNonEmpty && o.forceClean {
			step("in %s: DROP SCHEMA chat, sched CASCADE (--force-clean)", p.target)
		}
	} else {
		step("CREATE DATABASE %s OWNER %s", p.target, p.owner)
	}
	for _, s := range []pgSource{p.chat, p.sched} {
		tmp := p.tmpDB(s)
		step("CREATE DATABASE %s TEMPLATE %s  (a scratch copy; %s itself is never modified)", tmp, s.database, s.database)
		step("in %s: ALTER SCHEMA public RENAME TO %s", tmp, s.schema)
		step("pg_dump -Fc -n %s %s | pg_restore --single-transaction --no-owner --no-privileges --role=%s -d %s", s.schema, tmp, p.owner, p.target)
		step("DROP DATABASE %s", tmp)
	}
	step("verify: per-table row counts, sequence values, schema_migrations (target vs source); sources unchanged; every object owned by %s", p.owner)
	step("verify: open both stores through fleet's own code against %s (no migration may be pending) and read from each", p.target)
	step("back up %s to %s.pre-consolidate-<timestamp>", p.envFile, filepath.Base(p.envFile))
	step("set DATABASE_URL to %s; comment out %s", p.target, strings.Join(retiredKeys(p), ", "))
}

func retiredKeys(p *consolidatePlan) []string {
	keys := append([]string(nil), legacyEnvKeys...)
	if p.chat.from == config.DSNFromDBParts || p.sched.from == config.DSNFromDBParts {
		keys = append(keys, dbPartKeys...)
	}
	return keys
}

func consolidateBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ── apply ───────────────────────────────────────────────────────────────────

func applyConsolidation(ctx context.Context, o consolidateOptions, d consolidateDeps, p *consolidatePlan) (err error) {
	out := d.out
	logf := func(f string, a ...any) { fmt.Fprintf(out, "==> "+f+"\n", a...) }
	run := func(db, sql string) error { return d.pg.Exec(ctx, db, sql) }

	fmt.Fprintln(out)
	for _, tmp := range p.leftoverTmp {
		logf("dropping leftover scratch database %s", tmp)
		if err := run("postgres", `DROP DATABASE IF EXISTS `+quoteIdent(tmp)); err != nil {
			return fmt.Errorf("drop %s: %w", tmp, err)
		}
	}
	if !p.targetExists {
		logf("creating database %s owned by %s", p.target, p.owner)
		if err := run("postgres", `CREATE DATABASE `+quoteIdent(p.target)+` OWNER `+quoteIdent(p.owner)); err != nil {
			return fmt.Errorf("create %s: %w", p.target, err)
		}
	} else if p.targetNonEmpty && o.forceClean {
		logf("--force-clean: dropping schemas chat and sched in %s", p.target)
		if err := run(p.target, `DROP SCHEMA IF EXISTS chat CASCADE; DROP SCHEMA IF EXISTS sched CASCADE;`); err != nil {
			return fmt.Errorf("clean %s: %w", p.target, err)
		}
	}

	for _, s := range []pgSource{p.chat, p.sched} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := copyStore(ctx, d, p, s, logf); err != nil {
			return err
		}
	}
	if err := run(p.target, `ANALYZE`); err != nil {
		return fmt.Errorf("analyze %s: %w", p.target, err)
	}

	logf("verifying the copy")
	if err := verifyCopy(ctx, d, p); err != nil {
		return err
	}
	logf("opening both stores through fleet's code against %s", p.target)
	td := config.ResolveDSNs(func(k string) string {
		if k == "DATABASE_URL" {
			return p.targetDSN
		}
		return ""
	})
	if err := d.verifyStores(ctx, td.Chat, td.Sched); err != nil {
		return fmt.Errorf("store check against %s: %w", p.target, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	backup, err := rewriteEnvFile(p, d.now())
	if err != nil {
		return err
	}
	stamp := d.now().UTC().Format("2006-01-02")
	fmt.Fprintf(out, `
Done. %s now sets DATABASE_URL to database %q (schemas chat and sched).
  backup of the previous env file: %s

Next:
  systemctl start %s fleet-backup.timer fleet-maintenance.timer   (whichever you stopped)
  fleet status                         (both stores reachable; the server logs no legacy-layout notice)
  fleet backup                         (a first backup of the new layout)

Rollback (any time before you drop the old databases):
  systemctl stop %s
  cp -p %s %s
  systemctl start %s
  The old databases %q and %q were never modified, so this returns the box exactly to
  its state before the move. Anything written after the switch exists only in %q and
  is NOT carried back.

Once satisfied, drop the old databases (irreversible):
  runuser -u postgres -- psql -c 'DROP DATABASE %s' -c 'DROP DATABASE %s'
`, p.envFile, p.target, backup,
		serviceName(""), serviceName(""), backup, p.envFile, serviceName(""),
		p.chat.database, p.sched.database, p.target,
		quoteIdent(p.chat.database), quoteIdent(p.sched.database))
	if p.chat.role != "" && p.chat.role != p.owner {
		fmt.Fprintf(out, "  and the chat role, if nothing else uses it: runuser -u postgres -- psql -c 'DROP ROLE %s'\n", quoteIdent(p.chat.role))
	}
	fmt.Fprintf(out, "  and the commented-out keys in %s (marked %s) once you are sure.\n", p.envFile, stamp)
	return nil
}

// copyStore moves one source into its schema of the target through a
// scratch copy, which it always drops again.
func copyStore(ctx context.Context, d consolidateDeps, p *consolidatePlan, s pgSource, logf func(string, ...any)) (err error) {
	tmp := p.tmpDB(s)
	logf("%s: copying database %s to scratch database %s", s.label, s.database, tmp)
	if err := d.pg.Exec(ctx, "postgres", `CREATE DATABASE `+quoteIdent(tmp)+` TEMPLATE `+quoteIdent(s.database)); err != nil {
		return fmt.Errorf("copy %s to %s (is something connected to %s?): %w", s.database, tmp, s.database, err)
	}
	defer func() {
		// Background context: an interrupt must still remove the scratch copy.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if derr := d.pg.Exec(cctx, "postgres", `DROP DATABASE IF EXISTS `+quoteIdent(tmp)); derr != nil {
			err = errors.Join(err, fmt.Errorf("drop scratch database %s (drop it by hand): %w", tmp, derr))
		}
	}()
	if err := d.pg.Exec(ctx, tmp, `ALTER SCHEMA public RENAME TO `+quoteIdent(s.schema)+`; COMMENT ON SCHEMA `+quoteIdent(s.schema)+` IS NULL;`); err != nil {
		return fmt.Errorf("rename schema in %s: %w", tmp, err)
	}
	logf("%s: restoring schema %s into %s as role %s", s.label, s.schema, p.target, p.owner)
	if err := d.pg.DumpRestore(ctx, tmp, s.schema, p.target, p.owner); err != nil {
		return fmt.Errorf("copy schema %s into %s: %w", s.schema, p.target, err)
	}
	return nil
}

// verifyCopy compares the target with the inventory taken before the copy
// and recounts the sources, which must not have changed.
func verifyCopy(ctx context.Context, d consolidateDeps, p *consolidatePlan) error {
	var diffs []string
	for _, s := range []pgSource{p.chat, p.sched} {
		want := p.inv[s.label]
		got, err := takeInventory(ctx, d.pg, p.target, s.schema)
		if err != nil {
			return err
		}
		diffs = append(diffs, diffInventory(s.schema, want, got)...)
		again, err := takeInventory(ctx, d.pg, s.database, config.PublicSchema)
		if err != nil {
			return err
		}
		if dd := diffInventory("source "+s.database, want, again); len(dd) > 0 {
			diffs = append(diffs, "the "+s.label+" source changed during the copy (something is still writing to it): "+strings.Join(dd, "; "))
		}
	}
	rows, err := d.pg.Query(ctx, p.target, `SELECT kind || ' ' || name FROM (
		SELECT 'schema' AS kind, nspname AS name, nspowner AS owner FROM pg_namespace WHERE nspname IN ('chat', 'sched')
		UNION ALL SELECT 'relation', n.nspname || '.' || c.relname, c.relowner FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('chat', 'sched')
		UNION ALL SELECT 'type', n.nspname || '.' || t.typname, t.typowner FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname IN ('chat', 'sched')
		UNION ALL SELECT 'function', n.nspname || '.' || p.proname, p.proowner FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname IN ('chat', 'sched')
	) o WHERE pg_get_userbyid(owner) <> `+sqlLit(p.owner)+` ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("check ownership: %w", err)
	}
	for _, r := range rows {
		diffs = append(diffs, r[0]+" is not owned by "+p.owner)
	}
	rows, err = d.pg.Query(ctx, p.target, `SELECT count(*) AS public_objects FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'`)
	if err != nil {
		return fmt.Errorf("check public: %w", err)
	}
	if len(rows) != 1 || rows[0][0] != "0" {
		diffs = append(diffs, "the target's public schema is not empty")
	}
	if len(diffs) > 0 {
		return fmt.Errorf("verification failed:\n  %s", strings.Join(diffs, "\n  "))
	}
	for _, s := range []pgSource{p.chat, p.sched} {
		var total int64
		for _, t := range p.inv[s.label].tables {
			total += t.rows
		}
		fmt.Fprintf(d.out, "    %s: %d tables, %d rows, %d sequences, schema_migrations — identical\n", s.schema, len(p.inv[s.label].tables), total, len(p.inv[s.label].sequences))
	}
	return nil
}

// rewriteEnvFile backs the env file up, then in ONE atomic write sets
// DATABASE_URL to the target and comments out the retired keys. It re-reads
// the result and restores the backup if it does not resolve to the target in
// the one-database layout.
func rewriteEnvFile(p *consolidatePlan, now time.Time) (string, error) {
	info, err := os.Stat(p.envFile)
	if err != nil {
		return "", err
	}
	backup := fmt.Sprintf("%s.pre-consolidate-%s", p.envFile, now.UTC().Format("20060102T150405Z"))
	if err := copyFileKeepMode(p.envFile, backup, info); err != nil {
		return "", fmt.Errorf("back up %s: %w", p.envFile, err)
	}
	newLine, err := creds.EnvLine("DATABASE_URL", p.targetDSN)
	if err != nil {
		return "", err
	}
	retire := map[string]bool{"DATABASE_URL": true}
	for _, k := range retiredKeys(p) {
		retire[k] = true
	}
	note := fmt.Sprintf("# [fleet db consolidate %s: now DATABASE_URL, database %s] ", now.UTC().Format("2006-01-02"), p.target)
	err = creds.EditEnvLines(p.envFile, func(lines []string) ([]string, error) {
		out := make([]string, 0, len(lines)+2)
		placed := false
		for _, ln := range lines {
			k, _, ok := creds.ParseEnvLine(ln)
			if !ok || !retire[k] {
				out = append(out, ln)
				continue
			}
			out = append(out, note+ln)
			if !placed {
				out = append(out, newLine)
				placed = true
			}
		}
		if !placed {
			out = append(out, "", "# fleet db consolidate "+now.UTC().Format("2006-01-02")+": one database, schemas chat and sched (docs/ONE-DATABASE.md)", newLine)
		}
		return out, nil
	})
	if err != nil {
		return backup, fmt.Errorf("rewrite %s (the backup %s is intact): %w", p.envFile, backup, err)
	}
	vals, err := creds.ReadEnvValues(p.envFile)
	if err == nil {
		d := config.ResolveDSNs(func(k string) string { return vals[k] })
		if d.Layout != config.LayoutSchemas || config.DSNIdentity(d.Chat) != config.DSNIdentity(p.targetDSN) {
			err = fmt.Errorf("the rewritten env file resolves to %s, not to %s", d.Layout, p.target)
		}
	}
	if err != nil {
		if rerr := copyFileKeepMode(backup, p.envFile, info); rerr != nil {
			return backup, fmt.Errorf("%w; RESTORING THE BACKUP ALSO FAILED (copy %s back by hand): %v", err, backup, rerr)
		}
		return backup, fmt.Errorf("%w; the previous env file was restored", err)
	}
	return backup, nil
}

// copyFileKeepMode copies src to dst (0600) atomically and gives it like's
// owner.
func copyFileKeepMode(src, dst string, like os.FileInfo) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: the operator's env file
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".consolidate-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := creds.PreserveOwner(tmp.Name(), like); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// verifyStoresOpen is the production store check: neither store may have a
// pending migration against the target (read-only status first), then both
// open through their real constructors — which run the migration runners,
// no-ops by then — and answer a read.
func verifyStoresOpen(ctx context.Context, chatDSN, schedDSN string) error {
	raw, err := sql.Open("pgx", chatDSN)
	if err != nil {
		return err
	}
	crep, err := store.MigrationStatusDB(ctx, raw)
	_ = raw.Close()
	if err != nil {
		return fmt.Errorf("chat migration status: %w", err)
	}
	if len(crep.Pending) > 0 {
		return fmt.Errorf("chat has %d pending migration(s) in the target; the copy is incomplete", len(crep.Pending))
	}
	raw, err = sql.Open("pgx", schedDSN)
	if err != nil {
		return err
	}
	srep, err := scheddb.MigrationStatus(ctx, raw)
	_ = raw.Close()
	if err != nil {
		return fmt.Errorf("sched migration status: %w", err)
	}
	if len(srep.Pending) > 0 || srep.Dirty {
		return fmt.Errorf("sched has %d pending migration(s) (dirty=%v) in the target; the copy is incomplete", len(srep.Pending), srep.Dirty)
	}
	pool := store.DefaultPoolConfig()
	pool.MaxOpenConns, pool.MaxIdleConns = 2, 1
	cs, err := store.Open(chatDSN, pool)
	if err != nil {
		return fmt.Errorf("open chat store: %w", err)
	}
	_, lerr := cs.ListUsers(ctx)
	_ = cs.Close()
	if lerr != nil {
		return fmt.Errorf("chat store read: %w", lerr)
	}
	sp := scheddb.DefaultPoolConfig()
	sp.MaxOpenConns, sp.MaxIdleConns = 2, 1
	sdb := &scheddb.Database{}
	if err := sdb.Init(schedDSN, sp); err != nil {
		_ = sdb.Close()
		return fmt.Errorf("open sched store: %w", err)
	}
	_, cerr := sdb.CountUsers(ctx)
	_ = sdb.Close()
	if cerr != nil {
		return fmt.Errorf("sched store read: %w", cerr)
	}
	return nil
}

// ── the superuser runner ────────────────────────────────────────────────────

// execPG runs the Postgres client tools as the postgres OS user (prefix
// runuser -u postgres --, peer authentication over the local socket: no
// password anywhere). Tests run it with no prefix and connArgs/env aimed at
// a test server instead.
type execPG struct {
	prefix   []string
	connArgs []string // e.g. -h/-p/-U
	env      []string
	needRoot bool
}

func (e *execPG) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	argv := append(append(append([]string{}, e.prefix...), name), append(append([]string{}, e.connArgs...), args...)...)
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed client binaries; arguments are separate argv built from validated identifiers
	c.Env = append(os.Environ(), e.env...)
	// runuser keeps the cwd; the postgres user cannot read /root.
	c.Dir = "/"
	return c
}

func (e *execPG) Check(ctx context.Context) error {
	if e.needRoot && os.Geteuid() != 0 {
		return errors.New("run as root: superuser work goes through runuser -u postgres")
	}
	var missing []string
	bins := []string{"psql", "pg_dump", "pg_restore"}
	if len(e.prefix) > 0 {
		bins = append([]string{e.prefix[0]}, bins...)
	}
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("not found on PATH: %s (install the PostgreSQL client matching the server)", strings.Join(missing, ", "))
	}
	if _, err := e.Query(ctx, "postgres", "SELECT 1"); err != nil {
		return fmt.Errorf("cannot connect as the postgres superuser: %w", err)
	}
	return nil
}

var pgVersionRE = regexp.MustCompile(`\b(\d+)(?:\.\d+)?`)

func (e *execPG) ClientMajor(ctx context.Context) (int, error) {
	argv := append(append([]string{}, e.prefix...), "pg_dump", "--version")
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed binary
	c.Dir = "/"
	out, err := c.Output()
	if err != nil {
		return 0, fmt.Errorf("pg_dump --version: %w", err)
	}
	m := pgVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		return 0, fmt.Errorf("pg_dump --version: unrecognised %q", strings.TrimSpace(string(out)))
	}
	return strconv.Atoi(m[1])
}

const fieldSep = "\x1f"

func (e *execPG) Query(ctx context.Context, db, query string) ([][]string, error) {
	c := e.cmd(ctx, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-F", fieldSep, "-d", db, "-c", query)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("psql in %s: %w: %s", db, err, lastLines(stderr.String()))
	}
	var rows [][]string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		rows = append(rows, strings.Split(sc.Text(), fieldSep))
	}
	return rows, sc.Err()
}

func (e *execPG) Exec(ctx context.Context, db, sqlText string) error {
	c := e.cmd(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", db)
	c.Stdin = strings.NewReader(sqlText)
	var stderr strings.Builder
	c.Stderr = &stderr
	c.Stdout = io.Discard
	if err := c.Run(); err != nil {
		return fmt.Errorf("psql in %s: %w: %s", db, err, lastLines(stderr.String()))
	}
	return nil
}

func (e *execPG) DumpRestore(ctx context.Context, srcDB, schema, dstDB, role string) error {
	dump := e.cmd(ctx, "pg_dump", "-Fc", "-n", schema, "-d", srcDB)
	restore := e.cmd(ctx, "pg_restore", "--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges", "--role="+role, "-d", dstDB)
	var dumpErr, restoreErr strings.Builder
	dump.Stderr, restore.Stderr = &dumpErr, &restoreErr
	restore.Stdout = io.Discard
	pipe, err := dump.StdoutPipe()
	if err != nil {
		return err
	}
	restore.Stdin = pipe
	if err := dump.Start(); err != nil {
		return fmt.Errorf("start pg_dump: %w", err)
	}
	if err := restore.Start(); err != nil {
		_ = dump.Process.Kill()
		_ = dump.Wait()
		return fmt.Errorf("start pg_restore: %w", err)
	}
	rerr := restore.Wait()
	derr := dump.Wait()
	if derr != nil {
		return fmt.Errorf("pg_dump %s: %w: %s", srcDB, derr, lastLines(dumpErr.String()))
	}
	if rerr != nil {
		return fmt.Errorf("pg_restore into %s: %w: %s", dstDB, rerr, lastLines(restoreErr.String()))
	}
	return nil
}

func lastLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}

func sqlLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func sqlList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = sqlLit(n)
	}
	return strings.Join(q, ", ")
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
