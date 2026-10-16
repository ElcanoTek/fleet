package croncount

import (
	"testing"
	"time"
)

func TestEstimateRunsPerMonth(t *testing.T) {
	// Hourly → ~720/month (24*30); daily → ~30; weekly → ~4.
	for _, tc := range []struct {
		cron    string
		lo, hi  int
		wantErr bool
	}{
		{cron: "0 * * * *", lo: 700, hi: 744}, // hourly
		{cron: "0 0 * * *", lo: 28, hi: 32},   // daily
		{cron: "0 9 * * MON", lo: 3, hi: 6},   // weekly
		{cron: "not a cron", wantErr: true},
	} {
		n, ok := EstimateRunsPerMonth(tc.cron)
		if tc.wantErr {
			if ok {
				t.Errorf("cron %q: expected ok=false", tc.cron)
			}
			continue
		}
		if !ok {
			t.Errorf("cron %q: expected ok=true", tc.cron)
			continue
		}
		if n < tc.lo || n > tc.hi {
			t.Errorf("cron %q: runs/month = %d, want [%d,%d]", tc.cron, n, tc.lo, tc.hi)
		}
	}

	// Per-minute cron is capped, never an unbounded walk.
	n, ok := EstimateRunsPerMonth("* * * * *")
	if !ok || n != CountCap {
		t.Errorf("per-minute cron should hit the cap %d, got %d (ok=%v)", CountCap, n, ok)
	}

	// An impossible-but-parseable date (Feb 30) parses, but Next() never fires →
	// must report 0, NOT spin to the cap and claim "1000+ runs/month".
	if n, ok := EstimateRunsPerMonth("0 0 30 2 *"); !ok || n != 0 {
		t.Errorf("impossible cron (Feb 30): got %d (ok=%v), want 0", n, ok)
	}
}

// Regression: the estimate used robfig/cron's own Next, which repairs DST by
// whole hours, so across Australia/Lord_Howe's 30-minute offset changes it
// skipped a day (28 of 29 runs across the April fall-back, 29 of 30 across the
// October spring-forward) and a daily job read as running less often than it
// does. The count must equal a brute-force walk of the real instants in the
// window; Pacific/Chatham's 45-minute-offset zone is checked too.
func TestEstimateRunsAcrossNonHourDSTChange(t *testing.T) {
	for _, tc := range []struct {
		zone  string
		start time.Time
	}{
		{"Australia/Lord_Howe", time.Date(2026, 4, 3, 16, 0, 0, 0, time.UTC)}, // 03:00 Apr 4, +11
		{"Pacific/Chatham", time.Date(2026, 4, 2, 13, 0, 0, 0, time.UTC)},
		{"Australia/Lord_Howe", time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)}, // spring forward Oct 4
	} {
		loc, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		end := tc.start.AddDate(0, 0, 30)
		for at := tc.start.Truncate(time.Minute).Add(time.Minute); !at.After(end); at = at.Add(time.Minute) {
			if lt := at.In(loc); lt.Hour() == 3 && lt.Minute() == 0 {
				want++
			}
		}
		expr := "CRON_TZ=" + tc.zone + " 0 3 * * *"
		if n, ok := estimateRunsFrom(expr, tc.start); !ok || n != want {
			t.Errorf("%q from %s: runs = %d (ok=%v), want %d", expr, tc.start, n, ok, want)
		}
	}
}
