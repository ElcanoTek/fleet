package httpapi

import (
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

// Dynamic model discovery (#251).
//
// GET /api/v1/models routes the OpenRouter model catalog through fleet's own
// backend instead of having the browser fetch openrouter.ai directly. The
// server reads the catalog out of agentcore's shared, TTL-bounded cache (the
// same one the run loop maintains for context windows — no second fetch), can
// filter it against the operator allow-list before responding, and never sends
// the OpenRouter API key (the /models endpoint is public and unauthenticated,
// so no credential is in this path at all).
//
// Graceful degradation: if the upstream fetch has failed, AllEntries serves the
// last-known catalog; on a cold-start failure it is empty and the endpoint
// returns an empty list with a zero cached_at rather than an error, so a
// transient OpenRouter outage never 500s the picker.

// modelEntryResponse is one catalog entry as the UI consumes it: presentation-
// ready prices (per million tokens) and capability booleans. No credential
// material; mirrors the shape documented in issue #251.
type modelEntryResponse struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	ContextLength      int     `json:"context_length"`
	InputPricePerMtok  float64 `json:"input_price_per_mtok"`
	OutputPricePerMtok float64 `json:"output_price_per_mtok"`
	SupportsVision     bool    `json:"supports_vision"`
	SupportsThinking   bool    `json:"supports_thinking"`
	Provider           string  `json:"provider"`
}

// modelsResponse is the GET /api/v1/models envelope.
type modelsResponse struct {
	Models   []modelEntryResponse `json:"models"`
	CachedAt *time.Time           `json:"cached_at"`
}

// handleModels serves the live (cached) OpenRouter catalog, optionally filtered
// by the lockdown allow-list. The allow-list seam is reused here so an operator
// who already restricts models in lockdown mode sees the same restriction on the
// picker; when the list is empty the full catalog is returned unfiltered.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cache := agentcore.SharedModelsCache()
	resp := buildModelsResponse(cache.AllEntries(), s.cfg.LockdownAllowedModels, cache.LastFetchedAt())
	writeJSON(w, resp)
}

// buildModelsResponse projects the cache entries into the wire shape, dropping
// any entry not permitted by allow (when allow is non-empty) and stamping
// cached_at from fetchedAt (omitted when zero, i.e. before the first successful
// fetch). Pure — no global state — so the filtering/shaping is unit-testable
// without driving the process-wide cache.
func buildModelsResponse(entries []agentcore.ModelInfo, allow []string, fetchedAt time.Time) modelsResponse {
	out := make([]modelEntryResponse, 0, len(entries))
	for _, e := range entries {
		if len(allow) > 0 && !config.ModelAllowed(e.ID, allow) {
			continue
		}
		out = append(out, modelEntryResponse{
			ID:                 e.ID,
			Name:               e.Name,
			ContextLength:      e.ContextLength,
			InputPricePerMtok:  e.InputPricePerMTok,
			OutputPricePerMtok: e.OutputPricePerMTok,
			SupportsVision:     e.SupportsVision,
			SupportsThinking:   e.SupportsThinking,
			Provider:           e.Provider,
		})
	}
	resp := modelsResponse{Models: out}
	if !fetchedAt.IsZero() {
		utc := fetchedAt.UTC()
		resp.CachedAt = &utc
	}
	return resp
}
