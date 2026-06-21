package agentcore

import (
	"os"
	"strings"
	"testing"
)

// TestOpenRouterModelsFetch_Live hits the real OpenRouter /api/v1/models
// endpoint and verifies the live context-window lookup path end-to-end.
// Guarded by FLEET_MODELS_LIVE=1 to keep CI offline. No API key required —
// /models is public.
func TestOpenRouterModelsFetch_Live(t *testing.T) {
	if os.Getenv("FLEET_MODELS_LIVE") != "1" {
		t.Skip("set FLEET_MODELS_LIVE=1 to run this live test")
	}

	entries, err := fetchOpenRouterModels(modelsFetchTimeout)
	if err != nil {
		t.Fatalf("fetchOpenRouterModels returned error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one model entry; got 0")
	}

	for _, slug := range []string{DefaultMaxModel, "anthropic/claude-sonnet-4.6"} {
		var entry *orModelEntry
		for i := range entries {
			if strings.EqualFold(entries[i].ID, slug) {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			t.Logf("informational: %q not found in /models response (provider may have renamed)", slug)
			continue
		}
		t.Logf("OpenRouter reports %s context_length = %d", entry.ID, entry.ContextLength)
		if entry.ContextLength < 1_000_000 {
			t.Errorf("expected %s context_length >= 1_000_000 (1M beta), got %d", entry.ID, entry.ContextLength)
		}

		// End-to-end path through the live cache.
		if got := contextLengthFromOpenRouterLive(slug); got != entry.ContextLength {
			t.Errorf("contextLengthFromOpenRouterLive(%q) = %d, want %d", slug, got, entry.ContextLength)
		}
		// contextWindowForModel composes observed + live + static fallback.
		if w := contextWindowForModel(slug); w != entry.ContextLength {
			t.Errorf("contextWindowForModel(%q) = %d, want %d", slug, w, entry.ContextLength)
		}
	}
}
