package storage

import (
	"errors"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/cron"
)

// ErrRecurrenceNeverFires reports a cron expression that parses but has no
// future occurrence (for example `0 0 30 2 *`, the 30th of February).
var ErrRecurrenceNeverFires = errors.New("recurrence has no next run")

// TaskLocation resolves a stored task's timezone name for cron evaluation: the
// task's own zone when it loads, otherwise fallback (the scheduler's server
// location), otherwise UTC. ok is false when a non-empty name failed to load,
// so a caller can log it. Every path that computes a stored task's next fire —
// the scheduler's recurrence chain, the skip path, edits, re-runs and the
// upcoming-runs projection — resolves the zone here, so they cannot disagree
// about when a task with an empty or unloadable zone fires (#1947).
func TaskLocation(tz string, fallback *time.Location) (loc *time.Location, ok bool) {
	if fallback == nil {
		fallback = time.UTC
	}
	if tz == "" {
		return fallback, true
	}
	l, err := time.LoadLocation(tz)
	if err != nil {
		return fallback, false
	}
	return l, true
}

// NextRun parses a standard 5-field cron expression and returns its next fire
// strictly after now, evaluated in loc, as a UTC instant (scheduled_for is
// always stored in UTC). An expression with no future occurrence returns
// ErrRecurrenceNeverFires rather than the zero time, which as a scheduled_for
// would make the task due immediately.
func NextRun(recurrence string, loc *time.Location, now time.Time) (time.Time, error) {
	schedule, err := cron.ParseStandard(recurrence)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse recurrence: %w", err)
	}
	if loc == nil {
		loc = time.UTC
	}
	next := schedule.Next(now.In(loc))
	if next.IsZero() {
		return time.Time{}, ErrRecurrenceNeverFires
	}
	return next.UTC(), nil
}
