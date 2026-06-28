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
