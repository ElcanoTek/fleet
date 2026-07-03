package agentcore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ProbeProvider — the test-connection probe. Load-bearing assertions: the key
// travels only in the auth header, auth failures and unreachable endpoints
// fold into key-free !OK results, and the model cross-check reports absences
// as warnings without failing the probe.

func catalogServer(t *testing.T, wantAuth string, ids ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		type entry struct {
			ID string `json:"id"`
		}
		out := struct {
			Data []entry `json:"data"`
		}{}
		for _, id := range ids {
			out.Data = append(out.Data, entry{ID: id})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func TestProbeProviderOpenAICompatible(t *testing.T) {
	srv := catalogServer(t, "Bearer sk-test", "gpt-5.2", "gpt-5.2-mini")
	defer srv.Close()

	t.Run("valid key + all models served", func(t *testing.T) {
		res := ProbeProvider(context.Background(), ProviderConfig{
			Name: "gw", Type: ProviderTypeOpenAI, APIKey: "sk-test",
			BaseURL: srv.URL, Models: []string{"gpt-5.2"},
		})
		if !res.OK || res.ServedModelCount != 2 || len(res.MissingModels) != 0 {
			t.Fatalf("res = %+v, want OK with 2 served, none missing", res)
		}
	})

	t.Run("listed model absent → warning, not failure", func(t *testing.T) {
		res := ProbeProvider(context.Background(), ProviderConfig{
			Name: "gw", Type: ProviderTypeOpenAI, APIKey: "sk-test",
			BaseURL: srv.URL, Models: []string{"gpt-5.2", "nonexistent-model"},
		})
		if !res.OK {
			t.Fatalf("res = %+v, want OK despite missing model", res)
		}
		if len(res.MissingModels) != 1 || res.MissingModels[0] != "nonexistent-model" {
			t.Fatalf("missing = %v, want [nonexistent-model]", res.MissingModels)
		}
	})

	t.Run("bad key → auth failure, key never echoed", func(t *testing.T) {
		res := ProbeProvider(context.Background(), ProviderConfig{
			Name: "gw", Type: ProviderTypeOpenAI, APIKey: "sk-WRONG-secret",
			BaseURL: srv.URL,
		})
		if res.OK || res.Status != http.StatusUnauthorized {
			t.Fatalf("res = %+v, want 401 failure", res)
		}
		if strings.Contains(res.Detail, "sk-WRONG-secret") {
			t.Fatalf("detail leaks the key: %q", res.Detail)
		}
	})
}

func TestProbeProviderOllamaNeedsNoKey(t *testing.T) {
	srv := catalogServer(t, "", "llama3:latest")
	defer srv.Close()
	res := ProbeProvider(context.Background(), ProviderConfig{
		Name: "local", Type: ProviderTypeOllama, BaseURL: srv.URL,
	})
	if !res.OK || res.ServedModelCount != 1 {
		t.Fatalf("res = %+v, want OK with 1 served", res)
	}
}

func TestProbeProviderAnthropicHeaders(t *testing.T) {
	var gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-8"}]}`))
	}))
	defer srv.Close()
	res := ProbeProvider(context.Background(), ProviderConfig{
		Name: "anthro", Type: ProviderTypeAnthropic, APIKey: "sk-ant-x", BaseURL: srv.URL,
	})
	if !res.OK || gotKey != "sk-ant-x" || gotVersion == "" {
		t.Fatalf("res=%+v gotKey=%q gotVersion=%q — want anthropic auth headers", res, gotKey, gotVersion)
	}
}

func TestProbeProviderUnreachable(t *testing.T) {
	res := ProbeProvider(context.Background(), ProviderConfig{
		Name: "dead", Type: ProviderTypeOpenAI, APIKey: "sk-x",
		BaseURL: "http://127.0.0.1:1", // reserved port — nothing listens
	})
	if res.OK {
		t.Fatalf("res = %+v, want failure for unreachable endpoint", res)
	}
	if !strings.Contains(res.Detail, "unreachable") {
		t.Fatalf("detail = %q, want unreachable message", res.Detail)
	}
	if strings.Contains(res.Detail, "sk-x") {
		t.Fatalf("detail leaks the key: %q", res.Detail)
	}
}

// The OpenRouter probe hits the key-metadata endpoint (auth check only — the
// public catalog proves nothing) and honors the base-URL seam via BaseURL.
func TestProbeProviderOpenRouterKeyEndpoint(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":{"label":"test key"}}`))
	}))
	defer srv.Close()
	res := ProbeProvider(context.Background(), ProviderConfig{
		Name: "openrouter", Type: ProviderTypeOpenRouter, APIKey: "sk-or-x", BaseURL: srv.URL,
	})
	if !res.OK || path != "/api/v1/key" || auth != "Bearer sk-or-x" {
		t.Fatalf("res=%+v path=%q auth set=%v — want OK via /api/v1/key", res, path, auth != "")
	}
	if !strings.Contains(res.Detail, "key accepted") {
		t.Fatalf("detail = %q, want key-accepted summary", res.Detail)
	}
}
