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

// AgentRuntimeCounts summarizes sub-agent delegations, tool executions, and
// recently active conversations from the chat database for the Server & Live
// Agents admin view.
type AgentRuntimeCounts struct {
	SubagentsSpawnedTotal  int64
	SubagentsSpawned24h    int64
	ToolCalls24h           int64
	ToolCallsTotal         int64
	ActiveConversations24h int64
}

// AgentRuntimeCounts queries chat.tool_call_log and chat.messages for 24h and
// cumulative bot/agent execution totals since sinceUnix (unix seconds).
func (s *Store) AgentRuntimeCounts(ctx context.Context, sinceUnix int64) (AgentRuntimeCounts, error) {
	if s == nil || s.db == nil {
		return AgentRuntimeCounts{}, nil
	}
	var out AgentRuntimeCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN started_at >= $1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN tool_name = 'spawn_subagent' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN tool_name = 'spawn_subagent' AND started_at >= $1 THEN 1 ELSE 0 END), 0)
		FROM tool_call_log`,
		sinceUnix,
	).Scan(
		&out.ToolCallsTotal,
		&out.ToolCalls24h,
		&out.SubagentsSpawnedTotal,
		&out.SubagentsSpawned24h,
	)
	if err != nil {
		return out, err
	}
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT conversation_id) FROM messages WHERE created_at >= $1`,
		sinceUnix,
	).Scan(&out.ActiveConversations24h)
	return out, nil
}
