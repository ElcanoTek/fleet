package metrics

// Named metric families (#176) + thin record helpers. Kept separate from the
// registry mechanics so call sites read intent, not plumbing.

const (
	nameHTTPRequests = "fleet_http_requests_total"
	nameHTTPDuration = "fleet_http_request_duration_seconds"
	nameActiveAgents = "fleet_active_agents"
	nameCostUSD      = "fleet_cost_usd_total"
	//nolint:gosec // G101: this is a Prometheus metric NAME, not a credential — "token" refers to LLM tokens.
	nameTokenUsage   = "fleet_token_usage_total"
	nameSandboxPool  = "fleet_sandbox_pool_size"
	nameTurnTimeouts = "fleet_turn_timeouts_total"
	nameRunsPruned   = "fleet_sched_runs_pruned_total"
	nameIPBlocked    = "fleet_ip_blocked_total"

	// Per-task sandbox resource telemetry (#263). These are last-write-wins
	// gauges reflecting the most recently FINISHED sandbox run, deliberately
	// WITHOUT a task_id label: a task_id label would grow the time-series set
	// without bound (one new series per run, forever), the cardinality
	// anti-pattern Prometheus warns against. Per-task attribution belongs in
	// the structured task log, not the metrics stream; the gauge answers
	// "how hard is the sandbox being pushed lately" for alerting/right-sizing.
	nameSandboxCPUUsage     = "fleet_sandbox_cpu_usage_percent"
	nameSandboxMemUsage     = "fleet_sandbox_memory_usage_bytes"
	nameSandboxMemLimit     = "fleet_sandbox_memory_limit_bytes"
	nameSandboxIOBytes      = "fleet_sandbox_io_bytes"
	nameSandboxPidsPeak     = "fleet_sandbox_pids_peak"
	nameSandboxRunsObserved = "fleet_sandbox_runs_observed_total"
)

// RecordIPBlocked counts one request dropped by the IP access-control middleware
// (#314), labeled by the matching list: "allowlist" (no allowlist match) or
// "denylist" (explicitly blocked).
func RecordIPBlocked(reason string) {
	incCounter(nameIPBlocked, "Requests blocked by the IP access-control filter, by reason.",
		[]string{"reason"}, []string{reason}, 1)
}

// RecordRunsPruned counts task runs deleted by the automatic retention sweep (#252).
func RecordRunsPruned(n int) {
	if n <= 0 {
		return
	}
	incCounter(nameRunsPruned, "Total scheduled task runs deleted by the retention sweep.", nil, nil, float64(n))
}

// RecordHTTPRequest records one served request: a count by route/method/status
// and its latency in the duration histogram.
func RecordHTTPRequest(route, method, status string, durationSeconds float64) {
	incCounter(nameHTTPRequests, "Total HTTP requests by route, method, and status.",
		[]string{"route", "method", "status"}, []string{route, method, status}, 1)
	observeHistogram(nameHTTPDuration, "HTTP request latency in seconds.",
		[]string{"route", "method"}, []string{route, method}, durationSeconds)
}

// RecordTurnUsage records an agent turn's cost + token burn, labeled by model.
func RecordTurnUsage(model string, costUSD float64, promptTokens, completionTokens, cachedTokens int) {
	if model == "" {
		model = "unknown"
	}
	incCounter(nameCostUSD, "Cumulative LLM cost in USD by model.",
		[]string{"model"}, []string{model}, costUSD)
	tokens := []string{"model", "type"}
	incCounter(nameTokenUsage, "Cumulative token counts by model and type.", tokens, []string{model, "prompt"}, float64(promptTokens))
	incCounter(nameTokenUsage, "Cumulative token counts by model and type.", tokens, []string{model, "completion"}, float64(completionTokens))
	incCounter(nameTokenUsage, "Cumulative token counts by model and type.", tokens, []string{model, "cached"}, float64(cachedTokens))
}

// RecordTurnTimeout counts a turn that ended because its wall-clock deadline
// fired. kind is "interactive" or "scheduled".
func RecordTurnTimeout(kind string) {
	incCounter(nameTurnTimeouts, "Turn timeout events by kind.", []string{"kind"}, []string{kind}, 1)
}

// RegisterActiveAgents wires the pull-at-scrape gauge for in-flight turns.
// interactive/scheduled are evaluated each scrape so the value is always live.
func RegisterActiveAgents(interactive, scheduled func() int) {
	RegisterGauge(nameActiveAgents, "Currently running agent turns by kind.", []string{"kind"}, func() []GaugeSample {
		return []GaugeSample{
			{Labels: []string{"interactive"}, Value: float64(interactive())},
			{Labels: []string{"scheduled"}, Value: float64(scheduled())},
		}
	})
}

// RegisterSandboxPoolSize wires the pull-at-scrape gauge for warm sandbox depth.
func RegisterSandboxPoolSize(size func() int) {
	RegisterGauge(nameSandboxPool, "Warm sandbox containers currently parked in the pool.", nil, func() []GaugeSample {
		return []GaugeSample{{Value: float64(size())}}
	})
}

// RecordSandboxResourceUsage publishes one finished sandbox run's resource
// telemetry (#263): peak CPU %, peak resident memory, the configured memory
// limit, peak PID count, and cumulative block I/O. The gauges are last-write-
// wins (no task_id label — see the const block for the cardinality rationale)
// and io is split read/write via a `direction` label. netReported gates the
// network I/O series so NoNetwork runs don't publish misleading zeros.
//
// This is observability sampled read-only from `podman stats`; it never alters
// sandbox isolation or limits.
func RecordSandboxResourceUsage(cpuPeak float64, memPeakBytes, memLimitBytes, blockInBytes, blockOutBytes, pidsPeak uint64, netReported bool, netInBytes, netOutBytes uint64) {
	setGauge(nameSandboxCPUUsage, "Peak sandbox CPU percent for the most recently finished task run.", nil, nil, cpuPeak)
	setGauge(nameSandboxMemUsage, "Peak sandbox resident memory in bytes for the most recently finished task run.", nil, nil, float64(memPeakBytes))
	if memLimitBytes > 0 {
		setGauge(nameSandboxMemLimit, "Configured sandbox memory limit in bytes (from the most recently finished task run).", nil, nil, float64(memLimitBytes))
	}
	setGauge(nameSandboxPidsPeak, "Peak sandbox PID count for the most recently finished task run.", nil, nil, float64(pidsPeak))
	ioLabels := []string{"direction"}
	setGauge(nameSandboxIOBytes, "Cumulative sandbox block I/O bytes for the most recently finished task run, by direction.", ioLabels, []string{"read"}, float64(blockInBytes))
	setGauge(nameSandboxIOBytes, "Cumulative sandbox block I/O bytes for the most recently finished task run, by direction.", ioLabels, []string{"write"}, float64(blockOutBytes))
	if netReported {
		setGauge(nameSandboxIOBytes, "Cumulative sandbox block I/O bytes for the most recently finished task run, by direction.", ioLabels, []string{"net_in"}, float64(netInBytes))
		setGauge(nameSandboxIOBytes, "Cumulative sandbox block I/O bytes for the most recently finished task run, by direction.", ioLabels, []string{"net_out"}, float64(netOutBytes))
	}
	incCounter(nameSandboxRunsObserved, "Total sandbox runs for which resource telemetry was collected.", nil, nil, 1)
}
