package admincli

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestStatusFailsWhatTheRunnerRefuses: `fleet status` fails its migrations
// line on every ledger migrations.Up refuses — a version a newer fleet wrote,
// a row recorded from an edited migration — not only on a dirty one, and
// passes on a current one (Codex 4506166386 on #2087).
func TestStatusFailsWhatTheRunnerRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		name, mutate, want string
		fails              bool
	}{
		{"current", ``, "adopted (core.schema_versions)", false},
		{"unknown version", `INSERT INTO core.schema_versions (version, name, checksum) VALUES (400, '0400_from_a_newer_fleet', 'x')`, "version(s) 400 are not shipped", true},
		{"edited migration", `UPDATE core.schema_versions SET checksum = 'edited' WHERE version = 120`, "version(s) 120 were recorded from a different file", true},
		{"dirty", `UPDATE core.schema_versions SET dirty = true WHERE version = 177`, "DIRTY", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dsn := testdb.Empty(t)
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := migrations.Up(ctx, db); err != nil {
				t.Fatal(err)
			}
			if c.mutate != "" {
				if _, err := db.ExecContext(ctx, c.mutate); err != nil {
					t.Fatal(err)
				}
			}
			var buf bytes.Buffer
			r := &report{out: &buf, summary: &buf}
			checkMigrations(r, config.WithSchema(dsn, config.ChatSchema))
			if (r.failed > 0) != c.fails || !strings.Contains(buf.String(), c.want) {
				t.Fatalf("failed=%d, want fails=%v; output:\n%s", r.failed, c.fails, buf.String())
			}
		})
	}
}
