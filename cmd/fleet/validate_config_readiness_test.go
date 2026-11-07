package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestCheckDatabaseRunsStoreReadiness (#2055 review): validate-config runs the
// same read-only readiness check dbschema.Ensure runs at boot, so a database
// that answers SELECT 1 but that the stores would refuse — here a legacy
// install's database, whose public schema holds a fleet store — fails the
// check instead of predicting a clean boot.
func TestCheckDatabaseRunsStoreReadiness(t *testing.T) {
	dbDSN := testdb.Empty(t)
	scratch, err := sql.Open("pgx", dbDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Close()
	if _, err := scratch.ExecContext(t.Context(), "CREATE TABLE public.schema_migrations (version int)"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DatabaseURL:      config.WithSchema(dbDSN, config.ChatSchema),
		SchedDatabaseURL: config.WithSchema(dbDSN, config.SchedSchema),
	}
	res := checkDatabase(t.Context(), cfg, nil, validateOptions{})
	if res.Status != statusFail || !strings.Contains(res.Detail, "fleet db consolidate") {
		t.Fatalf("a legacy store in public = %s: %s, want a failure pointing at consolidation", res.Status, res.Detail)
	}
	if _, err := scratch.ExecContext(t.Context(), "DROP TABLE public.schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if res := checkDatabase(t.Context(), cfg, nil, validateOptions{}); res.Status != statusOK {
		t.Fatalf("a ready database = %s: %s", res.Status, res.Detail)
	}
}
