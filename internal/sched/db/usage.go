// Usage-analytics read model (#601 part 1): aggregation over the metering the
// run loop ALREADY persists — task_iterations (cost_usd, prompt_tokens,
// completion_tokens per iteration) joined to tasks for provenance (created_by,
// created_by_key_id, model). This is strictly a read model: no new table, no
// second accounting path, and nothing here can drift from what the governed
// core recorded.

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskUsageGroupExpr maps a validated group_by value to the SQL grouping
// expression for the task_iterations ⋈ tasks aggregation. The map is the
// injection guard: only these fixed expressions ever reach the query, and an
// unknown group_by errors instead of interpolating caller input.
//
// Dimensional coverage is honest, not padded: tasks carry no project, so
// group_by=project rolls every task row into the empty key (chat is the only
// project-aware meter today); group_by=user resolves created_by → username via
// a LEFT JOIN so deleted users degrade to their UUID rather than vanishing
// from the spend report. Day/week bucket on the iteration's started_at in UTC
// and key the bucket by its start date (ISO weeks for 'week').
var taskUsageGroupExpr = map[string]string{
	"user":    "COALESCE(u.username, COALESCE(t.created_by::text, ''))",
	"key":     "COALESCE(t.created_by_key_id, '')",
	"project": "''",
	"model":   "COALESCE(t.model, '')",
	"day":     "to_char(date_trunc('day', ti.started_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')",
	"week":    "to_char(date_trunc('week', ti.started_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')",
}

// TaskUsage aggregates the persisted per-iteration metering over [from, to)
// (filtered on the iteration's started_at) grouped by groupBy — one of
// user|key|project|model|day|week. Every iteration in range counts, including
// failed/cancelled ones: their cost was still spent. NULL metering columns
// (older rows) sum as 0.
// TaskUsageByUserDay aggregates the same per-iteration metering over
// [from, to) on BOTH the user and UTC-day dimensions at once — the adoption
// report needs per-user daily series (active days, sparklines), which the
// one-dimensional TaskUsage grouping can't express. Same provenance rules:
// created_by resolves to the username with a UUID fallback for deleted users,
// every iteration counts (failed/cancelled cost was still spent), and the
// grouping expressions are fixed strings so no caller input reaches SQL.
func (db *Database) TaskUsageByUserDay(ctx context.Context, from, to time.Time) ([]models.UserDayUsage, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			`+taskUsageGroupExpr["user"]+` AS bucket_user,
			`+taskUsageGroupExpr["day"]+` AS bucket_day,
			COALESCE(SUM(ti.cost_usd), 0)::float8,
			COALESCE(SUM(ti.prompt_tokens), 0),
			COALESCE(SUM(ti.completion_tokens), 0),
			COUNT(*)
		FROM task_iterations ti
		JOIN tasks t ON t.id = ti.task_id
		LEFT JOIN users u ON u.id = t.created_by
		WHERE ti.started_at >= $1 AND ti.started_at < $2
		GROUP BY 1, 2
		ORDER BY 1, 2`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.UserDayUsage
	for rows.Next() {
		var r models.UserDayUsage
		if err := rows.Scan(&r.User, &r.Day, &r.CostUSD, &r.PromptTokens, &r.CompletionTokens, &r.Units); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *Database) TaskUsage(ctx context.Context, from, to time.Time, groupBy string) ([]models.UsageBucket, error) {
	expr, ok := taskUsageGroupExpr[groupBy]
	if !ok {
		return nil, fmt.Errorf("invalid group_by %q", groupBy)
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			`+expr+` AS bucket_key,
			COALESCE(SUM(ti.cost_usd), 0)::float8,
			COALESCE(SUM(ti.prompt_tokens), 0),
			COALESCE(SUM(ti.completion_tokens), 0),
			COUNT(*)
		FROM task_iterations ti
		JOIN tasks t ON t.id = ti.task_id
		LEFT JOIN users u ON u.id = t.created_by
		WHERE ti.started_at >= $1 AND ti.started_at < $2
		GROUP BY 1
		ORDER BY 1`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.UsageBucket
	for rows.Next() {
		var b models.UsageBucket
		if err := rows.Scan(&b.Key, &b.TaskCostUSD, &b.PromptTokens, &b.CompletionTokens, &b.TaskIterations); err != nil {
			return nil, err
		}
		b.CostUSD = b.TaskCostUSD
		out = append(out, b)
	}
	return out, rows.Err()
}
