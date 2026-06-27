package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

func TestHealthz_OKWhenHealthy(t *testing.T) {
	s := New(&config.Config{}, &fakeEngine{}, nil)
	rr := httptest.NewRecorder()
	s.healthz(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Errorf("healthy: code=%d body=%q, want 200 ok", rr.Code, rr.Body.String())
	}
}

func TestHealthz_DegradedWhenCircuitOpen(t *testing.T) {
	fe := &fakeEngine{providerHealth: []agentcore.ModelHealth{
		{Slug: "anthropic/claude", State: "closed"},
		{Slug: "openai/gpt", State: "open"},
	}}
	s := New(&config.Config{}, fe, nil)
	rr := httptest.NewRecorder()
	s.healthz(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded: code=%d, want 503", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "degraded" || body["model"] != "openai/gpt" {
		t.Errorf("body = %v, want degraded/openai/gpt", body)
	}
}

func TestHealthz_HalfOpenIsHealthy(t *testing.T) {
	fe := &fakeEngine{providerHealth: []agentcore.ModelHealth{{Slug: "m", State: "half-open"}}}
	s := New(&config.Config{}, fe, nil)
	rr := httptest.NewRecorder()
	s.healthz(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("half-open (recovering) should be healthy: code=%d, want 200", rr.Code)
	}
}

func TestProviderHealthEndpoint(t *testing.T) {
	fe := &fakeEngine{providerHealth: []agentcore.ModelHealth{
		{Slug: "anthropic/claude", State: "open", RecentErrors: 7, LastError: "503 Service Unavailable"},
	}}
	s := New(&config.Config{}, fe, nil)
	rr := httptest.NewRecorder()
	s.handleProviderHealth(rr, httptest.NewRequest("GET", "/admin/provider-health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rr.Code)
	}
	var resp struct {
		Models []agentcore.ModelHealth `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 1 || resp.Models[0].Slug != "anthropic/claude" || resp.Models[0].State != "open" {
		t.Errorf("models = %+v, want one open anthropic/claude", resp.Models)
	}
}

func TestProviderHealthEndpoint_EmptyWhenNoEngine(t *testing.T) {
	s := New(&config.Config{}, nil, nil) // nil engine (mock mode)
	rr := httptest.NewRecorder()
	s.handleProviderHealth(rr, httptest.NewRequest("GET", "/admin/provider-health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rr.Code)
	}
	if rr.Body.String() != `{"models":[]}`+"\n" {
		t.Errorf("body=%q, want empty models array", rr.Body.String())
	}
}
