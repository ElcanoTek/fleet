package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskActiveStatuses are the statuses that HOLD a serialization key (#709): the
// task is (about to be) executing, so a same-key pending task must not start.
// Mirrors GetRunningTasks' definition of "in flight". paused_awaiting_input is
// deliberately NOT active: a paused run has stopped executing (its lease is
// released), and a resume re-queues the task as pending, which re-passes this
// gate before it can run again.
//
// Derived from the lifecycle table's ActiveTaskStatuses (#1127) — the same
// {leased, running} it always was, now defined once. If the set ever grows,
// the hard-coded $n placeholders in serializationNotBlockedSQL and the two
// IN-list callers below must grow with it; the lifecycle drift test pins the
// placeholder count to the set so that edit cannot be forgotten.
func taskActiveStatuses() []any {
	out := make([]any, len(models.ActiveTaskStatuses))
	for i, s := range models.ActiveTaskStatuses {
		out[i] = string(s)
	}
	return out
}

// serializationNotBlockedSQL filters out pending tasks whose serialization key
// is currently held by an active task, so a blocked task does not consume the
// claim's single candidate slot (the claim query is LIMIT 1 — without this, a
// blocked task at the head of the queue would starve every task behind it).
// This filter is best-effort VISIBILITY only; the correctness guarantee is the
// advisory-lock re-check in ClaimNextPendingTask, which runs under the per-key
// lock at claim time. $2–$3 are the active statuses (taskActiveStatuses).
const serializationNotBlockedSQL = `(
			tasks.serialization_key IS NULL
			OR NOT EXISTS (
				SELECT 1 FROM tasks blocked
				WHERE blocked.serialization_key = tasks.serialization_key
				AND blocked.id <> tasks.id
				AND blocked.status IN ($2, $3)
			)
		)`

// acquireSerializationLockTx takes a transaction-scoped advisory lock on the
// given serialization key (#709). It serializes concurrent same-key claim
// attempts DB-wide: two transactions claiming tasks with the same key execute
// their active-task existence check strictly one after the other, so both
// cannot pass it simultaneously. Released automatically at commit/rollback
// (pg_advisory_xact_lock), so callers never unlock explicitly. hashtext
// collisions across distinct keys are possible and harmless: they only make
// two unrelated claims briefly serialize, never interleave.
func acquireSerializationLockTx(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key)
	return err
}

// hasActiveTaskWithSerializationKeyTx reports whether any task other than
// excludeTaskID holds the given serialization key in an active state. Must be
// called within a transaction, after acquireSerializationLockTx, for the
// answer to be race-free.
func hasActiveTaskWithSerializationKeyTx(ctx context.Context, tx *sql.Tx, key string, excludeTaskID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM tasks
			WHERE serialization_key = $1
			AND id <> $2
			AND status IN ($3, $4)
		)`,
		append([]any{key, excludeTaskID}, taskActiveStatuses()...)...,
	).Scan(&exists)
	return exists, err
}

// ClaimNextPendingTask atomically claims the next pending task for the given
// lease owner using FOR UPDATE SKIP LOCKED, so two concurrent workers never
// claim the same row and a row another worker holds is skipped rather than
// blocked on. It leases the task (status=leased, lease_owner=owner,
// lease_expires_at=now+leaseDuration) inside one transaction and returns the
// claimed task, or (nil, nil) when no pending task is available.
//
// This is the in-process worker's claim path. It replaces moc's
// node-targeted AssignTaskToNode for the runner: there is one synthetic
// in-box lease owner, no node routing, no glob matching.
//
// Serialization gate (#709, moc#442 parity): at most one task per
// serialization_key may be active (leased/running) at a time. The
// candidate SELECT filters out visibly-blocked tasks (best-effort, so a
// blocked head-of-queue task never starves the tasks behind it), and a
// candidate that DOES carry a key is re-checked under a transaction-scoped
// per-key advisory lock before the lease is written — that locked re-check is
// the correctness guarantee against two same-key claims racing each other. A
// blocked candidate is declined (nil, nil), stays pending untouched, and is
// retried on a later claim pass — skipped, never failed. This claim is the
// ONLY pending→active transition (requeue/recovery/resume all re-queue to
// pending), so every path to execution re-passes this gate.
func (db *Database) ClaimNextPendingTask(ctx context.Context, leaseOwner string, leaseDuration time.Duration) (*models.Task, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is a no-op after a successful Commit (returns sql.ErrTxDone); on
	// the error paths the function already returns the underlying error, and a
	// rollback failure in a defer can't be surfaced — so the result is
	// intentionally ignored.
	defer func() { _ = tx.Rollback() }()

	// SKIP LOCKED: skip rows a concurrent claim already locked rather than
	// blocking, so two workers polling at once each get a distinct task.
	row := tx.QueryRowContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		AND `+serializationNotBlockedSQL+`
		ORDER BY effective_priority ASC, created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
		append([]any{string(models.TaskStatusPending)}, taskActiveStatuses()...)...)
	task, err := db.scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Locked re-check for serialized tasks: the visibility filter above ran
	// without the per-key lock, so two same-key candidates claimed by two
	// concurrent transactions could both have passed it. Under the advisory
	// lock the existence check is race-free: the loser blocks until the winner
	// commits its lease, then sees the now-active row and declines.
	if task.SerializationKey != nil {
		if err := acquireSerializationLockTx(ctx, tx, *task.SerializationKey); err != nil {
			return nil, err
		}
		blocked, err := hasActiveTaskWithSerializationKeyTx(ctx, tx, *task.SerializationKey, task.ID)
		if err != nil {
			return nil, err
		}
		if blocked {
			// Another same-key task is active: decline the claim. The rollback
			// releases the row lock and the task stays pending for a later pass.
			return nil, nil
		}
	}

	now := time.Now().UTC()
	expiresAt := now.Add(leaseDuration)
	task.Status = models.TaskStatusLeased
	task.LeaseOwner = &leaseOwner
	task.LeaseExpiresAt = &expiresAt
	// StartedAt is deliberately NOT set here; it is set on the first running update.

	if err := db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// PromoteStarvedTasks is the anti-starvation sweep (#230): any pending task that
// has waited longer than windowMinutes and is still LESS urgent than the
// starvation floor has its effective_priority raised TO that floor (never its
// submitted priority), so a sustained stream of higher-priority work can't keep
// it queued forever. The floor is High, never Critical, so relief for a starving
// batch task can't preempt genuinely critical work. windowMinutes <= 0 disables
// the sweep (no-op). Returns the number of tasks promoted.
func (db *Database) PromoteStarvedTasks(ctx context.Context, windowMinutes int) (int64, error) {
	if windowMinutes <= 0 {
		return 0, nil
	}
	// Compute the age cutoff in Go and compare against the TIMESTAMPTZ column
	// directly — avoids any driver-specific interval-parameter typing.
	cutoff := time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute)
	// Measure the wait from when the task became eligible to run, not from
	// created_at. A recurring occurrence ROW is created at the PREVIOUS
	// occurrence's completion, so by the time it flips to pending its created_at
	// is already ~one period old — keying the sweep on created_at would
	// floor-promote every recurring/retried task the instant it becomes pending,
	// inverting the priority queue exactly when the operator enables the window.
	// GREATEST(created_at, scheduled_for) is that eligibility time: a fresh
	// recurrence's scheduled_for is its (near-now) fire time, a retry's is the
	// backoff time, and a resume bumps scheduled_for to now() — so none are
	// mis-promoted. created_at is never NULL, so Postgres GREATEST simply ignores
	// a NULL scheduled_for (immediate tasks) and falls back to created_at.
	// (A crash-recovered task keeps its old scheduled_for and so is promoted
	// immediately — intended: recovered work should not lose more ground to
	// freshly-queued bulk work.)
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks
		SET effective_priority = $1
		WHERE status = $2
		  AND priority > $1
		  AND effective_priority > $1
		  AND GREATEST(created_at, scheduled_for) < $3`,
		models.StarvationFloorPriority, string(models.TaskStatusPending), cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SyncEffectivePriorityTx brings effective_priority back in line with a task
// whose submitted priority was just EDITED (UpdateEditableTask /
// ReplaceTaskDefinition), inside the caller's transaction. effective_priority
// is deliberately excluded from UpdateTaskTx (#230: only the anti-starvation
// sweep may lower it after INSERT), which had the side effect that changing a
// pending task's priority via PUT /tasks/{id} changed nothing about when it
// ran — the claim path orders by effective_priority alone, and the response
// even kept echoing the old value.
//
// Rule: if the sweep had already promoted the row (effective more urgent than
// the OLD submitted priority), keep whichever of that promotion and the new
// priority is more urgent — an operator demoting a task that has waited past
// the starvation window should not send it back to the end of the line, and
// the sweep would re-promote it on its next pass anyway. Otherwise the new
// priority is the effective one. A no-op when the priority did not change.
func (db *Database) SyncEffectivePriorityTx(ctx context.Context, tx *sql.Tx, task *models.Task, priorPriority int) error {
	if task.Priority == priorPriority {
		return nil
	}
	row := tx.QueryRowContext(ctx, `
		UPDATE tasks
		SET effective_priority = CASE
			WHEN effective_priority < $2 THEN LEAST(effective_priority, $3)
			ELSE $3
		END
		WHERE id = $1
		RETURNING effective_priority`,
		task.ID, priorPriority, task.Priority)
	if err := row.Scan(&task.EffectivePriority); err != nil {
		return fmt.Errorf("sync effective_priority for %s: %w", task.ID, err)
	}
	return nil
}

// RecoverExpiredLeases resets tasks with expired leases back to pending. This is
// the crash-safe backstop: a worker that died mid-task (systemd restart) lets
// its lease expire, and recovery re-queues the task for the next claim.
//
// Recovery is attempt-bounded (#1116): a task whose attempt budget is spent is
// routed to the dead-letter queue instead of re-queued. Without the bound, a
// task that kills the process itself (reliably OOMs the binary, or crashes at
// every restart) cycled recover→claim→crash forever — the only max-retries
// check was the in-process failure path, which a crash never reaches. The
// predicate is attempt_count >= max_retries, EXACT parity with the in-process
// retry gate (runner: AttemptCount < MaxRetries requeues): max_retries=R
// allows at most R+1 total executions, and R=0 ("never retry") means exactly
// one — the crashed attempt was already the last allowed one, so recovery
// quarantines rather than granting a free extra run of the task's external
// side effects. (The issue text sketched a strict `>`; parity won in review.)
//
// The dead-letter branch mirrors DeadLetterTaskWithContext's column writes
// (status/completed_at/dead_lettered_at/dead_letter_reason/dead_letter_attempts/
// error_message, output_json nulled, lease cleared, actual_duration_seconds
// derived from started_at like maybeComputeActualDuration — NULL for a leased
// row that never started) so a recovery-quarantined row reads identically in
// the DLQ listing and is replayable the same way. It runs FIRST so a row never
// both dead-letters and re-queues; the two predicates are disjoint on
// attempt_count either way.
//
// Returns (requeued, deadLettered): the rows reset to pending, and the rows
// quarantined. The storage wrapper owns the telemetry for the latter.
func (db *Database) RecoverExpiredLeases(ctx context.Context, now time.Time) (int, int, error) {
	quarantined, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET
			status = $1,
			completed_at = $4,
			dead_lettered_at = $4,
			dead_letter_reason = $5,
			dead_letter_attempts = attempt_count + 1,
			error_message = $5,
			output_json = NULL,
			actual_duration_seconds = COALESCE(actual_duration_seconds, CASE
				WHEN started_at IS NOT NULL
				THEN GREATEST(0, EXTRACT(EPOCH FROM ($4::timestamptz - started_at)))::int
			END),
			lease_owner = NULL,
			lease_expires_at = NULL
		WHERE status IN ($2, $3)
		AND (lease_expires_at < $4 OR lease_expires_at IS NULL)
		AND attempt_count >= max_retries`,
		string(models.TaskStatusDeadLettered),
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
		now,
		"crash-loop guard: the worker's lease expired past the retry budget (the process likely crashed or stalled mid-run on every attempt)",
	)
	if err != nil {
		return 0, 0, err
	}
	deadLettered, _ := quarantined.RowsAffected()

	result, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET
			status = $1,
			lease_owner = NULL,
			lease_expires_at = NULL,
			started_at = NULL,
			output_json = NULL,
			artifacts = NULL,
			attempt_count = attempt_count + 1
		WHERE status IN ($2, $3)
		AND (lease_expires_at < $4 OR lease_expires_at IS NULL)
		AND attempt_count < max_retries`,
		string(models.TaskStatusPending),
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
		now,
	)
	if err != nil {
		return 0, int(deadLettered), err
	}
	affected, _ := result.RowsAffected()
	return int(affected), int(deadLettered), nil
}
