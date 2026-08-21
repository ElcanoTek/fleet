package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// StrandedWakeGrace is how far past its wake deadline a parked task must be
// before ExpireStrandedWakeTasks fails it terminally.
//
// This is a BACKSTOP for a broken row, not a policy knob, which is why it is a
// constant. Every in-repo writer of paused_awaiting_wake sets wake_at, and
// WakeDueTasks re-queues a due row on the very next scheduler tick (~30s), so
// under normal operation nothing is ever a day past its deadline. A row that
// is — or a row with wake_at NULL, which WakeDueTasks filters out and therefore
// can NEVER wake — is stranded, and the pre-existing behaviour was to leave it
// parked forever with no terminal record and no operator signal.
//
// A day is far longer than any legitimate lateness while still bounded, and it
// is measured from paused_at (never from wake_at alone) so a task legitimately
// sleeping 30 days out is never touched.
const StrandedWakeGrace = 24 * time.Hour

// ExpireStrandedWakeTasks fails tasks parked in paused_awaiting_wake that no
// wake can ever reach, closing the one gap the #510 expiry sweep left open:
// that sweep covers paused_awaiting_input only, so the OTHER parked state had
// no terminal backstop at all.
//
// Two shapes qualify, both anchored on paused_at so a freshly parked task is
// never a candidate:
//
//  1. wake_at IS NULL. WakeDueTasks requires `wake_at IS NOT NULL`, so such a
//     row is unreachable by the wake sweep by construction — it waits forever.
//     No in-repo writer produces one (PauseTaskForWake always sets wake_at),
//     which is exactly why nothing would ever notice if one appeared.
//  2. wake_at more than `grace` in the past. The wake sweep runs every tick, so
//     a row this far overdue means the sweep is not reaching it.
//
// Like ExpirePausedTasks it moves rows to the terminal `error` status (a parked
// task holds no lease, so dead_lettered — the runner's lease-guarded status —
// would be wrong), and RETURNS them so the caller can preserve the recurrence
// chain. A non-positive grace is a no-op.
func (db *Database) ExpireStrandedWakeTasks(ctx context.Context, grace time.Duration) ([]*models.Task, error) {
	if grace <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-grace)
	// UPDATE ... RETURNING for the same reason ExpirePausedTasks uses it: the
	// transition and the capture of which rows transitioned happen under one
	// lock, so a concurrent WakeDueTasks / WakeTaskByEvent either commits first
	// (this WHERE then excludes the row) or wakes to find status='error'. A row
	// is never both woken and expired.
	rows, err := db.conn.QueryContext(ctx, `
		UPDATE tasks
		SET status = $1, completed_at = now(), error_message = $2,
		    wake_at = NULL, wake_event_key = NULL
		WHERE status = $3
		  AND paused_at IS NOT NULL
		  AND paused_at < $4
		  AND (wake_at IS NULL OR wake_at < $4)
		RETURNING `+taskColumns,
		string(models.TaskStatusError),
		fmt.Sprintf("expired: parked awaiting a wake that can no longer arrive (no wake fired within %s of the deadline)", grace),
		string(models.TaskStatusPausedAwaitingWake), cutoff)
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

// PauseTaskForWake parks a RUNNING task in paused_awaiting_wake (self-wake,
// docs/SELF-WAKE.md), clearing the lease so the parked task holds no
// sandbox/container — the exact shape of PauseTaskForQuestion, keyed on a
// deadline/event instead of a human. wake_at is ALWAYS set (a timer sleep's
// fire time, or an event wait's timeout deadline), so the wake sweep is the
// only expiry mechanism needed. wake_reason is nulled: like pending_answer,
// it belongs to the wake that has not happened yet. wake_cycles increments
// here, under the same guarded write, so the runner's cycle cap can't be
// raced past. Guarded on the caller's lease; returns whether it applied.
// paused_at stamps the park instant (#1116) for one consistent "entered its
// pause" record across both parked states; the wake expiry itself stays
// wake_at-driven (wake_at is ALWAYS set), so paused_at joins no wake predicate.
func (db *Database) PauseTaskForWake(ctx context.Context, taskID, leaseOwner uuid.UUID, wakeAt time.Time, eventKey, note string) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'paused_awaiting_wake',
			wake_at = $1, wake_event_key = NULLIF($2, ''), wake_note = $3, wake_reason = NULL,
			wake_cycles = wake_cycles + 1,
			paused_at = now(),
			lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $4 AND lease_owner = $5 AND status = 'running'`,
		wakeAt.UTC(), eventKey, note, taskID, leaseOwner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// WakeDueTasks re-queues every parked task whose wake deadline has passed:
// status → pending, scheduled_for = now so it is immediately claimable, and
// wake_reason records WHY it woke — a timer sleep's deadline fired, or an
// event wait timed out (the reason names the event so the resumed agent
// knows the event never arrived). Returns how many tasks it woke.
func (db *Database) WakeDueTasks(ctx context.Context) (int, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending', scheduled_for = now(),
			wake_reason = CASE
				WHEN wake_event_key IS NOT NULL AND wake_event_key <> ''
					THEN 'timed out waiting for event "' || wake_event_key || '"'
				ELSE 'the sleep timer fired'
			END
		WHERE status = 'paused_awaiting_wake' AND wake_at IS NOT NULL AND wake_at <= now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// WakeTaskByEvent wakes ONE parked task early because its named event
// arrived (POST /tasks/{id}/wake). Guarded on the paused status AND the
// exact event key, so a wake with the wrong key (or against a timer-only
// sleep) is a no-op reported to the caller. note, when non-empty, is carried
// into the wake reason so the resumed agent sees the event payload's gist.
func (db *Database) WakeTaskByEvent(ctx context.Context, taskID uuid.UUID, eventKey, note string) (bool, error) {
	reason := `event "` + eventKey + `" fired`
	if note != "" {
		reason += ": " + note
	}
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending', scheduled_for = now(), wake_reason = $1
		WHERE id = $2 AND status = 'paused_awaiting_wake' AND wake_event_key = $3`,
		reason, taskID, eventKey)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearWakeState clears a woken task's wake columns once the resumed run has
// consumed them, under the run's lease — the wake counterpart of
// ClearPendingQA (wake_cycles deliberately survives: it is the lifetime
// park counter the cycle cap checks). Best-effort.
func (db *Database) ClearWakeState(ctx context.Context, taskID, leaseOwner uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET wake_at = NULL, wake_event_key = NULL, wake_note = NULL, wake_reason = NULL
		WHERE id = $1 AND lease_owner = $2`, taskID, leaseOwner)
	return err
}
