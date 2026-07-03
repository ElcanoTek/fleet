// Admin-managed LLM providers (#289 follow-on): CRUD over the llm_providers
// table plus the resolver rebuild that makes an edit take effect immediately.
//
// Security invariants (mirroring the MCP credential accounts):
//   - API-key VALUES are write-only. Every response carries has_api_key, never
//     the key. There is no read endpoint for a stored key.
//   - All mutation is admin-gated (adminMiddleware). The names+models read used
//     by the model picker (GET /llm-provider-models) is member-level and
//     carries no secret material.
//   - A row is validated (eager client construction, no network) BEFORE it is
//     persisted, and the resolver swap is all-or-nothing — a bad edit leaves
//     the current routing table serving.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// llmProviderBody is the create/update payload. APIKey nil = leave unchanged
// (or absent on create); "" = clear the stored key.
type llmProviderBody struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	Enabled bool     `json:"enabled"`
	APIKey  *string  `json:"api_key"`
}

// handleAdminLLMProviders serves /admin/llm-providers: GET lists every row
// (no key values), POST creates one.
func (s *Server) handleAdminLLMProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		providers, err := s.store.ListLLMProviders(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"providers": providers})
	case http.MethodPost:
		var body llmProviderBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		in := store.LLMProviderInput{
			Name: body.Name, Type: body.Type, BaseURL: body.BaseURL,
			Models: body.Models, Enabled: body.Enabled, APIKey: body.APIKey,
		}
		key := ""
		if body.APIKey != nil {
			key = *body.APIKey
		}
		if err := validateLLMProviderRow(in, key); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, err := s.store.CreateLLMProvider(r.Context(), in)
		if err != nil {
			httpErrorForLLMProvider(w, err)
			return
		}
		s.applyLLMProviderChange(w, r, p)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminLLMProviderItem serves /admin/llm-providers/{id}: PUT updates,
// DELETE removes.
func (s *Server) handleAdminLLMProviderItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/llm-providers/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "provider id required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body llmProviderBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		in := store.LLMProviderInput{
			Name: body.Name, Type: body.Type, BaseURL: body.BaseURL,
			Models: body.Models, Enabled: body.Enabled, APIKey: body.APIKey,
		}
		// Effective-key check: an update that leaves the key untouched (nil)
		// counts the stored key; one that sets/clears it counts the new value.
		effectiveKey := ""
		if body.APIKey != nil {
			effectiveKey = *body.APIKey
		} else {
			existing, err := s.store.ListLLMProviders(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, p := range existing {
				if p.ID == id && p.HasAPIKey {
					// Placeholder — validateLLMProviderRow only checks presence,
					// never the value, so the real key needn't be decrypted here.
					effectiveKey = "stored"
					break
				}
			}
		}
		if err := validateLLMProviderRow(in, effectiveKey); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, err := s.store.UpdateLLMProvider(r.Context(), id, in)
		if err != nil {
			httpErrorForLLMProvider(w, err)
			return
		}
		s.applyLLMProviderChange(w, r, p)
	case http.MethodDelete:
		if err := s.store.DeleteLLMProvider(r.Context(), id); err != nil {
			httpErrorForLLMProvider(w, err)
			return
		}
		s.applyLLMProviderChange(w, r, map[string]any{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// applyLLMProviderChange rebuilds the resolver routing table after a persisted
// change, then responds with the row. A rebuild failure is reported (500) but
// the row stays persisted and the PREVIOUS routing table keeps serving — the
// admin sees the error and can fix or delete the row; chat never goes down.
func (s *Server) applyLLMProviderChange(w http.ResponseWriter, r *http.Request, result any) {
	if s.llmProvidersChanged != nil {
		if err := s.llmProvidersChanged(r.Context()); err != nil {
			http.Error(w, "saved, but applying the provider table failed: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, result)
}

// validateLLMProviderRow enforces what the store's shape checks can't: an
// enabled row must be constructible as a provider client, which for the keyed
// types means an API key must be in effect. Uses a placeholder key value —
// only presence is validated host-side; the provider itself validates the
// value on first use.
func validateLLMProviderRow(in store.LLMProviderInput, effectiveKey string) error {
	if !in.Enabled {
		return nil // disabled rows are excluded from the routing table
	}
	return agentcore.ValidateProvider(agentcore.ProviderConfig{
		Name:    strings.ToLower(strings.TrimSpace(in.Name)),
		Type:    agentcore.ProviderType(strings.ToLower(strings.TrimSpace(in.Type))),
		APIKey:  effectiveKey,
		BaseURL: in.BaseURL,
		Models:  in.Models,
	}, agentcore.DefaultProviderHeaders)
}

func httpErrorForLLMProvider(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrLLMProviderNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case strings.Contains(err.Error(), "already exists"),
		strings.Contains(err.Error(), "invalid provider"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleLLMProviderModels serves GET /llm-provider-models — the member-level
// read the model picker unions into its list: enabled providers' names, types,
// and model slugs. Slugs are prefixed "<provider>/<model>" (explicit routing)
// so a picked entry resolves through its provider regardless of list overlap.
// Catch-all providers (empty models list) contribute no rows — they serve any
// slug and have nothing to enumerate. No secret material in this response.
func (s *Server) handleLLMProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	providers, err := s.store.ListLLMProviders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type entry struct {
		ID       string `json:"id"`   // the slug to send as the turn's model
		Name     string `json:"name"` // display label
		Provider string `json:"provider"`
		Type     string `json:"type"`
	}
	out := []entry{}
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			out = append(out, entry{
				ID:       p.Name + "/" + m,
				Name:     p.Name + ": " + m,
				Provider: p.Name,
				Type:     p.Type,
			})
		}
	}
	writeJSON(w, map[string]any{"models": out})
}
