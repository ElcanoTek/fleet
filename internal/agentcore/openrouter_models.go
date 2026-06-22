package agentcore

// OpenRouter /api/v1/models live context-window cache (ported from chat/cutlass
// openrouter_models.go; the P2 deferral is resolved here).
//
// This is the authoritative source for per-model context_length: it replaces
// the static prefix table in models.go for any slug OpenRouter knows about. The
// static table (modelContextWindows) remains the cold-start / network-failure
// fallback, and the observed-context cache (populated from provider
// context-too-large errors via recordContextMax) is ground truth that overrides
// both.
//
// Resolution order, implemented in contextWindowForModel (models.go):
//  1. observed cache (recordContextMax write-backs) — per-request ground truth
//  2. live /api/v1/models cache (this file) — refreshed every 24h
//  3. static prefix table (models.go) — cold-start / offline fallback
//  4. defaultModelContextWindow
//
// Not a general-purpose cache: one process-wide map + RWMutex, 24h TTL. Cold
// reads block on the network (bounded by modelsFetchTimeout); warm reads are
// map lookups under RLock.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// modelsEndpointURL is the public OpenRouter models listing. No auth required.
const modelsEndpointURL = "https://openrouter.ai/api/v1/models"

// modelsCacheTTL is how long a successful fetch is considered fresh.
const modelsCacheTTL = 24 * time.Hour

// modelsFetchTimeout bounds the HTTP GET so a slow network can't block.
const modelsFetchTimeout = 5 * time.Second

// modelsCacheDisableEnvSuffix short-circuits all network fetches when set
// truthy (read via EnvPrefix so FLEET_/CHAT_/CUTLASS_ all work; the legacy
// CUTLASS_DISABLE_OPENROUTER_MODELS resolves through the back-compat aliases).
const modelsCacheDisableEnvSuffix = "DISABLE_OPENROUTER_MODELS"

// orModelEntry mirrors the relevant shape of one /api/v1/models entry.
type orModelEntry struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
}

// orModelsResponse is the top-level envelope: {"data": [...]}.
type orModelsResponse struct {
	Data []orModelEntry `json:"data"`
}

// modelsCache is the shared process-wide live cache.
type modelsCache struct {
	mu         sync.RWMutex
	contextMap map[string]int
	fetchedAt  time.Time
}

var sharedModelsCache = &modelsCache{
	contextMap: make(map[string]int),
}

// modelsCacheDisabled reports whether the live fetch is suppressed by env.
func modelsCacheDisabled() bool {
	return EnvPrefix("").lookupBool(modelsCacheDisableEnvSuffix)
}

// contextLengthFromOpenRouterLive returns the context_length OpenRouter reports
// for the given slug from the live cache, or 0 when unknown. A refresh is
// triggered when the cache is empty or stale; concurrent callers coalesce on
// the RWMutex. Never returns an error — 0 signals "fall back to static table".
func contextLengthFromOpenRouterLive(slug string) int {
	if strings.TrimSpace(slug) == "" {
		return 0
	}
	if modelsCacheDisabled() {
		return 0
	}
	sharedModelsCache.ensureFresh()
	sharedModelsCache.mu.RLock()
	defer sharedModelsCache.mu.RUnlock()
	return sharedModelsCache.contextMap[strings.ToLower(strings.TrimSpace(slug))]
}

// ensureFresh reloads the cache when empty or past the TTL. Holds the write
// lock across the HTTP call so concurrent callers only trigger one fetch.
func (c *modelsCache) ensureFresh() {
	c.mu.RLock()
	fresh := time.Since(c.fetchedAt) < modelsCacheTTL && len(c.contextMap) > 0
	c.mu.RUnlock()
	if fresh {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetchedAt) < modelsCacheTTL && len(c.contextMap) > 0 {
		return
	}
	entries, err := fetchOpenRouterModels(modelsFetchTimeout)
	if err != nil {
		// Don't spam on every cold-start miss; the static table still answers.
		log.Printf("⚠️  OpenRouter /models fetch failed (%v); using static context-window table", err)
		// Back off for half the TTL before trying again.
		c.fetchedAt = time.Now().Add(-modelsCacheTTL / 2)
		return
	}
	for _, e := range entries {
		if e.ContextLength <= 0 || e.ID == "" {
			continue
		}
		c.contextMap[strings.ToLower(e.ID)] = e.ContextLength
	}
	c.fetchedAt = time.Now()
	log.Printf("📥 OpenRouter /models refreshed: %d entries cached", len(c.contextMap))
}

// modelsEndpointFor returns the /models listing URL, honoring the
// OPENROUTER_BASE_URL override (E2E) so the context-window cache refresh hits
// the same fake origin as chat completions rather than the live network.
func modelsEndpointFor() string {
	if override := openRouterBaseURLOverride(); override != "" {
		return strings.TrimRight(override, "/") + "/api/v1/models"
	}
	return modelsEndpointURL
}

// fetchOpenRouterModels performs the HTTP GET. /api/v1/models is public.
func fetchOpenRouterModels(timeout time.Duration) ([]orModelEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsEndpointFor(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "fleet/1.0 (+https://github.com/ElcanoTek/fleet)")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var envelope orModelsResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return envelope.Data, nil
}
