package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

func sampleModelInfos() []agentcore.ModelInfo {
	return []agentcore.ModelInfo{
		{ID: "anthropic/claude-sonnet-4-5", Name: "Claude Sonnet 4.5", ContextLength: 200000,
			InputPricePerMTok: 3, OutputPricePerMTok: 15, SupportsVision: true, SupportsThinking: true, Provider: "anthropic"},
		{ID: "openai/gpt-5-mini", Name: "GPT-5 mini", ContextLength: 400000,
			InputPricePerMTok: 0.25, OutputPricePerMTok: 2, Provider: "openai"},
		{ID: "google/gemini-2.5-pro", Name: "Gemini 2.5 Pro", ContextLength: 1000000, Provider: "google"},
	}
}

func TestBuildModelsResponse_NoAllowlistReturnsAll(t *testing.T) {
	at := time.Date(2026, 6, 27, 3, 0, 0, 0, time.UTC)
	resp := buildModelsResponse(sampleModelInfos(), nil, at)
	if len(resp.Models) != 3 {
		t.Fatalf("expected all 3 models, got %d", len(resp.Models))
	}
	if resp.CachedAt == nil || !resp.CachedAt.Equal(at) {
		t.Fatalf("cached_at = %v, want %v", resp.CachedAt, at)
	}
	// Spot-check the wire projection.
	first := resp.Models[0]
	if first.ID != "anthropic/claude-sonnet-4-5" || first.InputPricePerMtok != 3 || !first.SupportsVision {
		t.Errorf("unexpected first entry: %+v", first)
	}
}

func TestBuildModelsResponse_AllowlistFilters(t *testing.T) {
	resp := buildModelsResponse(sampleModelInfos(), []string{"anthropic/*", "google/gemini-*"}, time.Time{})
	got := map[string]bool{}
	for _, m := range resp.Models {
		got[m.ID] = true
	}
	if !got["anthropic/claude-sonnet-4-5"] {
		t.Error("expected anthropic model to pass the allow-list")
	}
	if !got["google/gemini-2.5-pro"] {
		t.Error("expected gemini model to pass the allow-list")
	}
	if got["openai/gpt-5-mini"] {
		t.Error("openai model should have been filtered out")
	}
	if resp.CachedAt != nil {
		t.Errorf("cached_at should be nil for a zero fetch time, got %v", resp.CachedAt)
	}
}

func TestBuildModelsResponse_EmptyCatalogEncodesEmptyArray(t *testing.T) {
	resp := buildModelsResponse(nil, nil, time.Time{})
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Must serialize models as [] (not null) so the UI can iterate safely, and
	// cached_at as null when there has been no successful fetch.
	if want := `{"models":[],"cached_at":null}`; string(b) != want {
		t.Errorf("encoded %s, want %s", b, want)
	}
}

func TestHandleModels_RejectsNonGET(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", nil)
	rec := httptest.NewRecorder()
	s.handleModels(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleModels_GETReturnsJSON(t *testing.T) {
	// Disable the upstream fetch so the global cache stays empty and this test
	// never touches the network; the endpoint must still answer with a valid
	// (empty) envelope rather than erroring.
	t.Setenv("FLEET_DISABLE_OPENROUTER_MODELS", "1")
	s := &Server{cfg: &config.Config{}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()
	s.handleModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp modelsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Models == nil {
		t.Error("models must serialize as a (possibly empty) array, not null")
	}
}
