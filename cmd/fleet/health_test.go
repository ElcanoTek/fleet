package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/health"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestPingProbe(t *testing.T) {
	if got := pingProbe(fakePinger{})(context.Background()); got.Status != health.StatusOK {
		t.Errorf("healthy ping: got %+v", got)
	}
	got := pingProbe(fakePinger{err: errors.New("down")})(context.Background())
	if got.Status != health.StatusError || got.Detail != "down" {
		t.Errorf("failed ping: got %+v", got)
	}
}

func TestCachedSandboxProbe_MissingBinaryAndCaches(t *testing.T) {
	p := &cachedSandboxProbe{runtimeBin: "fleet-nonexistent-runtime-xyz", ttl: time.Minute}

	first := p.probe(context.Background())
	if first.Status != health.StatusError || first.Cached {
		t.Fatalf("first call: want fresh error (Cached=false), got %+v", first)
	}
	// Second call within TTL must be served from cache (no re-exec), flagged.
	second := p.probe(context.Background())
	if second.Status != health.StatusError || !second.Cached {
		t.Errorf("second call within TTL should be cached, got %+v", second)
	}
}

func TestWithHealthProbes(t *testing.T) {
	delegated := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delegated = true
		w.WriteHeader(http.StatusTeapot)
	})
	start := time.Now().Add(-30 * time.Second)
	checks := []health.Check{
		{Name: "chat_db", Critical: true, Probe: pingProbe(fakePinger{})},
	}
	notDraining := func() bool { return false }
	h := withHealthProbes(next, start, notDraining, checks)

	// /livez → 200 with uptime, no delegation.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/livez code: %d", rec.Code)
	}
	var live health.LiveResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &live)
	if live.Status != "ok" || live.UptimeSeconds < 29 {
		t.Errorf("/livez body: %+v", live)
	}

	// /readyz → 200 when the critical check passes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz healthy code: %d body=%s", rec.Code, rec.Body.String())
	}

	// /readyz → 503 when the critical check fails.
	hDown := withHealthProbes(next, start, notDraining, []health.Check{
		{Name: "chat_db", Critical: true, Probe: pingProbe(fakePinger{err: errors.New("x")})},
	})
	rec = httptest.NewRecorder()
	hDown.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with down critical check: want 503, got %d", rec.Code)
	}

	// While draining, /readyz is 503 even though all subsystem checks pass, but
	// /livez stays 200 (process alive; don't restart it).
	hDraining := withHealthProbes(next, start, func() bool { return true }, checks)
	rec = httptest.NewRecorder()
	hDraining.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz while draining: want 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	hDraining.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/livez while draining: want 200 (still alive), got %d", rec.Code)
	}

	// Unrelated path delegates to next.
	if delegated {
		t.Fatal("delegation happened during health requests")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conversations", nil))
	if !delegated || rec.Code != http.StatusTeapot {
		t.Errorf("non-health path should delegate to next (code=%d delegated=%v)", rec.Code, delegated)
	}

	// A non-GET /livez delegates (probes are GET-only).
	delegated = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/livez", nil))
	if !delegated {
		t.Error("POST /livez should delegate, not serve the probe")
	}
}
