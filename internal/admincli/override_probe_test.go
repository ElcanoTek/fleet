package admincli

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestDatabaseURLOverrideProbesTheTarget: a --database-url naming a database
// other than the deployment's (a DR host, a restored copy) gets its layout
// from that database's data — the store's schema for a one-database install
// or an empty database, verbatim (public) for a legacy store (#2055 review).
func TestDatabaseURLOverrideProbesTheTarget(t *testing.T) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", withDBName(t, base, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const name = "fleet_override_probe_test"
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		if isPermissionError(err) {
			t.Skipf("cannot CREATE DATABASE: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }()
	target := withDBName(t, base, name)

	// Empty (a restore target) or a one-database install: the store's schema.
	for _, verb := range []struct {
		dsn    func(string) (string, error)
		schema string
	}{{chatDSN, config.ChatSchema}, {schedDSN, config.SchedSchema}} {
		got, err := verb.dsn(target)
		if err != nil || config.DSNSchema(got) != verb.schema {
			t.Errorf("override to an empty database = %q, %v; want schema %q", got, err, verb.schema)
		}
	}

	// A legacy store's database: verbatim, so the verb works on public.
	tdb, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.ExecContext(ctx, `CREATE TABLE public.schema_migrations (version int)`); err != nil {
		t.Fatal(err)
	}
	_ = tdb.Close()
	if got, err := chatDSN(target); err != nil || got != target {
		t.Errorf("override to a legacy store = %q, %v; want it verbatim", got, err)
	}
}
