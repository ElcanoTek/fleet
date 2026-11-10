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
	"user":    "COALESCE(u.email, COALESCE(t.created_by::text, ''))",
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
		LEFT JOIN core.users u ON u.id = t.created_by
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
		LEFT JOIN core.users u ON u.id = t.created_by
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

// TaskOutlierRow summarizes one high-spend, high-token, high-iteration, or
// failing background task for /analytics.
type TaskOutlierRow struct {
	TaskID           string  `json:"task_id"`
	Title            string  `json:"title"`
	OwnerEmail       string  `json:"owner_email"`
	Status           string  `json:"status"`
	Model            string  `json:"model"`
	Recurrence       string  `json:"recurrence,omitempty"`
	Iterations       int64   `json:"iterations"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	ErrorMessage     string  `json:"error_message,omitempty"`
}

// TopTaskOutliers returns top background tasks ranked across cost, token
// volume, iteration count, and failure states over the last 30 days.
func (db *Database) TopTaskOutliers(ctx context.Context, limit int) ([]TaskOutlierRow, error) {
	if limit <= 0 {
		limit = 25
	}
	perRank := max(10, limit/2)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	rows, err := db.conn.QueryContext(ctx, `
		WITH ti_agg AS (
			SELECT task_id,
			       COUNT(*) AS iterations,
			       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
			       COALESCE(SUM(cost_usd), 0)::float8 AS cost_usd
			FROM task_iterations
			WHERE started_at >= $1
			GROUP BY task_id
		),
		base AS (
			SELECT
				t.id::text AS task_id,
				COALESCE(NULLIF(BTRIM(t.title), ''), LEFT(BTRIM(t.prompt), 80)) AS title,
				COALESCE(u.email, '') AS owner_email,
				t.status::text AS status,
				COALESCE(t.model, '') AS model,
				COALESCE(t.recurrence, '') AS recurrence,
				COALESCE(ti_agg.iterations, 0) AS iterations,
				COALESCE(ti_agg.prompt_tokens, 0) AS prompt_tokens,
				COALESCE(ti_agg.completion_tokens, 0) AS completion_tokens,
				COALESCE(ti_agg.cost_usd, 0)::float8 AS cost_usd,
				COALESCE(t.error_message, COALESCE(t.dead_letter_reason, '')) AS error_message
			FROM tasks t
			LEFT JOIN ti_agg ON ti_agg.task_id = t.id
			LEFT JOIN core.users u ON u.id = t.created_by
			WHERE ti_agg.task_id IS NOT NULL
			   OR (COALESCE(t.completed_at, t.dead_lettered_at, t.created_at) >= $1 AND t.status IN ('error', 'dead_lettered'))
		),
		ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (ORDER BY cost_usd DESC, (prompt_tokens + completion_tokens) DESC) AS rk_cost,
				ROW_NUMBER() OVER (ORDER BY (prompt_tokens + completion_tokens) DESC, iterations DESC) AS rk_tokens,
				ROW_NUMBER() OVER (ORDER BY iterations DESC, (prompt_tokens + completion_tokens) DESC) AS rk_iters,
				ROW_NUMBER() OVER (ORDER BY CASE WHEN status IN ('error', 'dead_lettered') THEN 1 ELSE 0 END DESC, cost_usd DESC) AS rk_fail
			FROM base
		)
		SELECT
			task_id, title, owner_email, status, model, recurrence,
			iterations, prompt_tokens, completion_tokens, cost_usd, error_message
		FROM ranked
		WHERE rk_cost <= $2 OR rk_tokens <= $2 OR rk_iters <= $2 OR (status IN ('error', 'dead_lettered') AND rk_fail <= $2)
		ORDER BY
			LEAST(rk_cost, rk_tokens, rk_iters, CASE WHEN status IN ('error', 'dead_lettered') THEN rk_fail ELSE 999999 END) ASC,
			cost_usd DESC, (prompt_tokens + completion_tokens) DESC, iterations DESC
		LIMIT $3`,
		cutoff, perRank, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TaskOutlierRow
	for rows.Next() {
		var r TaskOutlierRow
		if err := rows.Scan(
			&r.TaskID,
			&r.Title,
			&r.OwnerEmail,
			&r.Status,
			&r.Model,
			&r.Recurrence,
			&r.Iterations,
			&r.PromptTokens,
			&r.CompletionTokens,
			&r.CostUSD,
			&r.ErrorMessage,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
