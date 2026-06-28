package agentcore

// OpenRouter /api/v1/models live catalog cache (ported from chat/cutlass
// openrouter_models.go; the P2 deferral is resolved here).
//
// This is the authoritative source for per-model context_length: it replaces
// the static prefix table in models.go for any slug OpenRouter knows about. The
// static table (modelContextWindows) remains the cold-start / network-failure
// fallback, and the observed-context cache (populated from provider
// context-too-large errors via recordContextMax) is ground truth that overrides
// both.
//
// The same fetch also backs the dynamic model-discovery endpoint (#251):
// AllEntries() exposes the full catalog (id, name, pricing, capabilities) so an
// HTTP handler can populate a UI model picker from live OpenRouter data without
// the browser hitting OpenRouter directly. One fetch feeds both the
// context-window map and the catalog slice — there is no second network call.
//
// Resolution order for context windows, implemented in contextWindowForModel
// (models.go):
//  1. observed cache (recordContextMax write-backs) — per-request ground truth
//  2. live /api/v1/models cache (this file) — refreshed every modelsCacheTTL
//  3. static prefix table (models.go) — cold-start / offline fallback
//  4. defaultModelContextWindow
//
// Not a general-purpose cache: one process-wide map + slice behind one RWMutex.
// Cold reads block on the network (bounded by modelsFetchTimeout); warm reads
// are map/slice copies under RLock.
//
// Graceful degradation: a failed fetch leaves the last-known catalog (and the
// static context-window fallback) in place — AllEntries never returns a partial
// or torn view, and a transient OpenRouter outage degrades to last-known rather
// than empty.
//
// No API key is ever sent: /api/v1/models is a public, unauthenticated endpoint,
// so OPENROUTER_API_KEY never enters this fetch, its logs, or the catalog
// payload that reaches the model context. (The credentials-stay-host-side
// invariant in AGENTS.md.)

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// modelsEndpointURL is the public OpenRouter models listing. No auth required.
const modelsEndpointURL = "https://openrouter.ai/api/v1/models"

// defaultModelsCacheTTL is how long a successful fetch is considered fresh when
// no override is set. The historical behavior was a fixed 24h; a model-picker
// endpoint the UI calls at startup wants something fresher, so the default is
// dropped to 60 minutes and made tunable via FLEET_MODEL_CACHE_TTL_MINUTES
// (set it to 1440 to restore the old 24h cadence).
const defaultModelsCacheTTLMinutes = 60

// modelsCacheTTLEnvSuffix is the env knob (read via EnvPrefix so FLEET_/CHAT_/
// CUTLASS_ all resolve, matching DISABLE_OPENROUTER_MODELS) that overrides the
// catalog TTL, in minutes. A non-positive or unparseable value falls back to
// the default.
const modelsCacheTTLEnvSuffix = "MODEL_CACHE_TTL_MINUTES"

// modelsFetchTimeout bounds the HTTP GET so a slow network can't block.
const modelsFetchTimeout = 5 * time.Second

// modelsCacheDisableEnvSuffix short-circuits all network fetches when set
// truthy (read via EnvPrefix so FLEET_/CHAT_/CUTLASS_ all work; the legacy
// CUTLASS_DISABLE_OPENROUTER_MODELS resolves through the back-compat aliases).
const modelsCacheDisableEnvSuffix = "DISABLE_OPENROUTER_MODELS"

// orModelEntry mirrors the relevant shape of one /api/v1/models entry. The
// pricing/architecture/top_provider sub-objects are captured alongside the
// already-needed context_length so the discovery endpoint (#251) can surface
// pricing and capability badges without a second fetch.
type orModelEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Pricing       struct {
		// Per-token price strings as OpenRouter returns them (e.g. "0.000003").
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	Architecture struct {
		// InputModalities lists the modalities the model accepts ("text",
		// "image", ...); "image" implies vision support.
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		// MaxCompletionTokens is non-nil for models that expose a distinct
		// completion cap; used as a proxy for extended/thinking output support.
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

// ModelInfo is the catalog entry exposed to callers outside this package (the
// HTTP discovery handler). It is the parsed, presentation-ready projection of
// orModelEntry: per-token price strings are converted to per-million-token
// floats and capabilities are derived booleans, so HTTP layers need no
// knowledge of OpenRouter's wire shape. It carries no credential material.
type ModelInfo struct {
	ID                 string
	Name               string
	ContextLength      int
	InputPricePerMTok  float64
	OutputPricePerMTok float64
	SupportsVision     bool
	SupportsThinking   bool
	Provider           string
}

// orModelsResponse is the top-level envelope: {"data": [...]}.
type orModelsResponse struct {
	Data []orModelEntry `json:"data"`
}

// modelsCache is the shared process-wide live cache. One RWMutex guards both the
// context-window map (the per-slug lookup hot path) and the full catalog slice
// (the discovery endpoint), so a single fetch populates both atomically.
type modelsCache struct {
	mu         sync.RWMutex
	contextMap map[string]int
	entries    []orModelEntry
	fetchedAt  time.Time
}

var sharedModelsCache = &modelsCache{
	contextMap: make(map[string]int),
}

// ModelCatalog is the read surface HTTP layers consume for model discovery
// (#251): the full live catalog plus the time it was last refreshed. Returned as
// an interface so the concrete cache internals stay package-private while
// callers still get a stable, testable contract.
type ModelCatalog interface {
	// AllEntries returns the live catalog (refreshing if stale), degrading to
	// last-known on a fetch failure.
	AllEntries() []ModelInfo
	// LastFetchedAt is when the catalog last refreshed successfully (zero before
	// the first success).
	LastFetchedAt() time.Time
}

// SharedModelsCache returns the process-wide OpenRouter catalog so HTTP surfaces
// (the #251 model-discovery endpoint) can read the full model list out of the
// same cache the run loop already maintains for context windows — no second
// fetch, no duplicate TTL.
func SharedModelsCache() ModelCatalog { return sharedModelsCache }

// modelsCacheDisabled reports whether the live fetch is suppressed by env.
func modelsCacheDisabled() bool {
	return EnvPrefix("").lookupBool(modelsCacheDisableEnvSuffix)
}

// modelsCacheTTL resolves the catalog TTL from FLEET_MODEL_CACHE_TTL_MINUTES
// (with the CHAT_/CUTLASS_ aliases the EnvPrefix machinery honors), falling back
// to defaultModelsCacheTTLMinutes when unset, unparseable, or non-positive.
func modelsCacheTTL() time.Duration {
	raw := strings.TrimSpace(EnvPrefix("").lookup(modelsCacheTTLEnvSuffix))
	if raw != "" {
		if mins, err := strconv.Atoi(raw); err == nil && mins > 0 {
			return time.Duration(mins) * time.Minute
		}
	}
	return time.Duration(defaultModelsCacheTTLMinutes) * time.Minute
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

// AllEntries returns the full live catalog as presentation-ready ModelInfo
// values, refreshing the cache first if it is empty or stale. The returned
// slice is a fresh copy the caller may sort/filter freely. On a fetch failure
// it returns the last-known catalog (possibly empty on cold start) rather than
// an error — the discovery endpoint degrades gracefully to last-known/static.
func (c *modelsCache) AllEntries() []ModelInfo {
	if modelsCacheDisabled() {
		return nil
	}
	c.ensureFresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelInfo, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.toModelInfo())
	}
	return out
}

// LastFetchedAt returns when the catalog was last successfully refreshed (zero
// before the first successful fetch). Surfaced as `cached_at` by the discovery
// endpoint so clients can reason about staleness.
func (c *modelsCache) LastFetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetchedAt
}

// toModelInfo projects one wire entry into the public ModelInfo, converting
// per-token price strings to per-million-token floats and deriving capability
// booleans. An unparseable price degrades to 0 rather than failing the entry.
func (e orModelEntry) toModelInfo() ModelInfo {
	return ModelInfo{
		ID:                 e.ID,
		Name:               e.Name,
		ContextLength:      e.ContextLength,
		InputPricePerMTok:  perMillionTokens(e.Pricing.Prompt),
		OutputPricePerMTok: perMillionTokens(e.Pricing.Completion),
		SupportsVision:     hasModality(e.Architecture.InputModalities, "image"),
		SupportsThinking:   e.TopProvider.MaxCompletionTokens != nil,
		Provider:           providerFromSlug(e.ID),
	}
}

// perMillionTokens converts a per-token price string (OpenRouter's wire format)
// to a per-million-token float. An empty or unparseable value yields 0.
func perMillionTokens(perToken string) float64 {
	perToken = strings.TrimSpace(perToken)
	if perToken == "" {
		return 0
	}
	v, err := strconv.ParseFloat(perToken, 64)
	if err != nil {
		return 0
	}
	return v * 1_000_000
}

// hasModality reports whether want appears (case-insensitively) in modalities.
func hasModality(modalities []string, want string) bool {
	for _, m := range modalities {
		if strings.EqualFold(strings.TrimSpace(m), want) {
			return true
		}
	}
	return false
}

// providerFromSlug returns the provider segment of an OpenRouter slug
// ("anthropic/claude-sonnet-4-5" → "anthropic"). Returns "" when the slug has
// no provider prefix.
func providerFromSlug(slug string) string {
	slug = strings.TrimSpace(slug)
	if i := strings.IndexByte(slug, '/'); i > 0 {
		return slug[:i]
	}
	return ""
}

// ensureFresh reloads the cache when empty or past the TTL. Holds the write
// lock across the HTTP call so concurrent callers only trigger one fetch. A
// failed fetch leaves the existing catalog and context map untouched (last-known
// is preferable to empty), only nudging fetchedAt back so a retry happens sooner.
func (c *modelsCache) ensureFresh() {
	ttl := modelsCacheTTL()
	c.mu.RLock()
	fresh := time.Since(c.fetchedAt) < ttl && len(c.contextMap) > 0
	c.mu.RUnlock()
	if fresh {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetchedAt) < ttl && len(c.contextMap) > 0 {
		return
	}
	entries, err := fetchOpenRouterModels(modelsFetchTimeout)
	if err != nil {
		// Don't spam on every cold-start miss; the static table still answers
		// context windows and AllEntries keeps serving the last-known catalog.
		log.Printf("⚠️  OpenRouter /models fetch failed (%v); serving last-known catalog + static context-window table", err)
		// Back off for half the TTL before trying again.
		c.fetchedAt = time.Now().Add(-ttl / 2)
		return
	}
	// Rebuild both projections from the fresh fetch. Build into a new map so a
	// model removed upstream doesn't linger, then swap under the held lock.
	freshMap := make(map[string]int, len(entries))
	kept := make([]orModelEntry, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		kept = append(kept, e)
		if e.ContextLength > 0 {
			freshMap[strings.ToLower(e.ID)] = e.ContextLength
		}
	}
	c.contextMap = freshMap
	c.entries = kept
	c.fetchedAt = time.Now()
	log.Printf("📥 OpenRouter /models refreshed: %d entries cached", len(c.entries))
}

// modelsEndpointFor returns the /models listing URL, honoring the
// OPENROUTER_BASE_URL override (E2E) so the catalog cache refresh hits the same
// fake origin as chat completions rather than the live network.
func modelsEndpointFor() string {
	if override := openRouterBaseURLOverride(); override != "" {
		return strings.TrimRight(override, "/") + "/api/v1/models"
	}
	return modelsEndpointURL
}

// fetchOpenRouterModels performs the HTTP GET. /api/v1/models is public, so no
// Authorization header (and thus no API key) is ever attached.
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
