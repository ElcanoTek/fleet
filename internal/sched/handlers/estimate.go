// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"database/sql"
	"errors"
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

// authorizeTaskCreate enforces the SAME authorization as CreateTask: an admin
// API key, a scoped key carrying PermissionCreateTask, a user bearer token, or
// the Elcano scoped-tier cookie (which must resolve to a provisioned member).
// On failure it writes the appropriate error response and returns ok=false; the
// caller must stop. It is read-only — it creates nothing — so the forecast
// endpoint cannot become a weaker path to task creation.
func (h *Handlers) authorizeTaskCreate(w http.ResponseWriter, r *http.Request) (ok bool) {
	if h.verifyAdminKey(r) {
		return true
	}

	// Scoped API key with the create-task permission.
	if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
		perm := models.PermissionCreateTask
		valid, key, _ := h.apiKeys.ValidateKey(apiKey, &perm, nil, nil, nil)
		if valid && key != nil {
			return true
		}
	}

	// User token (password login).
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if u, err := h.storage.GetUserByToken(token); err == nil && u != nil {
			return true
		}
	}

	// Elcano unified-auth cookie (scoped tier): verify natively, then require the
	// email to be a provisioned user — matching CreateTask exactly.
	if sess := h.elcanoSessionFromRequest(r); sess != nil {
		u, err := h.lookupMember(r.Context(), sess.Email)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "Membership check failed")
			return false
		}
		if u == nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
			return false
		}
		return true
	}

	writeError(w, http.StatusUnauthorized, "Unauthorized")
	return false
}

// EstimateTask handles POST /tasks/estimate — same request body as POST /tasks
// (models.TaskCreate) but returns a pre-submission cost/token forecast WITHOUT
// creating or persisting anything. Pure local computation: no model call, no DB
// write (issue #233). Same auth + rate limiter as CreateTask.
func (h *Handlers) EstimateTask(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeTaskCreate(w, r) {
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
