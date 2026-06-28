// Package health implements fleet's liveness + readiness probe logic (#215).
//
// It is deliberately transport- and subsystem-agnostic: callers describe their
// subsystems as a slice of Check values (each a name, a criticality, and a
// probe func), and RunReadiness fans them out in parallel under a per-check
// deadline and folds the results into one overall status + HTTP code. The two
// fleet HTTP servers (chat :8080, orchestrator :8000) share this so a load
// balancer / systemd watchdog gets the same semantics on either port.
package health

import (
	"context"
	"sort"
	"sync"
	"time"
)

// perCheckTimeout bounds every individual probe so one stuck subsystem can't
// hang the whole readiness response.
const perCheckTimeout = 5 * time.Second

// Status values for an individual check.
const (
	StatusOK       = "ok"
	StatusError    = "error"
	StatusDegraded = "degraded"
)

// Overall readiness status values.
const (
	Ready    = "ready"
	Degraded = "degraded"
	NotReady = "not_ready"
)

// Result is one subsystem's probe outcome. Status is required; the rest are
// optional and omitted from JSON when zero.
type Result struct {
	Status    string `json:"status"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
	Healthy   int    `json:"healthy,omitempty"`
	Total     int    `json:"total,omitempty"`
}

// Check is one named subsystem probe. Critical checks failing drive the overall
// status to not_ready (HTTP 503); non-critical failures only degrade it (207).
// Probe should respect ctx (it is given a per-check deadline) and must not
// panic — RunReadiness recovers, but a panicking probe yields an error result.
type Check struct {
	Name     string
	Critical bool
	Probe    func(ctx context.Context) Result
}

// ReadyResponse is the /readyz body.
type ReadyResponse struct {
	Status string            `json:"status"`
	Checks map[string]Result `json:"checks"`
}

// LiveResponse is the /livez body.
type LiveResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// Liveness reports process liveness: always ok as long as the goroutine runs,
// with uptime derived from the process start time.
func Liveness(startTime time.Time, now time.Time) LiveResponse {
	up := int64(now.Sub(startTime).Seconds())
	if up < 0 {
		up = 0
	}
	return LiveResponse{Status: "ok", UptimeSeconds: up}
}

// RunReadiness runs every check in parallel (each under its own perCheckTimeout
// derived from ctx), then folds the results: any failing CRITICAL check →
// not_ready/503; else any failing non-critical check → degraded/207; else
// ready/200. Returns the response and the HTTP status code to send.
func RunReadiness(ctx context.Context, checks []Check) (ReadyResponse, int) {
	results := make(map[string]Result, len(checks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()
			res := runOne(ctx, c)
			mu.Lock()
			results[c.Name] = res
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	criticalDown, nonCriticalDown := false, false
	for _, c := range checks {
		if results[c.Name].Status == StatusOK {
			continue
		}
		if c.Critical {
			criticalDown = true
		} else {
			nonCriticalDown = true
		}
	}

	resp := ReadyResponse{Checks: results}
	switch {
	case criticalDown:
		resp.Status = NotReady
		return resp, 503
	case nonCriticalDown:
		resp.Status = Degraded
		return resp, 207
	default:
		resp.Status = Ready
		return resp, 200
	}
}

// runOne executes a single probe under a per-check deadline, recovering panics
// into an error result so one bad probe can't crash the handler.
func runOne(ctx context.Context, c Check) (res Result) {
	cctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	defer cancel()

	done := make(chan Result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- Result{Status: StatusError, Detail: "probe panicked"}
			}
		}()
		done <- c.Probe(cctx)
	}()

	select {
	case res = <-done:
		return res
	case <-cctx.Done():
		// The probe overran its deadline (or the parent ctx was cancelled).
		return Result{Status: StatusError, Detail: "timeout"}
	}
}

// CheckNames returns the check names in sorted order (handy for stable test
// assertions and logging).
func CheckNames(checks []Check) []string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}
