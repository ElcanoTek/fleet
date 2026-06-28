package scheduler

import (
	"testing"
	"time"
)

func TestDurationUntilHour(t *testing.T) {
	base := func(h, m int) time.Time {
		return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC)
	}

	cases := []struct {
		name string
		now  time.Time
		hour int
		want time.Duration
	}{
		{"before target same day", base(2, 0), 4, 2 * time.Hour},
		{"after target rolls to next day", base(5, 0), 4, 23 * time.Hour},
		{"exactly at target rolls to next day", base(4, 0), 4, 24 * time.Hour},
		{"just before target", base(3, 30), 4, 30 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := durationUntilHour(c.now, c.hour)
			if got != c.want {
				t.Errorf("durationUntilHour(%v, %d) = %v, want %v", c.now, c.hour, got, c.want)
			}
			if got <= 0 || got > 24*time.Hour {
				t.Errorf("duration must be in (0, 24h], got %v", got)
			}
		})
	}
}
