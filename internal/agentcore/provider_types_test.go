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
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
)

// The provider type table (#1703) is the one list of supported types. The
// load-bearing assertions: every listed type can be built and probed (so a
// type cannot be added to the table without a constructor), the required
// fields are enforced from the table, and the types added with it send the
// configured key, and nothing from the host's own environment, to the
// configured endpoint, on both the Generate and the Stream path: the OpenAI-SDK
// types (openaicompat, vercel, azure) nothing from OPENAI_*, and google nothing
// from the GOOGLE_*/GEMINI_* settings, never reaching Vertex AI.

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
	if got != "openrouter|anthropic|openai|ollama|openaicompat|vercel|azure|google" {
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
		{ProviderTypeGoogle, true, false},
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
				var trapHits atomic.Int32
				trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					trapHits.Add(1)
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
				if n := trapHits.Load(); n != 0 {
					t.Errorf("OPENAI_BASE_URL's server got %d request(s); the configured base_url must win", n)
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

// RoundTrip lets the same recorder stand in as an http.Client transport, for
// providers that take a client rather than a Doer (google).
func (d *recordingDoer) RoundTrip(r *http.Request) (*http.Response, error) { return d.Do(r) }

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

// Turns refuse a redirect to another host, as the connection test does
// (probeClient). The OpenAI SDK sends every request, redirect hops included,
// through a transport that rejects any origin but the configured one, so the
// Api-Key, which Go would copy to another host, never leaves the entry's
// endpoint. docs/LLM-PROVIDERS.md and the probe's redirect message rely on
// this; an SDK upgrade that drops the guard fails here.
func TestAzureTurnsDoNotFollowRedirectsToAnotherHost(t *testing.T) {
	var mu sync.Mutex
	elsewhere := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		elsewhere++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`)
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	p, err := buildProvider(ProviderConfig{Name: "az", Type: ProviderTypeAzure, APIKey: "az-key", BaseURL: redirecting.URL + "/openai/v1"}, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "gpt-5-prod")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}}
	if _, err := lm.Generate(context.Background(), call); err == nil {
		t.Error("Generate succeeded through a redirect to another host")
	}
	if stream, err := lm.Stream(context.Background(), call); err == nil {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeTextDelta {
				t.Error("Stream got text through a redirect to another host")
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if elsewhere != 0 {
		t.Fatalf("a turn followed a redirect to another host %d times", elsewhere)
	}
}

// ambientGoogleEnv is what a host can carry for its own Google setup, which
// the genai SDK reads when a client leaves the matching setting empty. Two of
// them point at a server that must never be called, set per test.
var ambientGoogleEnv = map[string]string{
	"GOOGLE_API_KEY":                 "google-env-must-not-leak",
	"GEMINI_API_KEY":                 "gemini-env-must-not-leak",
	"GOOGLE_GENAI_USE_VERTEXAI":      "true",
	"GOOGLE_GENAI_USE_ENTERPRISE":    "true",
	"GOOGLE_CLOUD_PROJECT":           "project-env-must-not-leak",
	"GOOGLE_CLOUD_LOCATION":          "location-env-must-not-leak",
	"GOOGLE_APPLICATION_CREDENTIALS": "/nonexistent/fleet-1703-adc.json",
}

// geminiServer answers generateContent and streamGenerateContent (SSE) like
// the Gemini API, and records each call's path and headers.
func geminiServer(t *testing.T) (*httptest.Server, func() (paths []string, headers []http.Header)) {
	t.Helper()
	h, calls := geminiHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, calls
}

// geminiHandler is geminiServer's handler.
func geminiHandler() (http.Handler, func() (paths []string, headers []http.Header)) {
	var mu sync.Mutex
	var paths []string
	var headers []http.Header
	chunk := func(text string, last bool) string {
		c := map[string]any{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": text}}}, "index": 0}
		resp := map[string]any{"candidates": []map[string]any{c}, "modelVersion": "gemini-2.5-flash"}
		if last {
			c["finishReason"] = "STOP"
			resp["usageMetadata"] = map[string]any{"promptTokenCount": 3, "candidatesTokenCount": 1, "totalTokenCount": 4}
		}
		b, _ := json.Marshal(resp)
		return string(b)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+chunk("po", false)+"\r\n\r\n")
			_, _ = io.WriteString(w, "data: "+chunk("ng", true)+"\r\n\r\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chunk("pong", true))
	})
	return h, func() ([]string, []http.Header) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...), append([]http.Header(nil), headers...)
	}
}

// A google entry sends its own key to the Gemini API at its base_url, on
// both paths, and nothing from the host's own Google environment: not its
// keys, not its Vertex switch or project, and not GOOGLE_GEMINI_BASE_URL.
func TestGoogleReachesTheConfiguredEndpoint(t *testing.T) {
	for _, env := range []string{"the host's Google env set", "no Google env"} {
		t.Run(env, func(t *testing.T) {
			var trapHits atomic.Int32
			trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				trapHits.Add(1)
				w.WriteHeader(http.StatusTeapot)
			}))
			t.Cleanup(trap.Close)
			vars := map[string]string{"GOOGLE_GEMINI_BASE_URL": trap.URL, "GOOGLE_VERTEX_BASE_URL": trap.URL}
			for k, v := range ambientGoogleEnv {
				vars[k] = v
			}
			for k, v := range vars {
				t.Setenv(k, v)
				if env == "no Google env" {
					_ = os.Unsetenv(k)
				}
			}
			srv, calls := geminiServer(t)
			cfg := ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "gemini-row-key", BaseURL: srv.URL}
			generated, streamed := callBothWays(t, cfg, "gemini-2.5-flash")
			if generated != "pong" || streamed != "pong" {
				t.Fatalf("Generate text = %q, Stream text = %q, want pong for both", generated, streamed)
			}
			paths, headers := calls()
			want := []string{"/v1beta/models/gemini-2.5-flash:generateContent", "/v1beta/models/gemini-2.5-flash:streamGenerateContent"}
			if strings.Join(paths, " ") != strings.Join(want, " ") {
				t.Fatalf("paths = %v, want %v", paths, want)
			}
			for i, h := range headers {
				if got := h.Values("X-Goog-Api-Key"); strings.Join(got, ",") != "gemini-row-key" {
					t.Errorf("call %d: x-goog-api-key = %q, want the entry's key", i, got)
				}
				if got := h.Get("Authorization"); got != "" {
					t.Errorf("call %d: Authorization = %q, want none", i, got)
				}
				for name, values := range h {
					for _, v := range values {
						if strings.Contains(v, "must-not-leak") {
							t.Errorf("call %d: header %s carries the host's Google env value %q", i, name, v)
						}
					}
				}
			}
			if n := trapHits.Load(); n != 0 {
				t.Errorf("GOOGLE_GEMINI_BASE_URL's server got %d request(s); the entry's base_url must win", n)
			}
		})
	}
}

// With no base_url a google entry still names the Gemini API itself, so the
// host's GOOGLE_GEMINI_BASE_URL cannot redirect its key. Recording on
// googleHTTPClient's transport keeps the request offline; fantasy reads it
// when the provider is built.
func TestGoogleDefaultEndpointIgnoresHostBaseURL(t *testing.T) {
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "http://203.0.113.10/trap")
	rec := &recordingDoer{}
	prev := googleHTTPClient.Transport
	googleHTTPClient.Transport = rec
	t.Cleanup(func() { googleHTTPClient.Transport = prev })
	p, err := buildProvider(ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "gemini-row-key"}, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	// The canned reply is not Gemini's shape, so Generate fails; only the URL
	// the request went to matters here.
	_, _ = lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}})
	if len(rec.reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(rec.reqs))
	}
	if got := rec.reqs[0].URL.String(); got != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("request URL = %q, want the Gemini API", got)
	}
}

// Turns refuse a redirect to another host, as the connection test does. Go
// copies x-goog-api-key to the host a redirect names (it drops only
// Authorization and cookies), so with fantasy's default http.DefaultClient
// an endpoint that redirected elsewhere would hand that host the key.
func TestGoogleTurnsDoNotFollowRedirectsToAnotherHost(t *testing.T) {
	other, calls := geminiServer(t)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	p, err := buildProvider(ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "gemini-row-key", BaseURL: redirecting.URL}, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "gemini-3.8-flash")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}}
	if _, err := lm.Generate(context.Background(), call); err == nil {
		t.Error("Generate succeeded through a redirect to another host")
	}
	if stream, err := lm.Stream(context.Background(), call); err == nil {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeTextDelta {
				t.Error("Stream got text through a redirect to another host")
			}
		}
	}
	if paths, _ := calls(); len(paths) != 0 {
		t.Fatalf("a turn followed a redirect to another host: %v", paths)
	}
}

// A redirect within the endpoint's origin is still followed, key and body
// included, on both paths.
func TestGoogleTurnsFollowRedirectsWithinTheOrigin(t *testing.T) {
	gemini, calls := geminiHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, moved := strings.CutPrefix(r.URL.RequestURI(), "/old"); moved {
			http.Redirect(w, r, rest, http.StatusTemporaryRedirect)
			return
		}
		gemini.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "gemini-row-key", BaseURL: srv.URL + "/old"}
	if generated, streamed := callBothWays(t, cfg, "gemini-3.8-flash"); generated != "pong" || streamed != "pong" {
		t.Fatalf("Generate text = %q, Stream text = %q, want pong for both", generated, streamed)
	}
	paths, headers := calls()
	want := []string{"/v1beta/models/gemini-3.8-flash:generateContent", "/v1beta/models/gemini-3.8-flash:streamGenerateContent"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i, h := range headers {
		if got := h.Get("X-Goog-Api-Key"); got != "gemini-row-key" {
			t.Errorf("call %d: x-goog-api-key = %q, want the entry's key", i, got)
		}
	}
}

// fantasy's google provider sends a model id naming Claude to Anthropic on
// Vertex AI, signed in with the host's Google credentials. A google entry
// refuses such an id before that lookup, whatever case it is written in.
func TestGoogleRefusesClaudeModelIDs(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/fleet-1703-adc.json")
	p, err := buildProvider(ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "k", BaseURL: "http://203.0.113.10"}, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, id := range []string{"anthropic/claude-opus-4.8", "claude-sonnet-4-6", "Claude-Opus"} {
		if _, err := p.LanguageModel(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not a Gemini API model") {
			t.Errorf("LanguageModel(%q) err = %v, want the not-a-Gemini-model refusal", id, err)
		}
	}
	if _, err := p.LanguageModel(context.Background(), "gemini-2.5-pro"); err != nil {
		t.Errorf("LanguageModel(gemini-2.5-pro): %v", err)
	}
}

// The signature that replay keeps reaches Gemini on the replayed call: a turn
// resumed from replayed messages (a context checkpoint, say) sends the call
// with its thoughtSignature, as the same turn's next step would have. The
// replay goes through geminiSignatureStep as a run's does, so a google model
// that step did not take for Gemini would lose the signature here.
func TestGoogleReplayedCallCarriesItsSignature(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],`+
			`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`)
	}))
	defer srv.Close()
	p, err := buildProvider(ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "k", BaseURL: srv.URL}, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "gemini-3-pro")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	replayed := messagesForReplay([]fantasy.Message{
		fantasy.NewUserMessage("task"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{ProviderOptions: fantasy.ProviderOptions{google.Name: &google.ReasoningMetadata{Signature: "sig-1", ToolID: "c1"}}},
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"command":"ls"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "ok"}},
		}},
	})
	_, step, err := geminiSignatureStep(context.Background(), fantasy.PrepareStepFunctionOptions{Model: lm, Messages: replayed})
	if err != nil {
		t.Fatalf("geminiSignatureStep: %v", err)
	}
	if step.Messages != nil {
		replayed = step.Messages
	}
	if _, err := lm.Generate(context.Background(), fantasy.Call{Prompt: replayed}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var req struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionCall     *struct{ Name string } `json:"functionCall"`
				ThoughtSignature string                 `json:"thoughtSignature"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	signed := false
	for _, c := range req.Contents {
		for _, part := range c.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == "bash" {
				signed = part.ThoughtSignature == "c2lnLTE=" // base64("sig-1")
			}
		}
	}
	if !signed {
		t.Fatalf("the replayed bash call went out without its thought signature: %s", body)
	}
}

// An object-rooted output_schema arrives JSON-decoded, its required list an
// []any, and fantasy's Gemini conversion keeps a tool's required list only as
// []string. The tool the terminal structured-output path hands a google model
// must still reach Gemini with its required fields, and the caller's schema
// must not change.
func TestGoogleStructuredOutputKeepsRequired(t *testing.T) {
	const schema = `{"type":"object","properties":{"answer":{"type":"string"},"note":{"type":"string"}},"required":["answer"]}`
	var decoded map[string]any
	if err := json.Unmarshal([]byte(schema), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, enveloped := terminalProviderSchema(decoded); enveloped {
		t.Fatal("an object-rooted schema was enveloped")
	}
	if _, still := decoded["required"].([]any); !still {
		t.Fatalf("terminalProviderSchema changed the caller's schema: %#v", decoded["required"])
	}
	// The tool the terminal path builds for a google model...
	captured := &terminalTestModel{provider: google.Name, model: "gemini-3.8-flash", responses: []*fantasy.Response{terminalToolResponse(`{"answer":"x"}`)}}
	eng, orch := testTerminalEngine(captured)
	if _, err := eng.generateTerminalStructuredOutput(t.Context(), captured, "", nil, json.RawMessage(schema), 1000, orch); err != nil {
		t.Fatalf("generateTerminalStructuredOutput: %v", err)
	}
	calls := captured.calls()
	if len(calls) != 1 || len(calls[0].Tools) != 1 {
		t.Fatalf("terminal path made %d call(s), want one with one tool", len(calls))
	}
	// ...sent to the Gemini API through a google entry.
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP","index":0}],`+
			`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`)
	}))
	defer srv.Close()
	p, err := buildProvider(ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "k", BaseURL: srv.URL}, DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "gemini-3.8-flash")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	if _, err := lm.Generate(context.Background(), fantasy.Call{
		Prompt:     fantasy.Prompt{fantasy.NewUserMessage("answer")},
		Tools:      calls[0].Tools,
		ToolChoice: calls[0].ToolChoice,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var req struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Parameters struct {
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Tools) == 0 || len(req.Tools[0].FunctionDeclarations) == 0 {
		t.Fatalf("request body %s: %v", body, err)
	}
	if got := req.Tools[0].FunctionDeclarations[0].Parameters.Required; strings.Join(got, ",") != "answer" {
		t.Fatalf("Gemini got required = %v, want [answer]", got)
	}
}
