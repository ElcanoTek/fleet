package scheduler

import (
	"context"
	"errors"
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

// TestStopReleasesBlockedSweep pins #2021: a storage sweep blocked on the
// database (modelled as a fn waiting on its ctx) is released by Stop instead
// of holding the tick forever.
func TestStopReleasesBlockedSweep(t *testing.T) {
	s := New(nil, "UTC")
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var got error
		s.sweep("blocked test sweep", func(ctx context.Context) (int, error) {
			close(entered)
			<-ctx.Done()
			got = ctx.Err()
			return 0, got
		}, func(int) { t.Error("onCount called for a failed sweep") })
		done <- got
	}()
	<-entered
	s.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sweep ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not release a sweep blocked on its context")
	}
}

// TestSweepCountsOnlyNonZero pins the quiet-queue contract the collapsed
// wrappers relied on: onCount runs only for a successful sweep touching rows.
func TestSweepCountsOnlyNonZero(t *testing.T) {
	s := New(nil, "UTC")
	defer s.Stop()
	var calls []int
	for _, n := range []int{0, 3} {
		s.sweep("count test sweep", func(ctx context.Context) (int, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("sweep ctx has no deadline")
			}
			return n, nil
		}, func(got int) { calls = append(calls, got) })
	}
	s.sweep("error test sweep", func(context.Context) (int, error) { return 5, errors.New("boom") },
		func(got int) { calls = append(calls, got) })
	if len(calls) != 1 || calls[0] != 3 {
		t.Fatalf("onCount calls = %v, want [3]", calls)
	}
}
