// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"strings"
	"sync"

	"charm.land/fantasy"
)

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
// Per-deployment overrides for the RUNTIME cost-accounting path are handled by
// the custom-pricing config (#297, PricingConfig below); this advisory forecast
// table is intentionally separate. LookupModelPrice is the single read seam so an
// override layer can be added in front of this map without touching callers.
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

// Custom model pricing overrides (#297).
//
// Cost accounting normally trusts the USD figure OpenRouter returns in the
// per-step provider metadata (openrouterCost). Operators on negotiated /
// enterprise rates or a private model endpoint pay a different price than the
// published OpenRouter rate, so the provider-returned cost over- or under-counts
// their real spend — which in turn makes the cost ceiling (maxCostUSD) and the
// cost-audit data unreliable.
//
// A PricingConfig lets an operator declare per-model rates in the client bundle
// manifest. When an override matches the active model slug, cost for that step is
// computed LOCALLY from the token counts using the operator's rates instead of
// the OpenRouter figure. With no overrides configured (the shipped default),
// behavior is byte-identical to before: the OpenRouter cost is used as-is.
//
// This is installed process-wide via ConfigurePricing, mirroring the
// ConfigureAgentPolicy seam: cmd/fleet translates the bundle's pricing block and
// installs it once at startup, before any turn runs. Keeping it global (rather
// than threading it through every policy constructor) means the existing
// orchestrationState/policy signatures are untouched and the change stays scoped
// to the usage-accounting path.

// PricingFallback selects what happens to cost for a model with no override.
type PricingFallback string

const (
	// PricingFallbackOpenRouter uses the OpenRouter-returned cost for unlisted
	// models. This is the default and reproduces the pre-#297 behavior exactly.
	PricingFallbackOpenRouter PricingFallback = "openrouter"
	// PricingFallbackZero suppresses cost for unlisted models (cost stays 0).
	// Useful for fully private deployments where OpenRouter prices are
	// meaningless and only the explicitly-listed models should accrue cost.
	PricingFallbackZero PricingFallback = "zero"
)

// PricingOverride is one operator-declared per-model rate. Rates are expressed
// per MILLION tokens (the unit pricing pages publish), so a $7.50/M input rate
// is the literal 7.5 here. A zero rate for a token class means that class is
// free under the override (e.g. a model with no cache pricing leaves the cache
// fields at 0).
type PricingOverride struct {
	Model                          string
	InputCostPerMillionTokens      float64
	OutputCostPerMillionTokens     float64
	CacheReadCostPerMillionTokens  float64
	CacheWriteCostPerMillionTokens float64
}

// PricingConfig is the resolved pricing policy: a set of per-model overrides plus
// the fallback policy for unlisted models. The zero value (no overrides, empty
// fallback) preserves the default OpenRouter behavior.
type PricingConfig struct {
	Overrides []PricingOverride
	Fallback  PricingFallback
}

// normalizeSlug lower-cases and trims a model slug for case-insensitive matching
// between the configured override and the active model.
func normalizeSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// lookup returns the override for modelSlug (case-insensitively) and whether one
// matched. First match wins, so an operator listing the same model twice gets the
// earlier entry — the order matches the manifest.
func (c PricingConfig) lookup(modelSlug string) (PricingOverride, bool) {
	slug := normalizeSlug(modelSlug)
	if slug == "" {
		return PricingOverride{}, false
	}
	for _, o := range c.Overrides {
		if normalizeSlug(o.Model) == slug {
			return o, true
		}
	}
	return PricingOverride{}, false
}

// computeOverrideCost prices a step's token usage with an operator override.
// Rates are per million tokens; each token class is charged independently so a
// model with no cache pricing (cache rates 0) simply contributes nothing for
// those classes.
func computeOverrideCost(o PricingOverride, usage fantasy.Usage) float64 {
	const perMillion = 1_000_000.0
	input := float64(usage.InputTokens) / perMillion * o.InputCostPerMillionTokens
	output := float64(usage.OutputTokens) / perMillion * o.OutputCostPerMillionTokens
	cacheRead := float64(usage.CacheReadTokens) / perMillion * o.CacheReadCostPerMillionTokens
	cacheWrite := float64(usage.CacheCreationTokens) / perMillion * o.CacheWriteCostPerMillionTokens
	return input + output + cacheRead + cacheWrite
}

// computeCostFromUsage resolves the cost for one step under the price-resolution
// order:
//
//  1. a manifest override matching modelSlug → compute locally from token counts
//  2. otherwise the fallback policy:
//     - "zero": cost is 0 (unlisted models accrue nothing)
//     - "openrouter" (default / empty): the OpenRouter-returned cost (orCost),
//     or 0 when the provider returned none (orCost == nil) — exactly the
//     pre-#297 behavior.
//
// It is a pure function (no global state) so the override math, the override
// lookup, and both fallback branches are unit-testable in isolation.
func computeCostFromUsage(modelSlug string, usage fantasy.Usage, orCost *float64, cfg PricingConfig) float64 {
	if o, ok := cfg.lookup(modelSlug); ok {
		return computeOverrideCost(o, usage)
	}
	if cfg.Fallback == PricingFallbackZero {
		return 0
	}
	if orCost != nil {
		return *orCost
	}
	return 0
}

var (
	pricingMu sync.RWMutex
	// activePricing is the process-wide pricing policy. The zero value (no
	// overrides, empty fallback) reproduces the default OpenRouter behavior, so an
	// operator who never calls ConfigurePricing sees no change.
	activePricing PricingConfig
)

// ConfigurePricing installs the client bundle's pricing policy process-wide. Call
// once at startup (cmd/fleet) before any turn runs. Mirrors ConfigureAgentPolicy.
// Safe to call with a zero PricingConfig, which yields the default behavior (the
// OpenRouter-returned cost for every model). Idempotent: each call fully replaces
// the previously installed policy. The slice is defensively copied so a later
// mutation of the caller's slice can't race the readers.
func ConfigurePricing(c PricingConfig) {
	pricingMu.Lock()
	defer pricingMu.Unlock()
	cp := PricingConfig{Fallback: c.Fallback}
	if len(c.Overrides) > 0 {
		cp.Overrides = append([]PricingOverride(nil), c.Overrides...)
	}
	activePricing = cp
}

// pricingConfig returns the installed pricing policy under the read lock. The
// returned value shares the underlying Overrides slice; callers must treat it as
// read-only (the accounting path only reads it).
func pricingConfig() PricingConfig {
	pricingMu.RLock()
	defer pricingMu.RUnlock()
	return activePricing
}

// ResolveStepCost prices one model step under the process-wide pricing policy
// installed by ConfigurePricing. It is the exported entry point for cost-bearing
// call sites OUTSIDE the governed run loop (e.g. the conversation summarizer in
// internal/agent) so they honor the same per-model overrides the run loop does.
// orCost is the OpenRouter-returned cost for the step (nil when the provider
// returned none). With no overrides installed (the default), the result is the
// OpenRouter cost or 0 — identical to the prior behavior.
func ResolveStepCost(modelSlug string, usage fantasy.Usage, orCost *float64) float64 {
	return computeCostFromUsage(modelSlug, usage, orCost, pricingConfig())
}
