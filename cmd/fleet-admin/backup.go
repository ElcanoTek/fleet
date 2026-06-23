package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// cmdBackup handles `fleet-admin backup [--db=chat|sched|all] [--out DIR]`.
func cmdBackup(argv []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	db := fs.String("db", "all", "which database to back up: chat|sched|all")
	out := fs.String("out", ".", "output directory for the dump file(s)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (else FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (else FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	dbs, err := selectDBs(*db)
	if err != nil {
		return errf(1, "%v", err)
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		return errf(1, "create out dir: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	for _, name := range dbs {
		dsn, err := dsnFor(name, *chatURL, *schedURL)
		if err != nil {
			return errf(1, "%v", err)
		}
		path := filepath.Join(*out, backupFileName(name, now))
		if err := runPgDump(ctx, dsn, path); err != nil {
			return errf(5, "backup %s: %v", name, err)
		}
		fmt.Fprintf(os.Stderr, "backed up %s DB → %s\n", name, path)
		fmt.Println(path)
	}
	return 0
}

// cmdRestore handles `fleet-admin restore --db=chat|sched FILE`. Restore is
// single-DB on purpose: it overwrites a live database, so the operator names the
// target explicitly (no --db=all foot-gun).
func cmdRestore(argv []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	db := fs.String("db", "", "which database to restore into: chat|sched (required)")
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
	if err := runPgRestore(context.Background(), dsn, inPath); err != nil {
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
