package store

import (
	"context"

	"github.com/ElcanoTek/fleet/internal/migrations"
)

// The chat store's schema is migrated by the one migration runner
// (internal/migrations, ADR-0107): Open calls migrations.Up, which brings the
// whole database — chat, core and sched — to this build's latest version.
// Its files live in internal/migrations/sql (docs/MIGRATIONS.md).

// MigrationStatus reports applied vs pending migrations of the one sequence
// (GET /admin/migrations on the chat server), read-only, so the HTTP layer
// can report it through the narrow chatStore interface without exposing the
// underlying *sql.DB.
func (s *Store) MigrationStatus(ctx context.Context) (migrations.Report, error) {
	return migrations.Status(ctx, s.db)
}
