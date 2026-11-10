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
	calls  int64
	cost   float64
	pings  int
	counts store.AgentRuntimeCounts
}

func (h *healthStubStore) Ping(context.Context) error { h.pings++; return nil }
func (h *healthStubStore) PoolStats() sql.DBStats {
	return sql.DBStats{OpenConnections: 3, InUse: 1, Idle: 2}
}
func (h *healthStubStore) LLMUsageSince(context.Context, int64) (int64, float64, error) {
	return h.calls, h.cost, nil
}
func (h *healthStubStore) AgentRuntimeCounts(context.Context, int64) (store.AgentRuntimeCounts, error) {
	return h.counts, nil
}

func TestHandleHealthSummary(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	ts := time.Date(2026, 6, 27, 15, 30, 45, 0, time.UTC)
	got := startOfUTCDay(ts)
	want := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC).Unix()
	if got != want {
		t.Errorf("startOfUTCDay = %d, want %d", got, want)
	}
}

func TestHandleServerStatsIncludesAgentMetrics(t *testing.T) {
	t.Parallel()

	st := &healthStubStore{
		calls: 14,
		cost:  2.75,
		counts: store.AgentRuntimeCounts{
			SubagentsSpawnedTotal:  42,
			SubagentsSpawned24h:    9,
			ToolCalls24h:           180,
			ToolCallsTotal:         1250,
			ActiveConversations24h: 11,
		},
	}
	s := New(&config.Config{}, &fakeEngine{}, st)
	s.version = "v2026.11.10.6"
	s.workerStats = func(context.Context) (*WorkerStats, error) {
		return &WorkerStats{
			QueuedTasks:    3,
			RunningTasks:   2,
			CompletedToday: 18,
			FailedToday:    1,
		}, nil
	}

	rr := httptest.NewRecorder()
	s.handleServerStats(rr, httptest.NewRequest(http.MethodGet, "/admin/server-stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}

	var got serverStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agents.RunningTasks != 2 || got.Agents.QueuedTasks != 3 || got.Agents.CompletedTasksToday != 18 {
		t.Fatalf("unexpected task stats: %+v", got.Agents)
	}
	if got.Agents.ActiveAgentsTotal < 2 {
		t.Fatalf("expected ActiveAgentsTotal >= 2, got %d", got.Agents.ActiveAgentsTotal)
	}
	if got.Agents.SubagentsSpawned24h != 9 || got.Agents.SubagentsSpawnedTotal != 42 {
		t.Fatalf("unexpected subagent counts: %+v", got.Agents)
	}
	if got.Agents.ToolCalls24h != 180 || got.Agents.ToolCallsTotal != 1250 || got.Agents.ActiveConversations24h != 11 {
		t.Fatalf("unexpected tool/conv counts: %+v", got.Agents)
	}
	if got.Agents.LLMCallsToday != 14 || got.Agents.LLMCostTodayUSD != 2.75 {
		t.Fatalf("unexpected llm stats: %+v", got.Agents)
	}
	if got.Agents.DBPoolSize != 3 || got.Agents.DBPoolInUse != 1 || got.Agents.DBPoolIdle != 2 {
		t.Fatalf("unexpected db pool stats: %+v", got.Agents)
	}
}
