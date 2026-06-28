// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func intptr(i int) *int { return &i }

// TestForecastTaskKnownModel exercises model resolution + the system-prompt and
// tool-count seams end to end (minus HTTP), asserting the forecast math lines up
// with the resolved inputs.
func TestForecastTaskKnownModel(t *testing.T) {
	h := &Handlers{
		config: Config{
			DefaultTaskModel:     "anthropic/claude-haiku-4-5",
			MaxCostUSD:           50.0,
			DefaultMaxIterations: 7,
		},
		systemPromptForPersona: func(string) string { return strings.Repeat("x", 4000) }, // 1000 tokens
		mcpCatalog: func() []MCPServerCatalogEntry {
			return []MCPServerCatalogEntry{
				{Name: "github", ToolCount: 5, Enabled: true},
				{Name: "slack", ToolCount: 3, Enabled: false}, // disabled → excluded
			}
		},
	}

	tc := &models.TaskCreate{Prompt: strings.Repeat("p", 400)} // 100 task tokens
	fc := h.forecastTask(tc)

	if fc.Model != "anthropic/claude-haiku-4-5" {
		t.Fatalf("model = %q, want default haiku", fc.Model)
	}
	if !fc.PricingKnown {
		t.Fatal("expected known pricing for haiku")
	}
	if fc.SystemPromptTokens != 1000 {
		t.Errorf("system tokens = %d, want 1000", fc.SystemPromptTokens)
	}
	// Only the enabled github server (5 tools) counts: 5 * 300 = 1500.
	if fc.ToolDefinitionsTokens != 1500 {
		t.Errorf("tool tokens = %d, want 1500", fc.ToolDefinitionsTokens)
	}
	if want := 1000 + 1500 + 100; fc.EstimatedPromptTokens != want {
		t.Errorf("prompt tokens = %d, want %d", fc.EstimatedPromptTokens, want)
	}
	if fc.MaxIterations != 7 {
		t.Errorf("max iterations = %d, want 7 (config default)", fc.MaxIterations)
	}
	if fc.PerIterationCostUSD == nil || fc.EstimatedTotalCostUSD == nil {
		t.Fatal("expected populated cost fields")
	}
}

// TestForecastTaskExplicitOverrides confirms the task body overrides the config
// defaults for model and iteration cap, and that an explicit mcp_selection
// drives the tool count instead of the enabled-by-default set.
func TestForecastTaskExplicitOverrides(t *testing.T) {
	h := &Handlers{
		config: Config{DefaultTaskModel: "anthropic/claude-haiku-4-5", DefaultMaxIterations: 7},
		mcpCatalog: func() []MCPServerCatalogEntry {
			return []MCPServerCatalogEntry{
				{Name: "github", ToolCount: 5, Enabled: true},
				{Name: "slack", ToolCount: 3, Enabled: false},
			}
		},
	}

	tc := &models.TaskCreate{
		Prompt:        "do the thing",
		Model:         strptr("anthropic/claude-sonnet-4-5"),
		MaxIterations: intptr(3),
		// Select only slack (disabled by default) — selection wins over the
		// enabled set, so the tool count is slack's 3.
		MCPSelection: models.MCPSelection{{Server: "slack"}},
	}
	fc := h.forecastTask(tc)

	if fc.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("model = %q, want explicit sonnet", fc.Model)
	}
	if fc.MaxIterations != 3 {
		t.Errorf("max iterations = %d, want explicit 3", fc.MaxIterations)
	}
	if fc.ToolDefinitionsTokens != 3*300 {
		t.Errorf("tool tokens = %d, want %d (slack selection)", fc.ToolDefinitionsTokens, 3*300)
	}
}

// TestForecastTaskUnknownModel confirms an unknown model resolves to a forecast
// with no pricing, so the handler returns 202.
func TestForecastTaskUnknownModel(t *testing.T) {
	h := &Handlers{config: Config{DefaultTaskModel: "vendor/unknown-model"}}
	fc := h.forecastTask(&models.TaskCreate{Prompt: "hello"})
	if fc.PricingKnown {
		t.Fatal("expected unknown pricing")
	}
	if fc.EstimatedTotalCostUSD != nil {
		t.Fatal("expected nil total cost for unknown pricing")
	}
}

// TestEstimateTaskToolCountNoProvider confirms the tool count is 0 (not a panic)
// when no MCP catalog provider is wired.
func TestEstimateTaskToolCountNoProvider(t *testing.T) {
	h := &Handlers{}
	if n := h.estimateTaskToolCount(&models.TaskCreate{}); n != 0 {
		t.Fatalf("tool count = %d, want 0 with no provider", n)
	}
}
