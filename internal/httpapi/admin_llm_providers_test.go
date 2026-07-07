package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/secretbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Admin LLM-provider endpoints. Load-bearing assertions: admin-gated CRUD,
// write-only keys (no response ever carries a key value), the swap callback
// fires after a persisted change, and the member-level picker read exposes
// only prefixed model slugs.

func llmFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := memberFixture(t, "boss@x.com", "user@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	key := make([]byte, secretbox.KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("key: %v", err)
	}
	c, err := secretbox.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.concreteStore(t).SetTokenCipher(c)
	return s, s.Routes()
}

func TestAdminLLMProvidersCRUD(t *testing.T) {
	s, h := llmFixture(t)

	swaps := 0
	s.llmProvidersChanged = func(context.Context) error { swaps++; return nil }

	// Non-admin: 403 on the CRUD surface.
	w := do(t, h, http.MethodGet, "/admin/llm-providers", nil, "user@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("member GET: status %d want 403", w.Code)
	}

	// Create (admin). Response must confirm the key is stored WITHOUT echoing it.
	body := map[string]any{
		"name": "anthropic-direct", "type": "anthropic",
		"models": []string{"claude-sonnet-4-5"}, "enabled": true, "api_key": "sk-ant-secret",
	}
	w = do(t, h, http.MethodPost, "/admin/llm-providers", body, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: status %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-ant-secret") {
		t.Fatalf("create response echoes the key: %s", w.Body.String())
	}
	var created store.LLMProvider
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !created.HasAPIKey || created.ID == "" {
		t.Fatalf("created = %+v, want id + has_api_key", created)
	}
	if swaps != 1 {
		t.Fatalf("swap callback fired %d times after create, want 1", swaps)
	}

	// A keyed type with no key in effect is refused before persisting.
	w = do(t, h, http.MethodPost, "/admin/llm-providers",
		map[string]any{"name": "broken", "type": "openai", "enabled": true}, "boss@x.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("keyless openai create: status %d want 400 (body %s)", w.Code, w.Body.String())
	}

	// Update with api_key omitted keeps the stored key (has_api_key stays true).
	w = do(t, h, http.MethodPut, "/admin/llm-providers/"+created.ID, map[string]any{
		"name": "anthropic-direct", "type": "anthropic",
		"models": []string{"claude-sonnet-4-5", "claude-opus-4-8"}, "enabled": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("update: status %d body %s", w.Code, w.Body.String())
	}
	var updated store.LLMProvider
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if !updated.HasAPIKey || len(updated.Models) != 2 {
		t.Fatalf("updated = %+v, want key kept + 2 models", updated)
	}

	// The admin list must confirm key presence WITHOUT the value.
	w = do(t, h, http.MethodGet, "/admin/llm-providers", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-ant-secret") {
		t.Fatalf("list response echoes the key: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"has_api_key":true`) {
		t.Fatalf("list response missing has_api_key: %s", w.Body.String())
	}

	// Member-level picker read: prefixed slugs, no secrets, member-accessible.
	// Over-serialization guard: the response is a narrow DTO — no base_url,
	// no has_api_key, no key material, ever.
	w = do(t, h, http.MethodGet, "/llm-provider-models", nil, "user@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("provider-models: status %d", w.Code)
	}
	for _, forbidden := range []string{"api_key", "base_url", "sk-ant-secret", "enabled"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("provider-models leaks %q: %s", forbidden, w.Body.String())
		}
	}
	var models struct {
		Models []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"models"`
		Providers []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			CatchAll bool   `json:"catch_all"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &models); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(models.Models) != 2 || models.Models[0].ID != "anthropic-direct/claude-sonnet-4-5" {
		t.Fatalf("models = %+v, want 2 prefixed slugs", models.Models)
	}
	// The provider roster rides along (name/type/catch_all only) so the web
	// tier can expand catch-all providers from a public model catalog.
	if len(models.Providers) != 1 || models.Providers[0].Name != "anthropic-direct" ||
		models.Providers[0].Type != "anthropic" || models.Providers[0].CatchAll {
		t.Fatalf("providers = %+v, want anthropic-direct/anthropic/catch_all=false", models.Providers)
	}

	// Delete; the row disappears from both reads.
	w = do(t, h, http.MethodDelete, "/admin/llm-providers/"+created.ID, nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: status %d body %s", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodGet, "/llm-provider-models", nil, "user@x.com")
	if !strings.Contains(w.Body.String(), `"models":[]`) {
		t.Fatalf("models after delete = %s, want empty", w.Body.String())
	}
}

// A catch-all row (no models list) contributes no model rows to the
// member-level read but IS listed in `providers` with catch_all=true, so the
// web tier can expand it from the catwalk catalog.
func TestLLMProviderModelsCatchAll(t *testing.T) {
	_, h := llmFixture(t)

	w := do(t, h, http.MethodPost, "/admin/llm-providers", map[string]any{
		"name": "gateway", "type": "openai", "enabled": true, "api_key": "sk-gw-secret",
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("catch-all create: status %d body %s", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodGet, "/llm-provider-models", nil, "user@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("provider-models: status %d", w.Code)
	}
	var models struct {
		Models    []struct{ ID string } `json:"models"`
		Providers []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			CatchAll bool   `json:"catch_all"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &models); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(models.Models) != 0 {
		t.Fatalf("models = %+v, want none (catch-all enumerates nothing)", models.Models)
	}
	if len(models.Providers) != 1 || models.Providers[0].Name != "gateway" ||
		models.Providers[0].Type != "openai" || !models.Providers[0].CatchAll {
		t.Fatalf("providers = %+v, want gateway/openai/catch_all=true", models.Providers)
	}
}

// The test-connection probe endpoint: admin-gated, works on DISABLED rows
// (verify before enabling), decrypts the stored key host-side, and returns a
// key-free summary. End-to-end against a local httptest endpoint — no seam
// stub, so the store-decrypt → agentcore-probe path is genuinely exercised.
func TestAdminLLMProviderTest(t *testing.T) {
	_, h := llmFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-live-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.2"}]}`))
	}))
	defer upstream.Close()

	// A DISABLED row with a sealed key pointing at the local endpoint.
	w := do(t, h, http.MethodPost, "/admin/llm-providers", map[string]any{
		"name": "gateway", "type": "openai", "base_url": upstream.URL,
		"models": []string{"gpt-5.2", "missing-model"}, "enabled": false, "api_key": "sk-live-secret",
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: status %d body %s", w.Code, w.Body.String())
	}
	var created store.LLMProvider
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Member: 403 (the probe reaches a decrypted key — admin-only).
	w = do(t, h, http.MethodPost, "/admin/llm-providers/"+created.ID+"/test", nil, "user@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("member test: status %d want 403", w.Code)
	}

	// Admin: the probe authenticates with the STORED key and cross-checks models.
	w = do(t, h, http.MethodPost, "/admin/llm-providers/"+created.ID+"/test", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("test: status %d body %s", w.Code, w.Body.String())
	}
	var probe struct {
		OK            bool     `json:"ok"`
		Detail        string   `json:"detail"`
		Served        int      `json:"served_model_count"`
		MissingModels []string `json:"missing_models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode probe: %v", err)
	}
	if !probe.OK || probe.Served != 1 || len(probe.MissingModels) != 1 || probe.MissingModels[0] != "missing-model" {
		t.Fatalf("probe = %+v, want ok with 1 served + missing-model warned", probe)
	}
	if strings.Contains(w.Body.String(), "sk-live-secret") {
		t.Fatalf("probe response leaks the key: %s", w.Body.String())
	}

	// Unknown id → 404.
	w = do(t, h, http.MethodPost, "/admin/llm-providers/00000000-0000-0000-0000-000000000000/test", nil, "boss@x.com")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status %d want 404", w.Code)
	}
}
