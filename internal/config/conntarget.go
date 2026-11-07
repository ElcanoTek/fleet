package config

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConnTarget is where a DSN connects: the server (host, port), database,
// role and password the driver itself would use. It is the ONE extraction
// of a DSN's target (#2055 review): every place that asks "which server,
// database or role does this DSN name" — DSNIdentity and so the
// distinct-stores guard, `fleet status`, backup/restore's client tools —
// reads it from here, never from a hand-written parser, so no question about
// a DSN can be answered differently from how the server's own pgx connection
// answers it.
//
// It is pgconn.ParseConfig's reading, so it is exactly pgx's:
//   - a query parameter (?host=, ?port=, ?dbname=, ?user=, ?password=)
//     overrides the URL's own part, the last occurrence winning;
//   - what the DSN leaves out comes from the PG* environment (PGHOST,
//     PGPORT, PGDATABASE, PGUSER, PGPASSWORD, PGSERVICE …), a service file,
//     .pgpass for the password, and then libpq's defaults (the first
//     existing default socket directory, port 5432, the OS user).
//     Deliberately: the process that asks connects with the same
//     environment, so its connections go where the target says;
//   - Database is "" when nothing names one; the server then uses the role
//     name (DB).
//
// A DSN listing several hosts (host=a,b or postgres://a,b/db) is refused
// with ErrMultiHost: fleet's stores, backups and the distinct-stores guard
// each need ONE server, and guessing the first would let two DSNs that share only a
// fallback host compare as one database.
type ConnTarget struct {
	Host     string // a host name, an IP address, or a Unix-socket directory ("/…")
	Port     string
	Database string
	User     string
	Password string
}

// DB is the database the connection lands in: Database, else the role name
// (the server's default).
func (t ConnTarget) DB() string {
	if t.Database != "" {
		return t.Database
	}
	return t.User
}

// Socket reports whether Host is a Unix-socket directory.
func (t ConnTarget) Socket() bool { return strings.HasPrefix(t.Host, "/") }

// ErrMultiHost is ParseConnTarget's refusal of a DSN naming several servers.
var ErrMultiHost = errors.New("the DSN lists several hosts; fleet needs a DSN naming one server")

// errUnparsable never carries pgconn's own error: its text can quote parts of
// the DSN, which may be a password in a malformed one.
var errUnparsable = errors.New("the DSN does not parse as a Postgres connection string")

// ParseConnTarget reads a DSN's target through pgconn.ParseConfig (see
// ConnTarget). The error never quotes the DSN.
func ParseConnTarget(dsn string) (ConnTarget, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		// pgconn would read a target from the environment alone; an empty
		// DSN names nothing here.
		return ConnTarget{}, errUnparsable
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return ConnTarget{}, errUnparsable
	}
	for _, f := range cfg.Fallbacks {
		// sslmode=prefer/allow add a same-server fallback (TLS, then not);
		// only a different server is a second host.
		if f.Host != cfg.Host || f.Port != cfg.Port {
			return ConnTarget{}, ErrMultiHost
		}
	}
	return ConnTarget{
		Host:     cfg.Host,
		Port:     strconv.Itoa(int(cfg.Port)),
		Database: cfg.Database,
		User:     cfg.User,
		Password: cfg.Password,
	}, nil
}

// DSNParam returns the value a DSN gives connection parameter key (sslmode,
// passfile, sslrootcert …), read as pgx reads it: the last occurrence wins,
// and a URL query value is percent-decoded without turning "+" into a space.
// It is "" when the DSN does not set it. For a DSN's target (host, port,
// database, role, password) use ParseConnTarget.
func DSNParam(dsn, key string) string {
	dsn = strings.TrimSpace(dsn)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		_, q, ok := strings.Cut(dsn, "?")
		if !ok {
			return ""
		}
		q, _, _ = strings.Cut(q, "#")
		v := ""
		for _, p := range strings.Split(q, "&") {
			k, raw, _ := strings.Cut(p, "=")
			if dk, err := url.PathUnescape(k); err != nil || dk != key {
				continue
			}
			if dv, err := url.PathUnescape(raw); err == nil {
				v = strings.TrimSpace(dv)
			}
		}
		return v
	}
	if kv, ok := parseKeywordDSN(dsn); ok {
		return kv.get(key)
	}
	return ""
}

// PGTargetEnv lists the PG* environment variables that change where a DSN
// connects when it leaves the setting out (ConnTarget).
var PGTargetEnv = []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGSERVICE", "PGSERVICEFILE"}
