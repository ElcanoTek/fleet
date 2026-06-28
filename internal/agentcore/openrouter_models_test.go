package agentcore

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// canned /api/v1/models body mirroring the OpenRouter shape the discovery
// endpoint parses: pricing strings, input modalities, top_provider.
const cannedModelsBody = `{
  "data": [
    {
      "id": "anthropic/claude-sonnet-4-5",
      "name": "Claude Sonnet 4.5",
      "context_length": 200000,
      "pricing": {"prompt": "0.000003", "completion": "0.000015"},
      "architecture": {"input_modalities": ["text", "image"]},
      "top_provider": {"max_completion_tokens": 64000}
    },
    {
      "id": "openai/gpt-5-mini",
      "name": "GPT-5 mini",
      "context_length": 400000,
      "pricing": {"prompt": "0.00000025", "completion": "0.000002"},
      "architecture": {"input_modalities": ["text"]},
      "top_provider": {"max_completion_tokens": null}
    }
  ]
}`

// stubModelsServer starts an httptest server that serves the canned catalog at
// /api/v1/models and points the OPENROUTER_BASE_URL override at it, so the cache
// fetch path is exercised end-to-end without live network. It returns a pointer
// to a fetch counter so tests can assert the TTL coalesces calls.
func stubModelsServer(t *testing.T) *int32 {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cannedModelsBody))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)
	return &hits
}

func TestModelsCache_ParsesEntriesAndDerivesFields(t *testing.T) {
	stubModelsServer(t)
	c := &modelsCache{contextMap: make(map[string]int)}

	got := c.AllEntries()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(got), got)
	}

	byID := map[string]ModelInfo{}
	for _, e := range got {
		byID[e.ID] = e
	}

	sonnet, ok := byID["anthropic/claude-sonnet-4-5"]
	if !ok {
		t.Fatalf("missing anthropic/claude-sonnet-4-5 in %+v", got)
	}
	if sonnet.Name != "Claude Sonnet 4.5" {
		t.Errorf("name = %q, want %q", sonnet.Name, "Claude Sonnet 4.5")
	}
	if sonnet.ContextLength != 200000 {
		t.Errorf("context_length = %d, want 200000", sonnet.ContextLength)
	}
	// "0.000003" per token → 3.00 per million tokens.
	if sonnet.InputPricePerMTok != 3.0 {
		t.Errorf("input price/MTok = %v, want 3.0", sonnet.InputPricePerMTok)
	}
	if sonnet.OutputPricePerMTok != 15.0 {
		t.Errorf("output price/MTok = %v, want 15.0", sonnet.OutputPricePerMTok)
	}
	if !sonnet.SupportsVision {
		t.Error("expected SupportsVision=true for an entry listing the image modality")
	}
	if !sonnet.SupportsThinking {
		t.Error("expected SupportsThinking=true for an entry with max_completion_tokens set")
	}
	if sonnet.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", sonnet.Provider)
	}

	gpt := byID["openai/gpt-5-mini"]
	if gpt.SupportsVision {
		t.Error("expected SupportsVision=false for a text-only model")
	}
	if gpt.SupportsThinking {
		t.Error("expected SupportsThinking=false when max_completion_tokens is null")
	}
	if gpt.Provider != "openai" {
		t.Errorf("provider = %q, want openai", gpt.Provider)
	}

	if c.LastFetchedAt().IsZero() {
		t.Error("expected LastFetchedAt to be set after a successful fetch")
	}
}

func TestModelsCache_TTLCoalescesFetches(t *testing.T) {
	hits := stubModelsServer(t)
	// Long TTL: the second read must hit the warm cache, not the network.
	t.Setenv("FLEET_MODEL_CACHE_TTL_MINUTES", "1440")
	c := &modelsCache{contextMap: make(map[string]int)}

	_ = c.AllEntries()
	_ = c.AllEntries()
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("expected exactly 1 upstream fetch within TTL, got %d", got)
	}
}

func TestModelsCache_FetchFailureServesLastKnown(t *testing.T) {
	// First serve a good catalog so the cache has a last-known value.
	hits := stubModelsServer(t)
	t.Setenv("FLEET_MODEL_CACHE_TTL_MINUTES", "1") // short so we can force a refresh
	c := &modelsCache{contextMap: make(map[string]int)}
	first := c.AllEntries()
	if len(first) != 2 {
		t.Fatalf("warm-up fetch: expected 2 entries, got %d", len(first))
	}

	// Now force the next refresh to fail (upstream 500) and expire the TTL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-time.Hour) // far past the 1-minute TTL
	c.mu.Unlock()

	// AllEntries must degrade to last-known rather than returning empty / erroring.
	after := c.AllEntries()
	if len(after) != 2 {
		t.Fatalf("after upstream failure expected last-known 2 entries, got %d", len(after))
	}
}

func TestModelsCache_DisableEnvSkipsFetch(t *testing.T) {
	hits := stubModelsServer(t)
	t.Setenv("FLEET_DISABLE_OPENROUTER_MODELS", "1")
	c := &modelsCache{contextMap: make(map[string]int)}

	if got := c.AllEntries(); got != nil {
		t.Errorf("expected nil catalog when discovery disabled, got %+v", got)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Errorf("expected no upstream fetch when disabled, got %d", got)
	}
}

func TestModelsCacheTTL_DefaultAndOverride(t *testing.T) {
	if got := modelsCacheTTL(); got != 60*time.Minute {
		t.Errorf("default TTL = %v, want 60m", got)
	}
	t.Setenv("FLEET_MODEL_CACHE_TTL_MINUTES", "1440")
	if got := modelsCacheTTL(); got != 1440*time.Minute {
		t.Errorf("override TTL = %v, want 1440m", got)
	}
	t.Setenv("FLEET_MODEL_CACHE_TTL_MINUTES", "not-a-number")
	if got := modelsCacheTTL(); got != 60*time.Minute {
		t.Errorf("unparseable TTL should fall back to 60m, got %v", got)
	}
	t.Setenv("FLEET_MODEL_CACHE_TTL_MINUTES", "0")
	if got := modelsCacheTTL(); got != 60*time.Minute {
		t.Errorf("non-positive TTL should fall back to 60m, got %v", got)
	}
}

func TestPerMillionTokens(t *testing.T) {
	cases := map[string]float64{
		"0.000003":   3.0,
		"0.000015":   15.0,
		"0.00000025": 0.25,
		"":           0,
		"free":       0,
	}
	for in, want := range cases {
		if got := perMillionTokens(in); got != want {
			t.Errorf("perMillionTokens(%q) = %v, want %v", in, got, want)
		}
	}
}
