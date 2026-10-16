// Package croncount holds the ONE runs-per-month estimator behind the
// approval card's cadence hint. Both the server side (httpapi's
// summarizeApprovalInput) and the terminal client (chattui, which re-runs the
// estimate after a local cron edit) render this number to an operator deciding
// whether to approve a recurring job, so the estimate lives here — a leaf both
// can import — rather than as a second, drift-prone copy on the client.
package croncount

import (
	"time"

	"github.com/ElcanoTek/fleet/internal/cron"
)

// CountCap bounds the cron-occurrence walk so a per-minute schedule can't
// spin counting ~43k iterations; at the cap the returned count is a FLOOR
// (1000 means "≥1000"). Renderers show it as "1000+", never "≈1000" — the
// count is understated by design past this point.
const CountCap = 1000

// EstimateRunsPerMonth counts how many times a standard cron expression fires
// in the next 30 days, for the approval card's frequency hint. Returns ok=false
// for an unparseable expression (the card then omits the frequency rather than
// guessing). Evaluated in UTC; the displayed estimate only needs to be
// order-of-magnitude correct, and the real schedule is timezone-resolved at
// create time by the storage path. Once the walk reaches CountCap the count is
// a floor — see CountCap.
//
// A CRON_TZ= prefix is walked in that zone by the DST-safe internal/cron Next
// (croncount used to call robfig's own Next, which skips a day — or never
// returns — across a non-hour offset change such as Australia/Lord_Howe's).
func EstimateRunsPerMonth(cronExpr string) (int, bool) {
	return estimateRunsFrom(cronExpr, time.Now().UTC())
}

// estimateRunsFrom is EstimateRunsPerMonth over the 30 days after start; the
// 30-day window is the conventional "per month" approximation.
func estimateRunsFrom(cronExpr string, start time.Time) (int, bool) {
	schedule, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return 0, false
	}
	end := start.AddDate(0, 0, 30)
	count := 0
	for t := schedule.Next(start); !t.IsZero() && !t.After(end); t = schedule.Next(t) {
		// A zero time means the schedule has no next firing (an impossible date
		// like "0 0 30 2 *" / Feb 30 — cron parses it but Next() never resolves).
		// Stop, so the card reports 0 rather than spinning to the CountCap floor
		// and claiming a never-firing task runs constantly.
		count++
		if count >= CountCap {
			break
		}
	}
	return count, true
}
