package agentcore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// The provider type table (#1703) is the one list of supported types. The
// load-bearing assertions: every listed type can be built and probed (so a
// type cannot be added to the table without a constructor), the required
// fields are enforced from the table, and the two OpenAI-SDK-backed types added
// with it send the configured key, and nothing from the host's own OPENAI_*
// environment, to the configured endpoint, on both the Generate and the Stream
// path.

func TestEveryProviderTypeBuildsAndProbes(t *testing.T) {
	for _, typ := range ProviderTypes() {
		t.Run(string(typ), func(t *testing.T) {
			cfg := ProviderConfig{Name: "p", Type: typ, APIKey: "k", BaseURL: "http://203.0.113.10/v1"}
			if _, err := buildProvider(cfg, DefaultProviderHeaders); err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			if endpoint, _, _ := probeTarget(cfg); endpoint == "" {
				t.Fatal("probeTarget has no endpoint for this type")
			}
		})
	}
}

func TestProviderTypeTable(t *testing.T) {
	got := ProviderTypeChoices()
	if got != "openrouter|anthropic|openai|ollama|openaicompat|vercel" {
		t.Fatalf("ProviderTypeChoices() = %q", got)
	}
	for _, tc := range []struct {
		typ                ProviderType
		needsKey, needsURL bool
	}{
		{ProviderTypeOpenRouter, true, false},
		{ProviderTypeAnthropic, true, false},
		{ProviderTypeOpenAI, true, false},
		{ProviderTypeOllama, false, false},
		{ProviderTypeOpenAICompat, false, true},
		{ProviderTypeVercel, true, false},
	} {
		spec, ok := LookupProviderType(string(tc.typ))
		if !ok || spec.APIKeyRequired != tc.needsKey || spec.BaseURLRequired != tc.needsURL {
			t.Errorf("LookupProviderType(%q) = %+v, %v; want key=%v url=%v", tc.typ, spec, ok, tc.needsKey, tc.needsURL)
		}
	}
	for _, name := range []string{"", "bogus", "OpenAI", " openai"} {
		if _, ok := LookupProviderType(name); ok {
			t.Errorf("LookupProviderType(%q) matched; callers normalize, the table does not", name)
		}
	}
}

// chatCompletionServer answers chat completions, streamed (SSE) or not,
// and records the request path and headers of every call.
func chatCompletionServer(t *testing.T) (*httptest.Server, func() (paths []string, headers []http.Header)) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"po"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
				`[DONE]`,
			} {
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-1", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "pong"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() ([]string, []http.Header) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...), append([]http.Header(nil), headers...)
	}
}

// callBothWays runs one Generate and one Stream call through the provider
// cfg builds, and returns the text of each.
func callBothWays(t *testing.T, cfg ProviderConfig, model string) (generated, streamed string) {
	t.Helper()
	p, err := buildProvider(cfg, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), model)
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}}
	resp, err := lm.Generate(context.Background(), call)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	stream, err := lm.Stream(context.Background(), call)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var b strings.Builder
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			t.Fatalf("stream error: %v", part.Error)
		}
		if part.Type == fantasy.StreamPartTypeTextDelta {
			b.WriteString(part.Delta)
		}
	}
	return resp.Content.Text(), b.String()
}

// ambientOpenAIEnv is what a host can carry for its own OpenAI setup, all of
// which the OpenAI Go SDK copies onto every request unless told otherwise.
// OPENAI_BASE_URL is set per test, to a server that must never be called.
// The Content-Type custom header shares its name with one the SDK sets
// itself: stripping the custom headers must not strip the SDK's own.
var ambientOpenAIEnv = map[string]string{
	"OPENAI_API_KEY":        "sk-env-must-not-leak",
	"OPENAI_ADMIN_KEY":      "sk-admin-env-must-not-leak",
	"OPENAI_ORG_ID":         "org-env-must-not-leak",
	"OPENAI_PROJECT_ID":     "proj-env-must-not-leak",
	"OPENAI_CUSTOM_HEADERS": "X-Proxy-Auth: proxy-env-must-not-leak\nAuthorization: Bearer custom-env-must-not-leak\nContent-Type: application/json",
}

func TestOpenAICompatAndVercelReachTheConfiguredEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		cfg      func(base string) ProviderConfig
		model    string
		wantAuth string
	}{
		{
			name: "openaicompat without a key sends none",
			cfg: func(base string) ProviderConfig {
				return ProviderConfig{Name: "vllm", Type: ProviderTypeOpenAICompat, BaseURL: base}
			},
			model: "qwen3-coder", wantAuth: "",
		},
		{
			name: "openaicompat with a key sends it",
			cfg: func(base string) ProviderConfig {
				return ProviderConfig{Name: "vllm", Type: ProviderTypeOpenAICompat, APIKey: "sk-compat", BaseURL: base}
			},
			model: "qwen3-coder", wantAuth: "Bearer sk-compat",
		},
		{
			name: "vercel sends its key to the overridden base",
			cfg: func(base string) ProviderConfig {
				return ProviderConfig{Name: "gateway", Type: ProviderTypeVercel, APIKey: "vck-gateway", BaseURL: base}
			},
			model: "anthropic/claude-opus-4.8", wantAuth: "Bearer vck-gateway",
		},
	}
	for _, env := range []string{"the host's OPENAI_* env set", "no OPENAI_* env"} {
		for _, tc := range cases {
			t.Run(env+"/"+tc.name, func(t *testing.T) {
				trapHits := 0
				trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					trapHits++
					w.WriteHeader(http.StatusTeapot)
				}))
				t.Cleanup(trap.Close)
				vars := map[string]string{"OPENAI_BASE_URL": trap.URL + "/v1"}
				for k, v := range ambientOpenAIEnv {
					vars[k] = v
				}
				for k, v := range vars {
					t.Setenv(k, v)
					if env == "no OPENAI_* env" {
						_ = os.Unsetenv(k)
					}
				}
				srv, calls := chatCompletionServer(t)
				generated, streamed := callBothWays(t, tc.cfg(srv.URL+"/v1"), tc.model)
				if generated != "pong" || streamed != "pong" {
					t.Fatalf("Generate text = %q, Stream text = %q, want pong for both", generated, streamed)
				}
				paths, headers := calls()
				if len(paths) != 2 {
					t.Fatalf("paths = %v, want one Generate and one Stream call", paths)
				}
				for i, h := range headers {
					if paths[i] != "/v1/chat/completions" {
						t.Errorf("call %d went to %q, want /v1/chat/completions", i, paths[i])
					}
					if got := h.Get("Authorization"); got != tc.wantAuth {
						t.Errorf("call %d: Authorization = %q, want %q", i, got, tc.wantAuth)
					}
					if got := h.Get("Content-Type"); got != "application/json" {
						t.Errorf("call %d: Content-Type = %q, want the SDK's application/json", i, got)
					}
					for name, values := range h {
						for _, v := range values {
							if strings.Contains(v, "must-not-leak") {
								t.Errorf("call %d: header %s carries the host's OPENAI_* env value %q", i, name, v)
							}
						}
					}
				}
				if trapHits != 0 {
					t.Errorf("OPENAI_BASE_URL's server got %d request(s); the configured base_url must win", trapHits)
				}
			})
		}
	}
}
