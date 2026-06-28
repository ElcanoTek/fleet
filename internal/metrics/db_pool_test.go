package metrics

import (
	"strings"
	"testing"
)

func TestRegisterDBPool(t *testing.T) {
	// chat is fully saturated (InUse == Max); sched has headroom.
	chat := DBPoolStats{
		MaxOpenConns: 25, OpenConns: 25, InUse: 25, Idle: 0,
		WaitCount: 3, WaitDurationSeconds: 1.5, MaxIdleClosed: 2, MaxLifetimeClosed: 4,
	}
	sched := DBPoolStats{
		MaxOpenConns: 10, OpenConns: 2, InUse: 1, Idle: 1,
	}
	RegisterDBPool(map[string]func() DBPoolStats{
		"chat":  func() DBPoolStats { return chat },
		"sched": func() DBPoolStats { return sched },
	})

	out := Render()
	want := []string{
		"# TYPE fleet_db_pool_in_use_conns gauge",
		`fleet_db_pool_in_use_conns{db="chat"} 25`,
		`fleet_db_pool_in_use_conns{db="sched"} 1`,
		`fleet_db_pool_max_conns{db="sched"} 10`,
		`fleet_db_pool_idle_conns{db="sched"} 1`,
		`fleet_db_pool_total_conns{db="chat"} 25`,
		// Cumulative values are COUNTERS, not gauges.
		"# TYPE fleet_db_pool_wait_total counter",
		`fleet_db_pool_wait_total{db="chat"} 3`,
		`fleet_db_pool_wait_duration_seconds{db="chat"} 1.5`,
		`fleet_db_pool_max_idle_closed_total{db="chat"} 2`,
		`fleet_db_pool_max_lifetime_closed_total{db="chat"} 4`,
		// Saturation: chat (25>=25) → 1, sched (1<10) → 0.
		`fleet_db_pool_saturated{db="chat"} 1`,
		`fleet_db_pool_saturated{db="sched"} 0`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("Render missing %q\n--- full ---\n%s", w, out)
		}
	}

	// wait_total must NOT be emitted as a gauge (it's a pull-at-scrape counter).
	if strings.Contains(out, "# TYPE fleet_db_pool_wait_total gauge") {
		t.Error("wait_total should be TYPE counter, not gauge")
	}

	// Pull-at-scrape: a later snapshot is reflected on the next Render. chat
	// frees a connection → no longer saturated.
	chat.InUse = 24
	if !strings.Contains(Render(), `fleet_db_pool_saturated{db="chat"} 0`) {
		t.Error("saturation gauge did not reflect updated state on re-scrape")
	}
}

func TestRegisterDBPool_EmptyIsNoop(_ *testing.T) {
	// Must not panic or register anything for an empty source map.
	RegisterDBPool(nil)
	RegisterDBPool(map[string]func() DBPoolStats{})
}
