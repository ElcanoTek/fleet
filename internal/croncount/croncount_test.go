package croncount

import "testing"

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
