// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The model picker's catalog endpoints. The OpenRouter listing is the SAME
// process-wide cache the run loop keeps for context windows
// (agentcore.SharedModelsCache: one fetch, honoring OPENROUTER_BASE_URL in
// tests, degrading to last-known on an outage). There is deliberately no price
// ceiling on what the picker offers: spend is governed by the per-run cost
// ceilings, not by model selection.

// modelsPageURL is where the picker sends a user who wants to browse.
const modelsPageURL = "https://openrouter.ai/models"

// errValidateModel is shown to the browser verbatim, exactly as the Next.js
// route worded it, so the picker's message did not change across ADR-0091.
var errValidateModel = errors.New("Failed to validate model.") //nolint:revive,staticcheck // user-facing copy, not a wrapped error

// majorLabs is the curated cross-lab picker: one row per lab, in this order.
// Meta and Cohere are intentionally omitted.
var majorLabs = []string{"z-ai", "anthropic", "openai", "google", "x-ai", "deepseek", "moonshotai", "qwen", "mistralai", "nvidia"}

// modelCatalogs holds the catwalk cache and the OpenRouter catalog seam.
type modelCatalogs struct {
	http       *http.Client
	catwalkURL string
	// openrouter is the catalog seam (tests substitute a fixed one).
	openrouter agentcore.ModelCatalog

	mu        sync.Mutex
	catwalk   []catwalkProvider
	catwalkAt time.Time
}

func newModelCatalogs(c *http.Client, catwalkURL string) *modelCatalogs {
	return &modelCatalogs{http: c, catwalkURL: catwalkURL, openrouter: agentcore.SharedModelsCache()}
}

// orCatalog is the picker's view of the OpenRouter listing: entries with a
// usable completion price, in listing order, plus a by-slug index that also
// resolves the dated canonical form.
type orCatalog struct {
	entries   []agentcore.ModelInfo
	bySlug    map[string]agentcore.ModelInfo
	fetchedAt time.Time
}

func (m *modelCatalogs) loadOpenRouter() (*orCatalog, error) {
	all := m.openrouter.AllEntries()
	c := &orCatalog{bySlug: map[string]agentcore.ModelInfo{}, fetchedAt: m.openrouter.LastFetchedAt()}
	for _, e := range all {
		// An entry without a numeric completion price cannot be reasoned about
		// for cost; OpenRouter's "0" (a free tier) is a real price.
		if e.ID == "" || !e.HasCompletionPrice {
			continue
		}
		c.entries = append(c.entries, e)
		c.bySlug[e.ID] = e
		if e.CanonicalSlug != "" {
			c.bySlug[e.CanonicalSlug] = e
		}
	}
	if len(c.entries) == 0 {
		return nil, errors.New("openrouter models response contained no usable entries")
	}
	return c, nil
}

// isTextCompletion: the chat UI renders text only, so a model must emit text
// and nothing else; an unknown modality list passes (legacy listings omit it).
func isTextCompletion(e agentcore.ModelInfo) bool {
	if len(e.OutputModalities) == 0 {
		return true
	}
	for _, m := range e.OutputModalities {
		if m != "text" {
			return false
		}
	}
	return true
}

// listAllowed: every text-completion model, most expensive first (a proxy
// for quality), slug as the tiebreak.
func listAllowed(c *orCatalog) []agentcore.ModelInfo {
	var out []agentcore.ModelInfo
	for _, e := range c.entries {
		if isTextCompletion(e) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CompletionPerToken != out[j].CompletionPerToken {
			return out[i].CompletionPerToken > out[j].CompletionPerToken
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// baseSlug strips an OpenRouter ":variant" suffix.
func baseSlug(slug string) string {
	if i := strings.IndexByte(slug, ':'); i >= 0 {
		return slug[:i]
	}
	return slug
}

// listLatestPerLab: per lab, the newest text-only model that is not a pinned
// tier slug (variant-insensitively) and not a ":free" variant; ties on
// created go to the higher completion price.
func listLatestPerLab(c *orCatalog, exclude []string) []agentcore.ModelInfo {
	skip := map[string]bool{}
	for _, s := range exclude {
		skip[baseSlug(s)] = true
	}
	var out []agentcore.ModelInfo
	for _, lab := range majorLabs {
		prefix := lab + "/"
		var best *agentcore.ModelInfo
		for i := range c.entries {
			e := c.entries[i]
			if !strings.HasPrefix(e.ID, prefix) || skip[baseSlug(e.ID)] || strings.HasSuffix(e.ID, ":free") || !isTextCompletion(e) {
				continue
			}
			if best == nil || e.Created > best.Created || (e.Created == best.Created && e.CompletionPerToken > best.CompletionPerToken) {
				best = &c.entries[i]
			}
		}
		if best != nil {
			out = append(out, *best)
		}
	}
	return out
}

func badGateway(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

type catalogModel struct {
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	ContextLength   int     `json:"context_length,omitempty"`
	Created         int64   `json:"created,omitempty"`
	PriceCompletion float64 `json:"price_completion"`
	PricePrompt     float64 `json:"price_prompt"`
}

type rankedModel struct {
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	Created         int64   `json:"created,omitempty"`
	PricePrompt     float64 `json:"price_prompt"`
	PriceCompletion float64 `json:"price_completion"`
}

func displayName(e agentcore.ModelInfo) string {
	if e.Name != "" {
		return e.Name
	}
	return e.ID
}

func (g *Gateway) registerModels(mux *http.ServeMux) {
	// GET /api/model-catalog — every text-capable model the picker offers.
	mux.HandleFunc("GET /api/model-catalog", func(w http.ResponseWriter, r *http.Request) {
		if g.session(r) == nil {
			unauthorized(w)
			return
		}
		c, err := g.models.loadOpenRouter()
		if err != nil {
			badGateway(w, err)
			return
		}
		models := []catalogModel{}
		for _, e := range listAllowed(c) {
			models = append(models, catalogModel{e.ID, displayName(e), e.ContextLength, e.Created, e.CompletionPerToken, e.PromptPerToken})
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models, "cached_at": c.fetchedAt.UnixMilli()})
	})

	// GET /api/model-rankings — one model per major lab (the newest), for the
	// picker before a search query. The pinned tier slugs are excluded so a
	// lab's newest row does not duplicate a pinned one.
	mux.HandleFunc("GET /api/model-rankings", func(w http.ResponseWriter, r *http.Request) {
		if g.session(r) == nil {
			unauthorized(w)
			return
		}
		c, err := g.models.loadOpenRouter()
		if err != nil {
			badGateway(w, err)
			return
		}
		entries := listLatestPerLab(c, []string{agentcore.DefaultCoreModel, agentcore.DefaultMaxModel})
		if len(entries) == 0 {
			badGateway(w, errors.New("no per-lab models were found"))
			return
		}
		models := []rankedModel{}
		for _, e := range entries {
			models = append(models, rankedModel{e.ID, displayName(e), e.Created, e.PromptPerToken, e.CompletionPerToken})
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models, "cached_at": c.fetchedAt.UnixMilli()})
	})

	// GET /api/model-check?slug= — the custom-slug input's verdict. Routing is
	// chat-server's (an admin-configured provider may own the slug); only an
	// OpenRouter-routed slug is looked up in the catalog. Catalog trouble is a
	// 502 so the UI fails open rather than blocking send.
	mux.HandleFunc("GET /api/model-check", g.handleModelCheck)

	// GET /api/catwalk-models — the catwalk model database, trimmed, so the
	// pickers can expand an admin's catch-all provider by type. Advisory.
	mux.HandleFunc("GET /api/catwalk-models", func(w http.ResponseWriter, r *http.Request) {
		if g.session(r) == nil {
			unauthorized(w)
			return
		}
		providers, err := g.models.loadCatwalk()
		if err != nil {
			badGateway(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
	})
}

func (g *Gateway) handleModelCheck(w http.ResponseWriter, r *http.Request) {
	s := g.session(r)
	if s == nil {
		unauthorized(w)
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		writeJSON(w, http.StatusOK, struct {
			Allowed   bool   `json:"allowed"`
			Slug      string `json:"slug"`
			Reason    string `json:"reason"`
			Message   string `json:"message"`
			ModelsURL string `json:"models_url"`
		}{false, slug, "empty", "Model slug is required.", modelsPageURL})
		return
	}
	status, hdr, body, ok := g.callCapture(g.chat, call{
		method: http.MethodGet,
		target: "/llm-provider-models?slug=" + encodeURIComponent(slug),
		header: g.chatHeaders(identityOf(s)),
	})
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "chat-server unreachable: no backend"})
		return
	}
	if status == http.StatusUnauthorized && hdr.Get("X-Session-Revoked") == "1" {
		g.clearSessionCookie(w, r)
	}
	if status < 200 || status > 299 {
		w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	var route map[string]any
	if err := json.Unmarshal(body, &route); err != nil {
		badGateway(w, errValidateModel)
		return
	}
	if allowed, isBool := route["allowed"].(bool); isBool && !allowed {
		route["models_url"] = "/settings/admin/providers"
		writeJSON(w, http.StatusOK, route)
		return
	}
	if allowed, isBool := route["allowed"].(bool); isBool && allowed && route["provider_type"] != "openrouter" {
		// Native/gateway slugs are not OpenRouter catalog ids; routing is all
		// that can be checked locally.
		writeJSON(w, http.StatusOK, map[string]any{"allowed": true, "slug": slug})
		return
	}
	c, err := g.models.loadOpenRouter()
	if err != nil {
		badGateway(w, err)
		return
	}
	// An unknown slug is allowed: a newly released model keeps working while
	// the cache is stale.
	entry, known := c.bySlug[slug]
	var perMillion any
	if known {
		perMillion = entry.CompletionPerToken * 1_000_000
	}
	writeJSON(w, http.StatusOK, struct {
		Allowed                 bool   `json:"allowed"`
		Slug                    string `json:"slug"`
		Known                   bool   `json:"known"`
		CompletionUSDPerMillion any    `json:"completion_usd_per_million"`
	}{true, slug, known, perMillion})
}

// ── catwalk ────────────────────────────────────────────────────────────────

type catwalkModel struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	ContextWindow float64 `json:"context_window,omitempty"`
	CostPer1MOut  float64 `json:"cost_per_1m_out,omitempty"`
}

type catwalkProvider struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Models []catwalkModel `json:"models"`
}

const catwalkTTL = 24 * time.Hour

func (m *modelCatalogs) catwalkProvidersURL() string {
	base := firstNonEmpty(m.catwalkURL, "https://catwalk.charm.land")
	return strings.TrimRight(base, "/") + "/v2/providers"
}

func (m *modelCatalogs) loadCatwalk() ([]catwalkProvider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.catwalk != nil && time.Since(m.catwalkAt) < catwalkTTL {
		return m.catwalk, nil
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, m.catwalkProvidersURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := *m.http
	client.Timeout = 8 * time.Second
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, errors.New("catwalk providers fetch failed: " + res.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	providers, err := parseCatwalkProviders(raw)
	if err != nil {
		return nil, err
	}
	m.catwalk, m.catwalkAt = providers, time.Now()
	return providers, nil
}

func parseCatwalkProviders(raw []byte) ([]catwalkProvider, error) {
	var payload []map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, errors.New("catwalk providers response is not an array")
	}
	trimmed := func(v any) string {
		s, _ := v.(string)
		return strings.TrimSpace(s)
	}
	positive := func(v any) float64 {
		f, _ := v.(float64)
		if f > 0 {
			return f
		}
		return 0
	}
	var out []catwalkProvider
	for _, p := range payload {
		id := trimmed(p["id"])
		if id == "" {
			continue
		}
		models := []catwalkModel{}
		rawModels, _ := p["models"].([]any)
		for _, rm := range rawModels {
			mm, _ := rm.(map[string]any)
			mid := trimmed(mm["id"])
			if mid == "" {
				continue
			}
			models = append(models, catwalkModel{
				ID:            mid,
				Name:          firstNonEmpty(trimmed(mm["name"]), mid),
				ContextWindow: positive(mm["context_window"]),
				CostPer1MOut:  positive(mm["cost_per_1m_out"]),
			})
		}
		out = append(out, catwalkProvider{ID: id, Name: firstNonEmpty(trimmed(p["name"]), id), Type: trimmed(p["type"]), Models: models})
	}
	if len(out) == 0 {
		return nil, errors.New("catwalk providers response contained no usable entries")
	}
	return out, nil
}
