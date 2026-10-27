package agentcore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/openai"
)

// The provider type table (#1703) is the one list of supported types. The
// load-bearing assertions: every listed type can be built and probed (so a
// type cannot be added to the table without a constructor), the required
// fields are enforced from the table, and the OpenAI-SDK-backed types added
// with it (openaicompat, vercel, azure) send the configured key, and nothing
// from the host's own OPENAI_* environment, to the configured endpoint, on both
// the Generate and the Stream path.

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
	if got != "openrouter|anthropic|openai|ollama|openaicompat|vercel|azure" {
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
		{ProviderTypeAzure, true, true},
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
// itself: stripping the custom headers must not strip the SDK's own. The
// Api-Key one shares its name with the header azure sends its key in: it must
// neither reach the other types nor replace azure's own key.
var ambientOpenAIEnv = map[string]string{
	"OPENAI_API_KEY":        "sk-env-must-not-leak",
	"OPENAI_ADMIN_KEY":      "sk-admin-env-must-not-leak",
	"OPENAI_ORG_ID":         "org-env-must-not-leak",
	"OPENAI_PROJECT_ID":     "proj-env-must-not-leak",
	"OPENAI_CUSTOM_HEADERS": "X-Proxy-Auth: proxy-env-must-not-leak\nAuthorization: Bearer custom-env-must-not-leak\nApi-Key: apikey-env-must-not-leak\nContent-Type: application/json",
}

func TestOpenAISDKTypesReachTheConfiguredEndpoint(t *testing.T) {
	cases := []struct {
		name       string
		cfg        func(base string) ProviderConfig
		model      string
		wantAuth   string // the Authorization header
		wantAPIKey string // the Api-Key header (azure's)
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
		{
			// Not an Azure host, so the base_url is used as given (azureBaseURL).
			name: "azure sends its key as Api-Key and no Authorization",
			cfg: func(base string) ProviderConfig {
				return ProviderConfig{Name: "az", Type: ProviderTypeAzure, APIKey: "az-key", BaseURL: base}
			},
			model: "gpt-5-deployment", wantAuth: "", wantAPIKey: "az-key",
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
					if got := h.Values("Api-Key"); strings.Join(got, ",") != tc.wantAPIKey {
						t.Errorf("call %d: Api-Key = %q, want %q", i, got, tc.wantAPIKey)
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

// recordingDoer answers every request with one canned chat completion and
// records it and its body, so a provider aimed at a real Azure host can run
// offline.
type recordingDoer struct {
	reqs   []*http.Request
	bodies []map[string]any
}

func (d *recordingDoer) Do(r *http.Request) (*http.Response, error) {
	d.reqs = append(d.reqs, r)
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	d.bodies = append(d.bodies, body)
	reply := `{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
		Request:    r,
	}, nil
}

// generateOnce makes one call on deployment with a token limit and a
// temperature, the two parameters fantasy sends differently to a reasoning
// model.
func generateOnce(t *testing.T, p fantasy.Provider, deployment string) {
	t.Helper()
	lm, err := p.LanguageModel(context.Background(), deployment)
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	maxOut, temp := int64(16384), 0.3
	if _, err := lm.Generate(context.Background(), fantasy.Call{
		Prompt:          fantasy.Prompt{fantasy.NewUserMessage("ping")},
		MaxOutputTokens: &maxOut,
		Temperature:     &temp,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

// fleet builds azure from fantasy's openai provider instead of calling
// azure.New, to add the host-header removals azure.New cannot take. With no
// OPENAI_* env set there is nothing to remove, so the two must send the same
// request: same URL, same headers and the same body, for a reasoning
// deployment and another one. A fantasy upgrade that changes how azure parses
// its endpoint, sends its key or shapes its requests fails here.
func TestAzureMatchesFantasyAzure(t *testing.T) {
	for k := range ambientOpenAIEnv {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	for _, tc := range []struct{ base, wantURL string }{
		{"https://myres.openai.azure.com", "https://myres.openai.azure.com/openai/v1/chat/completions"},
		{"https://myres.openai.azure.com/", "https://myres.openai.azure.com/openai/v1/chat/completions"},
		// The full target URI the portal shows for a deployment.
		{"https://myres.openai.azure.com/openai/deployments/gpt-5/chat/completions?api-version=2025-01-01-preview", "https://myres.openai.azure.com/openai/v1/chat/completions"},
		{"https://myres.cognitiveservices.azure.com/", "https://myres.openai.azure.com/openai/v1/chat/completions"},
		{"myres.services.ai.azure.com/api/projects/p1", "https://myres.openai.azure.com/openai/v1/chat/completions"},
		// Not an Azure host (a proxy in front of one): used as given.
		{"https://gateway.example.com/azure/v1", "https://gateway.example.com/azure/v1/chat/completions"},
		{"gateway.example.com/v1", "https://gateway.example.com/v1/chat/completions"},
	} {
		for _, deployment := range []string{"my-deployment", "gpt-5-prod"} {
			t.Run(tc.base+"/"+deployment, func(t *testing.T) {
				theirs, ours := &recordingDoer{}, &recordingDoer{}
				p, err := azure.New(azure.WithBaseURL(tc.base), azure.WithAPIKey("az-key"), azure.WithHTTPClient(theirs))
				if err != nil {
					t.Fatalf("azure.New: %v", err)
				}
				generateOnce(t, p, deployment)
				q, err := openai.New(append(azureOpenAIOptions("az-key", tc.base), openai.WithHTTPClient(ours))...)
				if err != nil {
					t.Fatalf("openai.New: %v", err)
				}
				generateOnce(t, q, deployment)
				if len(theirs.reqs) != 1 || len(ours.reqs) != 1 {
					t.Fatalf("requests: fantasy azure %d, fleet %d; want one each", len(theirs.reqs), len(ours.reqs))
				}
				if got := ours.reqs[0].URL.String(); got != tc.wantURL || got != theirs.reqs[0].URL.String() {
					t.Errorf("fleet URL = %q, fantasy azure URL = %q, want both %q", got, theirs.reqs[0].URL, tc.wantURL)
				}
				if !reflect.DeepEqual(ours.reqs[0].Header, theirs.reqs[0].Header) {
					t.Errorf("headers differ:\nfleet:          %v\nfantasy azure:  %v", ours.reqs[0].Header, theirs.reqs[0].Header)
				}
				if !reflect.DeepEqual(ours.bodies[0], theirs.bodies[0]) {
					t.Errorf("bodies differ:\nfleet:          %v\nfantasy azure:  %v", ours.bodies[0], theirs.bodies[0])
				}
				if got := ours.reqs[0].Header.Get("Api-Key"); got != "az-key" {
					t.Errorf("Api-Key = %q, want the key", got)
				}
			})
		}
	}
}

// fantasy tells a reasoning model by its id alone, and on azure the id is a
// deployment name the admin chose. Only a name that carries the model gets
// the parameters reasoning models require (max_completion_tokens, no
// temperature); Azure rejects max_tokens and a non-default temperature for
// them. docs/PROVIDERS.md, the admin form and the chat guide state this naming
// rule, and PROVIDERS.md spells out the match and that GPT-6 is outside it; if
// fantasy gains another way to mark a reasoning model, or widens the match,
// this fails and they should change with it.
func TestAzureReasoningDeploymentsAreToldByName(t *testing.T) {
	for _, tc := range []struct {
		deployment string
		reasoning  bool
	}{
		{"gpt-5-prod", true},
		{"GPT-5-prod", true}, // "gpt-5" matches in any case
		{"o4-mini", true},
		{"prod-o3", true},
		{"gpt-oss-120b", true},
		{"gpt-4o-prod", false},
		{"prod", false},           // a GPT-5 deployment named like this fails every turn
		{"gpt5-prod", false},      // the model's name without its hyphen
		{"O4-mini", false},        // the o-series match is case-sensitive
		{"gpt-6-luna-pro", false}, // a newer reasoning family: no name helps
	} {
		t.Run(tc.deployment, func(t *testing.T) {
			rec := &recordingDoer{}
			p, err := openai.New(append(azureOpenAIOptions("az-key", "https://res.openai.azure.com"), openai.WithHTTPClient(rec))...)
			if err != nil {
				t.Fatalf("openai.New: %v", err)
			}
			lm, err := p.LanguageModel(context.Background(), tc.deployment)
			if err != nil {
				t.Fatalf("LanguageModel: %v", err)
			}
			maxOut, temp := int64(16384), 0.3
			if _, err := lm.Generate(context.Background(), fantasy.Call{
				Prompt:          fantasy.Prompt{fantasy.NewUserMessage("ping")},
				MaxOutputTokens: &maxOut,
				Temperature:     &temp,
			}); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			body := rec.bodies[0]
			_, maxTokens := body["max_tokens"]
			_, maxCompletion := body["max_completion_tokens"]
			_, temperature := body["temperature"]
			if tc.reasoning && (maxTokens || temperature || !maxCompletion) {
				t.Errorf("reasoning deployment %q sent max_tokens=%v temperature=%v max_completion_tokens=%v; want only max_completion_tokens",
					tc.deployment, maxTokens, temperature, maxCompletion)
			}
			if !tc.reasoning && (!maxTokens || !temperature || maxCompletion) {
				t.Errorf("deployment %q sent max_tokens=%v temperature=%v max_completion_tokens=%v; want max_tokens and temperature",
					tc.deployment, maxTokens, temperature, maxCompletion)
			}
		})
	}
}
