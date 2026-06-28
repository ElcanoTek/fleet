package httpapi

import (
	"context"
	"net/http"
	"runtime"
	"time"
)

// WorkerStats is the scheduler worker/task snapshot the health summary embeds.
// It is populated by the WithWorkerStats provider (wired from the sched store in
// cmd/fleet) so this package stays sched-agnostic. nil → reported as null.
type WorkerStats struct {
	TotalNodes     int `json:"total"`
	ActiveNodes    int `json:"active"`
	IdleNodes      int `json:"idle"`
	QueuedTasks    int `json:"queued_tasks"`
	RunningTasks   int `json:"running_tasks"`
	CompletedToday int `json:"completed_today"`
	FailedToday    int `json:"failed_today"`
}

type healthDB struct {
	Chat     string `json:"chat"` // "healthy" | "unreachable"
	PoolSize int    `json:"pool_size"`
	InUse    int    `json:"in_use"`
	Idle     int    `json:"idle"`
}

type healthLLM struct {
	CallsToday     int64   `json:"calls_today"`
	CostTodayUSD   float64 `json:"cost_today_usd"`
	AvgCostPerCall float64 `json:"avg_cost_per_call"`
}

type healthMCP struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type healthSandbox struct {
	Size      int `json:"size"`
	Available int `json:"available"`
}

// healthIPFilter surfaces the active IP access-control state (#314) so an
// operator can confirm the filter from the admin dashboard without reading logs.
// CIDR/IP entries are rendered as strings; Enabled mirrors the middleware's own
// "any list set" activation rule.
type healthIPFilter struct {
	Enabled        bool     `json:"enabled"`
	Allowlist      []string `json:"allowlist"`
	Denylist       []string `json:"denylist"`
	TrustedProxies []string `json:"trusted_proxies"`
}

type healthSummary struct {
	FleetVersion        string         `json:"fleet_version"`
	UptimeSeconds       int64          `json:"uptime_seconds"`
	DB                  healthDB       `json:"db"`
	Workers             *WorkerStats   `json:"workers"`
	LLM                 healthLLM      `json:"llm"`
	MCPServers          []healthMCP    `json:"mcp_servers"`
	ConversationsActive int            `json:"conversations_active"`
	Sandbox             *healthSandbox `json:"sandbox_pool"`
	IPFilter            healthIPFilter `json:"ip_filter"`
	MemoryMB            uint64         `json:"memory_mb"`
	Goroutines          int            `json:"goroutines"`
}

// handleHealthSummary aggregates a single-pane system-health view (#301):
// runtime, DB pool, today's LLM spend, MCP catalog, active conversations,
// sandbox pool, and (when wired) scheduler worker/task counts. Admin-gated by
// the same adminMiddleware as /admin/stats.
func (s *Server) handleHealthSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	out := healthSummary{
		FleetVersion:        s.versionOrDefault(),
		ConversationsActive: s.ActiveTurns(),
		Goroutines:          runtime.NumGoroutine(),
		MCPServers:          []healthMCP{},
	}
	if !s.startTime.IsZero() {
		out.UptimeSeconds = int64(time.Since(s.startTime).Seconds())
	}

	// IP access-control state (#314): mirror the middleware's activation rule
	// (any list set ⇒ enabled) and render the configured entries for the dashboard.
	if s.cfg != nil {
		out.IPFilter = healthIPFilter{
			Enabled:        len(s.cfg.IPAllowlist) > 0 || len(s.cfg.IPDenylist) > 0,
			Allowlist:      cidrStrings(s.cfg.IPAllowlist),
			Denylist:       cidrStrings(s.cfg.IPDenylist),
			TrustedProxies: ipStrings(s.cfg.TrustedProxies),
		}
	}

	// DB liveness + pool snapshot.
	out.DB.Chat = "healthy"
	if s.store != nil {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := s.store.Ping(pingCtx); err != nil {
			out.DB.Chat = "unreachable"
		}
		cancel()
		st := s.store.PoolStats()
		out.DB.PoolSize = st.OpenConnections
		out.DB.InUse = st.InUse
		out.DB.Idle = st.Idle

		// Today's LLM spend (UTC midnight cutoff).
		since := startOfUTCDay(time.Now())
		if calls, cost, err := s.store.LLMUsageSince(r.Context(), since); err == nil {
			out.LLM.CallsToday = calls
			out.LLM.CostTodayUSD = cost
			if calls > 0 {
				out.LLM.AvgCostPerCall = cost / float64(calls)
			}
		}
	}

	// MCP catalog (names + enabled-by-default; we don't ping servers).
	if s.agent != nil {
		for _, info := range s.agent.MCPServerCatalog() {
			out.MCPServers = append(out.MCPServers, healthMCP{Name: info.Name, Enabled: info.EnabledByDefault})
		}
		if pool := s.agent.SandboxPool(); pool != nil {
			size, avail := pool.Stats()
			out.Sandbox = &healthSandbox{Size: size, Available: avail}
		}
	}

	// Memory (heap in use).
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out.MemoryMB = ms.Alloc / (1024 * 1024)

	// Scheduler worker/task counts (optional provider).
	if s.workerStats != nil {
		if ws, err := s.workerStats(r.Context()); err == nil {
			out.Workers = ws
		}
	}

	writeJSON(w, out)
}

func (s *Server) versionOrDefault() string {
	if s.version != "" {
		return s.version
	}
	return "fleet"
}

// startOfUTCDay returns the unix-seconds timestamp of 00:00 UTC for t's date —
// the cutoff for "today" in the LLM-spend rollup.
func startOfUTCDay(t time.Time) int64 {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}
