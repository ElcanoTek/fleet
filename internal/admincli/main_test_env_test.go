package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestMain runs the admin CLI suites against ONE scratch database with both
// stores in their schemas — the only layout fleet runs (#2049).
//
// The Postgres-backed suites follow the CI convention for their fixtures:
// FLEET_TEST_DATABASE_URL is the chat store (testdb.ChatDSN) and DATABASE_URL
// the sched store (testdb.SchedDB). The verbs under test resolve the
// environment as `fleet serve` does — DATABASE_URL alone, the chat store in
// schema chat and the sched store in schema sched — so the fixtures must use
// the same database and schemas. The CI test databases cannot: other
// packages keep their stores in public there, which the stores refuse to run
// next to (dbschema.Ensure). So TestMain creates a fresh database
// <DATABASE_URL's database>_admincli on the same server and points
// DATABASE_URL at its sched schema and FLEET_TEST_DATABASE_URL at its chat
// schema (WithSchema replaces the schema a verb's DSN carries, so DATABASE_URL
// naming sched still resolves chat to chat). It is dropped afterwards. A test
// that sets its own DSN keys (t.Setenv) still overrides these. M2's template
// databases (internal/testdb) replace this.
func TestMain(m *testing.M) {
	cleanup, err := oneTestDatabase()
	if err != nil {
		fmt.Fprintln(os.Stderr, "admincli TestMain:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func oneTestDatabase() (func(), error) {
	chat, sched := os.Getenv("FLEET_TEST_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if chat == "" || sched == "" {
		return func() {}, nil
	}
	u, err := url.Parse(sched)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, fmt.Errorf("DATABASE_URL must be a postgres:// URL for the admin CLI suites")
	}
	name := strings.TrimPrefix(u.Path, "/") + "_admincli"
	if !config.PlainIdentifier(name) {
		return nil, fmt.Errorf("cannot derive a scratch database name from DATABASE_URL (got %q)", name)
	}
	admin := *u
	admin.Path = "/postgres"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", admin.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		return nil, fmt.Errorf("drop %s: %w", name, err)
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	one := *u
	one.Path = "/" + name
	testInjectedEnv = map[string]string{"FLEET_TEST_DATABASE_URL": chat, "DATABASE_URL": sched}
	_ = os.Setenv("FLEET_TEST_DATABASE_URL", config.WithSchema(one.String(), config.ChatSchema))
	_ = os.Setenv("DATABASE_URL", config.WithSchema(one.String(), config.SchedSchema))
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db, err := sql.Open("pgx", admin.String())
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	}, nil
}

// testInjectedEnv maps each key TestMain rewrote to the value the test run
// was given, so the operator-script tests (runScript) hand the scripts the
// environment the test run was given, not the suite's scratch database.
var testInjectedEnv map[string]string

// scriptEnviron is os.Environ with testInjectedEnv's keys restored.
func scriptEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if orig, ok := testInjectedEnv[k]; ok {
			out = append(out, k+"="+orig)
			continue
		}
		out = append(out, kv)
	}
	return out
}
