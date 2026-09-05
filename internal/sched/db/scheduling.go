package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// UpdateTasksStatusBatch transitions tasks from fromStatus to toStatus, skipping
// any that have left fromStatus. Scheduled-to-pending promotion additionally
// rechecks that the current definition is due, cron-triggered, and ungated.
// Returns the number transitioned.
func (db *Database) UpdateTasksStatusBatch(ctx context.Context, taskIDs []uuid.UUID, fromStatus, toStatus models.TaskStatus) (int, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	query := `UPDATE tasks SET status = $1
		WHERE id = ANY($2::uuid[]) AND status = $3`
	if fromStatus == models.TaskStatusScheduled && toStatus == models.TaskStatusPending {
		// Selection and promotion are separate statements. A concurrent edit
		// can postpone a task, add a gate, or turn it into an inert template
		// without changing its status. Recheck dispatch eligibility under the
		// UPDATE's row lock so the stale selection cannot bypass that edit.
		query += ` AND scheduled_for <= now() AND trigger_type = 'cron' AND run_if IS NULL`
	}
	res, err := db.conn.ExecContext(ctx, query,
		string(toStatus),
		uuidStrings(taskIDs),
		string(fromStatus),
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// SettleGatedTask transitions one gated task out of `scheduled` to toStatus,
// but ONLY while the row still carries the scheduled_for the gate evaluation
// was dispatched against (NULL-safe compare). The status check alone is not
// enough: an edit or reschedule keeps the task `scheduled`, and since gates
// settle asynchronously (up to their full 300s runtime later), a stale
// verdict conditioned only on status would either run a task an operator had
// just postponed or clobber the operator's new scheduled_for. A task
// cancelled, claimed, edited, or rescheduled while its gate ran fails the
// WHERE and the verdict is discarded — the next due tick re-evaluates the
// task's current definition. Returns the number of rows transitioned (0 or 1).
//
// A TERMINAL toStatus (the end-of-recurrence cancel) also stamps completed_at:
// every other cancel path does, and both retention sweeps (CleanupOldRuns,
// DeleteOldHistory) select on completed_at IS NOT NULL — so a recurrence
// ended at its gate used to leave a cancelled row that was never pruned and
// showed no completion time anywhere.
func (db *Database) SettleGatedTask(ctx context.Context, taskID uuid.UUID, observedScheduledFor *time.Time, toStatus models.TaskStatus) (int, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = $1,
		    completed_at = CASE WHEN $5 THEN COALESCE(completed_at, NOW()) ELSE completed_at END
		WHERE id = $2 AND status = $3 AND scheduled_for IS NOT DISTINCT FROM $4`,
		string(toStatus),
		taskID,
		string(models.TaskStatusScheduled),
		observedScheduledFor,
		toStatus.IsTerminal(),
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// GetUnspawnedRecurringTasks returns terminal recurring occurrences whose
// next-occurrence spawn is still unsettled (#1116): status success/error (the
// only statuses that spawn — cancel and dead-letter deliberately end/park the
// chain), a non-empty recurrence, recurrence_spawned still FALSE, and
// completed_at older than olderThan (a grace window so the sweep never races
// the normal post-commit spawn that is usually milliseconds behind the
// terminal commit — the guarded spawn is idempotent regardless, this just
// avoids pointless contention). Ordered oldest-first and bounded by limit so
// one sweep can never balloon a tick. Backed by idx_tasks_recurrence_unspawned
// (migration 065).
func (db *Database) GetUnspawnedRecurringTasks(ctx context.Context, olderThan time.Time, limit int) ([]*models.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2)
		  AND recurrence IS NOT NULL AND recurrence <> ''
		  AND NOT recurrence_spawned
		  AND completed_at IS NOT NULL
		  AND completed_at < $3
		ORDER BY completed_at ASC
		LIMIT $4`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// RecordSkip records a pre-run-gate skip on a still-scheduled task (#269): it
// re-locks the row, re-checks it is still `scheduled` (a concurrent cancel or
// claim must win and suppress the skip) AND still carries the scheduled_for
// the gate evaluation was dispatched against (a concurrent edit/reschedule
// must win too — see SettleGatedTask), advances scheduled_for to nextRun,
// increments skip_count, and stamps last_skip_at / last_skip_reason. status is
// intentionally left `scheduled` (no promotion to pending). Returns the task
// (updated when recorded, the fresh row as-is otherwise) and whether the skip
// was actually recorded. A nil observedScheduledFor skips the reschedule guard
// (status-only, the pre-async behavior); the scheduler always passes the
// fetched row's value.
func (db *Database) RecordSkip(ctx context.Context, tx *sql.Tx, taskID uuid.UUID, reason string, nextRun time.Time, observedScheduledFor *time.Time) (*models.Task, bool, error) {
	task, err := db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, false, err
	}
	// Only a still-scheduled task can be skipped. A concurrent cancel/claim
	// (status moved off scheduled) wins and the skip is a no-op.
	if task.Status != models.TaskStatusScheduled {
		return task, false, nil
	}
	// A concurrent edit/reschedule moved scheduled_for while the gate ran: the
	// verdict is stale, so leave the operator's row untouched — the next due
	// tick re-evaluates the task's current definition.
	if observedScheduledFor != nil && !sameScheduledFor(task.ScheduledFor, observedScheduledFor) {
		return task, false, nil
	}
	now := time.Now().UTC()
	if !nextRun.IsZero() {
		task.ScheduledFor = &nextRun
	}
	task.SkipCount++
	task.LastSkipAt = &now
	if reason != "" {
		r := reason
		task.LastSkipReason = &r
	}
	if err := db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, false, err
	}
	return task, true, nil
}

// sameScheduledFor reports whether a re-fetched row's scheduled_for still
// equals the one a gate evaluation was dispatched against. Compared at
// microsecond precision: timestamptz resolves to microseconds and the pgx
// encoders truncate a finer time.Time on write, so truncating both sides lets
// an in-memory value that never round-tripped through the DB match its stored
// twin.
func sameScheduledFor(current, observed *time.Time) bool {
	if current == nil || observed == nil {
		return current == nil && observed == nil
	}
	return current.Truncate(time.Microsecond).Equal(observed.Truncate(time.Microsecond))
}
