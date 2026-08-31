package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// GetAllTasks gets all tasks.
func (db *Database) GetAllTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetScheduledTasks gets scheduled tasks that are due to run up to a limit,
// strictly after the (afterScheduledFor, afterID) keyset cursor in the total
// order (scheduled_for ASC, id ASC). Pass the zero time.Time / uuid.Nil to
// start from the beginning.
//
// Keyset pagination (not plain LIMIT) is what lets the scheduler page past
// soft-held rows that stay in the due set (#566): a row whose run_if gate
// declines keeps its scheduled_for, so with LIMIT-only paging a full page of
// held rows re-fetched identically forever. The tiebreaking `id` column
// matters: scheduled_for alone is not a total order, so a LIMIT prefix over it
// is not stable across queries and rows tied at the boundary could be masked
// indefinitely within one pass sequence.
func (db *Database) GetScheduledTasks(ctx context.Context, cutoff time.Time, afterScheduledFor time.Time, afterID uuid.UUID, limit int) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		AND scheduled_for IS NOT NULL
		AND scheduled_for <= $2
		AND trigger_type = 'cron'
		AND (scheduled_for, id) > ($3, $4)
		ORDER BY scheduled_for ASC, id ASC
		LIMIT $5`,
		string(models.TaskStatusScheduled), cutoff, afterScheduledFor, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetAllScheduledTasks returns all scheduled tasks regardless of due time.
func (db *Database) GetAllScheduledTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		ORDER BY scheduled_for ASC`,
		string(models.TaskStatusScheduled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// ListTasksForExport returns task definitions for GET /tasks/export (#238). It
// is a complete snapshot (no pagination) so the caller can download the whole
// file. ids, when non-empty, limits the result to those task IDs (the ?ids=
// filter); an empty slice exports every task. recurrenceOnly, when true,
// restricts the result to tasks with a non-empty recurrence (cron tasks only —
// the ?recurrence_only=true filter). Ordered by created_at for a stable diff.
func (db *Database) ListTasksForExport(ctx context.Context, ids []uuid.UUID, recurrenceOnly bool) ([]*models.Task, error) {
	q := "SELECT " + taskColumns + " FROM tasks WHERE 1=1"
	args := []any{}
	if len(ids) > 0 {
		q += " AND id = ANY($1::uuid[])"
		args = append(args, uuidStrings(ids))
	}
	if recurrenceOnly {
		q += " AND COALESCE(recurrence, '') <> ''"
	}
	q += " ORDER BY created_at ASC, id ASC"
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// FindTaskIDsByName resolves task IDs by non-empty name (#238). It is the
// pre-flight conflict-detection query for POST /tasks/import: a name present in
// the returned map collides with an existing task. Empty names are never
// matched (they cannot collide by name). Names are matched case-sensitively.
func (db *Database) FindTaskIDsByName(ctx context.Context, names []string) (map[string]uuid.UUID, error) {
	out := make(map[string]uuid.UUID)
	var filtered []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == 0 {
		return out, nil
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, name FROM tasks
		WHERE name = ANY($1::text[]) AND name <> ''`,
		filtered)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}

// GetTaskByName returns the task whose non-empty name matches, or (nil, nil)
// when no such task exists. Used by import conflict=replace to fetch the row to
// update in place (#238).
func (db *Database) GetTaskByName(ctx context.Context, name string) (*models.Task, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	row := db.conn.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE name = $1 AND name <> ''", name)
	t, err := db.scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// GetPendingTasks gets all pending tasks, ordered the same way the claim path
// dispatches them: effective_priority ASC (lower = more urgent, #230), then
// created_at ASC (FIFO within a tier).
func (db *Database) GetPendingTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		ORDER BY effective_priority ASC, created_at ASC`,
		string(models.TaskStatusPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// PendingQueueStats returns the per-effective-priority rollup of the pending
// queue (#230): the count and the longest wait (seconds) at each distinct
// effective_priority. The handler aggregates these into named tiers for
// GET /admin/queue.
func (db *Database) PendingQueueStats(ctx context.Context) ([]models.QueuePriorityBucket, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT effective_priority,
		       COUNT(*),
		       COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(created_at)))::bigint, 0)
		FROM tasks
		WHERE status = $1
		GROUP BY effective_priority
		ORDER BY effective_priority ASC`,
		string(models.TaskStatusPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.QueuePriorityBucket, 0)
	for rows.Next() {
		var b models.QueuePriorityBucket
		var ageSeconds int64
		if err := rows.Scan(&b.Priority, &b.Count, &ageSeconds); err != nil {
			return nil, err
		}
		b.OldestAgeSeconds = int(ageSeconds)
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetRunningTasks gets all currently running tasks.
func (db *Database) GetRunningTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2)`,
		string(models.TaskStatusRunning),
		string(models.TaskStatusLeased))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetTasksByStatus gets all tasks with a specific status.
func (db *Database) GetTasksByStatus(ctx context.Context, status models.TaskStatus) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE status = $1", string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetDeadLetteredTasks returns dead-lettered tasks (#253), ordered by when they
// entered the queue (newest first) for the DLQ review listing. A non-positive
// limit returns every matching row; otherwise limit/offset paginate. The partial
// index on dead_lettered_at (migration 034) backs the ORDER BY.
func (db *Database) GetDeadLetteredTasks(ctx context.Context, limit, offset int) ([]*models.Task, error) {
	query := "SELECT " + taskColumns + " FROM tasks WHERE status = $1 ORDER BY dead_lettered_at DESC NULLS LAST"
	args := []any{string(models.TaskStatusDeadLettered)}
	if limit > 0 {
		query += " LIMIT $2 OFFSET $3"
		args = append(args, limit, offset)
	}
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetTasksCompletedToday gets tasks completed today.
func (db *Database) GetTasksCompletedToday(ctx context.Context) ([]*models.Task, error) {
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)

	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE completed_at BETWEEN $1 AND $2",
		todayStart, todayEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetDashboardStats gets statistics for the dashboard.
func (db *Database) GetDashboardStats(ctx context.Context) (*models.DashboardStats, error) {
	stats := &models.DashboardStats{}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)

	err := db.conn.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = $1) as pending_tasks,
			COUNT(*) FILTER (WHERE status IN ($2, $3)) as running_tasks,
			COUNT(*) FILTER (WHERE status = $4 AND completed_at BETWEEN $5 AND $6) as completed_today,
			COUNT(*) FILTER (WHERE status = $7 AND completed_at BETWEEN $5 AND $6) as failed_today
		FROM tasks`,
		string(models.TaskStatusPending),
		string(models.TaskStatusRunning),
		string(models.TaskStatusLeased),
		string(models.TaskStatusSuccess),
		todayStart,
		todayEnd,
		string(models.TaskStatusError),
	).Scan(&stats.PendingTasks, &stats.RunningTasks, &stats.CompletedTasksToday, &stats.FailedTasksToday)
	if err != nil {
		return nil, fmt.Errorf("failed to get task stats: %w", err)
	}
	return stats, nil
}

// GetAllTasksPaginated gets tasks with pagination.
func (db *Database) GetAllTasksPaginated(ctx context.Context, limit, offset int) ([]*models.Task, int, error) {
	var total int
	err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks ORDER BY created_at DESC LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tasks, err := db.rowsToTasks(rows)
	return tasks, total, err
}

// TaskFilter contains optional filter parameters for task queries.
type TaskFilter struct {
	Status          *string
	Query           *string
	ScheduledOnly   bool
	CompletedToday  bool
	CompletedStatus *string
	CreatedBy       *uuid.UUID
	// HasDescription, when true, restricts to tasks carrying operator
	// documentation (#281): a non-null, non-empty description.
	HasDescription bool
	// Tags, when non-empty, restricts to tasks carrying ALL of these tags
	// (AND-semantics via jsonb containment) — #212.
	Tags []string
	// SourceTaskID, when set, restricts to tasks re-run/cloned from that source
	// task — the lineage view (#270).
	SourceTaskID *uuid.UUID
	// VisibleToUserID / VisibleToKeyID restrict to rows the principal created
	// (#1082): tasks whose created_by is this user, or whose created_by_key_id
	// is this API key. Set by the handler for principals without the
	// fleet-wide visibility grant; at most one is set per request. They AND
	// with every other filter, so a caller-supplied created_by can only
	// narrow further, never widen.
	VisibleToUserID *uuid.UUID
	VisibleToKeyID  *string
	// StatusIn, when non-empty, restricts to tasks in ANY of these statuses.
	// Added for the A2A ListTasks status filter (#1279), where one A2A
	// TaskState fans out to several fleet statuses (SUBMITTED covers
	// pending/scheduled/leased) — the single-status Status field cannot
	// express that without breaking pagination totals. ANDs with Status when
	// both are set, like every other filter.
	StatusIn []string
}

// GetTasksFiltered gets tasks with optional filters and pagination.
func (db *Database) GetTasksFiltered(ctx context.Context, filter TaskFilter, limit, offset int) ([]*models.Task, int, error) {
	whereClauses := []string{}
	args := []interface{}{}
	argIndex := 1

	if filter.Status != nil && *filter.Status != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argIndex))
		args = append(args, *filter.Status)
		argIndex++
	}

	if len(filter.StatusIn) > 0 {
		placeholders := make([]string, len(filter.StatusIn))
		for i, s := range filter.StatusIn {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, s)
			argIndex++
		}
		whereClauses = append(whereClauses, "status IN ("+strings.Join(placeholders, ", ")+")")
	}

	if filter.Query != nil && *filter.Query != "" {
		query := strings.TrimSpace(*filter.Query)
		if id, err := uuid.Parse(query); err == nil {
			whereClauses = append(whereClauses, fmt.Sprintf("id = $%d", argIndex))
			args = append(args, id)
			argIndex++
		} else {
			// title is matched alongside prompt: once a job has a title, the
			// title is what the operator remembers it by, and searching only the
			// prompt would fail to find a task by the label the list displays.
			whereClauses = append(whereClauses, fmt.Sprintf("(title ILIKE $%d OR prompt ILIKE $%d OR CAST(id AS TEXT) ILIKE $%d)", argIndex, argIndex, argIndex))
			args = append(args, "%"+query+"%")
			argIndex++
		}
	}

	if filter.ScheduledOnly {
		whereClauses = append(whereClauses, "(scheduled_for IS NOT NULL OR recurrence IS NOT NULL AND recurrence != '')")
	}

	if filter.CompletedToday {
		now := time.Now().UTC()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)
		whereClauses = append(whereClauses, fmt.Sprintf("completed_at BETWEEN $%d AND $%d", argIndex, argIndex+1))
		args = append(args, todayStart, todayEnd)
		argIndex += 2
		if filter.CompletedStatus != nil && *filter.CompletedStatus != "" {
			whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argIndex))
			args = append(args, *filter.CompletedStatus)
			argIndex++
		}
	}

	if filter.CreatedBy != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_by = $%d", argIndex))
		args = append(args, *filter.CreatedBy)
		argIndex++
	}

	if filter.HasDescription {
		whereClauses = append(whereClauses, "description IS NOT NULL AND description <> ''")
	}

	if len(filter.Tags) > 0 {
		// AND-semantics: task tags must contain ALL requested tags. jsonb `@>`
		// (contains) over the GIN index; the bind value is a JSON array string.
		whereClauses = append(whereClauses, fmt.Sprintf("tags @> $%d::jsonb", argIndex))
		args = append(args, marshalTags(filter.Tags))
		argIndex++
	}

	if filter.SourceTaskID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("source_task_id = $%d", argIndex))
		args = append(args, filter.SourceTaskID.String())
		argIndex++
	}

	if filter.VisibleToUserID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_by = $%d", argIndex))
		args = append(args, *filter.VisibleToUserID)
		argIndex++
	}

	if filter.VisibleToKeyID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_by_key_id = $%d", argIndex))
		args = append(args, *filter.VisibleToKeyID)
		argIndex++
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM tasks " + whereSQL
	err := db.conn.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	dataQuery := fmt.Sprintf("SELECT %s FROM tasks %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d",
		taskColumns, whereSQL, argIndex, argIndex+1)
	args = append(args, limit, offset)

	rows, err := db.conn.QueryContext(ctx, dataQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tasks, err := db.rowsToTasks(rows)
	return tasks, total, err
}

// TagCount is one row of the tag catalogue: a distinct tag and how many tasks
// carry it (#212).
type TagCount struct {
	Tag       string `json:"tag"`
	TaskCount int    `json:"task_count"`
}

// GetTagCatalogue returns every distinct tag in use with its task count, busiest
// first (then alphabetical). Drives GET /tasks/tags.
func (db *Database) GetTagCatalogue(ctx context.Context) ([]TagCount, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT tag, COUNT(*) AS task_count
		FROM tasks, jsonb_array_elements_text(tags) AS tag
		GROUP BY tag
		ORDER BY task_count DESC, tag ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.TaskCount); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
