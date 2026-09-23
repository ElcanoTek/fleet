// Package cronnext computes a cron schedule's next occurrence correctly across
// UTC-offset changes of any size.
//
// robfig/cron's SpecSchedule.Next walks wall-clock fields and repairs DST by
// adding or subtracting whole hours. In a zone whose offset changes by
// anything else — Australia/Lord_Howe's 30 minutes, Pacific/Chatham's
// +12:45/+13:45, Pacific/Apia's skipped 2011-12-30 — that repair can skip a
// day or loop forever: Lord Howe "0 3 * * *" from 03:00 on 2026-04-04
// returned Apr 6, not Apr 5.
//
// Next here keeps robfig's parsing and field semantics but never does
// wall-clock arithmetic across an offset change. It walks forward in real
// time one zone period at a time (time.Time.ZoneBounds: a span with a single
// UTC offset). Inside a period, wall clock = instant + offset exactly, so the
// next matching wall time comes from robfig's own Next evaluated in UTC —
// where it has no DST to get wrong — and maps back to a real instant by
// subtracting that offset. A match that falls past the period's end did not
// happen at that offset, so the walk moves on to the next period. That one
// rule covers every case the zone database can throw at it: spring-forward
// gaps and skipped days are never matched (no period shows them), a
// fall-back hour's repeated times match in both periods, and offsets with
// seconds or of any size need no special handling.
package cronnext

import (
	"time"

	"github.com/robfig/cron/v3"
)

// horizon is how far ahead Next looks before reporting no occurrence — the
// same five years robfig's own Next searches.
const horizon = 5 * 366 * 24 * time.Hour

// maxPeriods bounds the walk: real zones change offset a few times a year,
// so this is never reached before the horizon; it only stops a pathological
// location from spinning.
const maxPeriods = 1000

// Next returns the first occurrence of s strictly after t, in t's location,
// or the zero time when there is none within five years. A schedule that is
// not a SpecSchedule (@every) is delegated to s.Next unchanged.
func Next(s cron.Schedule, t time.Time) time.Time {
	spec, ok := s.(*cron.SpecSchedule)
	if !ok {
		return s.Next(t)
	}
	loc := t.Location()
	if spec.Location != nil && spec.Location != time.Local {
		loc = spec.Location // CRON_TZ= prefix, as robfig honours it
	}
	utc := *spec
	utc.Location = time.UTC

	deadline := t.Add(horizon)
	at := t.In(loc)
	strictlyAfter := true // the first period excludes t itself; later ones start inclusive
	for i := 0; i < maxPeriods && !at.After(deadline); i++ {
		_, offsetSecs := at.Zone()
		offset := time.Duration(offsetSecs) * time.Second
		_, end := at.ZoneBounds() // zero end: this offset holds forever

		// The wall clock `at` shows, as a UTC time, and robfig's next match
		// after it (at or after it, for a period we just entered).
		wall := time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.UTC)
		if !strictlyAfter {
			wall = wall.Add(-time.Nanosecond)
		}
		match := utc.Next(wall)
		if match.IsZero() {
			return time.Time{}
		}
		instant := match.Add(-offset)
		if end.IsZero() || instant.Before(end) {
			if instant.After(deadline) {
				return time.Time{}
			}
			return instant.In(t.Location())
		}
		at = end.In(loc)
		strictlyAfter = false
	}
	return time.Time{}
}
