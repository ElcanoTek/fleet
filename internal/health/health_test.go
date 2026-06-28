package health

import (
	"context"
	"testing"
	"time"
)

func ok(name string, critical bool) Check {
	return Check{Name: name, Critical: critical, Probe: func(context.Context) Result {
		return Result{Status: StatusOK, LatencyMs: 1}
	}}
}

func fail(name string, critical bool, status string) Check {
	return Check{Name: name, Critical: critical, Probe: func(context.Context) Result {
		return Result{Status: status, Detail: "boom"}
	}}
}

func TestRunReadiness_AllOK(t *testing.T) {
	resp, code := RunReadiness(context.Background(), []Check{ok("chat_db", true), ok("sandbox", false)})
	if resp.Status != Ready || code != 200 {
		t.Fatalf("want ready/200, got %s/%d", resp.Status, code)
	}
	if len(resp.Checks) != 2 || resp.Checks["chat_db"].Status != StatusOK {
		t.Errorf("checks map wrong: %+v", resp.Checks)
	}
}

func TestRunReadiness_NonCriticalDown_Degraded207(t *testing.T) {
	resp, code := RunReadiness(context.Background(), []Check{
		ok("chat_db", true),
		fail("sandbox", false, StatusError),
		fail("mcp_servers", false, StatusDegraded),
	})
	if resp.Status != Degraded || code != 207 {
		t.Fatalf("want degraded/207, got %s/%d", resp.Status, code)
	}
}

func TestRunReadiness_CriticalDown_NotReady503(t *testing.T) {
	// A critical failure wins even when non-critical checks also fail.
	resp, code := RunReadiness(context.Background(), []Check{
		fail("chat_db", true, StatusError),
		fail("sandbox", false, StatusError),
		ok("sched_db", true),
	})
	if resp.Status != NotReady || code != 503 {
		t.Fatalf("want not_ready/503, got %s/%d", resp.Status, code)
	}
}

func TestRunReadiness_Parallel(t *testing.T) {
	// Three probes each sleeping ~100ms must finish well under 300ms if run
	// concurrently (proves fan-out, not serialization).
	slow := func(name string) Check {
		return Check{Name: name, Probe: func(ctx context.Context) Result {
			select {
			case <-time.After(100 * time.Millisecond):
				return Result{Status: StatusOK}
			case <-ctx.Done():
				return Result{Status: StatusError}
			}
		}}
	}
	start := time.Now()
	resp, code := RunReadiness(context.Background(), []Check{slow("a"), slow("b"), slow("c")})
	elapsed := time.Since(start)
	if resp.Status != Ready || code != 200 {
		t.Fatalf("want ready/200, got %s/%d", resp.Status, code)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("checks did not run in parallel: took %s", elapsed)
	}
}

func TestRunReadiness_TimeoutBecomesError(t *testing.T) {
	// The probe deliberately IGNORES ctx and overruns. The parent deadline
	// (50ms) is shorter than perCheckTimeout (5s), so the only way to a fast
	// result is runOne's deadline branch — which must produce an error result
	// with Detail "timeout". Asserting that discriminating detail exercises the
	// timeout MECHANISM rather than the probe's own (uncooperative) return.
	hang := Check{Name: "chat_db", Critical: true, Probe: func(context.Context) Result {
		time.Sleep(300 * time.Millisecond)
		return Result{Status: StatusOK} // never reached in time
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp, code := RunReadiness(ctx, []Check{hang})
	if resp.Status != NotReady || code != 503 {
		t.Fatalf("hung critical check should be not_ready/503, got %s/%d", resp.Status, code)
	}
	if got := resp.Checks["chat_db"]; got.Status != StatusError || got.Detail != "timeout" {
		t.Errorf("want a timeout error result, got %+v", got)
	}
}

func TestRunReadiness_PanicRecovered(t *testing.T) {
	boom := Check{Name: "sandbox", Critical: false, Probe: func(context.Context) Result {
		panic("kaboom")
	}}
	resp, code := RunReadiness(context.Background(), []Check{ok("chat_db", true), boom})
	if resp.Status != Degraded || code != 207 {
		t.Fatalf("panicking non-critical probe should degrade to 207, got %s/%d", resp.Status, code)
	}
	if resp.Checks["sandbox"].Status != StatusError {
		t.Errorf("panicked probe should yield error result, got %+v", resp.Checks["sandbox"])
	}
}

func TestLiveness(t *testing.T) {
	start := time.Unix(1000, 0)
	now := time.Unix(1042, 0)
	lr := Liveness(start, now)
	if lr.Status != "ok" || lr.UptimeSeconds != 42 {
		t.Errorf("liveness: got %+v", lr)
	}
	// Clock skew (now < start) must not produce a negative uptime.
	if got := Liveness(now, start); got.UptimeSeconds != 0 {
		t.Errorf("negative uptime should clamp to 0, got %d", got.UptimeSeconds)
	}
}
