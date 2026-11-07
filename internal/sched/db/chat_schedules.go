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
// history. Every fire holds a lease (lease_owner = a per-claim token,
// lease_expires_at) from the claim to the settle, the same model the tasks
// table uses: a claim that dies mid-fire expires and the row is claimed again,
// and the chat side's idempotency key turns the repeated delivery into a
// no-op, so a crash never fires a slot twice and never loses one.

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

const chatScheduleFireColumns = `id, conversation_id, user_email, title, prompt, cron_expr,
	timezone, duration_seconds, run_count, next_run_at, lease_owner`

func scanChatScheduleFire(row interface{ Scan(...any) error }, trigger models.ChatScheduleTrigger, claimedAt time.Time) (models.ChatScheduleFire, error) {
	var (
		f        models.ChatScheduleFire
		runCount int
	)
	if err := row.Scan(&f.ScheduleID, &f.ConversationID, &f.UserEmail, &f.Title, &f.Prompt,
		&f.CronExpr, &f.Timezone, &f.DurationSeconds, &runCount, &f.DueAt, &f.LeaseToken); err != nil {
		return models.ChatScheduleFire{}, err
	}
	f.RunNumber = runCount + 1
	f.Trigger = trigger
	f.ClaimedAt = claimedAt
	return f, nil
}

func leaseSeconds(lease time.Duration) float64 {
	if lease < time.Second {
		lease = time.Second
	}
	return lease.Seconds()
}

// ClaimDueChatSchedules leases up to limit active schedules whose next_run_at
// is at or before now, soonest first. A row whose previous lease expired is
// due again (that fire died before it settled). SKIP LOCKED so a concurrent
// claim, edit or settle is passed over rather than waited on.
func (db *Database) ClaimDueChatSchedules(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]models.ChatScheduleFire, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := db.conn.QueryContext(ctx, `
		WITH due AS (
			SELECT id FROM chat_schedules
			 WHERE status = 'active' AND next_run_at <= $1
			   AND (lease_owner IS NULL OR lease_expires_at < now())
			 ORDER BY next_run_at ASC, id ASC
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE chat_schedules cs
		   SET lease_owner = gen_random_uuid()::text,
		       lease_expires_at = now() + make_interval(secs => $3)
		  FROM due
		 WHERE cs.id = due.id
		RETURNING cs.id, cs.conversation_id, cs.user_email, cs.title, cs.prompt, cs.cron_expr,
		          cs.timezone, cs.duration_seconds, cs.run_count, cs.next_run_at, cs.lease_owner`,
		now.Unix(), limit, leaseSeconds(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ChatScheduleFire
	for rows.Next() {
		f, err := scanChatScheduleFire(rows, models.ChatScheduleTriggerSchedule, now)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING order is unspecified; fire soonest-due first.
	sortFires(out)
	return out, nil
}

func sortFires(fs []models.ChatScheduleFire) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].DueAt != fs[j].DueAt {
			return fs[i].DueAt < fs[j].DueAt
		}
		return fs[i].ScheduleID < fs[j].ScheduleID
	})
}

// ClaimChatScheduleNow leases one active or paused schedule for a manual
// ("Run now") fire, whether or not it is due.
func (db *Database) ClaimChatScheduleNow(ctx context.Context, scheduleID string, now time.Time, lease time.Duration) (models.ChatScheduleFire, error) {
	f, err := scanChatScheduleFire(db.conn.QueryRowContext(ctx, `
		UPDATE chat_schedules
		   SET lease_owner = gen_random_uuid()::text,
		       lease_expires_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status IN ('active', 'paused')
		   AND (lease_owner IS NULL OR lease_expires_at < now())
		RETURNING `+chatScheduleFireColumns,
		scheduleID, leaseSeconds(lease)), models.ChatScheduleTriggerManual, now)
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

// SettleChatScheduleFire records what the chat server did with a fire and
// releases its lease, in one transaction, but only while the row still
// carries the fire's lease token: a fire whose lease expired and was claimed
// again leaves the row to the newer claim. Returns false when the lease was
// lost.
//
//   - queued: run_count and last_run_at advance; a recurring patrol moves to
//     its next slot after now (or completes at max_runs), a timer completes.
//     A schedule paused or cancelled while the fire was in flight keeps that
//     status.
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
			       updated_at = $3, lease_owner = NULL, lease_expires_at = NULL
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
			   SET next_run_at = $2, updated_at = $3, lease_owner = NULL, lease_expires_at = NULL
			 WHERE id = $1`,
			fire.ScheduleID, nextRunAt, nowUnix)
	case models.ChatScheduleGone:
		if live {
			status = models.ChatScheduleStatusCancelled
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE chat_schedules
			   SET status = $2, updated_at = $3, lease_owner = NULL, lease_expires_at = NULL
			 WHERE id = $1`,
			fire.ScheduleID, status, nowUnix)
	default: // coalesced, stale, deferred
		recordRun = d.Outcome == models.ChatScheduleCoalesced
		_, err = tx.ExecContext(ctx, `
			UPDATE chat_schedules SET lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`,
			fire.ScheduleID)
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
