package agentcore

// ADR-0067: a non-retryable 4xx the provider returned for one request/model
// pair promotes the configured fallback model from a safe checkpoint instead
// of dead-lettering the run, and the relayed upstream cause is named. Modeled
// on the 2026-09-16 Reklaim health-scan run: 45 clean tool steps on
// google/gemini-3.8-flash, then OpenRouter relayed a Google 400 as the opaque
// "bad request: Provider returned error" and the configured openai fallback
// was never consulted.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// openRouterRelayBody is an httputil response dump of OpenRouter's relay
// envelope for an upstream Google 400, as fantasy's openai adapter stores it
// in ProviderError.ResponseBody.
const openRouterRelayBody = "HTTP/2.0 400 Bad Request\r\nContent-Type: application/json\r\nX-Request-Id: req-123\r\n\r\n" +
	`{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"{\"error\":{\"code\":400,\"message\":\"Please ensure that function call turn comes immediately after a user turn or after a function response turn.\",\"status\":\"INVALID_ARGUMENT\"}}","provider_name":"Google"}},"user_id":"user_abc"}`

func openRouterRelayed400() *fantasy.ProviderError {
	return &fantasy.ProviderError{
		StatusCode:   http.StatusBadRequest,
		Title:        "bad request",
		Message:      "Provider returned error",
		ResponseBody: []byte(openRouterRelayBody),
	}
}

func TestClassifyProviderRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *fantasy.ProviderError
		want streamErrorClass
	}{
		{"400 is a rejection", &fantasy.ProviderError{StatusCode: 400}, streamErrorProviderRejected},
		{"403 is a rejection", &fantasy.ProviderError{StatusCode: 403}, streamErrorProviderRejected},
		{"404 is a rejection", &fantasy.ProviderError{StatusCode: 404}, streamErrorProviderRejected},
		{"422 is a rejection", &fantasy.ProviderError{StatusCode: 422}, streamErrorProviderRejected},
		{"401 credentials stay fatal", &fantasy.ProviderError{StatusCode: 401}, streamErrorFatal},
		{"402 billing stays fatal", &fantasy.ProviderError{StatusCode: 402}, streamErrorFatal},
		{"AuthError flag stays fatal", &fantasy.ProviderError{StatusCode: 400, AuthError: true}, streamErrorFatal},
		{"unknown status stays fatal", &fantasy.ProviderError{Message: "no status"}, streamErrorFatal},
		{"429 still retry-exhausted", &fantasy.ProviderError{StatusCode: 429}, streamErrorRetryExhausted},
		{"500 still retry-exhausted", &fantasy.ProviderError{StatusCode: 500}, streamErrorRetryExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			class, pe := classifyStreamError(tc.err)
			if class != tc.want {
				t.Fatalf("class = %s, want %s", class, tc.want)
			}
			if pe != tc.err {
				t.Fatal("provider error not surfaced alongside the class")
			}
		})
	}
	// Mid-stream SSE envelopes carrying an explicit 4xx code ride the same rule.
	class, _ := classifyStreamError(errors.New(`received error while streaming: {"code":400,"message":"Provider returned error"}`))
	if class != streamErrorProviderRejected {
		t.Fatalf("SSE 400 class = %s, want provider_rejected", class)
	}
	class, _ = classifyStreamError(errors.New(`received error while streaming: {"code":401,"message":"bad key"}`))
	if class != streamErrorFatal {
		t.Fatalf("SSE 401 class = %s, want fatal", class)
	}
	if streamErrorProviderRejected.String() != "provider_rejected" {
		t.Fatalf("String() = %q", streamErrorProviderRejected.String())
	}
}

// The incident shape: the primary rejects a fresh request before any tool
// ran in the attempt. The configured fallback takes the round.
func TestStreamRoundSwapsToFallbackOnProviderRejection(t *testing.T) {
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&primaryCalls, 1)
				return nil, openRouterRelayed400()
			},
		},
		name: "google/gemini-3.8-flash",
	}
	fallbackCalls := int32(0)
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return streamStop()(nil, call)
			},
		},
		name: "openai/gpt-5.6-sol",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	obs := &captureObserver{}
	sink := newStreamSink(obs)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), sink, 1000,
		[]fantasy.Message{fantasy.NewUserMessage("health scan")}, buildAgent(primary), primary, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected success via fallback, got: %v", err)
	}
	if !outcome.swappedToFallback || outcome.activeModel.Model() != "openai/gpt-5.6-sol" {
		t.Fatalf("swapped=%v active=%s", outcome.swappedToFallback, outcome.activeModel.Model())
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Errorf("primary called %d times, want 1 (a rejection is not retried in place)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
	if !slices.Contains(obs.events, "fleet.provider_failover") {
		t.Errorf("events = %v, want fleet.provider_failover", obs.events)
	}
}

// Without a fallback the rejection stays terminal, and the reason now names
// the upstream provider and its message instead of only the relay text.
func TestStreamRoundProviderRejectionWithoutFallbackIsTerminal(t *testing.T) {
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				return nil, openRouterRelayed400()
			},
		},
		name: "primary-model",
	}
	e := newMockEngine(t, primary)
	e.fallbackModel = nil
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}

	_, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), newStreamSink(nil), 1000,
		[]fantasy.Message{fantasy.NewUserMessage("health scan")}, buildAgent(primary), primary, false, buildAgent,
	)
	if err == nil {
		t.Fatal("expected terminal error without a fallback")
	}
	if errors.Is(err, ErrCommittedSideEffects) || errors.Is(err, ErrRetryBudgetExhausted) {
		t.Fatalf("rejection must stay terminal for the runner, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"provider_status=400, retryable=false", "provider=Google", "INVALID_ARGUMENT", "bad request: Provider returned error"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "X-Request-Id") || strings.Contains(msg, "user_abc") {
		t.Errorf("error leaked response headers/body beyond error.metadata: %q", msg)
	}
}

// A rejection after a tool executed with no completed-step checkpoint must not
// re-drive: the fallback would replay the executed call (ADR-0035). It stays
// terminal exactly as before, with the executed tool's record preserved.
func TestStreamRoundProviderRejectionAfterToolExecutionStaysTerminal(t *testing.T) {
	sink := newStreamSink(nil)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				sink.onToolCall("call-1", "bash", `{"command":"send the email"}`)
				sink.onToolResult("call-1", "bash", "email sent", false)
				return nil, openRouterRelayed400()
			},
		},
		name: "primary-model",
	}
	fallbackCalls := int32(0)
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return streamStop()(nil, call)
			},
		},
		name: "fallback-model",
	}
	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}

	_, err := e.streamRoundWithResilience(
		context.Background(), newOrchestrationState(e.logSession, 50), sink, 1000,
		[]fantasy.Message{fantasy.NewUserMessage("test task")}, buildAgent(primary), primary, false, buildAgent,
	)
	if err == nil {
		t.Fatal("expected terminal error")
	}
	if errors.Is(err, ErrCommittedSideEffects) {
		t.Errorf("a rejection is not transient infra; got %v", err)
	}
	if !strings.Contains(err.Error(), "provider_status=400") {
		t.Errorf("error = %v, want the provider status named", err)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Errorf("fallback called %d times after an unfenced tool execution, want 0", got)
	}
	entries, _ := sink.snapshot()
	if len(entries) != 2 || entries[0].Type != "tool_call" || entries[1].Type != "tool_result" {
		t.Errorf("entries = %+v, want the executed tool_call + tool_result preserved", entries)
	}
}

// The full incident: a completed tool step, then the provider rejects the
// NEXT request outright. The fallback resumes from the completed-step
// checkpoint (ADR-0065) — it sees the tool result, the write ran once, and no
// partial output is spliced.
func TestStreamRecoveryResumesOnRejectionAfterCompletedStep(t *testing.T) {
	var writes atomic.Int32
	tool := fantasy.NewAgentTool("download_report", "download a report",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			writes.Add(1)
			return fantasy.NewTextResponse(`{"path":"magnite_report.csv","bytes":35314}`), nil
		})
	primary := &namedMockModel{name: "google/gemini-3.8-flash"}
	var primaryCalls atomic.Int32
	primary.streamFunc = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		if primaryCalls.Add(1) == 1 {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "dl-1", ToolCallName: "download_report", ToolCallInput: `{}`})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 20, OutputTokens: 10}})
			}, nil
		}
		// Pre-stream rejection: the request never opened a stream.
		return nil, openRouterRelayed400()
	}
	fallback := &namedMockModel{name: "openai/gpt-5.6-sol"}
	var fallbackCalls atomic.Int32
	fallback.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		fallbackCalls.Add(1)
		results := 0
		for _, msg := range call.Prompt {
			for _, part := range msg.Content {
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && tr.ToolCallID == "dl-1" {
					results++
				}
			}
		}
		if results != 1 {
			t.Errorf("fallback did not resume from the completed step: tool results=%d", results)
		}
		return streamTextThenFinish("report parsed"), nil
	}
	e := newMockEngine(t, primary)
	e.fallbackModel = fallback
	e.maxIterations = 5
	sink := newStreamSink(nil)
	build := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"), fantasy.WithTools(tool))
	}
	orch := newOrchestrationState(e.logSession, 50)
	out, err := e.streamRoundWithResilience(context.Background(), orch, sink, 1000,
		[]fantasy.Message{fantasy.NewUserMessage("fetch and parse")}, build(primary), primary, false, build)
	if err != nil {
		t.Fatal(err)
	}
	if !out.swappedToFallback || writes.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("fallback=%v writes=%d fallbackCalls=%d", out.swappedToFallback, writes.Load(), fallbackCalls.Load())
	}
	entries, text := sink.snapshot()
	if !strings.Contains(text, "report parsed") {
		t.Fatalf("final text = %q", text)
	}
	results := 0
	for _, entry := range entries {
		if entry.Type == "tool_result" {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("tool transcript duplicated: %d", results)
	}
}

func TestProviderErrorUpstreamDetail(t *testing.T) {
	t.Run("http dump with metadata", func(t *testing.T) {
		got := providerErrorUpstreamDetail(openRouterRelayed400())
		for _, want := range []string{"provider=Google", `raw="`, "INVALID_ARGUMENT", "function call turn"} {
			if !strings.Contains(got, want) {
				t.Errorf("detail %q lacks %q", got, want)
			}
		}
		if strings.Contains(got, "X-Request-Id") || strings.Contains(got, "user_abc") || strings.Contains(got, "\n") {
			t.Errorf("detail carried headers, unrelated body fields or newlines: %q", got)
		}
	})
	t.Run("bare SSE envelope, object raw and typed fields", func(t *testing.T) {
		pe := &fantasy.ProviderError{ResponseBody: []byte(`{"error":{"code":429,"message":"Provider returned error","metadata":{"provider_name":"Google","error_type":"rate_limit_exceeded","provider_code":"RESOURCE_EXHAUSTED","raw":{"error":{"status":"RESOURCE_EXHAUSTED"}}}}}`)}
		got := providerErrorUpstreamDetail(pe)
		for _, want := range []string{"provider=Google", "error_type=rate_limit_exceeded", "provider_code=RESOURCE_EXHAUSTED", `raw="{\"error\":{\"status\":\"RESOURCE_EXHAUSTED\"}}"`} {
			if !strings.Contains(got, want) {
				t.Errorf("detail %q lacks %q", got, want)
			}
		}
	})
	t.Run("no metadata, nil, and non-JSON bodies yield nothing", func(t *testing.T) {
		for name, pe := range map[string]*fantasy.ProviderError{
			"nil":         nil,
			"empty":       {},
			"no metadata": {ResponseBody: []byte(`{"error":{"code":400,"message":"bad"}}`)},
			"html":        {ResponseBody: []byte("HTTP/1.1 502 Bad Gateway\r\n\r\n<html>upstream down</html>")},
			"oversized":   {ResponseBody: []byte(strings.Repeat("x", upstreamDetailMaxBody+1))},
		} {
			if got := providerErrorUpstreamDetail(pe); got != "" {
				t.Errorf("%s: detail = %q, want empty", name, got)
			}
		}
	})
	t.Run("raw is redacted and bounded", func(t *testing.T) {
		secret := "unit-test-upstream-secret-9f8e7d6c"
		RegisterSecretLiteral(secret)
		long := strings.Repeat("invalid argument ", 60)
		pe := &fantasy.ProviderError{ResponseBody: []byte(`{"error":{"metadata":{"raw":"key ` + secret + ` rejected: ` + long + `"}}}`)}
		got := providerErrorUpstreamDetail(pe)
		if strings.Contains(got, secret) {
			t.Fatalf("secret leaked into upstream detail: %q", got)
		}
		if len(got) > upstreamRawMaxLen+len(`raw=""`)+8 {
			t.Fatalf("detail not bounded: %d bytes", len(got))
		}
		if !strings.HasSuffix(got, `..."`) {
			t.Fatalf("long raw not visibly truncated: %q", got)
		}
	})
}

func TestFatalProviderErrorNamesUpstreamCause(t *testing.T) {
	base := errors.New("bad request: Provider returned error")
	err := fatalProviderError(base, openRouterRelayed400())
	if !errors.Is(err, base) {
		t.Fatal("cause not wrapped")
	}
	if !strings.Contains(err.Error(), "provider_status=400, retryable=false, provider=Google") {
		t.Fatalf("error = %q", err)
	}
	plain := fatalProviderError(base, &fantasy.ProviderError{StatusCode: 400})
	if plain.Error() != "fantasy agent error (provider_status=400, retryable=false): bad request: Provider returned error" {
		t.Fatalf("plain error = %q", plain)
	}
	if untyped := fatalProviderError(base, nil); untyped.Error() != "fantasy agent error: bad request: Provider returned error" {
		t.Fatalf("untyped error = %q", untyped)
	}
}

// openRouterExpiredCacheBody is the relay envelope for the Google 400 that
// dead-lettered a scheduled page refresh on 2026-09-21: the implicit prompt
// cache had been evicted between two steps.
const openRouterExpiredCacheBody = "HTTP/2.0 400 Bad Request\r\nContent-Type: application/json\r\n\r\n" +
	`{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"{ \"error\": { \"code\": 400, \"message\": \"Cache content 2946923576004968448 is expired.\", \"status\": \"INVALID_ARGUMENT\" } }","provider_name":"Google"}}}`

func TestExpiredPromptCacheIsAStreamBlipNotARejection(t *testing.T) {
	expired := &fantasy.ProviderError{
		StatusCode:   http.StatusBadRequest,
		Title:        "bad request",
		Message:      "Provider returned error",
		ResponseBody: []byte(openRouterExpiredCacheBody),
	}
	if !isExpiredPromptCacheRejection(expired) {
		t.Fatal("relayed 'Cache content ... is expired.' 400 must be recognised as an expired prompt cache")
	}
	if class, _ := classifyStreamError(expired); class != streamErrorStreamBlip {
		t.Fatalf("expired prompt cache must classify as a stream blip (same-model retry rebuilds the cache), got %v", class)
	}

	t.Run("bare SSE envelope", func(t *testing.T) {
		pe := &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Message: "Provider returned error",
			ResponseBody: []byte(`{"error":{"code":400,"message":"Provider returned error","metadata":{"provider_name":"Google","raw":{"error":{"code":400,"message":"Cache content 42 is expired.","status":"INVALID_ARGUMENT"}}}}}`)}
		if class, _ := classifyStreamError(pe); class != streamErrorStreamBlip {
			t.Fatalf("object-shaped raw: got %v, want stream blip", class)
		}
	})
	t.Run("adapter message without a relay body", func(t *testing.T) {
		pe := &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Message: "Cache content abc is expired."}
		if class, _ := classifyStreamError(pe); class != streamErrorStreamBlip {
			t.Fatalf("message-only: got %v, want stream blip", class)
		}
	})
	t.Run("other INVALID_ARGUMENT 400s stay rejections", func(t *testing.T) {
		if isExpiredPromptCacheRejection(openRouterRelayed400()) {
			t.Fatal("the function-call-turn 400 is a real rejection, not an expired cache")
		}
		if class, _ := classifyStreamError(openRouterRelayed400()); class != streamErrorProviderRejected {
			t.Fatalf("got %v, want provider rejection", class)
		}
	})
	t.Run("not a 400, or a credential failure, is never an expired cache", func(t *testing.T) {
		for name, pe := range map[string]*fantasy.ProviderError{
			"500 with the words": {StatusCode: http.StatusInternalServerError, Message: "Cache content 1 is expired."},
			"auth-flagged 400":   {StatusCode: http.StatusBadRequest, AuthError: true, Message: "Cache content 1 is expired."},
			"nil":                nil,
		} {
			if isExpiredPromptCacheRejection(pe) {
				t.Errorf("%s: must not be treated as an expired prompt cache", name)
			}
		}
	})
}
