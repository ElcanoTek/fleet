package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/health"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// circuitEngine is the chat server's engine with a real provider-health
// registry behind ProviderHealth. Only ProviderHealth is reached by the
// health endpoints; the embedded nil Manager supplies the rest of the method
// set and is never called here.
type circuitEngine struct {
	*agent.Manager
	reg *agentcore.ProviderHealthRegistry
}

func (e circuitEngine) ProviderHealth() []agentcore.ModelHealth { return e.reg.Snapshot() }

// openCircuitRegistry returns a registry whose circuit for slug is open: five
// provider errors inside the window, as a provider incident or a delisted
// model that a recurring task keeps calling produces.
func openCircuitRegistry(t *testing.T, slug string) *agentcore.ProviderHealthRegistry {
	t.Helper()
	reg := agentcore.NewProviderHealthRegistry()
	for range 5 {
		reg.RecordError(context.Background(), slug, "404 model not found")
	}
	if got := reg.State(slug); got != agentcore.CircuitOpen {
		t.Fatalf("five errors left the circuit %s, want open", got)
	}
	return reg
}

// probeState is what the health endpoints see.
type probeState struct {
	circuitOpen bool  // one model's circuit breaker is open
	dbErr       error // both database pings fail with this
	sandboxErr  error // the sandbox check (non-critical) fails with this
	draining    bool  // graceful shutdown has begun
}

// probeListener is the chat listener (FLEET_SERVER_ADDR) the way run()
// assembles it, in the given state.
type probeListener struct {
	srv *httpapi.Server
	h   http.Handler
}

func newProbeListener(t *testing.T, st probeState) probeListener {
	t.Helper()
	reg := agentcore.NewProviderHealthRegistry()
	if st.circuitOpen {
		reg = openCircuitRegistry(t, "vendor/delisted-model")
	}
	cfg := &config.Config{}
	srv := httpapi.New(cfg, circuitEngine{reg: reg}, nil)
	checks := []health.Check{
		{Name: "chat_db", Critical: true, Probe: pingProbe(fakePinger{err: st.dbErr})},
		{Name: "sched_db", Critical: true, Probe: pingProbe(fakePinger{err: st.dbErr})},
		{Name: "sandbox", Critical: false, Probe: func(context.Context) health.Result {
			if st.sandboxErr != nil {
				return health.Result{Status: health.StatusError, Detail: st.sandboxErr.Error()}
			}
			return health.Result{Status: health.StatusOK}
		}},
	}
	if st.draining {
		srv.BeginShutdown()
	}
	return probeListener{srv: srv, h: chatListenerHandler(srv, srv.Routes(), time.Now(), checks, tlsActive(cfg))}
}

func (l probeListener) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	l.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// One model's open circuit is a provider problem, not a box problem: runs
// fall back to another model and chat keeps working. /healthz reports it (a
// 503 naming the model, for monitors), while /livez and /readyz, the
// endpoints the probes use, stay 200.
func TestHealthEndpointsWithAnOpenModelCircuit(t *testing.T) {
	t.Parallel()

	l := newProbeListener(t, probeState{circuitOpen: true})

	rec := l.get(t, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz with an open circuit: code %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz body %q: %v", rec.Body.String(), err)
	}
	if body["status"] != "degraded" || body["model"] != "vendor/delisted-model" {
		t.Errorf("/healthz body = %v, want status degraded naming the model", body)
	}
	for _, path := range []string{"/livez", "/readyz"} {
		if rec := l.get(t, path); rec.Code != http.StatusOK {
			t.Errorf("%s with an open circuit: code %d, want 200; body %s", path, rec.Code, rec.Body.String())
		}
	}
}

// With the database down the box cannot serve: /readyz says so. /livez
// stays 200, because restarting the process does not bring a database back.
func TestHealthEndpointsWithTheDatabaseDown(t *testing.T) {
	t.Parallel()

	l := newProbeListener(t, probeState{dbErr: errors.New("connection refused")})

	if rec := l.get(t, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with the database down: code %d, want 503", rec.Code)
	}
	if rec := l.get(t, "/livez"); rec.Code != http.StatusOK {
		t.Errorf("/livez with the database down: code %d, want 200", rec.Code)
	}
}

// A ping through a store's pool waits for a free connection, so a pool kept
// busy by real work timed the database checks out while the database
// answered. With a real sched store whose pool is saturated, a ping through
// that store fails; the checks run() builds, on connections of their own,
// pass. (buildReadinessChecks takes readinessPinger, so a store cannot be
// handed to it.)
func TestReadinessDBChecksDoNotQueueBehindABusyPool(t *testing.T) {
	t.Parallel()

	fdb := fleetdb.New(t)
	cfg := &config.Config{DatabaseURL: fdb.ChatDSN(), SchedDatabaseURL: fdb.SchedDSN()}
	// The sched store, opened as run() opens it but with a one-connection
	// pool, and that connection checked out: the pool is saturated.
	schedStore := scheddb.New()
	pool := scheddb.DefaultPoolConfig()
	pool.MaxOpenConns, pool.MaxIdleConns = 1, 1
	if err := schedStore.Init(cfg.SchedDatabaseURL, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = schedStore.Close() })
	held, err := schedStore.Conn().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	// A ping through the store's pool, as the check once was, waits for the
	// held connection and fails.
	short, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if got := pingProbe(schedStore)(short); got.Status != health.StatusError {
		t.Fatalf("a ping through the saturated sched store = %+v; want it to wait and fail", got)
	}

	// The checks run() builds are on connections of their own: both pass.
	checks, closeChecks, err := openReadinessChecks(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeChecks)
	resp, _ := health.RunReadiness(t.Context(), checks)
	for _, name := range []string{"chat_db", "sched_db"} {
		if got := resp.Checks[name]; got.Status != health.StatusOK {
			t.Errorf("%s with the sched store's pool saturated = %+v, want ok", name, got)
		}
	}
}

// A database that does not answer still fails the checks: nothing listening
// (refused at once), or a listener that accepts and never replies (the check
// gives up at health.PerCheckTimeout).
func TestReadinessDBChecksFailWhenTheDatabaseDoesNotAnswer(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // held open, never answered
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	for _, c := range []struct{ name, dsn string }{
		{"nothing listening", "postgres://fleet@127.0.0.1:1/fleet?sslmode=disable"},
		{"a listener that never answers", "postgres://fleet@" + ln.Addr().String() + "/fleet?sslmode=disable"},
	} {
		checks, closeChecks, err := openReadinessChecks(&config.Config{DatabaseURL: c.dsn, SchedDatabaseURL: c.dsn}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		resp, code := health.RunReadiness(t.Context(), checks)
		elapsed := time.Since(start)
		closeChecks()
		if code != http.StatusServiceUnavailable || resp.Checks["chat_db"].Status != health.StatusError || resp.Checks["sched_db"].Status != health.StatusError {
			t.Errorf("%s: /readyz %d %+v, want 503 with both database checks failing", c.name, code, resp.Checks)
		}
		if limit := health.PerCheckTimeout + 2*time.Second; elapsed > limit {
			t.Errorf("%s: /readyz took %s, want the per-check timeout (%s) to bound it", c.name, elapsed, health.PerCheckTimeout)
		}
	}
}

// Each readiness connection is replaced on its store pool's schedule, so it
// authenticates again as often as the store's connections do.
func TestReadinessConnectionsFollowTheirStorePoolLifetime(t *testing.T) {
	t.Parallel()

	dsn := testdb.Empty(t)
	cfg := &config.Config{DatabaseURL: dsn, SchedDatabaseURL: dsn}
	cfg.ChatDBPool.ConnMaxLifetime = 200 * time.Millisecond
	cfg.SchedDBPool.ConnMaxLifetime = time.Hour
	chat, sched, err := openReadinessPingers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chat.Close(); _ = sched.Close() })
	ping := func() {
		t.Helper()
		for _, p := range []readinessPinger{chat, sched} {
			if err := p.Ping(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	ping()
	time.Sleep(400 * time.Millisecond)
	ping()
	if n := chat.db.Stats().MaxLifetimeClosed; n < 1 {
		t.Errorf("chat readiness connection closed for its lifetime %d times, want at least 1 (chat pool lifetime 200ms)", n)
	}
	if n := sched.db.Stats().MaxLifetimeClosed; n != 0 {
		t.Errorf("sched readiness connection closed for its lifetime %d times, want 0 (sched pool lifetime 1h)", n)
	}
}

// chartProbe is the slice of a Kubernetes probe this test reads.
type chartProbe struct {
	HTTPGet *struct {
		Path   string `yaml:"path"`
		Port   any    `yaml:"port"`
		Scheme string `yaml:"scheme"`
	} `yaml:"httpGet"`
	TimeoutSeconds   int `yaml:"timeoutSeconds"`
	PeriodSeconds    int `yaml:"periodSeconds"`
	FailureThreshold int `yaml:"failureThreshold"`
}

type chartObject struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name           string      `yaml:"name"`
					StartupProbe   *chartProbe `yaml:"startupProbe"`
					ReadinessProbe *chartProbe `yaml:"readinessProbe"`
					LivenessProbe  *chartProbe `yaml:"livenessProbe"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// helmTemplate renders the chart with the CI image values plus args. Without
// helm it skips locally, but fails under CI (GITHUB_ACTIONS), where the
// runner image ships helm and a skip would leave the probes unchecked.
func helmTemplate(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("helm is not on PATH; under CI this test must run, not skip")
		}
		t.Skip("helm not on PATH")
	}
	argv := make([]string, 0, 11+len(args))
	argv = append(argv, "template", "ci", filepath.Join("..", "..", "deploy", "helm", "fleet"),
		"--namespace", "fleet",
		"--set", "image.repository=registry.invalid/fleet", "--set", "image.tag=ci",
		"--set", "sandbox.image=registry.invalid/fleet-sandbox:ci")
	argv = append(argv, args...)
	return exec.Command("helm", argv...).CombinedOutput()
}

// renderedFleetProbes renders the chart with helm and returns the control
// plane container's startup, readiness and liveness probes.
func renderedFleetProbes(t *testing.T) (startup, readiness, liveness *chartProbe) {
	t.Helper()
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(out), -1) {
		var obj chartObject
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("rendered document does not parse: %v\n%s", err, doc)
		}
		if obj.Kind != "Deployment" {
			continue
		}
		for _, c := range obj.Spec.Template.Spec.Containers {
			if c.Name == "fleet" {
				return c.StartupProbe, c.ReadinessProbe, c.LivenessProbe
			}
		}
	}
	t.Fatal("the rendered chart has no Deployment with a fleet container")
	return nil, nil, nil
}

// probePasses is the kubelet's verdict on an HTTP probe answer.
func probePasses(code int) bool { return code >= 200 && code < 400 }

// TestChartProbesMatchTheirMeaning sends each probe the chart renders to the
// chat listener and checks that it fails only for what it is meant to act on.
// The chart runs ONE replica: a failing readiness probe takes the whole UI and
// API offline, and a failing liveness probe restarts the pod, killing every
// in-flight turn and task. So:
//
//   - a model's open circuit (a provider incident, a delisted model) must fail
//     no probe: runs fall back and chat keeps working;
//   - a non-critical check failing (sandbox, disk) makes /readyz answer 207,
//     which must fail no probe: the box still serves;
//   - the database down fails readiness only: a restart cannot fix it;
//   - a draining pod fails readiness only: killing it would cut the drain short;
//   - startup passes as soon as the listener answers, which is the last boot step.
func TestChartProbesMatchTheirMeaning(t *testing.T) {
	t.Parallel()

	startup, readiness, liveness := renderedFleetProbes(t)
	probes := map[string]*chartProbe{"startupProbe": startup, "readinessProbe": readiness, "livenessProbe": liveness}
	for name, p := range probes {
		if p == nil || p.HTTPGet == nil {
			t.Fatalf("%s: missing, or not an httpGet probe", name)
		}
		// The scenarios below run against the chat listener; a probe moved to
		// another port needs that listener built here instead.
		if p.HTTPGet.Port != "chat" || (p.HTTPGet.Scheme != "" && p.HTTPGet.Scheme != "HTTP") {
			t.Fatalf("%s targets port %v scheme %q; this test models the chat listener over HTTP", name, p.HTTPGet.Port, p.HTTPGet.Scheme)
		}
	}

	allPass := map[string]bool{"startupProbe": true, "readinessProbe": true, "livenessProbe": true}
	scenarios := []struct {
		name  string
		st    probeState
		wants map[string]bool // probe → should pass
	}{
		{"healthy", probeState{}, allPass},
		{"a model's circuit is open", probeState{circuitOpen: true}, allPass},
		{"the sandbox check fails", probeState{sandboxErr: errors.New("podman: executable file not found")}, allPass},
		{"the database is down", probeState{dbErr: errors.New("connection refused")},
			map[string]bool{"startupProbe": true, "readinessProbe": false, "livenessProbe": true}},
		{"draining", probeState{draining: true},
			map[string]bool{"readinessProbe": false, "livenessProbe": true}},
	}
	for _, sc := range scenarios {
		l := newProbeListener(t, sc.st)
		for name, want := range sc.wants {
			path := probes[name].HTTPGet.Path
			rec := l.get(t, path)
			if got := probePasses(rec.Code); got != want {
				t.Errorf("%s: %s (GET %s) answered %d, so the probe would %s; want it to %s",
					sc.name, name, path, rec.Code, verdict(got), verdict(want))
			}
		}
	}
	// The non-critical scenario passes readiness through a 207, not a 200.
	l := newProbeListener(t, probeState{sandboxErr: errors.New("podman: executable file not found")})
	if rec := l.get(t, readiness.HTTPGet.Path); rec.Code != http.StatusMultiStatus {
		t.Errorf("readiness with only the sandbox check failing answered %d, want 207", rec.Code)
	}

	// /readyz runs its checks in parallel, each bounded by
	// health.PerCheckTimeout, so it can take that long to answer 207 for a slow
	// non-critical check (the kubernetes sandbox probe calls the apiserver).
	// A readiness timeout at or below that (the kubelet default is 1s) fails
	// the probe on a slow apiserver and takes the pod out of service.
	if limit := int(health.PerCheckTimeout / time.Second); readiness.TimeoutSeconds <= limit {
		t.Errorf("readinessProbe.timeoutSeconds = %d, want more than /readyz's %ds per-check timeout", readiness.TimeoutSeconds, limit)
	}
	// One replica: readiness rides out a minute of failed database pings (a
	// failover, a network blip) before the pod leaves the Service.
	if tolerance := time.Duration(readiness.PeriodSeconds*readiness.FailureThreshold) * time.Second; tolerance < time.Minute {
		t.Errorf("readinessProbe tolerates %s of failures, want at least 1m", tolerance)
	}
	// Boot runs migrations before the listener binds; the default startup
	// budget must leave room for a large database's.
	if budget := time.Duration(startup.PeriodSeconds*startup.FailureThreshold) * time.Second; budget < 10*time.Minute {
		t.Errorf("startupProbe budget = %s, want at least 10m for first-boot migrations", budget)
	}
}

// The probes speak plain HTTP to port 8080. Settings in config.env that move
// the chat listener off that, or make it HTTPS, would fail every probe and
// restart the pod in a loop, so the chart refuses to render them; so is a TLS
// mode fleet would refuse at boot. The chart's own address (port 8080 on every
// interface) still renders, so an upgrade of a release that set it keeps
// working. Each value is compared as fleet reads that key: FLEET_TLS_MODE
// with one pair of quotes stripped (only when they are the first and last
// characters), then trimmed and lowercased; FLEET_SERVER_ADDR with one such
// pair stripped and nothing else.
func TestChartRefusesSettingsTheProbesCannotFollow(t *testing.T) {
	t.Parallel()

	const (
		https   = "HTTPS chat listener"
		invalid = "fleet accepts only off, manual or auto"
		addr    = "config.env.FLEET_SERVER_ADDR="
	)
	for _, c := range []struct {
		set    string // a --set-string argument (or --set, see below)
		refuse string // text the refusal carries; "" = renders
		why    string
	}{
		{"config.env.FLEET_TLS_MODE=manual", https, "HTTPS on the chat port"},
		{"config.env.FLEET_TLS_MODE=auto", https, "HTTPS on the chat port"},
		{`config.env.FLEET_TLS_MODE="auto"`, https, "fleet strips the quotes: auto"},
		{"config.env.FLEET_TLS_MODE= Manual ", https, "fleet trims and lowercases: manual"},
		{"config.env.FLEET_TLS_MODE= 'Manual' ", invalid, "the quotes are not the first and last characters, so fleet keeps them and reads 'manual', which it refuses at boot"},
		{"config.env.FLEET_TLS_MODE=false", invalid, "an unquoted off in a values file renders false, which fleet refuses at boot"},
		{"config.env.FLEET_TLS_MODE=off", "", "plain HTTP"},
		{`config.env.FLEET_TLS_MODE="off"`, "", "plain HTTP"},
		{"config.env.FLEET_SERVER_ADDR=0.0.0.0:8080", "", "the chart's own address"},
		{"config.env.FLEET_SERVER_ADDR=:8080", "", "port 8080 on every interface"},
		{"config.env.FLEET_SERVER_ADDR=[::]:8080", "", "port 8080 on every interface"},
		{`config.env.FLEET_SERVER_ADDR="0.0.0.0:8080"`, "", "fleet strips the quotes"},
		{"config.env.FLEET_SERVER_ADDR= '0.0.0.0:8080' ", addr, "fleet keeps quotes that are not the first and last characters, and the spaces: it cannot bind that"},
		{"config.env.FLEET_SERVER_ADDR= 0.0.0.0:8080", addr, "fleet does not trim the address: it cannot bind that"},
		{"config.env.FLEET_SERVER_ADDR=", addr, "no address"},
		{"config.env.FLEET_SERVER_ADDR=0.0.0.0:9090", addr, "another port"},
		{"config.env.FLEET_SERVER_ADDR=127.0.0.1:8080", addr, "loopback only: the kubelet probes the pod address"},
	} {
		out, err := helmTemplate(t, "--set-string", c.set)
		switch {
		case c.refuse == "" && err != nil:
			t.Errorf("--set-string %s (%s): refused (%v), want it to render\n%s", c.set, c.why, err, out)
		case c.refuse != "" && (err == nil || !strings.Contains(string(out), c.refuse)):
			t.Errorf("--set-string %s (%s): rendered (err %v), want a refusal carrying %q\n%s", c.set, c.why, err, c.refuse, out)
		}
	}
	// A values file's unquoted off is a YAML boolean; --set types it the same way.
	if out, err := helmTemplate(t, "--set", "config.env.FLEET_TLS_MODE=false"); err == nil || !strings.Contains(string(out), invalid) {
		t.Errorf("FLEET_TLS_MODE as the boolean false: rendered (err %v), want the refusal with the quoting hint\n%s", err, out)
	}
}

func verdict(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}
