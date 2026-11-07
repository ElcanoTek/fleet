package main

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestCheckDatabaseRunsStoreReadiness (#2055 review): validate-config runs the
// same read-only readiness check dbschema.Ensure runs at boot, so a database
// that answers SELECT 1 but that the stores would refuse — here a legacy
// install's database, whose public schema holds a fleet store — fails the
// check instead of predicting a clean boot.
func TestCheckDatabaseRunsStoreReadiness(t *testing.T) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set")
	}
	at := func(db string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + db
		return u.String()
	}
	admin, err := sql.Open("pgx", at("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const db = "fleet_vc_readiness_test"
	drop := func() { _, _ = admin.ExecContext(t.Context(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)") }
	drop()
	t.Cleanup(drop)
	if _, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+db); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	scratch, err := sql.Open("pgx", at(db))
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Close()
	if _, err := scratch.ExecContext(t.Context(), "CREATE TABLE public.schema_migrations (version int)"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DatabaseURL:      config.WithSchema(at(db), config.ChatSchema),
		SchedDatabaseURL: config.WithSchema(at(db), config.SchedSchema),
	}
	res := checkDatabase(t.Context(), cfg, nil, validateOptions{})
	if res.Status != statusFail || !strings.Contains(res.Detail, "removed two-database layout") {
		t.Fatalf("a legacy store in public = %s: %s, want a failure pointing at consolidation", res.Status, res.Detail)
	}
	if _, err := scratch.ExecContext(t.Context(), "DROP TABLE public.schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if res := checkDatabase(t.Context(), cfg, nil, validateOptions{}); res.Status != statusOK {
		t.Fatalf("a ready database = %s: %s", res.Status, res.Detail)
	}
}
