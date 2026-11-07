package fleetdb

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestOneDatabaseBothSchemas: a fleetdb database is the production
// one-database layout — DATABASE_URL alone resolves to it in schema mode,
// both stores open on it with nothing to migrate, each in its own schema.
func TestOneDatabaseBothSchemas(t *testing.T) {
	t.Parallel()
	d := New(t)
	r := config.ResolveDSNs(func(k string) string {
		if k == "DATABASE_URL" {
			return d.DSN
		}
		return ""
	})
	if r.Layout != config.LayoutSchemas || r.Chat != d.ChatDSN() || r.Sched != d.SchedDSN() {
		t.Fatalf("ResolveDSNs(DATABASE_URL=%s) = %+v, want schema mode with the fixture's DSNs", d.DSN, r)
	}
	chat, sched := d.Chat(t), d.SchedDB(t)
	if _, err := chat.CreateUser(t.Context(), "fleetdb@example.com", "placeholder-password"); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := sched.Conn().QueryRowContext(t.Context(), `SELECT table_schema FROM information_schema.tables WHERE table_name = 'tasks'`).Scan(&schema); err != nil || schema != config.SchedSchema {
		t.Fatalf("tasks table in schema %q (%v), want %q", schema, err, config.SchedSchema)
	}
	var n int
	if err := sched.Conn().QueryRowContext(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("public holds %d tables (%v), want none", n, err)
	}
	// Two fixtures are two databases.
	if New(t).DSN == d.DSN {
		t.Fatal("New returned the same database twice")
	}
}
