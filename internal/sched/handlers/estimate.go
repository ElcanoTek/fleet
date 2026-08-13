// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// SetSystemPromptProvider wires the assembled scheduled system prompt resolver
// used by the cost forecast (issue #233). cmd/fleet injects a closure backed by
// the scheduled runner so the forecast counts the SAME system prompt (default
// prompt + persona expertise) a real dispatch would send. Keeping the handlers
// package decoupled from clientconfig/scheduledrun, exactly like
// SetMCPCatalogProvider. nil → the forecast omits the system-prompt token line.
func (h *Handlers) SetSystemPromptProvider(p func(persona string) string) {
	h.systemPromptForPersona = p
}

// SetPersonaCatalog wires the list of persona names loadable from the client
// bundle (#720), read live per call so a bundle hot-reload is reflected without
// a restart. cmd/fleet injects a closure over the bundle's personas dir; nil
// (the default) disables the create-time existence check, keeping the handlers
// package decoupled from clientconfig exactly like SetSystemPromptProvider.
func (h *Handlers) SetPersonaCatalog(p func() []string) {
	h.personaCatalog = p
}

// EstimateTask handles POST /tasks/estimate — same request body as POST /tasks
// (models.TaskCreate) but returns a pre-submission cost/token forecast WITHOUT
// creating or persisting anything. Pure local computation: no model call, no DB
// write (issue #233). Same auth + rate limiter as CreateTask.
func (h *Handlers) EstimateTask(w http.ResponseWriter, r *http.Request) {
	// Authorization is the SHARED authorizeTaskCreator (batch.go) — admin key,
	// Next-proxy header trust with its session-epoch gate, scoped key with
	// create permission, user bearer token, Elcano cookie member — so the
	// creation-shaped endpoints cannot drift. This handler used to enforce a
	// hand-rolled copy that predated the header-trust path (#157), which made
	// the forecast silently dead ("Unauthorized") for every cookie-path
	// Operations Center user. The resolved creator is discarded: the forecast
	// creates nothing, so this cannot become a weaker path to task creation.
	if _, ok := h.authorizeTaskCreator(w, r); !ok {
		return
	}

	var tc models.TaskCreate
	if err := readJSON(r, &tc); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := h.validateTaskCreate(&tc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	forecast := h.forecastTask(&tc)

	// An unknown model still yields a useful token breakdown, so return 202
	// (Accepted-but-incomplete) with null cost fields rather than an error — the
	// caller can submit anyway; the forecast is advisory, never a gate.
	status := http.StatusOK
	if !forecast.PricingKnown {
		status = http.StatusAccepted
	}
	writeJSON(w, status, forecast)
}

// forecastTask resolves the model, system prompt, MCP tool count, iteration cap,
// and cost ceiling for a validated TaskCreate, then runs the pure forecast math
// in agentcore. Split out so the model/prompt/tool resolution is unit-testable
// without an HTTP round-trip.
func (h *Handlers) forecastTask(tc *models.TaskCreate) agentcore.CostForecast {
	model := h.config.DefaultTaskModel
	if tc.Model != nil && strings.TrimSpace(*tc.Model) != "" {
		model = strings.TrimSpace(*tc.Model)
	}

	systemPrompt := ""
	if h.systemPromptForPersona != nil {
		systemPrompt = h.systemPromptForPersona(tc.Persona)
	}

	numTools := h.estimateTaskToolCount(tc)

	systemToks, toolToks, promptToks := agentcore.EstimateTokens(systemPrompt, tc.Prompt, numTools)

	maxIter := h.config.DefaultMaxIterations
	if tc.MaxIterations != nil && *tc.MaxIterations > 0 {
		maxIter = *tc.MaxIterations
	}

	return agentcore.ForecastCost(model, systemToks, toolToks, promptToks, maxIter, h.config.MaxCostUSD)
}

// estimateTaskToolCount returns the number of MCP tool definitions that will be
// in scope for a task, summed from the read-only Optional-MCP catalog. With an
// explicit mcp_selection it counts the chosen servers; otherwise it counts the
// servers enabled by default. Returns 0 when no catalog provider is wired.
func (h *Handlers) estimateTaskToolCount(tc *models.TaskCreate) int {
	if h.mcpCatalog == nil {
		return 0
	}
	catalog := h.mcpCatalog()
	if catalog == nil {
		return 0
	}

	if len(tc.MCPSelection) > 0 {
		byName := make(map[string]int, len(catalog))
		for _, s := range catalog {
			byName[s.Name] = s.ToolCount
		}
		total := 0
		for _, choice := range tc.MCPSelection {
			total += byName[choice.Server]
		}
		return total
	}

	total := 0
	for _, s := range catalog {
		if s.Enabled {
			total += s.ToolCount
		}
	}
	return total
}
