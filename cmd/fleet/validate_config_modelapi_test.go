package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// TestCheckModelAPIProbesTheKeyEndpoint pins the #1264 fix: the check must hit
// an endpoint that actually REQUIRES auth. OpenRouter's /api/v1/models is
// public — it 200s with a garbage key — so probing it reported "API key
// authenticates" for a key the first real completion then rejected with 401.
func TestCheckModelAPIProbesTheKeyEndpoint(t *testing.T) {
	var gotPath, gotAuth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	cfg := &config.Config{OpenRouterAPIKey: "sk-or-v1-not-a-real-key"}

	res := checkModelAPI(context.Background(), cfg, nil, validateOptions{})
	if res.Status != statusOK || res.Detail != "API key authenticates" {
		t.Errorf("200: want ok/authenticates, got %+v", res)
	}
	if gotPath != "/api/v1/key" {
		t.Errorf("probed %q, want the authenticated /api/v1/key (a /api/v1/models probe is a false positive — the models list is public)", gotPath)
	}
	if want := "Bearer sk-or-v1-not-a-real-key"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}

	status = http.StatusUnauthorized
	res = checkModelAPI(context.Background(), cfg, nil, validateOptions{})
	if res.Status != statusWarn || !strings.Contains(res.Detail, "401") {
		t.Errorf("401: want a warning naming the rejection, got %+v", res)
	}

	status = http.StatusInternalServerError
	res = checkModelAPI(context.Background(), cfg, nil, validateOptions{})
	if res.Status != statusWarn || !strings.Contains(res.Detail, "500") {
		t.Errorf("500: want a warning naming the status, got %+v", res)
	}
}

// TestCheckModelAPIAgainstFakeLLM pins the seam contract: the fake-LLM server
// must serve /api/v1/key with the real endpoint's auth semantics, so E2E
// ladders running validate-config against the seam get a meaningful check
// instead of a spurious "unexpected status 404" warning.
func TestCheckModelAPIAgainstFakeLLM(t *testing.T) {
	srv := httptest.NewServer(fakellm.New().Handler())
	defer srv.Close()
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	cfg := &config.Config{OpenRouterAPIKey: "sk-or-v1-not-a-real-key"}
	if res := checkModelAPI(context.Background(), cfg, nil, validateOptions{}); res.Status != statusOK {
		t.Errorf("fake seam with a key: want ok, got %+v", res)
	}
}
