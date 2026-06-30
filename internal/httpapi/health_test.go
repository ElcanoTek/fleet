package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// healthStubStore overrides only the methods handleHealthSummary touches; the
// rest are promoted from the embedded (nil) *store.Store and never called here.
type healthStubStore struct {
	*store.Store
	calls int64
	cost  float64
	pings int
}

func (h *healthStubStore) Ping(context.Context) error { h.pings++; return nil }
func (h *healthStubStore) PoolStats() sql.DBStats {
	return sql.DBStats{OpenConnections: 3, InUse: 1, Idle: 2}
}
func (h *healthStubStore) LLMUsageSince(context.Context, int64) (int64, float64, error) {
	return h.calls, h.cost, nil
}

func TestHandleHealthSummary(t *testing.T) {
	st := &healthStubStore{calls: 10, cost: 1.5}
	s := New(&config.Config{}, &fakeEngine{}, st)
	s.startTime = time.Now().Add(-90 * time.Second)
	s.version = "test-1.2.3"
	s.workerStats = func(context.Context) (*WorkerStats, error) {
		return &WorkerStats{QueuedTasks: 4, RunningTasks: 2}, nil
	}

	rr := httptest.NewRecorder()
	s.handleHealthSummary(rr, httptest.NewRequest(http.MethodGet, "/admin/health-summary", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}

	var got healthSummary
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FleetVersion != "test-1.2.3" {
		t.Errorf("fleet_version = %q, want test-1.2.3", got.FleetVersion)
	}
	if got.UptimeSeconds < 80 || got.UptimeSeconds > 200 {
		t.Errorf("uptime_seconds = %d, want ~90", got.UptimeSeconds)
	}
	if got.DB.Chat != "healthy" || got.DB.PoolSize != 3 || got.DB.Idle != 2 {
		t.Errorf("db = %+v, want healthy/3/2", got.DB)
	}
	if got.LLM.CallsToday != 10 || got.LLM.CostTodayUSD != 1.5 {
		t.Errorf("llm = %+v, want 10/1.5", got.LLM)
	}
	if got.LLM.AvgCostPerCall < 0.149 || got.LLM.AvgCostPerCall > 0.151 {
		t.Errorf("avg_cost_per_call = %v, want 0.15", got.LLM.AvgCostPerCall)
	}
	if got.Workers == nil || got.Workers.QueuedTasks != 4 || got.Workers.RunningTasks != 2 {
		t.Errorf("workers = %+v, want 4 queued / 2 running", got.Workers)
	}
	if got.Goroutines <= 0 {
		t.Errorf("goroutines = %d, want > 0", got.Goroutines)
	}
	if st.pings != 1 {
		t.Errorf("Ping called %d times, want 1", st.pings)
	}
}

// TestHandleHealthSummary_NilWorkerStats: without a worker provider the workers
// section is null (allowed for unconfigured subsystems).
func TestHandleHealthSummary_NilWorkerStats(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, &healthStubStore{})
	rr := httptest.NewRecorder()
	s.handleHealthSummary(rr, httptest.NewRequest(http.MethodGet, "/admin/health-summary", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var got healthSummary
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Workers != nil {
		t.Errorf("workers = %+v, want null", got.Workers)
	}
	if got.FleetVersion != "fleet" {
		t.Errorf("fleet_version = %q, want default 'fleet'", got.FleetVersion)
	}
}

func TestStartOfUTCDay(t *testing.T) {
	ts := time.Date(2026, 6, 27, 15, 30, 45, 0, time.UTC)
	got := startOfUTCDay(ts)
	want := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC).Unix()
	if got != want {
		t.Errorf("startOfUTCDay = %d, want %d", got, want)
	}
}
