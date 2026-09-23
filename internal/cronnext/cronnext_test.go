package cronnext

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

const starBit = 1 << 63

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

func offsetAt(at time.Time, loc *time.Location) time.Duration {
	_, secs := at.In(loc).Zone()
	return time.Duration(secs) * time.Second
}

// bruteNext is the ground truth for zones on whole-minute offsets (every
// modern zone): step one minute at a time from just after t and return the
// first real instant whose wall clock in loc matches s.
func bruteNext(s *cron.SpecSchedule, t time.Time, loc *time.Location) time.Time {
	at := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 60*24*40; i, at = i+1, at.Add(time.Minute) {
		if matchesAt(s, at.In(loc)) {
			return at
		}
	}
	return time.Time{}
}

// bruteNextSeconds is the ground truth for historical offsets with a seconds
// part, which put local minute boundaries off the UTC minute grid: step one
// second at a time, for at most three days.
func bruteNextSeconds(s *cron.SpecSchedule, t time.Time, loc *time.Location) time.Time {
	at := t.Truncate(time.Second).Add(time.Second)
	for i := 0; i < 60*60*24*3; i, at = i+1, at.Add(time.Second) {
		if matchesAt(s, at.In(loc)) {
			return at
		}
	}
	return time.Time{}
}

func matchesAt(s *cron.SpecSchedule, lt time.Time) bool {
	return lt.Second() == 0 && bit(s.Month, int(lt.Month())) && dayMatches(s, lt) &&
		bit(s.Hour, lt.Hour()) && bit(s.Minute, lt.Minute())
}

func mustParse(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	s, err := cron.ParseStandard(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return s
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %q: %v", name, err)
	}
	return loc
}

// The regression: robfig skips a day across Lord Howe's 30-minute fall-back.
func TestNext_LordHoweDoesNotSkipADay(t *testing.T) {
	loc := mustLoad(t, "Australia/Lord_Howe")
	s := mustParse(t, "0 3 * * *")
	from := time.Date(2026, 4, 3, 16, 0, 0, 0, time.UTC) // 03:00 Apr 4, +11
	if robfig := s.Next(from.In(loc)).UTC(); !robfig.Equal(time.Date(2026, 4, 5, 16, 30, 0, 0, time.UTC)) {
		t.Logf("robfig no longer skips (got %s); this test still pins the right answer", robfig)
	}
	got := Next(s, from.In(loc)).UTC()
	want := time.Date(2026, 4, 4, 16, 30, 0, 0, time.UTC) // 03:00 Apr 5, +10:30
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

// Across every 2026 transition in zones with whole-hour, fractional and
// midnight transitions, Next matches the brute force, and it matches robfig
// wherever robfig itself matches the brute force.
func TestNext_MatchesBruteForceAroundTransitions(t *testing.T) {
	zones := []string{
		"America/New_York", "Europe/London", "Pacific/Auckland", "Australia/Lord_Howe",
		"Pacific/Chatham", "America/Santiago", "America/Havana", "Asia/Tokyo", "UTC",
		"Africa/Casablanca", "America/St_Johns", "Pacific/Apia", "Antarctica/Casey",
		"Antarctica/Troll", "America/Godthab", "Asia/Gaza",
	}
	exprs := []string{"30 2 * * *", "0 3 * * *", "45 1,2 * * *", "*/15 * * * *", "0 0 * * *", "0 23 * * 1-5", "30 0 1 * *"}
	for _, z := range zones {
		loc := mustLoad(t, z)
		var transitions []time.Time
		for h := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); h.Year() == 2026; h = h.Add(time.Hour) {
			if offsetAt(h, loc) != offsetAt(h.Add(time.Hour), loc) {
				transitions = append(transitions, h)
			}
		}
		if len(transitions) == 0 { // no DST: still sample the year
			transitions = []time.Time{time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
		}
		for _, e := range exprs {
			s := mustParse(t, e)
			spec := s.(*cron.SpecSchedule)
			for _, tr := range transitions {
				for from := tr.Add(-30 * time.Hour); from.Before(tr.Add(30 * time.Hour)); from = from.Add(29 * time.Minute) {
					want := bruteNext(spec, from, loc)
					got := Next(s, from.In(loc)).UTC()
					if !got.Equal(want) {
						t.Fatalf("%s %q from %s: Next = %s, brute force = %s", z, e, from, got, want)
					}
					if robfig := s.Next(from.In(loc)).UTC(); robfig.Equal(want) && !got.Equal(robfig) {
						t.Fatalf("%s %q from %s: diverges from a correct robfig result", z, e, from)
					}
				}
			}
		}
	}
}

func TestNext_KeepsCallerLocationAndCronTZ(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	from := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC).In(ny) // 08:00 EDT
	got := Next(mustParse(t, "0 8 * * 1-5"), from)
	if got.Location() != ny || !got.Equal(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next = %s (%v)", got, got.Location())
	}
	// CRON_TZ= overrides the caller's zone, as in robfig.
	tz := mustParse(t, "CRON_TZ=Asia/Tokyo 0 8 * * *")
	if got, want := Next(tz, from).UTC(), tz.Next(from).UTC(); !got.Equal(want) {
		t.Fatalf("CRON_TZ: Next = %s, robfig = %s", got, want)
	}
}

func TestNext_DelegatesNonStandardSchedules(t *testing.T) {
	every, err := cron.ParseStandard("@every 90s")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	if got, want := Next(every, from), every.Next(from); !got.Equal(want) {
		t.Fatalf("@every: Next = %s, want %s", got, want)
	}
}

func TestNext_ImpossibleScheduleIsZero(t *testing.T) {
	if got := Next(mustParse(t, "0 0 30 2 *"), time.Now()); !got.IsZero() {
		t.Fatalf("Feb 30 = %s, want zero", got)
	}
}

// Pacific/Apia skipped 2011-12-30 entirely (UTC-10 → UTC+14). A candidate that
// reads back as the right clock time but on another date is not a match, so
// "Dec 30 at noon" next falls in 2012. (robfig's own Next loops forever on
// this input, so it is not consulted here.)
func TestNext_SkippedCalendarDayIsNotAMatch(t *testing.T) {
	loc := mustLoad(t, "Pacific/Apia")
	s := mustParse(t, "0 12 30 12 *")
	from := time.Date(2011, 12, 29, 13, 0, 0, 0, loc)
	got := Next(s, from)
	want := time.Date(2012, 12, 30, 12, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

// The Upcoming forecast chains Next up to 366 times for each of up to 500
// tasks, so a dense schedule on ordinary days must cost about what robfig's
// own Next costs. The bound is relative, not wall-clock, so it holds on a slow
// runner and under -race alike (both sides slow down together); the minute-
// walking scan it guards against was ~60x slower than robfig.
func TestNext_DenseScheduleIsCheapToChain(t *testing.T) {
	s := mustParse(t, "* * * * *")
	ny := mustLoad(t, "America/New_York")
	chain := func(next func(time.Time) time.Time) time.Duration {
		started := time.Now()
		for task := 0; task < 200; task++ {
			at := time.Date(2026, 9, 23, 12, 0, 0, 0, ny)
			for i := 0; i < 366; i++ {
				n := next(at)
				if !n.Equal(at.Truncate(time.Minute).Add(time.Minute)) {
					t.Fatalf("Next(%s) = %s", at, n)
				}
				at = n
			}
		}
		return time.Since(started)
	}
	ours := chain(func(at time.Time) time.Time { return Next(s, at) })
	robfig := chain(s.Next)
	if ours > 5*robfig+50*time.Millisecond {
		t.Fatalf("200x366 chained Next took %s; robfig took %s", ours, robfig)
	}
}

// Historical zone periods the fixed-offset shortcuts got wrong: a four-second
// offset change (the 1914 midnight in Manaus never happened), a pre-1845
// offset beyond UTC-12 (Guam at UTC-14:21), and a backward jump across
// midnight (Antarctica/Casey, +11 → +8 on 2010-03-05). Each is checked against
// the second-stepping brute force.
func TestNext_HistoricalOffsets(t *testing.T) {
	cases := []struct {
		zone, expr string
		from       time.Time // interpreted in zone
	}{
		{"America/Manaus", "0 0 * * *", time.Date(1913, 12, 31, 23, 59, 0, 0, time.UTC)},
		{"Pacific/Guam", "0 23 * * *", time.Date(1840, 6, 1, 22, 0, 0, 0, time.UTC)},
		{"Antarctica/Casey", "* * * * *", time.Date(2010, 3, 5, 0, 0, 0, 0, time.UTC)},
		{"Antarctica/Casey", "30 23 * * *", time.Date(2010, 3, 4, 23, 45, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		loc := mustLoad(t, c.zone)
		from := time.Date(c.from.Year(), c.from.Month(), c.from.Day(), c.from.Hour(), c.from.Minute(), 0, 0, loc)
		s := mustParse(t, c.expr)
		want := bruteNextSeconds(s.(*cron.SpecSchedule), from, loc)
		if want.IsZero() {
			t.Fatalf("%s %q from %s: brute force found nothing within three days", c.zone, c.expr, from)
		}
		if got := Next(s, from); !got.Equal(want) {
			t.Errorf("%s %q from %s: Next = %s, brute force = %s", c.zone, c.expr, from, got, want.In(loc))
		}
	}
}

// The search horizon is robfig's calendar-year rule, not a fixed duration:
// Feb 29 from 2099 (no leap day until 2104) is found by robfig, so it must be
// found here too — and one year further is not.
func TestNext_HorizonMatchesRobfig(t *testing.T) {
	for _, c := range []struct {
		expr string
		from time.Time
	}{
		{"0 0 29 2 *", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"0 0 29 2 *", time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"0 0 29 2 *", time.Date(2026, 9, 23, 12, 0, 0, 0, mustLoad(t, "America/New_York"))},
		{"0 0 30 2 *", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		s := mustParse(t, c.expr)
		if got, want := Next(s, c.from), s.Next(c.from); !got.Equal(want) {
			t.Errorf("%q from %s: Next = %s, robfig = %s", c.expr, c.from, got, want)
		}
	}
}
