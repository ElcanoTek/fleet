// Package cronnext computes a cron schedule's next occurrence correctly across
// DST transitions of any size.
//
// robfig/cron's SpecSchedule.Next walks wall-clock fields and repairs DST by
// adding or subtracting whole hours. In a zone whose offset changes by a
// fraction of an hour — Australia/Lord_Howe (30 minutes), Pacific/Chatham
// (+12:45/+13:45) — that repair can land on the wrong side of midnight and
// skip a day: Lord Howe "0 3 * * *" from 03:00 on 2026-04-04 returns Apr 6,
// not Apr 5. Next here keeps robfig's parsing and field semantics (including
// its day-of-month/day-of-week rule) but finds the occurrence by scanning the
// zone's calendar as UTC-encoded wall clock — UTC has no DST — and mapping each
// candidate to the real instant(s) that show it. A wall time the zone skips
// (spring-forward) is not an occurrence; one it repeats (fall-back) can match
// twice. The web form's next-run preview (web/src/app/shared/lib/cronNext.ts)
// uses the same algorithm, so the two agree.
package cronnext

import (
	"time"

	"github.com/robfig/cron/v3"
)

const (
	// UTC offsets span UTC-12 to UTC+14, so a wall-clock time encoded as a
	// UTC instant is shown by a real instant within this window of it.
	maxAhead  = 14 * time.Hour
	maxBehind = 12 * time.Hour
	// Offsets are probed this far either side of a day: past any transition
	// that can touch the day's instants (no zone changes offset twice within).
	probe = 36 * time.Hour
	// Same horizon robfig uses before giving up.
	horizonDays = 5 * 366

	starBit = 1 << 63
)

// Next returns the first occurrence of s strictly after t, in t's location,
// or the zero time when there is none within five years. A schedule that is
// not a standard minute-resolution SpecSchedule (a seconds field, @every)
// is delegated to s.Next unchanged.
func Next(s cron.Schedule, t time.Time) time.Time {
	spec, ok := s.(*cron.SpecSchedule)
	if !ok || spec.Second != 1 {
		return s.Next(t)
	}
	loc := t.Location()
	if spec.Location != nil && spec.Location != time.Local {
		loc = spec.Location // CRON_TZ= prefix, as robfig honours it
	}

	after := t.Truncate(time.Minute).Add(time.Minute) // strictly after t
	y, mo, d := after.In(loc).Date()
	// Start a day early: a transition at midnight can put the next instant on
	// an earlier wall-clock date than `after` reads.
	day := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)

	for i := 0; i <= horizonDays; i, day = i+1, day.AddDate(0, 0, 1) {
		if !bit(spec.Month, int(day.Month())) || !dayMatches(spec, day) {
			continue
		}
		if day.Add(24*time.Hour + maxBehind).Before(after) {
			continue // every instant of this day is already past
		}
		before := offsetAt(day.Add(-probe), loc)
		later := offsetAt(day.Add(24*time.Hour+probe), loc)
		offsets := []time.Duration{before}
		if later != before {
			offsets = append(offsets, later)
		}

		// Wall order and instant order disagree inside a repeated hour, so
		// take the earliest qualifying instant of the whole day.
		var best time.Time
		for h := 0; h < 24; h++ {
			if !bit(spec.Hour, h) {
				continue
			}
			for m := 0; m < 60; m++ {
				if !bit(spec.Minute, m) {
					continue
				}
				wall := day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
				if wall.Add(maxBehind).Before(after) {
					continue
				}
				if !best.IsZero() && wall.Add(-maxAhead).After(best) {
					break
				}
				for _, off := range offsets {
					instant := wall.Add(-off)
					if instant.Before(after) || (!best.IsZero() && !instant.Before(best)) {
						continue
					}
					if len(offsets) > 1 {
						// Around a transition only an instant that reads back as
						// the requested time is real: this drops the wrong-offset
						// candidate and a spring-forward gap time.
						lt := instant.In(loc)
						if lt.Hour() != h || lt.Minute() != m {
							continue
						}
					}
					best = instant
				}
			}
		}
		if !best.IsZero() {
			return best.In(t.Location())
		}
	}
	return time.Time{}
}

// offsetAt is how far loc's wall clock runs ahead of UTC at instant at.
func offsetAt(at time.Time, loc *time.Location) time.Duration {
	_, secs := at.In(loc).Zone()
	return time.Duration(secs) * time.Second
}

func bit(field uint64, v int) bool { return field&(1<<uint(v)) != 0 }

// dayMatches is robfig's rule: when either day field is "*" both must match,
// otherwise either may.
func dayMatches(s *cron.SpecSchedule, day time.Time) bool {
	domMatch := bit(s.Dom, day.Day())
	dowMatch := bit(s.Dow, int(day.Weekday()))
	if s.Dom&starBit > 0 || s.Dow&starBit > 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}
