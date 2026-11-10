package httpapi

import (
	"context"
	"math"
	"net/http"
	"runtime"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/hoststats"
	"github.com/ElcanoTek/fleet/internal/store"
)

// ServerAgentStats summarizes live and 24h/cumulative agent, sub-agent, task
// worker, tool-call, LLM, sandbox, and runtime pool metrics alongside the host
// hardware snapshot in GET /api/admin/server-stats.
type ServerAgentStats struct {
	ActiveAgentsTotal      int     `json:"active_agents_total"`
	ActiveChatTurns        int     `json:"active_chat_turns"`
	RunningTasks           int     `json:"running_tasks"`
	QueuedTasks            int     `json:"queued_tasks"`
	CompletedTasksToday    int     `json:"completed_tasks_today"`
	FailedTasksToday       int     `json:"failed_tasks_today"`
	ActiveSubagents        int     `json:"active_subagents"`
	SubagentsSpawned24h    int64   `json:"subagents_spawned_24h"`
	SubagentsSpawnedTotal  int64   `json:"subagents_spawned_total"`
	ToolCalls24h           int64   `json:"tool_calls_24h"`
	ToolCallsTotal         int64   `json:"tool_calls_total"`
	ActiveConversations24h int64   `json:"active_conversations_24h"`
	LLMCallsToday          int     `json:"llm_calls_today"`
	LLMCostTodayUSD        float64 `json:"llm_cost_today_usd"`
	SandboxPoolSize        int     `json:"sandbox_pool_size"`
	SandboxPoolAvailable   int     `json:"sandbox_pool_available"`
	DBPoolInUse            int32   `json:"db_pool_in_use"`
	DBPoolIdle             int32   `json:"db_pool_idle"`
	DBPoolSize             int32   `json:"db_pool_size"`
	MCPServersEnabled      int     `json:"mcp_servers_enabled"`
	MCPServersTotal        int     `json:"mcp_servers_total"`
	Goroutines             int     `json:"goroutines"`
	ProcessMemoryMB        float64 `json:"process_memory_mb"`
	FleetVersion           string  `json:"fleet_version"`
}

type serverStatsResponse struct {
	hoststats.Snapshot
	Agents ServerAgentStats `json:"agents"`
}

// handleServerStats is the admin-only, read-only host and runtime snapshot used
// by Settings → Admin → Server and Analytics → Server & Live Agents.
// Authorization is applied at route registration; this handler deliberately
// exposes no process list, command line, environment, addresses, or filesystem
// names beyond the root mount.
func (s *Server) handleServerStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var snap hoststats.Snapshot
	if s.hostStats == nil {
		snap = hoststats.Snapshot{Warnings: []string{"host statistics collector unavailable"}}
	} else {
		snap = s.hostStats.Collect()
	}
	writeJSON(w, r, serverStatsResponse{
		Snapshot: snap,
		Agents:   s.collectServerAgentStats(r.Context()),
	})
}

func (s *Server) collectServerAgentStats(ctx context.Context) ServerAgentStats {
	chatTurns := s.ActiveTurns()
	activeSubs := agent.ActiveSubagents()
	sessionSubs := agent.SubagentsSpawnedSession()

	out := ServerAgentStats{
		ActiveChatTurns:       chatTurns,
		ActiveSubagents:       activeSubs,
		SubagentsSpawned24h:   sessionSubs,
		SubagentsSpawnedTotal: sessionSubs,
		Goroutines:            runtime.NumGoroutine(),
		FleetVersion:          s.versionOrDefault(),
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out.ProcessMemoryMB = math.Round(float64(ms.Alloc)/(1024*1024)*10) / 10

	if s.workerStats != nil {
		qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if ws, err := s.workerStats(qCtx); err == nil && ws != nil {
			out.RunningTasks = ws.RunningTasks
			out.QueuedTasks = ws.QueuedTasks
			out.CompletedTasksToday = ws.CompletedToday
			out.FailedTasksToday = ws.FailedToday
		}
		cancel()
	}

	out.ActiveAgentsTotal = out.ActiveChatTurns + out.RunningTasks + out.ActiveSubagents

	if s.store != nil {
		st := s.store.PoolStats()
		out.DBPoolSize = int32(st.OpenConnections)
		out.DBPoolInUse = int32(st.InUse)
		out.DBPoolIdle = int32(st.Idle)

		qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		sinceToday := startOfUTCDay(time.Now())
		if calls, cost, err := s.store.LLMUsageSince(qCtx, sinceToday); err == nil {
			out.LLMCallsToday = int(calls)
			out.LLMCostTodayUSD = cost
		}
		since24h := time.Now().Add(-24 * time.Hour).Unix()
		if arc, ok := s.store.(interface {
			AgentRuntimeCounts(ctx context.Context, sinceUnix int64) (store.AgentRuntimeCounts, error)
		}); ok {
			if rc, err := arc.AgentRuntimeCounts(qCtx, since24h); err == nil {
				if rc.SubagentsSpawned24h > out.SubagentsSpawned24h {
					out.SubagentsSpawned24h = rc.SubagentsSpawned24h
				}
				if rc.SubagentsSpawnedTotal > out.SubagentsSpawnedTotal {
					out.SubagentsSpawnedTotal = rc.SubagentsSpawnedTotal
				}
				out.ToolCalls24h = rc.ToolCalls24h
				out.ToolCallsTotal = rc.ToolCallsTotal
				out.ActiveConversations24h = rc.ActiveConversations24h
			}
		}
		cancel()
	}

	if s.agent != nil {
		for _, info := range s.agent.MCPServerCatalog() {
			out.MCPServersTotal++
			if info.EnabledByDefault {
				out.MCPServersEnabled++
			}
		}
		if pool := s.agent.SandboxPool(); pool != nil {
			size, avail := pool.Stats()
			out.SandboxPoolSize = size
			out.SandboxPoolAvailable = avail
		}
	}

	return out
}
