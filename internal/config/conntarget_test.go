package config

import (
	"errors"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range append([]string{"PGPASSWORD", "PGPASSFILE"}, PGTargetEnv...) {
		t.Setenv(k, "")
	}
}

// TestConnTargetIsPgconns (#2055 review round 6): ParseConnTarget is
// pgconn.ParseConfig's reading, field for field, over many DSN shapes — so
// no hand-written parser can disagree with the driver again.
func TestConnTargetIsPgconns(t *testing.T) {
	clearPGEnv(t)
	for _, dsn := range []string{
		"postgres://u:pw@h:5433/db?sslmode=disable",
		"postgresql://u@h/db",
		"postgres://u@/db",
		"postgres://u@/db?host=%2Fvar%2Frun%2Fpostgresql&port=5444",
		"postgres://u@a.example:5432/db?host=b.example",
		"postgres://u@a.example:5432/db?port=6000",
		"postgres://u@a.example/db?dbname=other",
		"postgres://u@a.example/db?database=other",
		"postgres://u@a.example/db?user=svc&password=placeholder-not-a-secret",
		"postgres://u:pw@a.example:5432/db?host=b.example&port=6000&dbname=x&user=y",
		"postgres://u@a.example/db?host=b.example&host=c.example",
		"postgres://u@[::1]:6000/db",
		"postgres://u@h/db+x",
		"postgres://u@h/d%20b",
		"postgres://u@h@x/db",
		"postgres://fleet@h",
		"postgres://",
		"host=h port=5433 dbname=db user=x password=placeholder-not-a-secret",
		"host=a.example dbname=db host=b.example",
		"host=h dbname=a database=b",
		"host=/tmp dbname=db",
		"dbname=db",
	} {
		got, err := ParseConnTarget(dsn)
		cfg, perr := pgconn.ParseConfig(dsn)
		if err != nil || perr != nil {
			t.Errorf("%q: ParseConnTarget %v, pgconn %v", dsn, err, perr)
			continue
		}
		want := ConnTarget{Host: cfg.Host, Port: strconv.Itoa(int(cfg.Port)), Database: cfg.Database, User: cfg.User, Password: cfg.Password}
		if got != want {
			t.Errorf("%q: ParseConnTarget = %+v, pgconn = %+v", dsn, got, want)
		}
	}
	// What pgconn refuses, and several hosts.
	for _, dsn := range []string{"", "not a dsn", "postgres://u@h/db?port=notaport", "postgres://u@h/db?flag", "mysql://x/y"} {
		if _, err := ParseConnTarget(dsn); err == nil {
			t.Errorf("%q: accepted", dsn)
		}
	}
	for _, dsn := range []string{"postgres://a,b/db", "host=a,b dbname=db", "postgres://h:5432,h:5433/db"} {
		if _, err := ParseConnTarget(dsn); !errors.Is(err, ErrMultiHost) {
			t.Errorf("%q: %v, want ErrMultiHost", dsn, err)
		}
	}
	// sslmode=prefer's same-server fallback is not a second host.
	if _, err := ParseConnTarget("postgres://u@db.example/db?sslmode=prefer"); err != nil {
		t.Errorf("sslmode=prefer refused: %v", err)
	}
}

// TestDSNIdentityHonoursQueryOverrides: Codex's round-6 examples — a ?host=
// (and ?port=, ?dbname=) override wins over the authority, as in pgx.
func TestDSNIdentityHonoursQueryOverrides(t *testing.T) {
	clearPGEnv(t)
	for dsn, want := range map[string]string{
		"postgres://u@a.example:5432/db?host=b.example":                              "b.example:5432/db",
		"postgres://u@a.example:5432/db?port=6000":                                   "a.example:6000/db",
		"postgres://u@a.example/db?dbname=other":                                     "a.example:5432/other",
		"postgres://u@a.example?user=svc":                                            "a.example:5432/svc",
		"postgres://u:pw@a.example:5432/db?host=b.example&port=6000&dbname=x&user=y": "b.example:6000/x",
		"postgres://u@127.0.0.1:5432/db?host=%2Frun%2Fpostgresql":                    "local:5432/db",
		"postgres://u@db.example/db?host=localhost":                                  "local:5432/db",
		"postgres://u@h/db?port=05432":                                               "h:5432/db",
		"postgres://a,b/db":                                                          "",
	} {
		if got := DSNIdentity(dsn); got != want {
			t.Errorf("DSNIdentity(%q) = %q, want %q", dsn, got, want)
		}
	}
	// Two DSNs that differ only in where the override sends them are two
	// databases — and the same database spelled two ways is one.
	if sameDatabase("postgres://u@a.example/db", "postgres://u@a.example/db?host=b.example") {
		t.Error("a ?host= override to another server compared as the same database")
	}
	if !sameDatabase("postgres://u@b.example/db", "postgres://u@a.example/db?host=b.example") {
		t.Error("the same server spelled through ?host= did not compare as the same database")
	}
}

// TestDSNIdentityReadsThePGEnvironment documents the deliberate choice: what
// a DSN leaves out comes from PG* variables, as the connection's does.
func TestDSNIdentityReadsThePGEnvironment(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("PGHOST", "env.example")
	t.Setenv("PGPORT", "6001")
	t.Setenv("PGDATABASE", "envdb")
	if got := DSNIdentity("postgres://u@/"); got != "env.example:6001/envdb" {
		t.Errorf("PG* defaults: %q", got)
	}
	if got := DSNIdentity("postgres://u@h:5432/db"); got != "h:5432/db" {
		t.Errorf("the DSN's own parts must win over PG*: %q", got)
	}
}

func TestDSNParam(t *testing.T) {
	for _, c := range []struct{ dsn, key, want string }{
		{"postgres://h/db?sslmode=require", "sslmode", "require"},
		{"postgres://h/db?sslmode=disable&sslmode=require", "sslmode", "require"},
		{"postgres://h/db?passfile=%2Fetc%2Fa+b", "passfile", "/etc/a+b"},
		{"postgres://h/db", "sslmode", ""},
		{"host=h sslmode=verify-full", "sslmode", "verify-full"},
	} {
		if got := DSNParam(c.dsn, c.key); got != c.want {
			t.Errorf("DSNParam(%q, %q) = %q, want %q", c.dsn, c.key, got, c.want)
		}
	}
}
