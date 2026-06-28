// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import "strings"

// ModelPrice holds per-1M-token input/output prices in USD for one model.
type ModelPrice struct {
	InputPerM  float64 `json:"input_per_m"`  // USD per 1M input tokens (prompt)
	OutputPerM float64 `json:"output_per_m"` // USD per 1M output tokens (completion)
}

// KnownModelPricing maps an OpenRouter model slug to its advisory per-1M-token
// price. It is used ONLY by the pre-submission cost forecast (issue #233) to
// project a scheduled task's cost before it runs; it is NEVER used at runtime —
// actual billing comes from the cost field the OpenRouter response body reports,
// which agentcore accumulates post-hoc (see orchestrationState.CostUSD).
//
// These numbers are advisory and dated: OpenRouter's published prices change as
// models are released and repriced, so a slug missing here (or a stale number)
// only affects the FORECAST, never a real charge. Update the table when a model
// is added or OpenRouter changes a price; values below were sourced from
// OpenRouter public pricing as of 2025-Q2.
//
// Per-deployment overrides are intentionally out of scope here; custom/private
// pricing is tracked separately (#297). LookupModelPrice is the single read
// seam so an override layer can be added in front of this map without touching
// callers.
var KnownModelPricing = map[string]ModelPrice{
	"anthropic/claude-sonnet-4-5": {InputPerM: 3.00, OutputPerM: 15.00},
	"anthropic/claude-haiku-4-5":  {InputPerM: 0.80, OutputPerM: 4.00},
	"anthropic/claude-opus-4-5":   {InputPerM: 15.00, OutputPerM: 75.00},
	"openai/gpt-4o":               {InputPerM: 2.50, OutputPerM: 10.00},
	"openai/gpt-4o-mini":          {InputPerM: 0.15, OutputPerM: 0.60},
	"google/gemini-2.0-flash-001": {InputPerM: 0.10, OutputPerM: 0.40},
}

// LookupModelPrice returns the advisory price for an OpenRouter model slug and
// whether one is known. The slug is matched case-insensitively after trimming
// surrounding whitespace, mirroring how the runner normalizes a task's model
// before resolving it. A false second return means the forecast must report
// unknown pricing rather than a fabricated cost.
//
// This is the one read seam over KnownModelPricing: a future custom-pricing
// override (#297) can be consulted here ahead of the built-in table without
// changing any caller.
func LookupModelPrice(slug string) (ModelPrice, bool) {
	key := strings.ToLower(strings.TrimSpace(slug))
	if key == "" {
		return ModelPrice{}, false
	}
	for known, price := range KnownModelPricing {
		if strings.ToLower(known) == key {
			return price, true
		}
	}
	return ModelPrice{}, false
}

// charsPerToken is the ~4-chars-per-token heuristic shared with
// estimateMessageTokens (engine.go). Centralized so the forecast and the
// runtime context-pressure estimate stay on the same approximation.
const charsPerToken = 4

// tokensPerMCPTool is a conservative per-tool-schema token estimate. A typical
// MCP tool definition (name + description + JSON input schema) lands well under
// this; rounding up keeps the forecast a slight over-estimate rather than an
// under-estimate, so an operator is warned a little early rather than late.
const tokensPerMCPTool = 300

// EstimateTokens approximates the prompt tokens a scheduled run will send to the
// model from the assembled system prompt, the task prompt, and the number of MCP
// tool definitions in scope. It reuses the same chars/4 heuristic the runtime
// uses for its context-pressure estimate (estimateMessageTokens) so the forecast
// reflects the same approximation the live run is measured against — it is NOT a
// true tokenizer, and (like the runtime estimate) it omits provider-side framing
// overhead, so treat it as a lower-bound prompt size.
//
// It returns the system-prompt, tool-definition, and combined prompt token
// counts so the forecast can show the breakdown.
func EstimateTokens(systemPrompt, taskPrompt string, numMCPTools int) (systemToks, toolToks, promptToks int) {
	systemToks = len(systemPrompt) / charsPerToken
	if numMCPTools > 0 {
		toolToks = numMCPTools * tokensPerMCPTool
	}
	taskToks := len(taskPrompt) / charsPerToken
	promptToks = systemToks + toolToks + taskToks
	return systemToks, toolToks, promptToks
}

// DefaultForecastOutputTokens is the assumed median completion length per
// iteration used by the cost forecast when the real output size is unknown
// (no run has happened yet). It is deliberately a single constant rather than a
// per-model value; the forecast surfaces a wide range around the median to
// account for how much actual output varies with task complexity.
const DefaultForecastOutputTokens = 800

// Forecast range multipliers around the median per-iteration cost. The low end
// models a light run that exits after a few cheap tool calls; the high end
// models every iteration maxing its output. The median itself assumes the loop
// runs to its iteration cap.
const (
	forecastRangeMinMultiplier = 0.25
	forecastRangeMaxMultiplier = 1.0
)

// CostForecast is the pure result of the pre-submission cost computation for a
// scheduled task (issue #233). All cost fields are pointers so they can be
// reported as JSON null when the model's pricing is unknown — the token
// estimates are still meaningful in that case, but a fabricated dollar figure
// would not be. Nothing here touches the model: it is local arithmetic over the
// token estimate and the advisory price table.
type CostForecast struct {
	Model                 string      `json:"model"`
	EstimatedPromptTokens int         `json:"estimated_prompt_tokens"`
	SystemPromptTokens    int         `json:"system_prompt_tokens"`
	ToolDefinitionsTokens int         `json:"tool_definitions_tokens"`
	AvgOutputTokens       int         `json:"avg_output_tokens"`
	MaxIterations         int         `json:"max_iterations"`
	PricingKnown          bool        `json:"pricing_known"`
	PerIterationCostUSD   *float64    `json:"per_iteration_cost_usd"`
	EstimatedTotalCostUSD *float64    `json:"estimated_total_cost_usd"`
	EstimatedTotalRange   *CostRange  `json:"estimated_total_cost_range"`
	MaxCostCeilingUSD     float64     `json:"max_cost_ceiling_usd"`
	WouldHitCeiling       bool        `json:"would_hit_ceiling"`
	Price                 *ModelPrice `json:"price,omitempty"`
	Note                  string      `json:"note"`
}

// CostRange is the [min, max] envelope around the median total cost.
type CostRange struct {
	MinUSD float64 `json:"min"`
	MaxUSD float64 `json:"max"`
}

// ForecastCost computes the pre-submission cost forecast from a token estimate,
// the model slug, the loop's iteration cap, and the per-task cost ceiling. It is
// pure: no model call, no I/O, no clock. When the slug has no known price it
// returns a forecast whose cost fields are nil and whose Note explains the gap,
// so the caller can surface the token breakdown without inventing a dollar
// amount.
//
//   - promptToks / systemToks / toolToks come from EstimateTokens.
//   - maxIterations is the run's iteration cap (clamped to >= 1).
//   - maxCostCeilingUSD is the per-task cost ceiling; <= 0 disables the
//     would-hit-ceiling comparison (matching orchestrationState.checkCeilings,
//     where a non-positive ceiling means "no ceiling").
func ForecastCost(model string, systemToks, toolToks, promptToks, maxIterations int, maxCostCeilingUSD float64) CostForecast {
	if maxIterations < 1 {
		maxIterations = 1
	}
	fc := CostForecast{
		Model:                 model,
		EstimatedPromptTokens: promptToks,
		SystemPromptTokens:    systemToks,
		ToolDefinitionsTokens: toolToks,
		AvgOutputTokens:       DefaultForecastOutputTokens,
		MaxIterations:         maxIterations,
		MaxCostCeilingUSD:     maxCostCeilingUSD,
	}

	price, ok := LookupModelPrice(model)
	if !ok {
		fc.Note = "pricing for model " + model + " is unknown; cost fields are null. " +
			"Add the slug to agentcore.KnownModelPricing to get a dollar estimate."
		return fc
	}
	fc.PricingKnown = true
	priceCopy := price
	fc.Price = &priceCopy

	perIter := float64(promptToks)/1e6*price.InputPerM +
		float64(DefaultForecastOutputTokens)/1e6*price.OutputPerM
	median := perIter * float64(maxIterations)
	rng := CostRange{
		MinUSD: median * forecastRangeMinMultiplier,
		MaxUSD: median * forecastRangeMaxMultiplier,
	}

	fc.PerIterationCostUSD = &perIter
	fc.EstimatedTotalCostUSD = &median
	fc.EstimatedTotalRange = &rng
	fc.WouldHitCeiling = maxCostCeilingUSD > 0 && median > maxCostCeilingUSD
	fc.Note = "Range is 0.25x-1x the median; actual cost depends on task complexity. " +
		"Estimate is local arithmetic over a chars/4 token heuristic and advisory pricing — not a live model call."
	return fc
}
