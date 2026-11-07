package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Chat schedules (#2049 M5, ADR-0104): the scheduler's half. The chat server
// edits definitions in chat_schedules and delivers each fire as a turn; this
// file is the engine's claim → settle cycle over those rows and their run
// history.
//
// Every fire holds a lease (lease_owner = a per-claim token,
// lease_expires_at) from the claim to the settle, the same model the tasks
// table uses, and names the fire it is for (fire_id, fire_trigger,
// fire_run_number). A claim that dies mid-fire leaves the lease to expire;
// the recovery pass then re-claims the SAME fire — whatever the schedule's
// status or due time — under a new token, and the chat side's idempotency key
// (built from fire_id) turns the repeated delivery into a no-op. So a crash
// never fires a slot twice and never loses one, and a retried Run now never
// mints a second turn for the same run.

// Errors from ClaimChatScheduleNow (defined in models so the chat HTTP layer
// can map them without importing this package).
var (
	ErrChatScheduleNotFound    = models.ErrChatScheduleNotFound
	ErrChatScheduleNotRunnable = models.ErrChatScheduleNotRunnable
	ErrChatScheduleFiring      = models.ErrChatScheduleFiring
)

// chatScheduleBusyRetry is how far a busy one-shot timer moves when its chat
// cannot take the turn yet (a recurring patrol moves to its next slot).
const chatScheduleBusyRetry = 30 * time.Second

// Backoff for a fire whose delivery failed: the fire keeps its lease, pushed
// out to now + min(base << (failures-1), max), and is re-driven by the
// recovery pass when that expires.
const (
	chatScheduleFailureBackoffBase = 30 * time.Second
	chatScheduleFailureBackoffMax  = 30 * time.Minute
)

// ChatScheduleFailureBackoff is the lease extension after the n-th
// consecutive failed delivery of a schedule's fire (n >= 1).
func ChatScheduleFailureBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := chatScheduleFailureBackoffBase
	for i := 1; i < n && d < chatScheduleFailureBackoffMax; i++ {
		d *= 2
	}
	if d > chatScheduleFailureBackoffMax {
		d = chatScheduleFailureBackoffMax
	}
	return d
}

const chatScheduleFireReturning = `cs.id, cs.conversation_id, cs.user_email, cs.title, cs.prompt, cs.cron_expr,
	cs.timezone, cs.duration_seconds, cs.next_run_at, cs.lease_owner, cs.fire_id,
	cs.fire_trigger, cs.fire_run_number`

func scanChatScheduleFire(row interface{ Scan(...any) error }, claimedAt time.Time) (models.ChatScheduleFire, error) {
	var (
		f       models.ChatScheduleFire
		trigger string
	)
	if err := row.Scan(&f.ScheduleID, &f.ConversationID, &f.UserEmail, &f.Title, &f.Prompt,
		&f.CronExpr, &f.Timezone, &f.DurationSeconds, &f.DueAt, &f.LeaseToken, &f.FireID,
		&trigger, &f.RunNumber); err != nil {
		return models.ChatScheduleFire{}, err
	}
	f.Trigger = models.ChatScheduleTrigger(trigger)
	f.ClaimedAt = claimedAt
	return f, nil
}

func scanChatScheduleFires(rows *sql.Rows, claimedAt time.Time) ([]models.ChatScheduleFire, error) {
	defer rows.Close()
	var out []models.ChatScheduleFire
	for rows.Next() {
		f, err := scanChatScheduleFire(rows, claimedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING order is unspecified; fire soonest-due first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].DueAt != out[j].DueAt {
			return out[i].DueAt < out[j].DueAt
		}
		return out[i].ScheduleID < out[j].ScheduleID
	})
	return out, nil
}

func leaseSeconds(lease time.Duration) float64 {
	if lease < time.Second {
		lease = time.Second
	}
	return lease.Seconds()
}

// ClaimDueChatSchedules starts a fire for up to limit active, unleased
// schedules whose next_run_at is at or before now, soonest first, skipping
// the ids in exclude (rows this pass already attempted). SKIP LOCKED so a
// concurrent claim, edit or settle is passed over rather than waited on. A
// leased row is never claimed here, even with its lease expired: that is a
// fire in flight, and RecoverChatScheduleFires re-drives it as itself.
func (db *Database) ClaimDueChatSchedules(ctx context.Context, now time.Time, lease time.Duration, limit int, exclude []string) ([]models.ChatScheduleFire, error) {
	if limit <= 0 {
		limit = 25
	}
	if exclude == nil {
		exclude = []string{}
	}
	rows, err := db.conn.QueryContext(ctx, `
		WITH due AS (
			SELECT id FROM chat_schedules
			 WHERE status = 'active' AND next_run_at <= $1
			   AND lease_owner IS NULL
			   AND NOT (id = ANY($4::text[]))
			 ORDER BY next_run_at ASC, id ASC
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE chat_schedules cs
		   SET lease_owner = gen_random_uuid()::text,
		       lease_expires_at = now() + make_interval(secs => $3),
		       fire_id = gen_random_uuid()::text,
		       fire_trigger = 'schedule',
		       fire_run_number = cs.run_count + 1
		  FROM due
		 WHERE cs.id = due.id
		RETURNING `+chatScheduleFireReturning,
		now.Unix(), limit, leaseSeconds(lease), exclude)
	if err != nil {
		return nil, err
	}
	return scanChatScheduleFires(rows, now)
}

// RecoverChatScheduleFires re-claims up to limit fires whose lease expired —
// the claimer died before settling, or a failed delivery's backoff ran out —
// under a new token, keeping each fire's id, trigger and run number, whatever
// the schedule's status or due time. The chat side then judges the fire
// against the row as it is now (a paused schedule's scheduled fire is stale;
// a delivered one is found by its key).
func (db *Database) RecoverChatScheduleFires(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]models.ChatScheduleFire, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := db.conn.QueryContext(ctx, `
		WITH stuck AS (
			SELECT id FROM chat_schedules
			 WHERE lease_owner IS NOT NULL AND lease_expires_at < now()
			 ORDER BY lease_expires_at ASC, id ASC
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE chat_schedules cs
		   SET lease_owner = gen_random_uuid()::text,
		       lease_expires_at = now() + make_interval(secs => $2),
		       fire_id = COALESCE(cs.fire_id, gen_random_uuid()::text),
		       fire_trigger = COALESCE(cs.fire_trigger, 'schedule'),
		       fire_run_number = COALESCE(cs.fire_run_number, cs.run_count + 1)
		  FROM stuck
		 WHERE cs.id = stuck.id
		RETURNING `+chatScheduleFireReturning,
		limit, leaseSeconds(lease))
	if err != nil {
		return nil, err
	}
	return scanChatScheduleFires(rows, now)
}

// ClaimChatScheduleNow starts a manual ("Run now") fire of one active or
// paused schedule, whether or not it is due. A schedule with a fire in flight
// (leased, even past its expiry — the recovery pass owns that one) answers
// ErrChatScheduleFiring.
func (db *Database) ClaimChatScheduleNow(ctx context.Context, scheduleID string, now time.Time, lease time.Duration) (models.ChatScheduleFire, error) {
	f, err := scanChatScheduleFire(db.conn.QueryRowContext(ctx, `
		UPDATE chat_schedules cs
		   SET lease_owner = gen_random_uuid()::text,
		       lease_expires_at = now() + make_interval(secs => $2),
		       fire_id = gen_random_uuid()::text,
		       fire_trigger = 'manual',
		       fire_run_number = cs.run_count + 1
		 WHERE cs.id = $1 AND cs.status IN ('active', 'paused') AND cs.lease_owner IS NULL
		RETURNING `+chatScheduleFireReturning,
		scheduleID, leaseSeconds(lease)), now)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return models.ChatScheduleFire{}, err
	}
	var status string
	err = db.conn.QueryRowContext(ctx, `SELECT status FROM chat_schedules WHERE id = $1`, scheduleID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return models.ChatScheduleFire{}, ErrChatScheduleNotFound
	case err != nil:
		return models.ChatScheduleFire{}, err
	case status != models.ChatScheduleStatusActive && status != models.ChatScheduleStatusPaused:
		return models.ChatScheduleFire{}, fmt.Errorf("%w (it is %s)", ErrChatScheduleNotRunnable, status)
	default:
		return models.ChatScheduleFire{}, ErrChatScheduleFiring
	}
}

// releaseFireSQL ends a fire: no lease, no fire in flight, failures reset.
const releaseFireSQL = `lease_owner = NULL, lease_expires_at = NULL, fire_id = NULL,
	fire_trigger = NULL, fire_run_number = NULL, fire_failures = 0`

// FailChatScheduleFire records a delivery that errored. The fire stays in
// flight (its id, so its idempotency key, is kept — the delivery may have
// committed before the error) and its lease is pushed out by
// ChatScheduleFailureBackoff, so the recovery pass retries it then instead of
// every pass claiming it again. Returns false when the lease was lost.
func (db *Database) FailChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire) (bool, time.Duration, error) {
	var failures int
	err := db.conn.QueryRowContext(ctx, `
		UPDATE chat_schedules SET fire_failures = fire_failures + 1
		 WHERE id = $1 AND lease_owner = $2
		RETURNING fire_failures`, fire.ScheduleID, fire.LeaseToken).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	backoff := ChatScheduleFailureBackoff(failures)
	if _, err := db.conn.ExecContext(ctx, `
		UPDATE chat_schedules SET lease_expires_at = now() + make_interval(secs => $3)
		 WHERE id = $1 AND lease_owner = $2`,
		fire.ScheduleID, fire.LeaseToken, backoff.Seconds()); err != nil {
		return false, 0, err
	}
	return true, backoff, nil
}

// SettleChatScheduleFire records what the chat server did with a fire and
// ends it, in one transaction, but only while the row still carries the
// fire's lease token: a fire re-claimed after its lease expired, or revoked
// by a pause or cancel, leaves the row alone. Returns false when the lease
// was lost.
//
//   - queued: run_count and last_run_at advance; a recurring patrol moves to
//     its next slot after now (or completes at max_runs), a timer completes.
//     A schedule cancelled meanwhile keeps that status.
//   - busy: the slot is skipped — a patrol moves to its next slot, a timer
//     retries in chatScheduleBusyRetry — so a blocked schedule never sits at
//     the head of the due scan.
//   - gone: the schedule is cancelled.
//   - coalesced, stale, deferred: the definition is left as it is.
//
// queued, coalesced, busy and gone each add a row to the run history.
func (db *Database) SettleChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire, d models.ChatScheduleDelivery, now time.Time) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		status, cronExpr, tz string
		maxRuns, runCount    int
		nextRunAt            int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT status, cron_expr, timezone, max_runs, run_count, next_run_at
		  FROM chat_schedules
		 WHERE id = $1 AND lease_owner = $2
		 FOR UPDATE`,
		fire.ScheduleID, fire.LeaseToken,
	).Scan(&status, &cronExpr, &tz, &maxRuns, &runCount, &nextRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	nowUnix := now.Unix()
	live := status == models.ChatScheduleStatusActive || status == models.ChatScheduleStatusPaused
	recordRun := true
	runNumber := fire.RunNumber
	switch d.Outcome {
	case models.ChatScheduleQueued:
		runCount++
		runNumber = runCount
		if live {
			if cronExpr == "" {
				status = models.ChatScheduleStatusCompleted
			} else if maxRuns > 0 && runCount >= maxRuns {
				status = models.ChatScheduleStatusCompleted
			} else if next, nerr := models.NextChatScheduleRun(cronExpr, tz, now); nerr == nil {
				nextRunAt = next.Unix()
			} else {
				status = models.ChatScheduleStatusCancelled
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE chat_schedules
			   SET run_count = $2, last_run_at = $3, next_run_at = $4, status = $5,
			       updated_at = $3, `+releaseFireSQL+`
			 WHERE id = $1`,
			fire.ScheduleID, runCount, nowUnix, nextRunAt, status)
	case models.ChatScheduleBusy:
		if live {
			nextRunAt = nowUnix + int64(chatScheduleBusyRetry/time.Second)
			if cronExpr != "" {
				if next, nerr := models.NextChatScheduleRun(cronExpr, tz, now); nerr == nil {
					nextRunAt = next.Unix()
				}
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE chat_schedules
			   SET next_run_at = $2, updated_at = $3, `+releaseFireSQL+`
			 WHERE id = $1`,
			fire.ScheduleID, nextRunAt, nowUnix)
	case models.ChatScheduleGone:
		if live {
			status = models.ChatScheduleStatusCancelled
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE chat_schedules
			   SET status = $2, updated_at = $3, `+releaseFireSQL+`
			 WHERE id = $1`,
			fire.ScheduleID, status, nowUnix)
	default: // coalesced, stale, deferred
		recordRun = d.Outcome == models.ChatScheduleCoalesced
		_, err = tx.ExecContext(ctx, `UPDATE chat_schedules SET `+releaseFireSQL+` WHERE id = $1`, fire.ScheduleID)
	}
	if err != nil {
		return false, err
	}

	if recordRun {
		var dueAt any
		if fire.Trigger == models.ChatScheduleTriggerSchedule {
			dueAt = fire.DueAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_schedule_runs (schedule_id, run_number, trigger_kind, outcome, input_id, due_at, fired_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT DO NOTHING`,
			fire.ScheduleID, runNumber, string(fire.Trigger), string(d.Outcome), d.InputID, dueAt, now.UTC(),
		); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// NextChatScheduleDueAt is the earliest next_run_at after `after` of any
// active chat schedule (Unix seconds); ok is false when there is none. The
// scheduler arms its wake-up with it so a schedule fires on time instead of
// on the next tick. Only future slots count: a row already due that the last
// pass could not fire (leased by a live fire, deferred) waits for the tick,
// so it can never spin the wake-up.
func (db *Database) NextChatScheduleDueAt(ctx context.Context, after int64) (int64, bool, error) {
	var next sql.NullInt64
	if err := db.conn.QueryRowContext(ctx,
		`SELECT min(next_run_at) FROM chat_schedules WHERE status = 'active' AND next_run_at > $1`, after,
	).Scan(&next); err != nil {
		return 0, false, err
	}
	return next.Int64, next.Valid, nil
}

// ListChatScheduleRuns returns a schedule's run history, newest first.
func (db *Database) ListChatScheduleRuns(ctx context.Context, scheduleID string, limit int) ([]models.ChatScheduleRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT schedule_id, run_number, trigger_kind, outcome, input_id, due_at, fired_at
		  FROM chat_schedule_runs
		 WHERE schedule_id = $1
		 ORDER BY fired_at DESC, id DESC
		 LIMIT $2`, scheduleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ChatScheduleRun
	for rows.Next() {
		var (
			r       models.ChatScheduleRun
			trigger string
			outcome string
			dueAt   sql.NullInt64
		)
		if err := rows.Scan(&r.ScheduleID, &r.RunNumber, &trigger, &outcome, &r.InputID, &dueAt, &r.FiredAt); err != nil {
			return nil, err
		}
		r.Trigger = models.ChatScheduleTrigger(trigger)
		r.Outcome = models.ChatScheduleOutcome(outcome)
		if dueAt.Valid {
			v := dueAt.Int64
			r.DueAt = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CleanupOldChatScheduleRuns prunes chat schedule run history older than
// retentionDays, always keeping each schedule's newest keepPerSchedule rows —
// the same policy CleanupOldRuns applies to task runs, on the same daily
// sweep. retentionDays<=0 disables it. Returns the number of rows deleted.
func (db *Database) CleanupOldChatScheduleRuns(ctx context.Context, retentionDays, keepPerSchedule int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	if keepPerSchedule < 0 {
		keepPerSchedule = 0
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	res, err := db.conn.ExecContext(ctx, `
		DELETE FROM chat_schedule_runs WHERE id IN (
			SELECT id FROM (
				SELECT id, fired_at,
				       ROW_NUMBER() OVER (PARTITION BY schedule_id ORDER BY fired_at DESC, id DESC) AS rn
				  FROM chat_schedule_runs
			) ranked
			WHERE rn > $1 AND fired_at < $2
		)`, keepPerSchedule, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
