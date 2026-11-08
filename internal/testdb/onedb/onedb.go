// Package onedb is the one-database test template (#2049 M2, M6): a database
// migrated by the one migration runner (internal/migrations) — schemas chat,
// core and sched, exactly as a fresh `fleet serve` leaves it.
//
// It imports neither store, so every Postgres-backed test package clones
// this one template: internal/testdb/fleetdb (which opens the stores on it)
// and the stores' own internal tests, which cannot import fleetdb.
package onedb

import (
	"context"
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib" // the pgx database/sql driver

	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// Spec is the template: keyed by the one migration set's fingerprint.
var Spec = testdb.Spec{
	Name:        "fleet",
	Fingerprint: migrations.Fingerprint,
	Migrate: func(ctx context.Context, dsn string) error {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(2)
		return migrations.Up(ctx, db)
	},
}
