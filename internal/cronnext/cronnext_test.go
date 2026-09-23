package cronnext

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// bruteNext is the ground truth: step one minute at a time from just after t
// and return the first real instant whose wall clock in loc matches s.
func bruteNext(s *cron.SpecSchedule, t time.Time, loc *time.Location) time.Time {
	at := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 60*24*40; i, at = i+1, at.Add(time.Minute) {
		lt := at.In(loc)
		if bit(s.Month, int(lt.Month())) && dayMatches(s, lt) && bit(s.Hour, lt.Hour()) && bit(s.Minute, lt.Minute()) {
			return at
		}
	}
	return time.Time{}
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
