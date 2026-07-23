package store

// Usage-analytics read model (#601 part 1), chat side: aggregation over the
// turn_metrics rows RecordTurn already persists (one per completed turn,
// success or cancelled — the cost was spent either way). Strictly a read
// model: no new table, no parallel accounting path. Dimensions come from the
// conversation the turn belongs to (model override, project) — turn_metrics
// itself carries only the principal email and the meters.

import (
	"context"
	"fmt"
	"time"
)

// UsageRow is one aggregated bucket of chat spend for UsageSummary. Key is the
// grouping value (email, project id, model slug, or YYYY-MM-DD bucket start);
// the empty key collects turns without the dimension (no project, the default
// model, or — for group_by=key — every turn, since chat has no API-key
// dimension). Label is the project name when grouping by project.
type UsageRow struct {
	Key              string
	Label            string
	CostUSD          float64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Turns            int64
}

// UserDayUsage is one per-user-per-day aggregation row for the adoption
// report's chat side: the same meters as UsageRow, but on both the user and
// UTC-day dimensions at once (per-user daily series can't be expressed by the
// one-dimensional UsageSummary grouping). Turns counts completed turns that
// day, cancelled included.
type UserDayUsage struct {
	User             string
	Day              string // YYYY-MM-DD, UTC
	CostUSD          float64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Turns            int64
}

// UsageByUserDay aggregates turn_metrics over [from, to) grouped by
// (user_email, UTC day of completed_at). No conversation join: every
// dimension lives on the metric row itself, so spend attribution here
// survives conversation deletion with nothing degrading to an empty key.
func (s *Store) UsageByUserDay(ctx context.Context, from, to time.Time) ([]UserDayUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			m.user_email,
			`+chatUsageGroupExpr["day"]+` AS bucket_day,
			COALESCE(SUM(m.cost_usd), 0),
			COALESCE(SUM(m.prompt_tokens), 0),
			COALESCE(SUM(m.completion_tokens), 0),
			COALESCE(SUM(m.cached_tokens), 0),
			COUNT(*)
		FROM turn_metrics m
		WHERE m.completed_at >= $1 AND m.completed_at < $2
		GROUP BY 1, 2
		ORDER BY 1, 2`,
		from.UTC().Unix(), to.UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UserDayUsage
	for rows.Next() {
		var r UserDayUsage
		if err := rows.Scan(&r.User, &r.Day, &r.CostUSD, &r.PromptTokens, &r.CompletionTokens, &r.CachedTokens, &r.Turns); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// chatUsageGroupExpr maps a validated group_by value to its SQL grouping
// expression. Fixed map = injection guard: unknown group_by errors instead of
// interpolating caller input. conversations/projects are LEFT-joined so a
// metric row whose conversation was deleted still counts (under the empty
// key) — spend must never silently disappear from the report.
var chatUsageGroupExpr = map[string]string{
	"user":    "m.user_email",
	"key":     "''",
	"project": "COALESCE(c.project_id, '')",
	"model":   "COALESCE(c.model, '')",
	"day":     "to_char(date_trunc('day', to_timestamp(m.completed_at) AT TIME ZONE 'UTC'), 'YYYY-MM-DD')",
	"week":    "to_char(date_trunc('week', to_timestamp(m.completed_at) AT TIME ZONE 'UTC'), 'YYYY-MM-DD')",
}

// UsageSummary aggregates turn_metrics over [from, to) (on the turn's
// completed_at) grouped by groupBy — one of user|key|project|model|day|week.
// Cancelled turns count: their partial cost/tokens were still consumed.
func (s *Store) UsageSummary(ctx context.Context, from, to time.Time, groupBy string) ([]UsageRow, error) {
	expr, ok := chatUsageGroupExpr[groupBy]
	if !ok {
		return nil, fmt.Errorf("invalid group_by %q", groupBy)
	}
	label := "''"
	if groupBy == "project" {
		label = "COALESCE(MAX(p.name), '')"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			`+expr+` AS bucket_key,
			`+label+` AS bucket_label,
			COALESCE(SUM(m.cost_usd), 0),
			COALESCE(SUM(m.prompt_tokens), 0),
			COALESCE(SUM(m.completion_tokens), 0),
			COALESCE(SUM(m.cached_tokens), 0),
			COUNT(*)
		FROM turn_metrics m
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN projects p ON p.id = c.project_id
		WHERE m.completed_at >= $1 AND m.completed_at < $2
		GROUP BY 1
		ORDER BY 1`,
		from.UTC().Unix(), to.UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Label, &r.CostUSD, &r.PromptTokens, &r.CompletionTokens, &r.CachedTokens, &r.Turns); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
