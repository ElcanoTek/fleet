package admincli

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestDatabaseURLOverrideProbesTheTarget: a --database-url naming a database
// other than the deployment's (a DR host, a restored copy) gets its layout
// from that database's data — the store's schema for a one-database install
// or an empty database, verbatim (public) for a legacy store (#2055 review).
func TestDatabaseURLOverrideProbesTheTarget(t *testing.T) {
	ctx := context.Background()
	target := testdb.Empty(t)

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
