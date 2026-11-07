package agentcore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// The bedrock type (#1703) sends the entry's own key, and nothing from the
// host's AWS or Anthropic environment, to the endpoint its region or base_url
// names, on both the Generate and the Stream path. Its region and base_url
// are checked before any of that, since the region becomes part of a host
// name.

// ambientBedrockEnv is what a host can carry for its own AWS and Anthropic
// setup. The Anthropic SDK's Bedrock adapter sends AWS_BEARER_TOKEN_BEDROCK in
// place of the key it is given; the AWS SDK reads the rest when it loads the
// host's configuration; the Anthropic SDK reads the ANTHROPIC_* ones for its
// first-party API. ANTHROPIC_BASE_URL is set per test, to a server that must
// never be called.
var ambientBedrockEnv = map[string]string{
	"AWS_BEARER_TOKEN_BEDROCK": "bearer-env-must-not-leak",
	"AWS_ACCESS_KEY_ID":        "AKIAENVMUSTNOTLEAK",
	"AWS_SECRET_ACCESS_KEY":    "secret-env-must-not-leak",
	"AWS_SESSION_TOKEN":        "session-env-must-not-leak",
	"AWS_REGION":               "eu-north-1",
	"AWS_DEFAULT_REGION":       "eu-north-1",
	"AWS_PROFILE":              "profile-env-must-not-leak",
	"ANTHROPIC_API_KEY":        "sk-ant-env-must-not-leak",
	"ANTHROPIC_AUTH_TOKEN":     "token-env-must-not-leak",
	"ANTHROPIC_CUSTOM_HEADERS": "X-Proxy-Auth: proxy-env-must-not-leak",
}

// bedrockStreamFrame encodes a "chunk" event of the AWS event-stream framing
// InvokeModelWithResponseStream answers in, whose payload carries one
// Anthropic stream event, base64-encoded.
func bedrockStreamFrame(event string) []byte {
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	return eventStreamFrame([][2]string{{":event-type", "chunk"}, {":content-type", "application/json"}, {":message-type", "event"}}, payload)
}

// bedrockExceptionFrame encodes an exception Bedrock sends inside a stream,
// such as a throttlingException.
func bedrockExceptionFrame(exception, message string) []byte {
	payload, _ := json.Marshal(map[string]string{"message": message})
	return eventStreamFrame([][2]string{{":exception-type", exception}, {":content-type", "application/json"}, {":message-type", "exception"}}, payload)
}

// eventStreamFrame encodes one message of AWS's event-stream framing: the
// total and header lengths, a CRC32 of those, the headers (string values), the
// payload and a CRC32 of everything before it.
func eventStreamFrame(hs [][2]string, payload []byte) []byte {
	var headers bytes.Buffer
	for _, h := range hs {
		headers.WriteByte(byte(len(h[0])))
		headers.WriteString(h[0])
		headers.WriteByte(7) // string value
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(h[1])))
		headers.WriteString(h[1])
	}
	var msg bytes.Buffer
	_ = binary.Write(&msg, binary.BigEndian, uint32(12+headers.Len()+len(payload)+4))
	_ = binary.Write(&msg, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&msg, binary.BigEndian, crc32.ChecksumIEEE(msg.Bytes()))
	msg.Write(headers.Bytes())
	msg.Write(payload)
	_ = binary.Write(&msg, binary.BigEndian, crc32.ChecksumIEEE(msg.Bytes()))
	return msg.Bytes()
}

// bedrockCall is one request a fake Bedrock endpoint received.
type bedrockCall struct {
	path   string
	header http.Header
	body   map[string]any
}

// bedrockServer serves bedrockHandler.
func bedrockServer(t *testing.T) (*httptest.Server, func() []bedrockCall) {
	t.Helper()
	h, calls := bedrockHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, calls
}

// bedrockHandler answers InvokeModel and InvokeModelWithResponseStream like
// Bedrock does for a Claude model, and records every call.
func bedrockHandler() (http.Handler, func() []bedrockCall) {
	var mu sync.Mutex
	var calls []bedrockCall
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, bedrockCall{path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream") {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			for _, ev := range []string{
				`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			} {
				_, _ = w.Write(bedrockStreamFrame(ev))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude",`+
			`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","stop_sequence":null,`+
			`"usage":{"input_tokens":3,"output_tokens":1}}`)
	})
	return h, func() []bedrockCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]bedrockCall(nil), calls...)
	}
}

// A bedrock entry sends its own key as a Bearer token to InvokeModel at its
// base_url, on both paths, in Bedrock's shape (the model in the path, an
// anthropic_version in the body), with or without the host's environment:
// the host's AWS_BEARER_TOKEN_BEDROCK in particular must not replace the
// entry's key. Without ANTHROPIC_API_KEY, the Anthropic SDK's environment
// defaults would go on to read ANTHROPIC_AUTH_TOKEN and
// ANTHROPIC_CUSTOM_HEADERS, so that case runs on its own.
func TestBedrockReachesTheConfiguredEndpoint(t *testing.T) {
	const model = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	for _, env := range []string{"the host's AWS and Anthropic env set", "the same without ANTHROPIC_API_KEY", "no such env"} {
		t.Run(env, func(t *testing.T) {
			var trapHits atomic.Int32
			trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				trapHits.Add(1)
				w.WriteHeader(http.StatusTeapot)
			}))
			t.Cleanup(trap.Close)
			vars := map[string]string{"ANTHROPIC_BASE_URL": trap.URL}
			for k, v := range ambientBedrockEnv {
				vars[k] = v
			}
			for k, v := range vars {
				t.Setenv(k, v)
				if env == "no such env" || env == "the same without ANTHROPIC_API_KEY" && k == "ANTHROPIC_API_KEY" {
					_ = os.Unsetenv(k)
				}
			}
			srv, calls := bedrockServer(t)
			cfg := ProviderConfig{Name: "bedrock-prod", Type: ProviderTypeBedrock, APIKey: "bedrock-row-key", BaseURL: srv.URL, Region: "us-west-2"}
			generated, streamed := callBothWays(t, cfg, model)
			if generated != "pong" || streamed != "pong" {
				t.Fatalf("Generate text = %q, Stream text = %q, want pong for both", generated, streamed)
			}
			got := calls()
			want := []string{"/model/" + model + "/invoke", "/model/" + model + "/invoke-with-response-stream"}
			if len(got) != 2 || got[0].path != want[0] || got[1].path != want[1] {
				t.Fatalf("calls = %+v, want %v", got, want)
			}
			for i, c := range got {
				if auth := c.header.Values("Authorization"); strings.Join(auth, ",") != "Bearer bedrock-row-key" {
					t.Errorf("call %d: Authorization = %q, want the entry's key", i, auth)
				}
				if v := c.body["anthropic_version"]; v != "bedrock-2023-05-31" {
					t.Errorf("call %d: anthropic_version = %v, want bedrock-2023-05-31", i, v)
				}
				for _, field := range []string{"model", "stream"} {
					if _, ok := c.body[field]; ok {
						t.Errorf("call %d: body carries %q, which Bedrock takes from the path", i, field)
					}
				}
				for name, values := range c.header {
					for _, v := range values {
						if strings.Contains(v, "must-not-leak") || strings.Contains(v, "MUSTNOTLEAK") {
							t.Errorf("call %d: header %s carries the host's env value %q", i, name, v)
						}
					}
				}
			}
			if n := trapHits.Load(); n != 0 {
				t.Errorf("ANTHROPIC_BASE_URL's server got %d request(s)", n)
			}
		})
	}
}

// With no base_url, turns and the connection test both go to the entry's
// Region's public endpoint, us-east-1's when it names none, and never to the
// Region in the host's AWS_REGION. Recording on bedrockHTTPClient's transport
// keeps the requests offline.
func TestBedrockRegionPicksTheEndpoint(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-north-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")
	for _, tc := range []struct{ region, host string }{
		{"eu-west-1", "bedrock-runtime.eu-west-1.amazonaws.com"},
		{"", "bedrock-runtime.us-east-1.amazonaws.com"},
	} {
		t.Run("region "+tc.region, func(t *testing.T) {
			rec := &recordingDoer{}
			prev := bedrockHTTPClient.Transport
			bedrockHTTPClient.Transport = rec
			t.Cleanup(func() { bedrockHTTPClient.Transport = prev })
			cfg := ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "k", Region: tc.region}
			p, err := buildProvider(cfg)
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			lm, err := p.LanguageModel(context.Background(), "anthropic.claude-haiku-4-5-20251001-v1:0")
			if err != nil {
				t.Fatalf("LanguageModel: %v", err)
			}
			// The canned reply is not an Anthropic message, so Generate fails;
			// only where the request went matters here.
			_, _ = lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}})
			if len(rec.reqs) != 1 {
				t.Fatalf("requests = %d, want 1", len(rec.reqs))
			}
			if got := rec.reqs[0].URL; got.Scheme != "https" || got.Host != tc.host {
				t.Fatalf("turn went to %s, want https://%s", got, tc.host)
			}
			if endpoint, _, _ := probeTarget(cfg); endpoint != "https://"+tc.host+"/async-invoke?maxResults=1" {
				t.Fatalf("probe endpoint = %q, want the turn's host", endpoint)
			}
		})
	}
}

// A turn whose endpoint redirects to another origin fails there: the request,
// conversation and all, never reaches the other host. The other host answers
// like Bedrock, so a followed redirect would succeed. The failure is the same
// non-retryable refusal an anthropic entry gets (refuseCrossOriginRedirect).
func TestBedrockTurnsDoNotFollowRedirectsToAnotherHost(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			other, calls := bedrockServer(t)
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+r.URL.RequestURI(), status)
			}))
			t.Cleanup(redirect.Close)
			p, err := buildProvider(ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "k", BaseURL: redirect.URL})
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			lm, err := p.LanguageModel(context.Background(), "anthropic.claude-haiku-4-5-20251001-v1:0")
			if err != nil {
				t.Fatalf("LanguageModel: %v", err)
			}
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}}
			const refusal = "refused a redirect to another origin"
			_, err = lm.Generate(context.Background(), call)
			if err == nil || !strings.Contains(err.Error(), refusal) {
				t.Errorf("Generate err = %v, want one naming the refused redirect", err)
			}
			var providerErr *fantasy.ProviderError
			if !errors.As(err, &providerErr) || providerErr.IsRetryable() || providerErr.StatusCode != status {
				t.Errorf("Generate err = %#v, want a non-retryable ProviderError with status %d", providerErr, status)
			}
			if class, _ := classifyStreamError(err); class != streamErrorFatal {
				t.Errorf("classifyStreamError = %v, want fatal", class)
			}
			stream, streamErr := lm.Stream(context.Background(), call)
			if streamErr == nil {
				for part := range stream {
					if part.Type == fantasy.StreamPartTypeTextDelta {
						t.Error("Stream got text through a redirect to another host")
					}
					if part.Type == fantasy.StreamPartTypeError {
						streamErr = part.Error
					}
				}
			}
			if streamErr == nil || !strings.Contains(streamErr.Error(), refusal) {
				t.Errorf("Stream err = %v, want one naming the refused redirect", streamErr)
			}
			if got := calls(); len(got) != 0 {
				t.Fatalf("a turn followed a redirect to another host: %+v", got)
			}
		})
	}
}

// The bedrock client keeps the response-header timeout the Anthropic SDK's own
// default client sets, which a client of fleet's own replaces.
func TestBedrockClientKeepsTheSDKHeaderTimeout(t *testing.T) {
	transport, ok := bedrockHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("bedrockHTTPClient.Transport is %T, want *http.Transport", bedrockHTTPClient.Transport)
	}
	if transport.ResponseHeaderTimeout != 10*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %v, want 10m", transport.ResponseHeaderTimeout)
	}
}

func TestCheckProviderFields(t *testing.T) {
	bedrock, _ := LookupProviderType(string(ProviderTypeBedrock))
	openai, _ := LookupProviderType(string(ProviderTypeOpenAI))
	for _, tc := range []struct {
		name          string
		spec          ProviderTypeSpec
		base, region  string
		wantErrSubstr string // "" = accepted
	}{
		{"bedrock with no region", bedrock, "", "", ""},
		{"bedrock region", bedrock, "", "eu-central-1", ""},
		{"GovCloud region", bedrock, "", "us-gov-west-1", ""},
		{"region with spaces around it", bedrock, "", " ap-southeast-4 ", ""},
		{"capitalized region", bedrock, "", "US-EAST-1", "not an AWS Region name"},
		{"region that is a host", bedrock, "", "evil.example/x", "not an AWS Region name"},
		{"region that ends a host", bedrock, "", "us-east-1.example.com#", "not an AWS Region name"},
		{"region on another type", openai, "", "us-east-1", "applies only to a bedrock provider"},
		// A China or ISO Region's endpoints are not under amazonaws.com, where
		// the default endpoint would be built.
		{"China region with no base", bedrock, "", "cn-north-1", "outside amazonaws.com"},
		{"ISO region with no base", bedrock, "", "us-isob-east-1", "outside amazonaws.com"},
		{"China region with its endpoint", bedrock, "https://bedrock-runtime.cn-north-1.amazonaws.com.cn", "cn-north-1", ""},
		{"bedrock origin", bedrock, "https://bedrock-runtime.us-east-1.amazonaws.com", "", ""},
		{"bedrock origin with a slash and a port", bedrock, "http://203.0.113.10:8080/", "", ""},
		{"bedrock base with a path", bedrock, "https://gateway.example/bedrock", "", "no path"},
		{"bedrock base with a query", bedrock, "https://gateway.example/?x=1", "", "no path"},
		{"bedrock base with no scheme", bedrock, "bedrock-runtime.us-east-1.amazonaws.com", "", "no path"},
		// URL parsing keeps no trace of an empty query or fragment, which
		// would make the probe's URL differ from the turns'.
		{"bedrock base with an empty query", bedrock, "https://gateway.example?", "", "no path"},
		{"bedrock base with an empty fragment", bedrock, "https://gateway.example#", "", "no path"},
		{"bedrock base with credentials", bedrock, "https://user:pw@gateway.example", "", "no path"},
		{"another type keeps its path", openai, "https://gateway.example/v1", "", ""},
	} {
		err := CheckProviderFields(tc.spec, tc.base, tc.region)
		switch {
		case tc.wantErrSubstr == "" && err != nil:
			t.Errorf("%s: %v, want accepted", tc.name, err)
		case tc.wantErrSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr)):
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.wantErrSubstr)
		}
	}
	// buildProvider refuses the same entries, so the API's eager check does.
	if _, err := buildProvider(ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "k", Region: "evil.example/x"}); err == nil {
		t.Error("buildProvider accepted a region that is not a Region name")
	}
	if _, err := buildProvider(ProviderConfig{Name: "o", Type: ProviderTypeOpenAI, APIKey: "k", Region: "us-east-1"}); err == nil {
		t.Error("buildProvider accepted a region on an openai entry")
	}
	if _, err := buildProvider(ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "k", BaseURL: "https://gateway.example/bedrock"}); err == nil {
		t.Error("buildProvider accepted a bedrock base_url with a path")
	}
}

// A redirect within the endpoint's origin is followed on both paths, with the
// entry's key (not the host's AWS_BEARER_TOKEN_BEDROCK) and the request body
// sent again.
func TestBedrockTurnsFollowRedirectsWithinTheOrigin(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bearer-env-must-not-leak")
	const model = "anthropic.claude-haiku-4-5-20251001-v1:0"
	h, _ := bedrockHandler()
	var mu sync.Mutex
	var hops []bedrockCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		hops = append(hops, bedrockCall{path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, "/moved"+r.URL.EscapedPath(), http.StatusTemporaryRedirect)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	generated, streamed := callBothWays(t, ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "bedrock-row-key", BaseURL: srv.URL}, model)
	if generated != "pong" || streamed != "pong" {
		t.Fatalf("Generate text = %q, Stream text = %q, want pong for both through the redirect", generated, streamed)
	}
	want := []string{
		"/model/" + model + "/invoke", "/moved/model/" + model + "/invoke",
		"/model/" + model + "/invoke-with-response-stream", "/moved/model/" + model + "/invoke-with-response-stream",
	}
	if len(hops) != len(want) {
		t.Fatalf("hops = %+v, want %v", hops, want)
	}
	for i, h := range hops {
		if h.path != want[i] {
			t.Errorf("hop %d: path = %q, want %q", i, h.path, want[i])
		}
		if got := h.header.Get("Authorization"); got != "Bearer bedrock-row-key" {
			t.Errorf("hop %d: Authorization = %q, want the entry's key", i, got)
		}
		if h.body["anthropic_version"] != "bedrock-2023-05-31" || h.body["messages"] == nil {
			t.Errorf("hop %d: body = %v, want the request sent again", i, h.body)
		}
	}
}

// An exception Bedrock sends inside a stream for a temporary condition is
// retryable, so a configured fallback takes the turn over, as it does for an
// overloaded Anthropic stream. A validationException stays fatal.
func TestBedrockStreamExceptionsAreTransient(t *testing.T) {
	for _, tc := range []struct {
		exception string
		want      streamErrorClass
	}{
		{"throttlingException", streamErrorRetryExhausted},
		{"serviceUnavailableException", streamErrorRetryExhausted},
		{"internalServerException", streamErrorRetryExhausted},
		{"modelStreamErrorException", streamErrorRetryExhausted},
		{"modelTimeoutException", streamErrorRetryExhausted},
		{"validationException", streamErrorFatal},
	} {
		t.Run(tc.exception, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				_, _ = w.Write(bedrockStreamFrame(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`))
				_, _ = w.Write(bedrockExceptionFrame(tc.exception, "Too many requests, please wait before trying again."))
			}))
			t.Cleanup(srv.Close)
			p, err := buildProvider(ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "k", BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("buildProvider: %v", err)
			}
			lm, err := p.LanguageModel(context.Background(), "anthropic.claude-haiku-4-5-20251001-v1:0")
			if err != nil {
				t.Fatalf("LanguageModel: %v", err)
			}
			stream, err := lm.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}})
			if err == nil {
				for part := range stream {
					if part.Type == fantasy.StreamPartTypeError {
						err = part.Error
					}
				}
			}
			if err == nil {
				t.Fatal("the stream ended without the exception")
			}
			if class, _ := classifyStreamError(err); class != tc.want {
				t.Fatalf("classifyStreamError(%v) = %v, want %v", err, class, tc.want)
			}
		})
	}
}

// Fleet's prompt-cache markers reach a Bedrock request: the cache step marks a
// Bedrock Claude id, and fantasy's bedrock provider, being its anthropic
// provider, writes them into Bedrock's body as cache_control blocks.
func TestBedrockRequestsCarryCacheMarkers(t *testing.T) {
	const model = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	srv, calls := bedrockServer(t)
	p, err := buildProvider(ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), model)
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	msgs := []fantasy.Message{fantasy.NewSystemMessage("You are a test."), fantasy.NewUserMessage("ping")}
	_, prepared, err := PromptCachingStep(model)(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("PromptCachingStep: %v", err)
	}
	for _, prompt := range []fantasy.Prompt{msgs, prepared.Messages} {
		if _, err := lm.Generate(context.Background(), fantasy.Call{Prompt: prompt}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("calls = %d, want 2", len(got))
	}
	count := func(c bedrockCall) int {
		b, _ := json.Marshal(c.body)
		return strings.Count(string(b), `"cache_control"`)
	}
	if n := count(got[0]); n != 0 {
		t.Fatalf("an unmarked prompt carried %d cache_control block(s)", n)
	}
	if n := count(got[1]); n < 2 {
		t.Fatalf("the marked prompt carried %d cache_control block(s), want the system and the last user message", n)
	}
}

// When Bedrock refuses a turn, the error the run loop gets names neither the
// entry's key nor the host's AWS_BEARER_TOKEN_BEDROCK: not in its text, and
// not in the request or response fantasy keeps on a ProviderError.
func TestBedrockTurnErrorsCarryNoKey(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bearer-env-must-not-leak")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"The security token included in the request is invalid."}`)
	}))
	t.Cleanup(srv.Close)
	p, err := buildProvider(ProviderConfig{Name: "b", Type: ProviderTypeBedrock, APIKey: "bedrock-row-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "anthropic.claude-haiku-4-5-20251001-v1:0")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}}
	_, genErr := lm.Generate(context.Background(), call)
	var streamErr error
	if stream, err := lm.Stream(context.Background(), call); err != nil {
		streamErr = err
	} else {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeError {
				streamErr = part.Error
			}
		}
	}
	for name, err := range map[string]error{"Generate": genErr, "Stream": streamErr} {
		if err == nil {
			t.Fatalf("%s: no error from a 403", name)
		}
		texts := []string{err.Error()}
		var pe *fantasy.ProviderError
		if errors.As(err, &pe) {
			texts = append(texts, pe.URL, string(pe.RequestBody), string(pe.ResponseBody))
		}
		for _, s := range texts {
			if strings.Contains(s, "bedrock-row-key") || strings.Contains(s, "must-not-leak") {
				t.Errorf("%s: error carries a key: %q", name, s)
			}
		}
	}
}
