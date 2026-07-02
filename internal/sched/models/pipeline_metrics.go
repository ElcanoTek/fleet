package models

import "sort"

// Pipeline metrics (#543) — the sensor behind data-driven optimization
// decisions (#505's reopen criteria): how tool-heavy are real runs, measured
// from the session logs fleet ALREADY persists. Derivation is a pure view over
// stored logs (no new columns, works retroactively on every retained run).

// RunPipelineMetrics summarizes one run's tool-pipeline shape.
type RunPipelineMetrics struct {
	TaskID string `json:"task_id"`
	// ToolTurns is the number of assistant messages that carried ≥1 tool call —
	// the "sequential tool turns" #505's reopen threshold is defined over.
	ToolTurns int `json:"tool_turns"`
	// TotalToolCalls counts every tool call in the run (a turn may carry several).
	TotalToolCalls int `json:"total_tool_calls"`
	// DistinctTools is the number of unique tool names invoked.
	DistinctTools int `json:"distinct_tools"`
	// MaxCallsInOneTurn is the widest single turn (parallel tool use).
	MaxCallsInOneTurn int     `json:"max_calls_in_one_turn"`
	PromptTokens      int     `json:"prompt_tokens"`
	CompletionTokens  int     `json:"completion_tokens"`
	CostUSD           float64 `json:"cost_usd"`
	// WallClockSeconds is UpdatedAt−CreatedAt on the session — run latency as
	// the log recorded it.
	WallClockSeconds int64 `json:"wall_clock_seconds"`
	// CreatedAt orders runs for the "most recent N" listing.
	CreatedAt int64 `json:"created_at"`
}

// ComputePipelineMetrics derives one run's metrics from its stored session log.
func ComputePipelineMetrics(taskID string, ls *LogSession) RunPipelineMetrics {
	m := RunPipelineMetrics{TaskID: taskID}
	if ls == nil {
		return m
	}
	m.PromptTokens = ls.PromptTokens
	m.CompletionTokens = ls.CompletionTokens
	m.CostUSD = ls.Cost
	m.CreatedAt = ls.CreatedAt
	if ls.UpdatedAt >= ls.CreatedAt {
		m.WallClockSeconds = ls.UpdatedAt - ls.CreatedAt
	}
	distinct := map[string]struct{}{}
	for i := range ls.Messages {
		msg := &ls.Messages[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		m.ToolTurns++
		m.TotalToolCalls += len(msg.ToolCalls)
		if len(msg.ToolCalls) > m.MaxCallsInOneTurn {
			m.MaxCallsInOneTurn = len(msg.ToolCalls)
		}
		for _, tc := range msg.ToolCalls {
			distinct[tc.Name] = struct{}{}
		}
	}
	m.DistinctTools = len(distinct)
	return m
}

// PipelineMetricsAggregate is the fleet-wide rollup GET /admin/pipeline-metrics
// returns alongside the most recent per-run rows. The histogram buckets match
// how the #505 discussion frames "long multi-tool pipelines".
type PipelineMetricsAggregate struct {
	Runs int `json:"runs"`
	// ToolTurnHistogram counts runs by tool-turn band: "0", "1", "2-4", "5-9", "10+".
	ToolTurnHistogram map[string]int `json:"tool_turn_histogram"`
	// PctRunsAtLeast5ToolTurns is the headline threshold number: the share of
	// runs (0–100) that were long multi-tool pipelines.
	PctRunsAtLeast5ToolTurns float64 `json:"pct_runs_at_least_5_tool_turns"`
	AvgToolTurns             float64 `json:"avg_tool_turns"`
	AvgDistinctTools         float64 `json:"avg_distinct_tools"`
	AvgPromptTokens          float64 `json:"avg_prompt_tokens"`
	AvgCompletionTokens      float64 `json:"avg_completion_tokens"`
	AvgWallClockSeconds      float64 `json:"avg_wall_clock_seconds"`
}

// toolTurnBucket names the histogram band for a run's tool-turn count.
func toolTurnBucket(turns int) string {
	switch {
	case turns == 0:
		return "0"
	case turns == 1:
		return "1"
	case turns <= 4:
		return "2-4"
	case turns <= 9:
		return "5-9"
	default:
		return "10+"
	}
}

// AggregatePipelineMetrics rolls per-run metrics up fleet-wide and sorts runs
// most-recent-first (callers typically truncate for display).
func AggregatePipelineMetrics(runs []RunPipelineMetrics) PipelineMetricsAggregate {
	agg := PipelineMetricsAggregate{
		Runs:              len(runs),
		ToolTurnHistogram: map[string]int{"0": 0, "1": 0, "2-4": 0, "5-9": 0, "10+": 0},
	}
	if len(runs) == 0 {
		return agg
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt > runs[j].CreatedAt })
	var turns, distinct, prompt, completion, wall, longRuns int
	for _, r := range runs {
		agg.ToolTurnHistogram[toolTurnBucket(r.ToolTurns)]++
		turns += r.ToolTurns
		distinct += r.DistinctTools
		prompt += r.PromptTokens
		completion += r.CompletionTokens
		wall += int(r.WallClockSeconds)
		if r.ToolTurns >= 5 {
			longRuns++
		}
	}
	n := float64(len(runs))
	agg.PctRunsAtLeast5ToolTurns = float64(longRuns) / n * 100
	agg.AvgToolTurns = float64(turns) / n
	agg.AvgDistinctTools = float64(distinct) / n
	agg.AvgPromptTokens = float64(prompt) / n
	agg.AvgCompletionTokens = float64(completion) / n
	agg.AvgWallClockSeconds = float64(wall) / n
	return agg
}
