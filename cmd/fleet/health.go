package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/health"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
)

// sandboxProbeTTL caches the sandbox runtime probe so /readyz — which is
// unauthenticated — cannot be turned into a fork bomb: the `<runtime> --version`
// subprocess runs at most once per TTL regardless of request rate (#215).
const sandboxProbeTTL = 10 * time.Second

// dbPinger is the readiness surface both DB handles satisfy (#215).
type dbPinger interface {
	Ping(ctx context.Context) error
}

var (
	_ dbPinger = (*store.Store)(nil)
	_ dbPinger = (*scheddb.Database)(nil)
)

// buildReadinessChecks assembles the /readyz probes (#215). The two Postgres
// pools are CRITICAL — if either is down the box can't serve, so /readyz returns
// 503. The sandbox runtime is non-critical (a missing runtime degrades to 207
// but the process can still answer DB-only requests and surface the problem).
//
// llm_api and per-server mcp_servers probes are intentionally NOT included here
// yet (documented follow-ups): a live LLM completion probe needs the authed
// client + cost-aware caching, and MCP liveness needs a real broker round-trip —
// reporting either without actually probing would violate the honesty invariant.
func buildReadinessChecks(cfg *config.Config, chatDB, schedDB dbPinger) []health.Check {
	runtimeBin := strings.TrimSpace(cfg.SandboxRuntime)
	if runtimeBin == "" {
		runtimeBin = "podman"
	}
	return []health.Check{
		{Name: "chat_db", Critical: true, Probe: pingProbe(chatDB)},
		{Name: "sched_db", Critical: true, Probe: pingProbe(schedDB)},
		{Name: "sandbox", Critical: false, Probe: (&cachedSandboxProbe{runtimeBin: runtimeBin, ttl: sandboxProbeTTL}).probe},
	}
}

// pingProbe times a DB ping and reports latency, or an error result on failure.
func pingProbe(db dbPinger) func(context.Context) health.Result {
	return func(ctx context.Context) health.Result {
		start := time.Now()
		if err := db.Ping(ctx); err != nil {
			return health.Result{Status: health.StatusError, Detail: err.Error()}
		}
		return health.Result{Status: health.StatusOK, LatencyMs: time.Since(start).Milliseconds()}
	}
}

// cachedSandboxProbe runs `<runtime> --version` to confirm the container runtime
// is present and responsive, returning its version in the detail. A lightweight
// check suitable for frequent polling; deep functional verification (rootless
// setup, image presence) lives in the boot-time `fleet validate-config`
// preflight, not in a readiness probe.
//
// The result is cached for ttl: the mutex is held across the subprocess so
// concurrent /readyz hits collapse to a single exec (singleflight), and cached
// hits return instantly with Cached=true. This bounds podman spawns from an
// unauthenticated endpoint to ~1 per ttl regardless of request rate (#215).
type cachedSandboxProbe struct {
	runtimeBin string
	ttl        time.Duration

	mu        sync.Mutex
	cached    health.Result
	at        time.Time
	hasCached bool
}

func (c *cachedSandboxProbe) probe(ctx context.Context) health.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasCached && time.Since(c.at) < c.ttl {
		hit := c.cached
		hit.Cached = true
		return hit
	}
	var res health.Result
	//nolint:gosec // G702: runtimeBin is operator config (FLEET_SANDBOX_RUNTIME, default "podman"), not request input — the same trusted binary the sandbox already execs for every tool call.
	out, err := exec.CommandContext(ctx, c.runtimeBin, "--version").Output()
	if err != nil {
		res = health.Result{Status: health.StatusError, Detail: c.runtimeBin + ": " + err.Error()}
	} else {
		res = health.Result{Status: health.StatusOK, Detail: strings.TrimSpace(string(out))}
	}
	c.cached, c.at, c.hasCached = res, time.Now(), true
	return res
}

// withHealthProbes intercepts GET /livez and GET /readyz, delegating everything
// else to next (#215). Mounted on BOTH HTTP servers so a load balancer / systemd
// watchdog gets identical semantics on either port. The probes are served by
// this wrapper rather than registered on the underlying mux/chi router, so they
// are not part of the documented API surface (and don't trip the orchestrator
// OpenAPI drift test).
//
// draining reports whether graceful shutdown has begun. /livez stays 200 while
// draining (the process is alive; don't restart it), but /readyz short-circuits
// to not_ready/503 so load balancers stop sending new traffic to a draining
// instance — matching /healthz and honoring the #278 drain.
func withHealthProbes(next http.Handler, startTime time.Time, draining func() bool, checks []health.Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/livez":
				writeHealthJSON(w, http.StatusOK, health.Liveness(startTime, time.Now()))
				return
			case "/readyz":
				if draining != nil && draining() {
					writeHealthJSON(w, http.StatusServiceUnavailable, health.ReadyResponse{
						Status: health.NotReady,
						Checks: map[string]health.Result{"draining": {Status: health.StatusError, Detail: "shutting_down"}},
					})
					return
				}
				resp, code := health.RunReadiness(r.Context(), checks)
				writeHealthJSON(w, code, resp)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeHealthJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
