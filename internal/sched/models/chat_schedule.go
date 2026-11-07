package models

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/cron"
)

// Chat schedules (#2049 M5, ADR-0104) are in-chat patrols (recurring cron) and
// timers (one-shot) that the scheduler fires into a conversation. The rows
// live in the sched schema (chat_schedules); the chat server edits their
// definitions and delivers each fire as a turn, and the scheduler's one tick
// loop claims, leases, settles and records every fire.

// Chat schedule status values (the chat_schedules CHECK constraint).
const (
	ChatScheduleStatusActive    = "active"
	ChatScheduleStatusPaused    = "paused"
	ChatScheduleStatusCompleted = "completed"
	ChatScheduleStatusCancelled = "cancelled"
)

// Errors from a Run now claim (the scheduler's ClaimChatScheduleNow).
var (
	// ErrChatScheduleNotFound: no schedule with that id.
	ErrChatScheduleNotFound = errors.New("chat schedule not found")
	// ErrChatScheduleNotRunnable: the schedule is completed or cancelled.
	ErrChatScheduleNotRunnable = errors.New("chat schedule is not active or paused")
	// ErrChatScheduleFiring: another fire holds the schedule's lease.
	ErrChatScheduleFiring = errors.New("chat schedule is firing; try again in a moment")
)

// ChatScheduleTrigger says what fired a run.
type ChatScheduleTrigger string

const (
	// ChatScheduleTriggerSchedule is a fire of a due schedule by the tick.
	ChatScheduleTriggerSchedule ChatScheduleTrigger = "schedule"
	// ChatScheduleTriggerManual is a "Run now" from the chat UI.
	ChatScheduleTriggerManual ChatScheduleTrigger = "manual"
)

// ChatScheduleOutcome is what the chat server did with a fire.
type ChatScheduleOutcome string

const (
	// ChatScheduleQueued: a turn was enqueued (Delivery.InputID). The run counts.
	ChatScheduleQueued ChatScheduleOutcome = "queued"
	// ChatScheduleCoalesced: a manual fire found a patrol turn already queued
	// or running for the schedule's owner (Delivery.InputID names it). The run
	// does not count and is not recorded; Run now answers it as
	// ErrChatScheduleFiring.
	ChatScheduleCoalesced ChatScheduleOutcome = "coalesced"
	// ChatScheduleBusy: a scheduled fire found a patrol in flight, an approval
	// waiting on the owner or a full input queue. The slot is skipped.
	ChatScheduleBusy ChatScheduleOutcome = "busy"
	// ChatScheduleGone: the conversation was deleted or the owner can no
	// longer use it. The schedule is cancelled.
	ChatScheduleGone ChatScheduleOutcome = "gone"
	// ChatScheduleStale: the fire no longer matches the row (it was paused,
	// cancelled or re-claimed after this claim). Nothing is recorded.
	ChatScheduleStale ChatScheduleOutcome = "stale"
	// ChatScheduleDeferred: the chat server is not taking turns right now
	// (shutting down, update drain). The lease is released untouched and the
	// fire is retried on a later tick. Nothing is recorded.
	ChatScheduleDeferred ChatScheduleOutcome = "deferred"
)

// ChatScheduleFire is one claimed fire of a chat schedule: the definition as
// it stood at the claim, the run number it would be, and the lease token that
// guards the settle.
type ChatScheduleFire struct {
	ScheduleID      string
	ConversationID  string
	UserEmail       string
	Title           string
	Prompt          string
	CronExpr        string
	Timezone        string
	DurationSeconds int
	// RunNumber is run_count + 1 at the fire's first claim: the number a
	// queued turn takes.
	RunNumber int
	// DueAt is next_run_at at the claim (Unix seconds).
	DueAt   int64
	Trigger ChatScheduleTrigger
	// LeaseToken is the lease_owner the claim wrote. Delivery and settle act
	// only while the row still carries it.
	LeaseToken string
	// FireID identifies the fire across lease expiries: a fire re-claimed by
	// the recovery pass keeps it (a new LeaseToken, the same FireID), so its
	// delivery's idempotency key is the same.
	FireID string
	// ClaimedAt is the claim time.
	ClaimedAt time.Time
}

// ChatScheduleDelivery is the chat server's answer to a fire.
type ChatScheduleDelivery struct {
	Outcome ChatScheduleOutcome
	// InputID is the chat_input_queue row of a queued or coalesced fire.
	InputID string
}

// ChatScheduleRun is one row of a schedule's run history.
type ChatScheduleRun struct {
	ScheduleID string
	// RunNumber is the counted run's number for a queued run, 0 (NULL) for
	// any other outcome.
	RunNumber int
	Trigger   ChatScheduleTrigger
	Outcome   ChatScheduleOutcome
	InputID   string
	DueAt     *int64
	FiredAt   time.Time
}

// ChatScheduleLocation resolves a schedule's timezone ("" is UTC).
func ChatScheduleLocation(tz string) (string, *time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return "UTC", time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", nil, err
	}
	return tz, loc, nil
}

// NextChatScheduleRun is the first firing of a chat patrol's cron expression
// strictly after now, in the schedule's timezone. Both the chat server (on an
// edit) and the scheduler (on a settle) compute next_run_at with it.
func NextChatScheduleRun(cronExpr, tz string, now time.Time) (time.Time, error) {
	_, loc, err := ChatScheduleLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	sched, err := cron.ParseStandard(strings.TrimSpace(cronExpr))
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	next := sched.Next(now.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("cron expression %q never fires", cronExpr)
	}
	return next, nil
}
