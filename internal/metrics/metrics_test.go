package metrics

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRender_CountersAndLabels(t *testing.T) {
	RecordTurnUsage("anthropic/claude", 0.25, 1000, 200, 50)
	RecordTurnUsage("anthropic/claude", 0.25, 1000, 0, 0) // accumulates

	out := Render()

	if !strings.Contains(out, "# TYPE fleet_cost_usd_total counter") {
		t.Error("missing cost counter TYPE line")
	}
	// 0.25 + 0.25 = 0.5 for the model.
	if !strings.Contains(out, `fleet_cost_usd_total{model="anthropic/claude"} 0.5`) {
		t.Errorf("cost not accumulated by model:\n%s", out)
	}
	// prompt tokens 1000 + 1000 = 2000.
	if !strings.Contains(out, `fleet_token_usage_total{model="anthropic/claude",type="prompt"} 2000`) {
		t.Errorf("prompt tokens wrong:\n%s", out)
	}
	if !strings.Contains(out, `type="completion"`) || !strings.Contains(out, `type="cached"`) {
		t.Error("token types missing")
	}
}

func TestRender_Histogram(t *testing.T) {
	RecordHTTPRequest("/tasks", "GET", "200", 0.03)
	RecordHTTPRequest("/tasks", "GET", "200", 0.4)

	out := Render()
	if !strings.Contains(out, "# TYPE fleet_http_request_duration_seconds histogram") {
		t.Error("missing histogram TYPE")
	}
	if !strings.Contains(out, `fleet_http_request_duration_seconds_count{route="/tasks",method="GET"} 2`) {
		t.Errorf("histogram count wrong:\n%s", out)
	}
	if !strings.Contains(out, `le="+Inf"`) {
		t.Error("histogram missing +Inf bucket")
	}
	if !strings.Contains(out, `fleet_http_requests_total{route="/tasks",method="GET",status="200"} 2`) {
		t.Errorf("request counter wrong:\n%s", out)
	}
}

func TestRender_GaugeCallback(t *testing.T) {
	turns := 3
	RegisterActiveAgents(func() int { return turns }, func() int { return 1 })
	RegisterSandboxPoolSize(func() int { return 2 })

	out := Render()
	if !strings.Contains(out, `fleet_active_agents{kind="interactive"} 3`) {
		t.Errorf("active interactive gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, `fleet_active_agents{kind="scheduled"} 1`) {
		t.Errorf("active scheduled gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, "fleet_sandbox_pool_size 2") {
		t.Errorf("sandbox gauge wrong:\n%s", out)
	}
	// Gauge is pull-at-scrape: a state change is reflected on the next Render.
	turns = 5
	if !strings.Contains(Render(), `fleet_active_agents{kind="interactive"} 5`) {
		t.Error("gauge did not reflect updated state on re-scrape")
	}
}

func TestRender_SandboxResourceGauges(t *testing.T) {
	// First finished run.
	RecordSandboxResourceUsage(45.5, 256<<20, 512<<20, 1000, 2000, 12, true, 50, 60)

	out := Render()
	if !strings.Contains(out, "# TYPE fleet_sandbox_cpu_usage_percent gauge") {
		t.Errorf("missing cpu gauge TYPE line:\n%s", out)
	}
	if !strings.Contains(out, "fleet_sandbox_cpu_usage_percent 45.5") {
		t.Errorf("cpu gauge value wrong:\n%s", out)
	}
	if !strings.Contains(out, "fleet_sandbox_memory_usage_bytes "+formatFloat(float64(256<<20))) {
		t.Errorf("mem usage gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, "fleet_sandbox_memory_limit_bytes "+formatFloat(float64(512<<20))) {
		t.Errorf("mem limit gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, `fleet_sandbox_io_bytes{direction="read"} 1000`) {
		t.Errorf("io read gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, `fleet_sandbox_io_bytes{direction="write"} 2000`) {
		t.Errorf("io write gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, `fleet_sandbox_io_bytes{direction="net_in"} 50`) {
		t.Errorf("io net_in gauge wrong:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE fleet_sandbox_runs_observed_total counter") {
		t.Errorf("missing runs-observed counter:\n%s", out)
	}

	// Gauge is last-write-wins: a second run overwrites (does not accumulate).
	RecordSandboxResourceUsage(80.0, 300<<20, 512<<20, 3000, 4000, 20, false, 0, 0)
	out2 := Render()
	if !strings.Contains(out2, "fleet_sandbox_cpu_usage_percent 80") {
		t.Errorf("gauge should overwrite to 80, got:\n%s", out2)
	}
	if strings.Contains(out2, "fleet_sandbox_cpu_usage_percent 45.5") {
		t.Errorf("old gauge value should be gone:\n%s", out2)
	}
	// runs_observed is a counter — it accumulates across the two runs.
	if !strings.Contains(out2, "fleet_sandbox_runs_observed_total 2") {
		t.Errorf("runs-observed counter should be 2:\n%s", out2)
	}
}

func TestRender_TurnTimeoutAndLabelEscaping(t *testing.T) {
	RecordTurnTimeout("interactive")
	RecordTurnUsage(`weird"\model`, 0.0, 1, 0, 0)
	out := Render()
	if !strings.Contains(out, `fleet_turn_timeouts_total{kind="interactive"} 1`) {
		t.Errorf("turn timeout counter missing:\n%s", out)
	}
	// The model value's quote + backslash must be escaped in the label.
	if !strings.Contains(out, `model="weird\"\\model"`) {
		t.Errorf("label not escaped:\n%s", out)
	}
}

func TestRender_ToolOutputBoundaryMetrics(t *testing.T) {
	RecordToolOutputTruncation("run_python", "json")
	RecordToolOutputArtifact("success")
	RecordToolContextReduction("result_preview", 2)
	RecordToolContextPressure("deepseek/test", "before", 120000, 0.9375)
	out := Render()
	for _, want := range []string{
		`fleet_tool_output_truncations_total{tool="native",format="json"} 1`,
		`fleet_tool_output_artifacts_total{result="success"} 1`,
		`fleet_tool_context_reductions_total{kind="result_preview"} 2`,
		`fleet_tool_context_estimated_tokens{model="deepseek/test",phase="before"} 120000`,
		`fleet_tool_context_pressure_ratio{model="deepseek/test",phase="before"} 0.9375`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestToolOutputMetricUsesBoundedToolClasses(t *testing.T) {
	RecordToolOutputTruncation("mcp_tenant_supplied_random_name", "text")
	RecordToolOutputTruncation("mcp_another_catalog_name", "text")
	out := Render()
	if !strings.Contains(out, `fleet_tool_output_truncations_total{tool="mcp",format="text"} 2`) {
		t.Fatalf("remote names did not collapse to bounded class:\n%s", out)
	}
	if strings.Contains(out, "tenant_supplied") || strings.Contains(out, "another_catalog") {
		t.Fatalf("untrusted catalog name leaked into metric labels:\n%s", out)
	}
}

// TestRender_CallbacksRunOutsideRegistryLock: a gauge callback that records a
// metric of its own (re-entrant) must not deadlock the scrape, and a slow
// callback must not block recorders. Both are the same property — callbacks
// are invoked after reg.mu is released.
func TestRender_CallbacksRunOutsideRegistryLock(t *testing.T) {
	RegisterGauge("fleet_test_reentrant_gauge", "test", nil, func() []GaugeSample {
		// Re-entrant: recording under the registry lock would self-deadlock.
		incCounter("fleet_test_reentrant_side_effect_total", "test", nil, nil, 1)
		return []GaugeSample{{Value: 1}}
	})
	done := make(chan string, 1)
	go func() { done <- Render() }()
	select {
	case out := <-done:
		if !strings.Contains(out, "fleet_test_reentrant_gauge 1") {
			t.Fatalf("re-entrant gauge missing:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Render deadlocked on a re-entrant gauge callback")
	}
	// The side effect landed (visible on the next scrape).
	if !strings.Contains(Render(), "fleet_test_reentrant_side_effect_total") {
		t.Fatal("re-entrant record was lost")
	}
}

// TestSeriesCapDropsAndCounts pins the per-family series bound for free-text
// labels: past maxSeriesPerFamily new label sets are dropped, existing ones
// keep accumulating, and each drop is counted under
// fleet_metrics_series_dropped_total{family=...}.
func TestSeriesCapDropsAndCounts(t *testing.T) {
	const family = "fleet_test_capped_total"
	for i := 0; i < maxSeriesPerFamily+25; i++ {
		incCounter(family, "test", []string{"task"}, []string{fmt.Sprintf("task-%d", i)}, 1)
	}
	// An existing series is still writable at the cap.
	incCounter(family, "test", []string{"task"}, []string{"task-0"}, 1)

	reg.mu.Lock()
	n := len(reg.counters[family].values)
	reg.mu.Unlock()
	if n != maxSeriesPerFamily {
		t.Fatalf("family holds %d series, want the cap %d", n, maxSeriesPerFamily)
	}
	out := Render()
	if !strings.Contains(out, family+`{task="task-0"} 2`) {
		t.Errorf("existing series stopped accumulating at the cap:\n%s", out)
	}
	if strings.Contains(out, fmt.Sprintf(`{task="task-%d"}`, maxSeriesPerFamily+10)) {
		t.Error("a series past the cap was admitted")
	}
	if !strings.Contains(out, nameSeriesDropped+`{family="`+family+`"} 25`) {
		t.Errorf("dropped series not counted:\n%s", out)
	}
}
