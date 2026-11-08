package admincli

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
)

// Backup/restore wrap pg_dump -Fc / pg_restore for the chat and sched Postgres
// stores (chat: conversations + turn events; sched: tasks + nodes + API keys)
// and the identity schema core both reference (#2049 M4: the one users
// table). Every store is a schema of the one database (#2049):
//
//   - --db=all (the default, and the backup timer's) is ONE pg_dump of
//     chat, core and sched: one snapshot, so the accounts and the chat and
//     sched rows that name them are consistent with each other.
//   - --db=chat / --db=sched dump that store's schema plus core, so each
//     dump records the accounts its rows name. A sched dump still does not
//     restore alone into an empty database: a sched restore never writes
//     core (see checkSchedRestore).
//
// Restore takes --db=all for a whole-database dump (all three schemas, in
// one transaction), or one store: its schema, plus core for chat (whose
// migrations own it). Old two-file backups restore one store at a time as
// before. The custom (-Fc) format is what pg_restore consumes and supports
// --clean/--if-exists for an idempotent restore-over-existing-DB.
//
// Credentials never reach argv: the DSN is parsed and the password is passed to
// the child via PGPASSWORD in its environment (visible only to the same user via
// /proc, never in `ps`), matching the host-side-credentials invariant. Logged DSNs
// are always run through redactDSN.

// pgConn is the connection detail parsed out of a Postgres DSN, split so the
// password can be handed to pg_dump/pg_restore via the environment (PGPASSWORD)
// instead of argv.
type pgConn struct {
	host     string
	port     string
	user     string
	password string
	dbname   string
	sslmode  string
	passfile string
}

// parsePGConn splits a DSN into the connection parts pg_dump/pg_restore get
// through their environment. The target — host, port, database, role,
// password — is config.ParseConnTarget's, i.e. exactly where the store's own
// pgx connection goes (query overrides included), so the client tools never
// reach another cluster or database than the store's. A DSN listing several
// hosts is refused.
func parsePGConn(dsn string) (pgConn, error) {
	t, err := config.ParseConnTarget(dsn)
	if err != nil {
		return pgConn{}, fmt.Errorf("parse DSN: %w", err)
	}
	c := pgConn{
		host:     t.Host,
		port:     t.Port,
		user:     t.User,
		password: t.Password,
		dbname:   t.DB(),
		sslmode:  config.DSNParam(dsn, "sslmode"),
		passfile: config.DSNParam(dsn, "passfile"),
	}
	if c.dbname == "" {
		return pgConn{}, fmt.Errorf("DSN has no database name")
	}
	return c, nil
}

// env returns the child-process environment for a pg_* invocation: the parent env
// plus the PG* connection vars. The password rides in PGPASSWORD (env, never argv).
func (c pgConn) env() []string {
	e := append([]string{}, os.Environ()...)
	e = append(e,
		"PGHOST="+c.host,
		"PGPORT="+c.port,
		"PGUSER="+c.user,
		"PGDATABASE="+c.dbname,
	)
	if c.password != "" {
		e = append(e, "PGPASSWORD="+c.password)
	}
	if c.sslmode != "" {
		e = append(e, "PGSSLMODE="+c.sslmode)
	}
	if c.passfile != "" {
		e = append(e, "PGPASSFILE="+c.passfile)
	}
	return e
}

// runPgDump writes a custom-format (-Fc) dump of one store to outPath: the
// DSN's schema plus the identity schema core, which both stores' rows
// reference (#2049 M4), so the dump records the accounts its rows name (a
// sched restore reads them to check creators; it does not restore them). One pg_dump is one
// snapshot. Connection params (incl. password) travel via the environment,
// not argv.
func runPgDump(ctx context.Context, dsn, outPath string) error {
	// A store's dump is its schema and core, nothing else (the database
	// holds the other store's schema too, and may hold schemas that are not
	// fleet's). chatDSN / schedDSN always give the DSN its store's schema.
	schema := config.DSNSchema(dsn)
	if schema == "" {
		return fmt.Errorf("pg_dump %s: the DSN names no store schema", redactDSN(dsn))
	}
	return pgDump(ctx, dsn, outPath, []string{schema, config.IdentitySchema})
}

// allSchemas are the schemas of a whole-fleet dump (--db=all), in one
// snapshot: the stores and the identity schema between them.
var allSchemas = []string{config.ChatSchema, config.IdentitySchema, config.SchedSchema}

// runPgDumpAll writes ONE custom-format dump of chat, core and sched: a
// single pg_dump, so a single snapshot — the accounts in core and the chat
// and sched rows that reference them are consistent with each other.
//
// Every one of the three must be in the archive: pg_dump runs with
// --strict-names (a schema that is missing fails it), and the written
// archive's table of contents is checked again before the backup reports
// success — a "whole fleet" dump without core or sched is not one.
func runPgDumpAll(ctx context.Context, dsn, outPath string) (err error) {
	defer func() {
		if err != nil {
			_ = os.Remove(outPath) // no partial "backup" left for a restore to find
		}
	}()
	if err := pgDump(ctx, dsn, outPath, allSchemas, "--strict-names"); err != nil {
		return err
	}
	got, err := dumpSchemas(ctx, outPath)
	if err != nil {
		return err
	}
	for _, s := range allSchemas {
		if !slices.Contains(got, s) {
			return fmt.Errorf("the whole-fleet dump %s lacks schema %s (it holds %s): refusing to report it as a backup", outPath, s, strings.Join(got, ", "))
		}
	}
	return nil
}

// pgDump runs one pg_dump of the given schemas. A pattern that matches no
// schema is skipped (no --strict-names): core does not exist before the
// chat store first migrates.
func pgDump(ctx context.Context, dsn, outPath string, schemas []string, extra ...string) error {
	c, err := parsePGConn(dsn)
	if err != nil {
		return err
	}
	// -Fc custom format, --no-owner/--no-acl so a restore into a differently-owned
	// target DB (the common DR case) does not fail on role/grant mismatches.
	args := append([]string{"-Fc", "--no-owner", "--no-acl", "-f", outPath}, extra...)
	for _, schema := range schemas {
		args = append(args, "-n", schema)
	}
	//nolint:gosec // G204: fixed "pg_dump" binary; outPath is an operator-supplied path passed as a separate argv (no shell interpolation); connection params (incl. password) ride in the env, never argv.
	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Env = c.env()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump %s: %w", redactDSN(dsn), err)
	}
	// A dump IS the database — every conversation, every credential row the
	// schema holds. pg_dump creates it with the process umask (0644 under a
	// typical root shell), so clamp it to owner-only the way the env file is.
	if err := os.Chmod(outPath, 0o600); err != nil {
		return fmt.Errorf("chmod 0600 %s: %w", outPath, err)
	}
	return nil
}

// singleStoreRestoreRefusal is the refusal of `fleet restore --db=chat|sched`
// (#2049 M6, ADR-0107): the whole database is one migration history, so it is
// restored as one.
const singleStoreRestoreRefusal = "single-store restores are not supported any more (#2049 M6): the chat and sched stores and " +
	"the identity schema between them share one migration history (core.schema_versions), so a restore puts back the " +
	"whole database. Restore a whole-database dump with --db=all (fleet backup --db=all writes fleet-all-<stamp>.dump; " +
	"see docs/BACKUP_RESTORE.md)"

// runPgRestoreAll restores a whole-fleet dump (--db=all: chat, core and
// sched, taken in one snapshot) in ONE transaction, so the restored accounts
// and the rows that reference them — and the migration ledgers — are
// consistent. It is the only restore (#2049 M6). Refused before anything
// changes:
//
//   - a dump missing any of the three schemas (a per-store dump, an old
//     two-file backup, a dump of the removed two-database layout);
//   - a dump from before the one identity table (no core.users: its
//     accounts unmerged in sched.users), which M4 and later cannot run;
//   - a dump from before the one migration ledger (no core.schema_versions)
//     over a database past the ledger's boundary (a version >= 200 ran).
//
// Inside the restore transaction the three fleet schemas (chat, core, sched)
// are dropped with CASCADE and recreated before the archive loads, so no
// object created by a migration newer than the dump survives next to the
// older restored ledger; any other schema of the database is left alone.
func runPgRestoreAll(ctx context.Context, dsn, inPath string) error {
	got, err := dumpSchemas(ctx, inPath)
	if err != nil {
		return err
	}
	var missing []string
	for _, s := range allSchemas {
		if !slices.Contains(got, s) {
			missing = append(missing, s)
		}
	}
	switch {
	case len(got) == 0:
		return fmt.Errorf("cannot tell what %s holds: its table of contents has no schema or table entry; refusing to restore it", inPath)
	case slices.Equal(got, []string{config.PublicSchema}):
		return fmt.Errorf("%s is a dump of the removed two-database layout (its tables are in schema public); fleet restores only whole-database dumps taken since M4 (fleet-all-<stamp>.dump) — see docs/BACKUP_RESTORE.md for importing an old one by hand", inPath)
	case len(missing) > 0:
		return fmt.Errorf("%s is not a whole-fleet dump (it lacks schema(s) %s; it holds %s): fleet restores only whole-database dumps (fleet backup --db=all writes fleet-all-<stamp>.dump)", inPath, strings.Join(missing, ", "), strings.Join(got, ", "))
	}
	if err := checkLedgerRestore(ctx, dsn, inPath); err != nil {
		return err
	}
	return pgRestoreAll(ctx, dsn, inPath)
}

// pgRestoreAll restores allSchemas from inPath into dsn in ONE transaction
// that first drops and recreates chat, core and sched with CASCADE, so no
// table, view, sequence, function or legacy ledger from a migration newer
// than the dump survives next to the restored ledger, and any failure rolls
// back the schema reset together with the restore.
func pgRestoreAll(ctx context.Context, dsn, inPath string) error {
	c, err := parsePGConn(dsn)
	if err != nil {
		return err
	}
	dumpArgs := []string{"--no-owner", "--no-acl"}
	var reset strings.Builder
	reset.WriteString("BEGIN;\nSET LOCAL client_min_messages = warning;\n")
	for _, schema := range allSchemas {
		if !config.PlainIdentifier(schema) {
			return fmt.Errorf("schema %q is not a plain identifier", schema)
		}
		reset.WriteString(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE;` + "\n")
		reset.WriteString(`CREATE SCHEMA "` + schema + `";` + "\n")
		dumpArgs = append(dumpArgs, "-n", schema)
	}
	dumpArgs = append(dumpArgs, "-f", "-", inPath)

	restoreCmd := exec.CommandContext(ctx, "pg_restore", dumpArgs...)
	restoreCmd.Stderr = os.Stderr
	dumpOut, err := restoreCmd.StdoutPipe()
	if err != nil {
		return err
	}
	//nolint:gosec // G204: fixed "psql" binary; dbname is parsed from the DSN; credentials ride in the env.
	psqlCmd := exec.CommandContext(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", c.dbname)
	psqlCmd.Env = c.env()
	psqlCmd.Stdout = io.Discard
	psqlCmd.Stderr = os.Stderr
	psqlIn, err := psqlCmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := restoreCmd.Start(); err != nil {
		return fmt.Errorf("pg_restore %s: %w", redactDSN(dsn), err)
	}
	if err := psqlCmd.Start(); err != nil {
		_ = restoreCmd.Process.Kill()
		_ = restoreCmd.Wait()
		return fmt.Errorf("psql %s: %w", redactDSN(dsn), err)
	}
	_, copyErr := io.WriteString(psqlIn, reset.String())
	if copyErr == nil {
		_, copyErr = io.Copy(psqlIn, dumpOut)
	}
	if copyErr != nil {
		_ = restoreCmd.Process.Kill()
	}
	restoreErr := restoreCmd.Wait()
	if copyErr == nil && restoreErr == nil {
		_, copyErr = io.WriteString(psqlIn, "\nCOMMIT;\n")
	}
	closeErr := psqlIn.Close()
	psqlErr := psqlCmd.Wait()
	for _, err := range []error{restoreErr, psqlErr, copyErr, closeErr} {
		if err != nil {
			return fmt.Errorf("pg_restore %s: %w", redactDSN(dsn), err)
		}
	}
	return nil
}

// pgRestore runs one pg_restore of the given schemas, creating each first if
// missing (pg_restore -n restores a schema's objects, not the schema).
func pgRestore(ctx context.Context, dsn, inPath string, schemas []string, oneTx bool) error {
	c, err := parsePGConn(dsn)
	if err != nil {
		return err
	}
	args := []string{"--clean", "--if-exists", "--no-owner", "--no-acl"}
	if oneTx {
		args = append(args, "--single-transaction")
	}
	for _, schema := range schemas {
		if err := ensureRestoreSchema(ctx, dsn, schema); err != nil {
			return err
		}
		args = append(args, "-n", schema)
	}
	args = append(args, "-d", c.dbname, inPath)
	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	cmd.Env = c.env()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore %s: %w", redactDSN(dsn), err)
	}
	return nil
}

// ensureRestoreSchema creates the store's schema when it is missing (a
// restore into a fresh database): pg_restore -n restores the schema's
// objects, not the schema itself.
func ensureRestoreSchema(ctx context.Context, dsn, schema string) error {
	if !config.PlainIdentifier(schema) {
		return fmt.Errorf("schema %q is not a plain identifier", schema)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	// schema passed config.PlainIdentifier above ([a-z_][a-z0-9_]*), and DDL takes no bind parameters.
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS "`+schema+`"`); err != nil {
		return fmt.Errorf("create schema %s before the restore: %w", schema, err)
	}
	return nil
}

// dumpSchemas lists the schemas a custom-format dump holds — the names of its
// SCHEMA entries and the schemas of its TABLE entries — from pg_restore
// --list (which reads only the archive's table of contents), sorted and
// distinct.
func dumpSchemas(ctx context.Context, path string) ([]string, error) {
	//nolint:gosec // G204: fixed "pg_restore" binary; path is an operator-supplied path passed as a separate argv. --list is read-only.
	out, err := exec.CommandContext(ctx, "pg_restore", "--list", path).Output()
	if err != nil {
		return nil, fmt.Errorf("read the dump's table of contents (%s): %w", path, err)
	}
	return parseTOCSchemas(string(out)), nil
}

// parseTOCSchemas extracts the schemas of a pg_restore --list output: entry
// lines read "<id>; <oid> <oid> <TYPE> <schema> <name> <owner>", where a
// SCHEMA entry has "-" for its schema and the schema's name as its name.
// Only SCHEMA and TABLE entries count (other types span several words, and
// every other object of a store sits next to a table of it).
func parseTOCSchemas(toc string) []string {
	seen := map[string]bool{}
	for _, ln := range strings.Split(toc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), ";") {
			continue
		}
		_, rest, ok := strings.Cut(ln, "; ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		switch {
		case len(f) >= 5 && f[2] == "SCHEMA" && f[3] == "-":
			seen[f[4]] = true
		case len(f) >= 5 && f[2] == "TABLE" && f[3] != "DATA":
			seen[f[3]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// backupFileName is the default per-DB dump filename: fleet-<db>-<UTC stamp>.dump.
// The timestamp keeps successive backups from clobbering one another.
func backupFileName(db string, now time.Time) string {
	return fmt.Sprintf("fleet-%s-%s.dump", db, now.UTC().Format("20060102T150405Z"))
}

// backupFilePattern matches the dump files this tool writes (fleet-{chat,sched}-*.dump),
// so prune only ever deletes our own artifacts — never an unrelated file in the dir.
var backupFilePattern = regexp.MustCompile(`^fleet-(all|chat|sched)-.*\.dump$`)

// verifyDump confirms a freshly-written file is a valid pg custom-format archive
// by listing its table of contents (pg_restore --list: no connection, no data
// written). A corrupt dump fails here so a backup run never reports success on an
// unrestorable file. Skipped (with a warning) when pg_restore is not on PATH.
func verifyDump(ctx context.Context, path string) error {
	if _, err := exec.LookPath("pg_restore"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pg_restore not found; skipping integrity check of %s\n", path)
		//nolint:nilerr // intentional: pg_restore absent = skip verification, not a verify failure.
		return nil
	}
	//nolint:gosec // G204: fixed "pg_restore" binary; path is an operator-supplied path passed as a separate argv (no shell). --list is read-only (no connection, no writes).
	cmd := exec.CommandContext(ctx, "pg_restore", "--list", path)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("integrity check failed for %s (not a valid pg custom-format archive): %w", path, err)
	}
	return nil
}

// pruneOldBackups removes this tool's dump files older than retentionDays from
// dir. Returns the number removed. Pure os — no new dependency.
func pruneOldBackups(dir string, retentionDays int) (int, error) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read backup dir: %w", err)
	}
	var pruned int
	for _, e := range entries {
		if e.IsDir() || !backupFilePattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return pruned, fmt.Errorf("remove %s: %w", e.Name(), err)
		}
		pruned++
	}
	return pruned, nil
}

// configuredBackupDir is FLEET_BACKUP_DIR as the deployment configures it:
// the process env, else the server env file. The ONE read both `fleet backup`
// (backupDir) and `fleet timers install` (resolveBackupDir) go through, so the
// verb and the timer it installs can never disagree about where dumps land —
// backupDir used to read the process env only, so a FLEET_BACKUP_DIR set in
// /etc/fleet/fleet.env steered the timer but not a hand-run `fleet backup`.
func configuredBackupDir() string {
	return strings.TrimSpace(envOrFile("FLEET_BACKUP_DIR"))
}

// backupDir resolves the output directory: the --out flag, else FLEET_BACKUP_DIR
// (process env or the env file), else ".". Lets an operator set a default
// location once in the env file.
func backupDir(flagOut string) string {
	if v := strings.TrimSpace(flagOut); v != "" {
		return v
	}
	if v := configuredBackupDir(); v != "" {
		return v
	}
	return "."
}

// ensureBackupDir creates the dump directory owner-only. 0700, not 0750: the
// dumps inside are 0600 and nothing else on the box needs to list them.
func ensureBackupDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// retentionDays resolves the prune cutoff: FLEET_BACKUP_RETENTION_DAYS, else
// 30. The knob is a scopeExternal row in the one env-knob registry (#1273), and
// `fleet backup` is a verb that never calls config.Load — so there is no boot
// gate here and the read goes through config.EnvKnobInt, which applies the
// registry's parser and its `must be >= 1` bound. A malformed value is returned
// as an ERROR and refuses the prune rather than silently pruning on the
// 30-day default: deleting backups off a misread retention is not a
// recoverable mistake.
func retentionDays() (int, error) {
	return config.EnvKnobInt("FLEET_BACKUP_RETENTION_DAYS", 30)
}

// isTerminal reports whether f is an interactive terminal (so the restore
// confirmation prompt is only shown to a human, never in a pipe/CI). Uses the
// char-device bit — no x/term dependency.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// cmdBackup handles `fleet backup [--db=chat|sched|all] [--out DIR]`.
func cmdBackup(argv []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	db := fs.String("db", "all", "which database to back up: chat|sched|all")
	out := fs.String("out", "", "output directory for the dump file(s) (else FLEET_BACKUP_DIR from the shell or the env file, else .)")
	prune := fs.Bool("prune", false, "after backing up, delete dumps older than FLEET_BACKUP_RETENTION_DAYS (default 30)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (else DATABASE_URL, schema chat)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (else DATABASE_URL, schema sched)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	dbs, err := selectDBs(*db)
	if err != nil {
		return errf(1, "%v", err)
	}
	outDir := backupDir(*out)
	if err := ensureBackupDir(outDir); err != nil {
		return errf(1, "create out dir: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	for _, name := range dbs {
		dsn, err := dsnFor(name, *chatURL, *schedURL)
		if err != nil {
			return errf(1, "%v", err)
		}
		dump := runPgDump
		if name == "all" {
			dump = runPgDumpAll
		}
		path := filepath.Join(outDir, backupFileName(name, now))
		if err := dump(ctx, dsn, path); err != nil {
			return errf(5, "backup %s: %v", name, err)
		}
		// Always verify the freshly-written dump is restorable before reporting
		// success — a silently-corrupt backup is worse than a loud failure.
		if err := verifyDump(ctx, path); err != nil {
			return errf(5, "%v", err)
		}
		fmt.Fprintf(os.Stderr, "backed up %s → %s (verified)\n", storeLabel(name), path)
		fmt.Println(path)
	}
	if *prune {
		days, err := retentionDays()
		if err != nil {
			return errf(1, "prune: %v", err)
		}
		n, err := pruneOldBackups(outDir, days)
		if err != nil {
			return errf(5, "prune: %v", err)
		}
		fmt.Fprintf(os.Stderr, "pruned %d old backup(s) older than %d days\n", n, days)
	}
	return 0
}

// cmdRestore handles `fleet restore --db=all|chat|sched FILE`. There is no
// default: it overwrites a live database, so the operator names the target.
// --db=all takes a whole-fleet dump (fleet-all-*.dump) and restores chat,
// core and sched in one transaction; --db=chat / --db=sched restore one
// store (an old two-file backup restores this way, one file each).
func cmdRestore(argv []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	db := fs.String("db", "all", "what to restore: all (a fleet-all dump; the only restore since #2049 M6)")
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive overwrite confirmation (for scripted restores)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (else DATABASE_URL, schema chat)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (else DATABASE_URL, schema sched)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	switch *db {
	case "all":
	case "chat", "sched":
		return errf(1, "%s", singleStoreRestoreRefusal)
	default:
		return errf(1, "--db must be all (restore overwrites the live database)")
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errf(1, "usage: fleet restore [--db=all] <fleet-all dump file>")
	}
	inPath := rest[0]
	if _, err := os.Stat(inPath); err != nil {
		return errf(1, "dump file: %v", err)
	}
	dsn, err := dsnFor(*db, *chatURL, *schedURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	ctx := context.Background()
	// Verify the archive is restorable before we drop the live DB's objects.
	if err := verifyDump(ctx, inPath); err != nil {
		return errf(5, "%v", err)
	}
	// Restore OVERWRITES the live database (--clean --if-exists). Confirm on a TTY
	// unless --no-confirm; in a pipe/CI a confirmation can't be answered, so a
	// non-TTY without --no-confirm is refused rather than silently proceeding.
	if !*noConfirm {
		if !isTerminal(os.Stdin) {
			return errf(1, "refusing to overwrite the %s without confirmation: pass --no-confirm for scripted restores", storeLabel(*db))
		}
		fmt.Fprintf(os.Stderr, "WARNING: this will OVERWRITE the live %s from %s. Continue? [y/N]: ", storeLabel(*db), inPath)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Fprintln(os.Stderr, "restore aborted")
			return 1
		}
	}
	if err := runPgRestoreAll(ctx, dsn, inPath); err != nil {
		return errf(5, "restore %s: %v", *db, err)
	}
	fmt.Fprintf(os.Stderr, "restored %s from %s\n", storeLabel(*db), inPath)
	return 0
}

// selectDBs maps the --db flag to the dumps to write. --db=all is one dump of
// the whole fleet (one snapshot), not one per store.
func selectDBs(db string) ([]string, error) {
	switch db {
	case "chat":
		return []string{"chat"}, nil
	case "sched":
		return []string{"sched"}, nil
	case "all", "":
		return []string{"all"}, nil
	default:
		return nil, fmt.Errorf("--db must be chat, sched, or all (got %q)", db)
	}
}

// dsnFor resolves the DSN for a named DB, reusing the same precedence the other
// fleet verbs use (flag → FLEET_<DB>_DATABASE_URL → DATABASE_URL). "all" is
// the one database both stores live in: the chat and sched DSNs must reach
// the same database, or there is no one snapshot of both.
func dsnFor(name, chatURL, schedURL string) (string, error) {
	switch name {
	case "all":
		chat, err := chatDSN(chatURL)
		if err != nil {
			return "", err
		}
		sched, err := schedDSN(schedURL)
		if err != nil {
			return "", err
		}
		ct, err := config.ParseConnTarget(chat)
		if err != nil {
			return "", fmt.Errorf("parse chat DSN: %w", err)
		}
		st, err := config.ParseConnTarget(sched)
		if err != nil {
			return "", fmt.Errorf("parse sched DSN: %w", err)
		}
		if ct.Host != st.Host || ct.Port != st.Port || ct.DB() != st.DB() {
			return "", fmt.Errorf("the chat and sched DSNs reach different databases (%s, %s): --db=all is one snapshot of the one database; back each store up with --db=chat / --db=sched", redactDSN(chat), redactDSN(sched))
		}
		return chat, nil
	case "chat":
		return chatDSN(chatURL)
	case "sched":
		return schedDSN(schedURL)
	default:
		return "", fmt.Errorf("unknown database %q", name)
	}
}

// storeLabel names what a --db value backs up or restores, for messages.
func storeLabel(db string) string {
	if db == "all" {
		return "whole fleet database (chat, core, sched)"
	}
	return db + " database"
}
