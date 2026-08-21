package db

import (
	"context"
	"database/sql"
	"math"
	"strconv"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// GetRunningTasksWithSLA returns the in-flight tasks that carry an SLA
// (expected_duration_minutes IS NOT NULL) for the SLA monitor goroutine (#274).
// "In-flight" mirrors GetRunningTasks: leased / running — the
// statuses where StartedAt is set and the task has not yet reached a terminal
// state. The partial index idx_tasks_sla does NOT cover this query (it is
// keyed on completed_at), but the in-flight set is small (one host, capped
// pool) so a seq scan filtered by status + expected_duration_minutes IS NOT
// NULL is cheap; an extra index would not pay for itself.
func (db *Database) GetRunningTasksWithSLA(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2)
		AND expected_duration_minutes IS NOT NULL`,
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// MarkSLABreached latches sla_breached=true on a task the SLA monitor flagged
// as having crossed its fail threshold (#274). It is a narrow, single-column
// UPDATE so it cannot race a concurrent terminal-status write on the broader
// row. Idempotent: setting true on an already-breached row is a no-op.
func (db *Database) MarkSLABreached(ctx context.Context, taskID uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE tasks SET sla_breached = TRUE WHERE id = $1`, taskID)
	return err
}

// GetSLAReport aggregates the per-prompt SLA actuals over the last windowDays
// (#274): the p50/p95 actual run duration and the breach rate for each
// (prompt, expected_duration_minutes) bucket. Rows without an expected duration
// or an actual duration are excluded. windowDays is clamped to [1, 90]; the
// partial index idx_tasks_sla backs the WHERE filter. Buckets are ordered by
// breach rate (worst first) so the most violated SLAs surface at the top.
//
// The window uses make_interval(days => $1) rather than ($1 || ' days')::INTERVAL:
// the latter makes Postgres infer $1 as TEXT, which the pgx driver then refuses
// to encode a Go int into ("cannot find encode plan"), so the report errored for
// every caller. make_interval's days param is typed int, so the bound int4
// encodes cleanly. Do NOT revert to string concatenation (#458).
func (db *Database) GetSLAReport(ctx context.Context, windowDays int) (*models.SLAReport, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	if windowDays > 90 {
		windowDays = 90
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			-- Group by the operator's title when the job has one: every
			-- occurrence of a recurring task shares its title, so titled jobs
			-- collapse into one row per job instead of one per prompt variant.
			-- Untitled tasks keep the historical prompt grouping.
			COALESCE(NULLIF(title, ''), prompt)                      AS task_name,
			expected_duration_minutes,
			COALESCE(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY actual_duration_seconds), 0) / 60.0,
			COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY actual_duration_seconds), 0) / 60.0,
			CASE WHEN COUNT(*) = 0 THEN 0.0
			     ELSE 100.0 * SUM(CASE WHEN sla_breached THEN 1 ELSE 0 END) / COUNT(*) END,
			COUNT(*)
		FROM tasks
		WHERE completed_at >= NOW() - make_interval(days => $1)
		AND expected_duration_minutes IS NOT NULL
		AND actual_duration_seconds IS NOT NULL
		GROUP BY COALESCE(NULLIF(title, ''), prompt), expected_duration_minutes
		ORDER BY 5 DESC, 1 ASC`,
		windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &models.SLAReport{
		Period:     "last_" + strconv.Itoa(windowDays) + "_days",
		WindowDays: windowDays,
		Tasks:      []models.SLAReportTask{},
	}
	for rows.Next() {
		var (
			taskName    string
			expectedMin sql.NullInt64
			p50Min      sql.NullFloat64
			p95Min      sql.NullFloat64
			breachRate  sql.NullFloat64
			sampleCount sql.NullInt64
		)
		if err := rows.Scan(&taskName, &expectedMin, &p50Min, &p95Min, &breachRate, &sampleCount); err != nil {
			return nil, err
		}
		row := models.SLAReportTask{TaskName: taskName}
		if expectedMin.Valid {
			row.ExpectedMinutes = int(expectedMin.Int64)
		}
		if p50Min.Valid {
			row.P50ActualMinutes = p50Min.Float64
		}
		if p95Min.Valid {
			row.P95ActualMinutes = p95Min.Float64
		}
		if breachRate.Valid {
			// Round to 1 decimal place, mirroring the SQL ROUND(...,1) in the issue.
			row.BreachRatePercent = math.Round(breachRate.Float64*10) / 10
		}
		if sampleCount.Valid {
			row.SampleCount = int(sampleCount.Int64)
		}
		out.Tasks = append(out.Tasks, row)
	}
	return out, rows.Err()
}
