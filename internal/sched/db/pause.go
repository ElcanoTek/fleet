package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ExpirePausedTasks fails tasks that have sat in paused_awaiting_input past the
// window (#510) — an unattended ask-pause otherwise waits forever. A non-positive
// window is a no-op (the default — waits forever). It returns the rows it moved
// to the terminal state so the caller can spawn the next occurrence for recurring
// tasks (see Storage.ExpirePausedTasks).
//
// It moves them to the TERMINAL `error` status (not dead_lettered, which is the
// runner's lease-guarded status by convention — a paused task holds no lease),
// stamping completed_at + error_message and clearing the pending question so
// the row reads as a clean terminal failure. Age is measured from paused_at
// (#1116) — the instant PauseTaskForQuestion parked the task. It used to be
// measured from started_at, "acceptably conservative" for short runs, but a
// run that executed 2h before asking under a 60-minute window was expired on
// the next tick: a zero TTL. Migration 064 backfills paused_at from started_at
// for rows already paused at upgrade time; a paused row with NULL paused_at
// (no in-repo writer produces one) is deliberately never expired — failing
// open to "waits forever", the sweep's own disabled-window default.
func (db *Database) ExpirePausedTasks(ctx context.Context, windowMinutes int) ([]*models.Task, error) {
	if windowMinutes <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute)
	// UPDATE ... RETURNING makes the terminal transition AND the capture of which
	// rows transitioned atomic under one lock acquisition: a concurrent ResumeTask
	// (guarded by `status='paused_awaiting_input'`) either commits first (this
	// WHERE then excludes the row) or blocks and wakes to find status='error' —
	// so a row is never both resumed and expired, and each expired row is returned
	// exactly once. The caller spawns the next recurrence for returned recurring
	// rows (see Storage.ExpirePausedTasks); without that, an expired occurrence of
	// a recurring task would silently end the whole schedule.
	rows, err := db.conn.QueryContext(ctx, `
		UPDATE tasks
		SET status = $1, completed_at = now(), error_message = $2, pending_question = NULL
		WHERE status = $3
		  AND paused_at IS NOT NULL
		  AND paused_at < $4
		RETURNING `+taskColumns,
		string(models.TaskStatusError),
		fmt.Sprintf("expired: awaited input for more than %d minute(s) with no answer", windowMinutes),
		string(models.TaskStatusPausedAwaitingInput), cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var expired []*models.Task
	for rows.Next() {
		t, serr := db.scanTask(rows)
		if serr != nil {
			return nil, serr
		}
		expired = append(expired, t)
	}
	return expired, rows.Err()
}

// PauseTaskForQuestion parks a RUNNING task in paused_awaiting_input with the
// agent's question (#510), clearing the lease so the paused task holds no
// sandbox/container. Guarded on the caller's lease so a recovered run can't
// pause a task it no longer owns. Returns whether it applied. pending_answer
// is nulled alongside the new question: since #582 the runner clears the Q&A
// columns only at a terminal transition, so a resumed run that pauses AGAIN
// would otherwise leave the prior answer dangling next to the new question.
// paused_at stamps the pause instant (#1116) — ExpirePausedTasks counts the
// ask window from it, so a long run's question gets the full TTL instead of
// one already eroded by execution time.
func (db *Database) PauseTaskForQuestion(ctx context.Context, taskID, leaseOwner uuid.UUID, question string) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'paused_awaiting_input', pending_question = $1, pending_answer = NULL,
			paused_at = now(),
			lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $2 AND lease_owner = $3 AND status = 'running'`,
		question, taskID, leaseOwner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ResumeTask answers a paused task's question and re-queues it (#510): status →
// pending, pending_answer set, scheduled_for = now so it is immediately
// claimable. Guarded on the paused status. Returns whether it applied.
func (db *Database) ResumeTask(ctx context.Context, taskID uuid.UUID, answer string) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending', pending_answer = $1, scheduled_for = now()
		WHERE id = $2 AND status = 'paused_awaiting_input'`,
		answer, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearPendingQA clears a resumed task's question+answer once the run has
// consumed them, under the run's lease so a stale writer can't wipe a fresh
// pause. Best-effort (the run proceeds regardless).
func (db *Database) ClearPendingQA(ctx context.Context, taskID, leaseOwner uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET pending_question = NULL, pending_answer = NULL
		WHERE id = $1 AND lease_owner = $2`, taskID, leaseOwner)
	return err
}

// ListPausedTasks returns tasks awaiting a human answer (#510), newest first.
func (db *Database) ListPausedTasks(ctx context.Context, limit int) ([]*models.Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE status = 'paused_awaiting_input' ORDER BY started_at DESC NULLS LAST, created_at DESC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.Task
	for rows.Next() {
		t, err := db.scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
