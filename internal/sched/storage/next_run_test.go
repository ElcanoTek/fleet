package storage

import (
	"errors"
	"testing"
	"time"
)

// TestTaskLocation pins the one zone-resolution rule every next-fire path
// shares (#1947): the task's own zone, else the fallback, else UTC.
func TestTaskLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	cases := []struct {
		name     string
		tz       string
		fallback *time.Location
		want     *time.Location
		wantOK   bool
	}{
		{"valid zone wins", "America/New_York", time.UTC, ny, true},
		{"empty uses fallback", "", ny, ny, true},
		{"invalid uses fallback", "Nowhere/Bogus", ny, ny, false},
		{"nil fallback is UTC", "", nil, time.UTC, true},
	}
	for _, tc := range cases {
		got, ok := TaskLocation(tc.tz, tc.fallback)
		if got.String() != tc.want.String() || ok != tc.wantOK {
			t.Errorf("%s: TaskLocation(%q) = %v, %v; want %v, %v", tc.name, tc.tz, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestNextRun covers a normal fire in the task's zone, a DST gap, a
// recurrence that never fires, and an unparseable one.
func TestNextRun(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // 07:00 in New York

	next, err := NextRun("0 9 * * *", ny, now)
	if err != nil {
		t.Fatal(err)
	}
	if next.Location() != time.UTC || next.In(ny).Hour() != 9 || !next.After(now) {
		t.Errorf("NextRun = %v, want today 09:00 New York as a UTC instant", next)
	}

	// 02:30 does not exist on the spring-forward day; the next fire must still
	// be a real, later instant rather than the zero time.
	dst := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	if next, err := NextRun("30 2 * * *", ny, dst); err != nil || !next.After(dst) {
		t.Errorf("DST gap: NextRun = %v, %v; want a later instant", next, err)
	}

	if _, err := NextRun("0 0 30 2 *", time.UTC, now); !errors.Is(err, ErrRecurrenceNeverFires) {
		t.Errorf("Feb 30: err = %v, want ErrRecurrenceNeverFires", err)
	}
	if _, err := NextRun("not a cron", time.UTC, now); err == nil || errors.Is(err, ErrRecurrenceNeverFires) {
		t.Errorf("unparseable: err = %v, want a parse error", err)
	}
}
