package metrics

import (
	"strings"
	"testing"
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
