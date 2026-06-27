package store

import (
	"context"
	"database/sql"
)

// Ping verifies the chat DB is reachable (used by the admin health summary, #301).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// PoolStats returns the connection-pool snapshot (open/idle/in-use) for the
// admin health summary (#301).
func (s *Store) PoolStats() sql.DBStats {
	return s.db.Stats()
}

// LLMUsageSince returns the number of completed turns and their total USD cost
// since the given unix timestamp — the chat-side LLM spend for the health
// summary (#301). Cancelled turns still count their partial cost.
func (s *Store) LLMUsageSince(ctx context.Context, since int64) (calls int64, costUSD float64, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM turn_metrics WHERE completed_at >= $1`,
		since,
	)
	if err := row.Scan(&calls, &costUSD); err != nil {
		return 0, 0, err
	}
	return calls, costUSD, nil
}
