// Package cron parses standard cron expressions and computes their next
// occurrence correctly across UTC-offset changes of any size.
//
// The parser, the field bit sets and the wall-clock walk are copied from
// github.com/robfig/cron/v3 v3.0.1 (spec.go, constantdelay.go, the Schedule
// interface from cron.go) and modified; the job runner is not. Copyright (C)
// 2012 Rob Figueiredo; see LICENSE. The DST-safe SpecSchedule.Next is fleet's
// own (it used to live in internal/cronnext, wrapped around the library).
// SPDX-License-Identifier: MIT
//
// Why Next is not upstream's: robfig's SpecSchedule.Next walks wall-clock
// fields and repairs DST by adding or subtracting whole hours. In a zone whose
// offset changes by anything else — Australia/Lord_Howe's 30 minutes,
// Pacific/Chatham's +12:45/+13:45, Pacific/Apia's skipped 2011-12-30 — that
// repair can skip a day or loop forever: Lord Howe "0 3 * * *" from 03:00 on
// 2026-04-04 returned Apr 6, not Apr 5.
//
// Next here keeps robfig's parsing and field semantics but never does
// wall-clock arithmetic across an offset change. It walks forward in real
// time one zone period at a time (time.Time.ZoneBounds: a span with a single
// UTC offset). Inside a period, wall clock = instant + offset exactly, so the
// next matching wall time comes from the upstream walk evaluated in UTC —
// where it has no DST to get wrong — and maps back to a real instant by
// subtracting that offset. A match that falls past the period's end did not
// happen at that offset, so the walk moves on to the next period. That one
// rule covers every case the zone database can throw at it: spring-forward
// gaps and skipped days are never matched (no period shows them), a
// fall-back hour's repeated times match in both periods, and offsets with
// seconds or of any size need no special handling.
package cron

import "time"

// Schedule describes a job's duty cycle.
type Schedule interface {
	// Next returns the next activation time, later than the given time, or
	// the zero time when there is none.
	Next(time.Time) time.Time
}

// SpecSchedule specifies a duty cycle (to the second granularity), based on a
// traditional crontab specification. It is computed initially and stored as bit sets.
type SpecSchedule struct {
	Second, Minute, Hour, Dom, Month, Dow uint64

	// Override location for this schedule.
	Location *time.Location
}

// bounds provides a range of acceptable values (plus a map of name to value).
type bounds struct {
	min, max uint
	names    map[string]uint
}

// The bounds for each field.
var (
	seconds = bounds{0, 59, nil}
	minutes = bounds{0, 59, nil}
	hours   = bounds{0, 23, nil}
	dom     = bounds{1, 31, nil}
	months  = bounds{1, 12, map[string]uint{
		"jan": 1,
		"feb": 2,
		"mar": 3,
		"apr": 4,
		"may": 5,
		"jun": 6,
		"jul": 7,
		"aug": 8,
		"sep": 9,
		"oct": 10,
		"nov": 11,
		"dec": 12,
	}}
	dow = bounds{0, 6, map[string]uint{
		"sun": 0,
		"mon": 1,
		"tue": 2,
		"wed": 3,
		"thu": 4,
		"fri": 5,
		"sat": 6,
	}}
)

const (
	// Set the top bit if a star was included in the expression.
	starBit = 1 << 63
)

// yearHorizon is how far ahead Next looks before reporting no occurrence:
// robfig's own rule, a match in a wall-clock year more than five past the
// starting one does not count (so "0 0 29 2 *" from 2099 still finds 2104).
const yearHorizon = 5

// maxPeriods bounds the walk: real zones change offset a few times a year,
// so this is never reached before the horizon; it only stops a pathological
// location from spinning.
const maxPeriods = 1000

// Next returns the first occurrence of s strictly after t, in t's location,
// or the zero time when there is none within five years. A CRON_TZ= prefix
// evaluates the schedule in that zone instead, as robfig does.
func (s *SpecSchedule) Next(t time.Time) time.Time {
	loc := t.Location()
	if s.Location != nil && s.Location != time.Local {
		loc = s.Location
	}
	utc := *s
	utc.Location = time.UTC

	at := t.In(loc)
	// robfig rounds up to the next whole second before taking the year, so a
	// start in the last second of a year counts from the new one.
	lastYear := at.Add(time.Second-time.Duration(at.Nanosecond())).Year() + yearHorizon
	strictlyAfter := true // the first period excludes t itself; later ones start inclusive
	for i := 0; i < maxPeriods && at.Year() <= lastYear; i++ {
		_, offsetSecs := at.Zone()
		offset := time.Duration(offsetSecs) * time.Second
		_, end := at.ZoneBounds() // zero end: this offset holds forever

		// The wall clock `at` shows, as a UTC time, and the walk's next match
		// after it (at or after it, for a period we just entered).
		wall := time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.UTC)
		if !strictlyAfter {
			wall = wall.Add(-time.Nanosecond)
		}
		match := utc.wallNext(wall)
		if match.IsZero() || match.Year() > lastYear {
			return time.Time{} // robfig's horizon, counted from where Next started
		}
		instant := match.Add(-offset)
		if end.IsZero() || instant.Before(end) {
			return instant.In(t.Location())
		}
		at = end.In(loc)
		strictlyAfter = false
	}
	return time.Time{}
}

// wallNext is robfig's SpecSchedule.Next, unchanged: it walks wall-clock
// fields in the schedule's (or t's) location. Correct only where the offset
// does not change, so Next calls it in UTC; tests keep it as the reference
// the DST-safe Next must agree with wherever it is right.
func (s *SpecSchedule) wallNext(t time.Time) time.Time {
	// General approach
	//
	// For Month, Day, Hour, Minute, Second:
	// Check if the time value matches.  If yes, continue to the next field.
	// If the field doesn't match the schedule, then increment the field until it matches.
	// While incrementing the field, a wrap-around brings it back to the beginning
	// of the field list (since it is necessary to re-verify previous field
	// values)

	// Convert the given time into the schedule's timezone, if one is specified.
	// Save the original timezone so we can convert back after we find a time.
	// Note that schedules without a time zone specified (time.Local) are treated
	// as local to the time provided.
	origLocation := t.Location()
	loc := s.Location
	if loc == time.Local {
		loc = t.Location()
	}
	if s.Location != time.Local {
		t = t.In(s.Location)
	}

	// Start at the earliest possible time (the upcoming second).
	t = t.Add(1*time.Second - time.Duration(t.Nanosecond())*time.Nanosecond)

	// This flag indicates whether a field has been incremented.
	added := false

	// If no time is found within five years, return zero.
	yearLimit := t.Year() + 5

WRAP:
	if t.Year() > yearLimit {
		return time.Time{}
	}

	// Find the first applicable month.
	// If it's this month, then do nothing.
	for 1<<uint(t.Month())&s.Month == 0 {
		// If we have to add a month, reset the other parts to 0.
		if !added {
			added = true
			// Otherwise, set the date at the beginning (since the current time is irrelevant).
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		}
		t = t.AddDate(0, 1, 0)

		// Wrapped around.
		if t.Month() == time.January {
			goto WRAP
		}
	}

	// Now get a day in that month.
	//
	// NOTE: This causes issues for daylight savings regimes where midnight does
	// not exist.  For example: Sao Paulo has DST that transforms midnight on
	// 11/3 into 1am. Handle that by noticing when the Hour ends up != 0.
	for !dayMatches(s, t) {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		}
		t = t.AddDate(0, 0, 1)
		// Notice if the hour is no longer midnight due to DST.
		// Add an hour if it's 23, subtract an hour if it's 1.
		if t.Hour() != 0 {
			if t.Hour() > 12 {
				t = t.Add(time.Duration(24-t.Hour()) * time.Hour)
			} else {
				t = t.Add(time.Duration(-t.Hour()) * time.Hour)
			}
		}

		if t.Day() == 1 {
			goto WRAP
		}
	}

	for 1<<uint(t.Hour())&s.Hour == 0 {
		if !added {
			added = true
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		}
		t = t.Add(1 * time.Hour)

		if t.Hour() == 0 {
			goto WRAP
		}
	}

	for 1<<uint(t.Minute())&s.Minute == 0 {
		if !added {
			added = true
			t = t.Truncate(time.Minute)
		}
		t = t.Add(1 * time.Minute)

		if t.Minute() == 0 {
			goto WRAP
		}
	}

	for 1<<uint(t.Second())&s.Second == 0 {
		if !added {
			added = true
			t = t.Truncate(time.Second)
		}
		t = t.Add(1 * time.Second)

		if t.Second() == 0 {
			goto WRAP
		}
	}

	return t.In(origLocation)
}

// dayMatches returns true if the schedule's day-of-week and day-of-month
// restrictions are satisfied by the given time.
func dayMatches(s *SpecSchedule, t time.Time) bool {
	var (
		domMatch = 1<<uint(t.Day())&s.Dom > 0
		dowMatch = 1<<uint(t.Weekday())&s.Dow > 0
	)
	if s.Dom&starBit > 0 || s.Dow&starBit > 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

// ConstantDelaySchedule represents a simple recurring duty cycle, e.g. "Every 5 minutes".
// It does not support jobs more frequent than once a second.
type ConstantDelaySchedule struct {
	Delay time.Duration
}

// Every returns a crontab Schedule that activates once every duration.
// Delays of less than a second are not supported (will round up to 1 second).
// Any fields less than a Second are truncated.
func Every(duration time.Duration) ConstantDelaySchedule {
	if duration < time.Second {
		duration = time.Second
	}
	return ConstantDelaySchedule{
		Delay: duration - time.Duration(duration.Nanoseconds())%time.Second,
	}
}

// Next returns the next time this should be run.
// This rounds so that the next activation time will be on the second.
func (schedule ConstantDelaySchedule) Next(t time.Time) time.Time {
	return t.Add(schedule.Delay - time.Duration(t.Nanosecond())*time.Nanosecond)
}
