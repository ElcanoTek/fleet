package metrics

import "sort"

// DBPoolStats is a metrics-friendly snapshot of a database/sql pool (#276). The
// caller adapts sql.DBStats into this so the metrics package stays free of a
// database/sql dependency in its public surface and stays unit-testable without
// a live DB.
type DBPoolStats struct {
	MaxOpenConns        int
	OpenConns           int
	InUse               int
	Idle                int
	WaitCount           int64
	WaitDurationSeconds float64
	MaxIdleClosed       int64
	MaxLifetimeClosed   int64
}

const (
	nameDBPoolTotal       = "fleet_db_pool_total_conns"
	nameDBPoolIdle        = "fleet_db_pool_idle_conns"
	nameDBPoolInUse       = "fleet_db_pool_in_use_conns"
	nameDBPoolMax         = "fleet_db_pool_max_conns"
	nameDBPoolSaturated   = "fleet_db_pool_saturated"
	nameDBPoolWaitCount   = "fleet_db_pool_wait_total"
	nameDBPoolWaitSeconds = "fleet_db_pool_wait_duration_seconds"
	nameDBPoolIdleClosed  = "fleet_db_pool_max_idle_closed_total"
	nameDBPoolLifeClosed  = "fleet_db_pool_max_lifetime_closed_total"
)

// RegisterDBPool exposes connection-pool health for each named pool (#276),
// scraped live from db.Stats(). sources maps a pool label ("chat", "sched") to a
// snapshot function. Each metric is registered ONCE with a callback that emits
// one sample per pool, so there are no duplicate HELP/TYPE lines regardless of
// how many pools are supplied. Point-in-time values are gauges; cumulative
// monotonic values (waits, forced closes) are pull-at-scrape counters.
//
// fleet_db_pool_saturated{db} is 1 when the pool is fully checked out
// (InUse >= MaxOpenConns) — alert on `... == 1 for: 1m` to catch sustained
// exhaustion without a stateful rolling-window counter in-process.
func RegisterDBPool(sources map[string]func() DBPoolStats) {
	if len(sources) == 0 {
		return
	}
	// Stable label order across scrapes.
	labels := make([]string, 0, len(sources))
	for label := range sources {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	// sample builds one GaugeSample per pool from a value extractor.
	sample := func(val func(DBPoolStats) float64) []GaugeSample {
		out := make([]GaugeSample, 0, len(labels))
		for _, label := range labels {
			out = append(out, GaugeSample{Labels: []string{label}, Value: val(sources[label]())})
		}
		return out
	}

	RegisterGauge(nameDBPoolTotal, "Open connections in the pool (in-use + idle).", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return float64(s.OpenConns) }) })
	RegisterGauge(nameDBPoolIdle, "Idle connections in the pool.", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return float64(s.Idle) }) })
	RegisterGauge(nameDBPoolInUse, "Connections currently in use.", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return float64(s.InUse) }) })
	RegisterGauge(nameDBPoolMax, "Configured max open connections.", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return float64(s.MaxOpenConns) }) })
	RegisterGauge(nameDBPoolSaturated, "1 when the pool is fully checked out (in-use >= max).", []string{"db"},
		func() []GaugeSample {
			return sample(func(s DBPoolStats) float64 {
				if s.MaxOpenConns > 0 && s.InUse >= s.MaxOpenConns {
					return 1
				}
				return 0
			})
		})

	RegisterCounterFunc(nameDBPoolWaitCount, "Total times a connection acquisition had to wait.", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return float64(s.WaitCount) }) })
	RegisterCounterFunc(nameDBPoolWaitSeconds, "Cumulative time spent waiting for a connection, in seconds.", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return s.WaitDurationSeconds }) })
	RegisterCounterFunc(nameDBPoolIdleClosed, "Connections closed because the max-idle-connections count cap was exceeded (SetMaxIdleConns).", []string{"db"},
		func() []GaugeSample { return sample(func(s DBPoolStats) float64 { return float64(s.MaxIdleClosed) }) })
	RegisterCounterFunc(nameDBPoolLifeClosed, "Connections closed because they exceeded the lifetime limit.", []string{"db"},
		func() []GaugeSample {
			return sample(func(s DBPoolStats) float64 { return float64(s.MaxLifetimeClosed) })
		})
}
