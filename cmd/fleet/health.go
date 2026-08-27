package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/diskguard"
	"github.com/ElcanoTek/fleet/internal/health"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
)

// sandboxProbeTTL caches the sandbox runtime probe so /readyz — which is
// unauthenticated — cannot be turned into a resource drain: the probe (a
// `<runtime> --version` subprocess under the podman backend; one apiserver
// GET /version under kubernetes) runs at most once per TTL regardless of
// request rate (#215).
const sandboxProbeTTL = 10 * time.Second

// k8sSandboxProbeTimeout bounds the apiserver /version call inside the
// kubernetes sandbox probe. The cache mutex is held across the probe
// (deliberately — that is the singleflight), so an unbounded call against a
// hung apiserver would park every /readyz request behind it for as long as
// the slowest client keeps its request open.
const k8sSandboxProbeTimeout = 5 * time.Second

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
// Disk headroom is non-critical for the same reason and a stronger one: a box
// shedding scheduled work on low disk still serves chat perfectly, and chat is
// how an operator reclaims the space — marking it critical would 503 /readyz,
// drain the box, and remove the remedy along with the symptom.
//
// llm_api and per-server mcp_servers probes are intentionally NOT included here
// yet (documented follow-ups): a live LLM completion probe needs the authed
// client + cost-aware caching, and MCP liveness needs a real broker round-trip —
// reporting either without actually probing would violate the honesty invariant.
func buildReadinessChecks(cfg *config.Config, chatDB, schedDB dbPinger, guard *diskguard.Guard, sbPool *sandbox.Pool) []health.Check {
	return []health.Check{
		{Name: "chat_db", Critical: true, Probe: pingProbe(chatDB)},
		{Name: "sched_db", Critical: true, Probe: pingProbe(schedDB)},
		{Name: "sandbox", Critical: false, Probe: (&cachedProbe{fresh: sandboxProbeFor(cfg, sbPool), ttl: sandboxProbeTTL}).probe},
		{Name: "disk", Critical: false, Probe: diskProbe(guard)},
	}
}

// sandboxProbeFor picks the sandbox liveness probe for the resolved backend.
// Under podman it is `<runtime> --version` on this host; under kubernetes the
// host has no runtime binary at all — the sandbox runtime IS the apiserver —
// so probing podman there reported a permanent, misleading "degraded" (#1264).
func sandboxProbeFor(cfg *config.Config, sbPool *sandbox.Pool) func(context.Context) health.Result {
	if cfg.SandboxBackend == sandbox.BackendKubernetes {
		// Concrete-type nil check BEFORE the interface conversion in
		// k8sSandboxProbe's argument, so a nil handle cannot ride in as a
		// typed non-nil interface.
		if sbPool != nil {
			if backend := sbPool.KubernetesBackend(); backend != nil {
				return k8sSandboxProbe(backend)
			}
		}
		// Boot fails closed before serving when the kubernetes backend cannot
		// be built, so this arm should be unreachable — but a probe must never
		// lie, and "ok" or a podman exec would both lie here.
		return func(context.Context) health.Result {
			return health.Result{Status: health.StatusError, Detail: "kubernetes backend selected but the sandbox pool has no backend handle"}
		}
	}
	// Probe a best-effort guess at the runtime binary: an empty runtime means
	// the podman default, else the OCI runtime's conventional binary name
	// ("kata" → "kata-runtime", "krun" → "krun"). Mapping via RuntimeBinary keeps
	// `<bin> --version` from spuriously failing on a bare "kata" (#217).
	//
	// Deliberately a GUESS, not podman's resolution: this readiness probe runs
	// on every /readyz and must stay cheap, whereas resolving through podman
	// costs a `podman info` per call. Under a containers.conf remap it can
	// therefore report on a different binary than podman execs. That is
	// acceptable here because it is a liveness signal, not a security gate —
	// the fail-closed boot preflight (sandbox.PreflightRuntime) does resolve
	// through podman and is what the ADR-0010 guarantees rest on.
	runtimeBin := "podman"
	if bin := sandbox.RuntimeBinary(cfg.SandboxRuntime); bin != "" {
		runtimeBin = bin
	}
	return podmanSandboxProbe(runtimeBin)
}

// apiserverProber is the narrow slice of sandbox.KubernetesBackend the
// kubernetes sandbox probe consumes (a seam for tests).
type apiserverProber interface {
	ApiserverVersion(ctx context.Context) (string, error)
}

// k8sSandboxProbe reports apiserver reachability as the sandbox liveness
// signal: every sandbox operation under this backend is an apiserver call, so
// GET /version — the same call the boot preflight opens with — is the honest
// equivalent of the podman `--version` subprocess. Version text and errors
// are already sanitized by the sandbox client before they reach us.
func k8sSandboxProbe(prober apiserverProber) func(context.Context) health.Result {
	return func(ctx context.Context) health.Result {
		ctx, cancel := context.WithTimeout(ctx, k8sSandboxProbeTimeout)
		defer cancel()
		version, err := prober.ApiserverVersion(ctx)
		if err != nil {
			return health.Result{Status: health.StatusError, Detail: "apiserver: " + err.Error()}
		}
		return health.Result{Status: health.StatusOK, Detail: "apiserver " + version}
	}
}

// podmanSandboxProbe runs `<runtime> --version` to confirm the container
// runtime is present and responsive, returning its version in the detail. A
// lightweight check suitable for frequent polling; deep functional
// verification (rootless setup, image presence) lives in the boot-time
// `fleet validate-config` preflight, not in a readiness probe.
func podmanSandboxProbe(runtimeBin string) func(context.Context) health.Result {
	return func(ctx context.Context) health.Result {
		//nolint:gosec // G702: runtimeBin is operator config (FLEET_SANDBOX_RUNTIME, default "podman"), not request input — the same trusted binary the sandbox already execs for every tool call.
		out, err := exec.CommandContext(ctx, runtimeBin, "--version").Output()
		if err != nil {
			return health.Result{Status: health.StatusError, Detail: runtimeBin + ": " + err.Error()}
		}
		return health.Result{Status: health.StatusOK, Detail: strings.TrimSpace(string(out))}
	}
}

// diskProbe reports host disk headroom on /readyz. The guard caches its statfs,
// so this unauthenticated endpoint costs no syscall per hit.
//
// Three outcomes, and the mapping matters:
//   - shedding → degraded (207). The box is up and serving chat; only the
//     scheduled queue has paused. Never an error, which would 503 and drain it.
//   - unmeasurable → ok, with the reason in the detail. The guard fails open, so
//     the probe must not claim a degradation the guard itself does not act on.
//   - otherwise → ok, with the free percentage for an operator reading the body.
func diskProbe(guard *diskguard.Guard) func(context.Context) health.Result {
	return func(context.Context) health.Result {
		st := guard.Status()
		switch {
		case st.SampledAt.IsZero():
			return health.Result{Status: health.StatusOK, Detail: "disk guard not wired"}
		case !st.Available:
			return health.Result{Status: health.StatusOK, Detail: "not measurable: " + st.Err}
		case st.Shedding:
			return health.Result{
				Status: health.StatusDegraded,
				Detail: fmt.Sprintf("%.1f%% free on %s is below the %d%% floor; scheduled task claims are paused (chat unaffected)",
					st.FreePercent, st.Path, st.MinFreePercent),
			}
		default:
			return health.Result{Status: health.StatusOK, Detail: fmt.Sprintf("%.1f%% free on %s", st.FreePercent, st.Path)}
		}
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

// cachedProbe memoizes an expensive probe (a subprocess, a network call) for
// ttl: the mutex is held across the fresh call so concurrent /readyz hits
// collapse to a single execution (singleflight), and cached hits return
// instantly with Cached=true. This bounds what an unauthenticated endpoint
// can make the process do to ~1 probe per ttl regardless of request rate
// (#215).
type cachedProbe struct {
	fresh func(context.Context) health.Result
	ttl   time.Duration

	mu        sync.Mutex
	cached    health.Result
	at        time.Time
	hasCached bool
}

func (c *cachedProbe) probe(ctx context.Context) health.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasCached && time.Since(c.at) < c.ttl {
		hit := c.cached
		hit.Cached = true
		return hit
	}
	res := c.fresh(ctx)
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
