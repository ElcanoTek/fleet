package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Backup/restore wrap pg_dump -Fc / pg_restore for the chat and sched Postgres
// databases. fleet runs one cluster with two logical DBs (chat: conversations +
// turn events + users; sched: tasks + nodes + API keys), each reached by its own
// DSN. We therefore dump PER DATABASE (custom-format, one file each) rather than a
// single cluster-wide pg_dumpall: each DB has independent credentials, --db=chat
// needs only the chat DSN, and restore targets one DB at a time. The custom (-Fc)
// format is what pg_restore consumes and supports --clean/--if-exists for an
// idempotent restore-over-existing-DB.
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
}

// parsePGConn parses a postgres:// DSN into its connection parts. dbname is
// required (it is the unit a per-DB dump/restore operates on).
func parsePGConn(dsn string) (pgConn, error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return pgConn{}, fmt.Errorf("parse DSN: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return pgConn{}, fmt.Errorf("DSN scheme %q is not postgres", u.Scheme)
	}
	c := pgConn{
		host:    u.Hostname(),
		port:    u.Port(),
		dbname:  strings.TrimPrefix(u.Path, "/"),
		sslmode: u.Query().Get("sslmode"),
	}
	if c.port == "" {
		c.port = "5432"
	}
	if u.User != nil {
		c.user = u.User.Username()
		c.password, _ = u.User.Password()
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
	return e
}

// runPgDump writes a custom-format (-Fc) dump of the DSN's database to outPath.
// Connection params (incl. password) travel via the environment, not argv.
func runPgDump(ctx context.Context, dsn, outPath string) error {
	c, err := parsePGConn(dsn)
	if err != nil {
		return err
	}
	// -Fc custom format, --no-owner/--no-acl so a restore into a differently-owned
	// target DB (the common DR case) does not fail on role/grant mismatches.
	//nolint:gosec // G204: fixed "pg_dump" binary; outPath is an operator-supplied path passed as a separate argv (no shell interpolation); connection params (incl. password) ride in the env, never argv.
	cmd := exec.CommandContext(ctx, "pg_dump", "-Fc", "--no-owner", "--no-acl", "-f", outPath)
	cmd.Env = c.env()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump %s: %w", redactDSN(dsn), err)
	}
	return nil
}

// runPgRestore restores a custom-format dump into the DSN's database. --clean
// --if-exists drops existing objects first so the restore is idempotent over an
// already-migrated DB; --no-owner ignores dump ownership.
func runPgRestore(ctx context.Context, dsn, inPath string) error {
	c, err := parsePGConn(dsn)
	if err != nil {
		return err
	}
	//nolint:gosec // G204: fixed "pg_restore" binary; dbname comes from the parsed operator DSN and inPath is an operator-supplied path, both separate argv (no shell interpolation); connection params ride in the env.
	cmd := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists", "--no-owner", "--no-acl", "-d", c.dbname, inPath)
	cmd.Env = c.env()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore %s: %w", redactDSN(dsn), err)
	}
	return nil
}

// backupFileName is the default per-DB dump filename: fleet-<db>-<UTC stamp>.dump.
// The timestamp keeps successive backups from clobbering one another.
func backupFileName(db string, now time.Time) string {
	return fmt.Sprintf("fleet-%s-%s.dump", db, now.UTC().Format("20060102T150405Z"))
}

// backupFilePattern matches the dump files this tool writes (fleet-{chat,sched}-*.dump),
// so prune only ever deletes our own artifacts — never an unrelated file in the dir.
var backupFilePattern = regexp.MustCompile(`^fleet-(chat|sched)-.*\.dump$`)

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

// backupDir resolves the output directory: the --out flag, else FLEET_BACKUP_DIR,
// else ".". Lets an operator set a default location once in the env file.
func backupDir(flagOut string) string {
	if v := strings.TrimSpace(flagOut); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_BACKUP_DIR")); v != "" {
		return v
	}
	return "."
}

// retentionDays resolves the prune cutoff: FLEET_BACKUP_RETENTION_DAYS, else 30.
func retentionDays() int {
	if v := strings.TrimSpace(os.Getenv("FLEET_BACKUP_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 30
}

// isTerminal reports whether f is an interactive terminal (so the restore
// confirmation prompt is only shown to a human, never in a pipe/CI). Uses the
// char-device bit — no x/term dependency.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// cmdBackup handles `fleet-admin backup [--db=chat|sched|all] [--out DIR]`.
func cmdBackup(argv []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	db := fs.String("db", "all", "which database to back up: chat|sched|all")
	out := fs.String("out", "", "output directory for the dump file(s) (else FLEET_BACKUP_DIR, else .)")
	prune := fs.Bool("prune", false, "after backing up, delete dumps older than FLEET_BACKUP_RETENTION_DAYS (default 30)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (else FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (else FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	dbs, err := selectDBs(*db)
	if err != nil {
		return errf(1, "%v", err)
	}
	outDir := backupDir(*out)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return errf(1, "create out dir: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	for _, name := range dbs {
		dsn, err := dsnFor(name, *chatURL, *schedURL)
		if err != nil {
			return errf(1, "%v", err)
		}
		path := filepath.Join(outDir, backupFileName(name, now))
		if err := runPgDump(ctx, dsn, path); err != nil {
			return errf(5, "backup %s: %v", name, err)
		}
		// Always verify the freshly-written dump is restorable before reporting
		// success — a silently-corrupt backup is worse than a loud failure.
		if err := verifyDump(ctx, path); err != nil {
			return errf(5, "%v", err)
		}
		fmt.Fprintf(os.Stderr, "backed up %s DB → %s (verified)\n", name, path)
		fmt.Println(path)
	}
	if *prune {
		n, err := pruneOldBackups(outDir, retentionDays())
		if err != nil {
			return errf(5, "prune: %v", err)
		}
		fmt.Fprintf(os.Stderr, "pruned %d old backup(s) older than %d days\n", n, retentionDays())
	}
	return 0
}

// cmdRestore handles `fleet-admin restore --db=chat|sched FILE`. Restore is
// single-DB on purpose: it overwrites a live database, so the operator names the
// target explicitly (no --db=all foot-gun).
func cmdRestore(argv []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	db := fs.String("db", "", "which database to restore into: chat|sched (required)")
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive overwrite confirmation (for scripted restores)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (else FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (else FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if *db != "chat" && *db != "sched" {
		return errf(1, "--db must be chat or sched (restore is single-DB; it overwrites a live database)")
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errf(1, "usage: fleet-admin restore --db=chat|sched <dump-file>")
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
			return errf(1, "refusing to overwrite the %s database without confirmation: pass --no-confirm for scripted restores", *db)
		}
		fmt.Fprintf(os.Stderr, "WARNING: this will OVERWRITE the live %s database from %s. Continue? [y/N]: ", *db, inPath)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Fprintln(os.Stderr, "restore aborted")
			return 1
		}
	}
	if err := runPgRestore(ctx, dsn, inPath); err != nil {
		return errf(5, "restore %s: %v", *db, err)
	}
	fmt.Fprintf(os.Stderr, "restored %s DB from %s\n", *db, inPath)
	return 0
}

// selectDBs maps the --db flag to the concrete DB list (deterministic order so
// --db=all backs up chat then sched).
func selectDBs(db string) ([]string, error) {
	switch db {
	case "chat":
		return []string{"chat"}, nil
	case "sched":
		return []string{"sched"}, nil
	case "all", "":
		return []string{"chat", "sched"}, nil
	default:
		return nil, fmt.Errorf("--db must be chat, sched, or all (got %q)", db)
	}
}

// dsnFor resolves the DSN for a named DB, reusing the same precedence the other
// fleet-admin verbs use (flag → FLEET_<DB>_DATABASE_URL → DATABASE_URL).
func dsnFor(name, chatURL, schedURL string) (string, error) {
	switch name {
	case "chat":
		return chatDSN(chatURL)
	case "sched":
		return schedDSN(schedURL)
	default:
		return "", fmt.Errorf("unknown database %q", name)
	}
}
