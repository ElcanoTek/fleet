package agentcore

// Lifted+adapted from cutlass resilience_test.go. The only structural change is
// *Agent → *engine (agentcore's focused resilience host) and newMockAgent →
// newMockEngine; the assertions are unchanged. retryMaxAttemptsEnv resolves
// through the EnvPrefix back-compat aliases so t.Setenv still works.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"golang.org/x/net/http2"
)

func TestClassifyStreamError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass streamErrorClass
	}{
		{"nil", nil, streamErrorNone},
		{"context.Canceled", context.Canceled, streamErrorCancelled},
		{"context.DeadlineExceeded", context.DeadlineExceeded, streamErrorCancelled},
		{"wrapped cancel", fmt.Errorf("wrapped: %w", context.Canceled), streamErrorCancelled},

		{"429 raw", &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "rate limit"}, streamErrorRetryExhausted},
		{"500 raw", &fantasy.ProviderError{StatusCode: http.StatusInternalServerError, Message: "srv"}, streamErrorRetryExhausted},
		{"503 raw", &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable, Message: "overloaded"}, streamErrorRetryExhausted},
		{"504 raw", &fantasy.ProviderError{StatusCode: http.StatusGatewayTimeout, Message: "gw timeout"}, streamErrorRetryExhausted},
		{"408 raw", &fantasy.ProviderError{StatusCode: http.StatusRequestTimeout, Message: "req timeout"}, streamErrorRetryExhausted},
		{"unexpected EOF cause", &fantasy.ProviderError{Cause: io.ErrUnexpectedEOF, Message: "short read"}, streamErrorRetryExhausted},

		{"context-too-large via flag", &fantasy.ProviderError{ContextTooLargeErr: true, Message: "too big"}, streamErrorContextTooLarge},
		{"context-too-large via token counts", &fantasy.ProviderError{ContextMaxTokens: 8000, ContextUsedTokens: 9000}, streamErrorContextTooLarge},

		{"retry-error wraps 429s", &fantasy.RetryError{Errors: []error{
			&fantasy.ProviderError{StatusCode: http.StatusTooManyRequests},
			&fantasy.ProviderError{StatusCode: http.StatusTooManyRequests},
		}}, streamErrorRetryExhausted},
		{"retry-error wraps context-too-large", &fantasy.RetryError{Errors: []error{
			&fantasy.ProviderError{ContextTooLargeErr: true},
		}}, streamErrorContextTooLarge},

		{"400 is fatal", &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Message: "bad req"}, streamErrorFatal},
		{"401 is fatal", &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "bad key"}, streamErrorFatal},
		{"403 is fatal", &fantasy.ProviderError{StatusCode: http.StatusForbidden, Message: "forbidden"}, streamErrorFatal},
		{"404 is fatal", &fantasy.ProviderError{StatusCode: http.StatusNotFound, Message: "no model"}, streamErrorFatal},
		{"plain error is fatal", errors.New("boom"), streamErrorFatal},

		{
			"sse mid-stream 502",
			errors.New(`received error while streaming: {"code":502,"message":"Network connection lost.","metadata":{"error_type":"provider_unavailable"}}`),
			streamErrorStreamBlip,
		},
		{
			"sse mid-stream 503",
			errors.New(`received error while streaming: {"code":503,"message":"overloaded"}`),
			streamErrorStreamBlip,
		},
		{
			"sse mid-stream 429",
			errors.New(`received error while streaming: {"code":429,"message":"rate limited"}`),
			streamErrorStreamBlip,
		},
		{
			"sse mid-stream string code",
			errors.New(`received error while streaming: {"code":"502","message":"x"}`),
			streamErrorStreamBlip,
		},
		{
			"sse mid-stream wrapped by fmt.Errorf",
			fmt.Errorf("stream error: %w",
				errors.New(`received error while streaming: {"code":502,"message":"x"}`)),
			streamErrorStreamBlip,
		},
		{
			"sse mid-stream 400 is fatal",
			errors.New(`received error while streaming: {"code":400,"message":"bad prompt"}`),
			streamErrorFatal,
		},
		{
			"sse mid-stream unparseable body defaults retryable",
			errors.New(`received error while streaming: not json`),
			streamErrorStreamBlip,
		},

		{
			"http2 stream error typed (INTERNAL_ERROR)",
			http2.StreamError{StreamID: 29, Code: http2.ErrCodeInternal},
			streamErrorStreamBlip,
		},
		{
			"http2 stream error typed (PROTOCOL_ERROR)",
			http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol},
			streamErrorStreamBlip,
		},
		{
			"http2 stream error wrapped by fmt.Errorf",
			fmt.Errorf("read failed: %w", http2.StreamError{StreamID: 7, Code: http2.ErrCodeRefusedStream}),
			streamErrorStreamBlip,
		},
		{
			"http2 stream error string fallback",
			errors.New("stream error: stream ID 29; INTERNAL_ERROR; received from peer"),
			streamErrorStreamBlip,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyStreamError(tt.err)
			if got != tt.wantClass {
				t.Errorf("class = %v, want %v (err=%v)", got, tt.wantClass, tt.err)
			}
		})
	}
}

func TestLoadResilienceConfig(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(retryMaxAttemptsEnv, "")
		if got := loadResilienceConfig().maxAttempts; got != defaultRetryMaxAttempts {
			t.Errorf("got %d, want %d", got, defaultRetryMaxAttempts)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv(retryMaxAttemptsEnv, "2")
		if got := loadResilienceConfig().maxAttempts; got != 2 {
			t.Errorf("got %d, want 2", got)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		t.Setenv(retryMaxAttemptsEnv, "0")
		if got := loadResilienceConfig().maxAttempts; got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("negative falls back to default", func(t *testing.T) {
		t.Setenv(retryMaxAttemptsEnv, "-1")
		if got := loadResilienceConfig().maxAttempts; got != defaultRetryMaxAttempts {
			t.Errorf("got %d, want %d", got, defaultRetryMaxAttempts)
		}
	})
	t.Run("non-numeric falls back to default", func(t *testing.T) {
		t.Setenv(retryMaxAttemptsEnv, "not-a-number")
		if got := loadResilienceConfig().maxAttempts; got != defaultRetryMaxAttempts {
			t.Errorf("got %d, want %d", got, defaultRetryMaxAttempts)
		}
	})
	t.Run("canonical FLEET prefix", func(t *testing.T) {
		t.Setenv(retryMaxAttemptsEnv, "")
		t.Setenv("FLEET_RETRY_MAX_ATTEMPTS", "7")
		if got := loadResilienceConfig().maxAttempts; got != 7 {
			t.Errorf("got %d, want 7 via FLEET_ prefix", got)
		}
	})
}

func TestDropTrailingAssistant(t *testing.T) {
	assistantPartial := fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: "partial"}},
	}
	user := fantasy.NewUserMessage("hi")

	tests := []struct {
		name     string
		in       []fantasy.Message
		wantLen  int
		wantLast fantasy.MessageRole
	}{
		{"empty slice", nil, 0, ""},
		{"only user stays", []fantasy.Message{user}, 1, fantasy.MessageRoleUser},
		{"trailing assistant dropped", []fantasy.Message{user, assistantPartial}, 1, fantasy.MessageRoleUser},
		{"assistant-then-user stays", []fantasy.Message{assistantPartial, user}, 2, fantasy.MessageRoleUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dropTrailingAssistant(tt.in)
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && got[len(got)-1].Role != tt.wantLast {
				t.Errorf("last role = %q, want %q", got[len(got)-1].Role, tt.wantLast)
			}
		})
	}
}

func TestCanSwapFallback(t *testing.T) {
	primary := &namedMockModel{name: "primary"}
	fallback := &namedMockModel{name: "fallback"}

	t.Run("nil engine cannot swap", func(t *testing.T) {
		if canSwapFallback(nil, primary, false) {
			t.Error("expected false for nil engine")
		}
	})
	t.Run("nil fallback cannot swap", func(t *testing.T) {
		e := &engine{fallbackModel: nil}
		if canSwapFallback(e, primary, false) {
			t.Error("expected false when fallback is nil")
		}
	})
	t.Run("nil active model cannot swap", func(t *testing.T) {
		e := &engine{fallbackModel: fallback}
		if canSwapFallback(e, nil, false) {
			t.Error("expected false when active is nil")
		}
	})
	t.Run("already swapped cannot swap again", func(t *testing.T) {
		e := &engine{fallbackModel: fallback}
		if canSwapFallback(e, primary, true) {
			t.Error("expected false when already swapped")
		}
	})
	t.Run("same slug cannot swap", func(t *testing.T) {
		e := &engine{fallbackModel: primary}
		if canSwapFallback(e, primary, false) {
			t.Error("expected false when fallback slug matches active slug")
		}
	})
	t.Run("different slug can swap", func(t *testing.T) {
		e := &engine{fallbackModel: fallback}
		if !canSwapFallback(e, primary, false) {
			t.Error("expected true when fallback has a distinct slug")
		}
	})
}

func TestParseSSEStreamError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantNil    bool
	}{
		{"nil error → nil", nil, 0, true},
		{"unrelated text → nil", errors.New("some other failure"), 0, true},
		{
			"openrouter 502 payload",
			errors.New(`received error while streaming: {"code":502,"message":"Network connection lost."}`),
			502, false,
		},
		{
			"string code coerced",
			errors.New(`received error while streaming: {"code":"429","message":"rate"}`),
			429, false,
		},
		{
			"missing code defaults to 502",
			errors.New(`received error while streaming: {"message":"something"}`),
			http.StatusBadGateway, false,
		},
		{
			"unparseable body defaults to 502",
			errors.New(`received error while streaming: not json at all`),
			http.StatusBadGateway, false,
		},
		{
			"wrapped by fmt.Errorf still parsed",
			fmt.Errorf("stream error: %w",
				errors.New(`received error while streaming: {"code":504,"message":"gw"}`)),
			504, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSSEStreamError(tt.err)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected ProviderError, got nil")
			}
			if got.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.StatusCode, tt.wantStatus)
			}
			if !got.IsRetryable() {
				if tt.wantStatus >= 500 || tt.wantStatus == 408 || tt.wantStatus == 409 || tt.wantStatus == 429 {
					t.Errorf("status %d should be retryable", got.StatusCode)
				}
			}
		})
	}
}

func TestParseHTTP2StreamError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantNil bool
	}{
		{"nil → nil", nil, true},
		{"unrelated text → nil", errors.New("some other failure"), true},
		{"sse-only message → nil", errors.New(`received error while streaming: {"code":502}`), true},
		{
			"typed http2.StreamError",
			http2.StreamError{StreamID: 29, Code: http2.ErrCodeInternal},
			false,
		},
		{
			"wrapped http2.StreamError",
			fmt.Errorf("read body: %w", http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol}),
			false,
		},
		{
			"string match without typed error",
			errors.New("stream error: stream ID 29; INTERNAL_ERROR; received from peer"),
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHTTP2StreamError(tt.err)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected ProviderError, got nil")
			}
			if got.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want %d", got.StatusCode, http.StatusBadGateway)
			}
			if !got.IsRetryable() {
				t.Errorf("synthesized 502 should be retryable")
			}
		})
	}
}

func TestTrimToJSONObject(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"no braces", ""},
		{`{"a":1}`, `{"a":1}`},
		{`junk {"a":1} junk`, `{"a":1}`},
		{`{"a":{"b":2}}`, `{"a":{"b":2}}`},
		{`{"a":"}"}`, `{"a":"}"}`},
		{`{"a":"b\"c","d":1}`, `{"a":"b\"c","d":1}`},
		{`{incomplete`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := trimToJSONObject(tt.in); got != tt.want {
				t.Errorf("trimToJSONObject(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStreamRoundRetriesStreamBlipInPlace(t *testing.T) {
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				n := atomic.AddInt32(&primaryCalls, 1)
				if n == 1 {
					return nil, errors.New(`received error while streaming: {"code":502,"message":"Network connection lost."}`)
				}
				return streamStop()(nil, call)
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
	e.resilience = resilienceConfig{maxAttempts: 0}

	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), orch, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected success via in-place retry, got: %v", err)
	}
	if outcome.swappedToFallback {
		t.Error("expected swappedToFallback=false when same-model retry succeeds")
	}
	if got := outcome.activeModel.Model(); got != "primary-model" {
		t.Errorf("active model = %q, want primary-model (no swap expected)", got)
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 2 {
		t.Errorf("primary called %d times, want 2 (fail, then succeed)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 0 {
		t.Errorf("fallback called %d times, want 0", got)
	}
}

func TestStreamRoundSwapsOnPersistentStreamBlip(t *testing.T) {
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&primaryCalls, 1)
				return nil, errors.New(`received error while streaming: {"code":502,"message":"still broken"}`)
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
	e.resilience = resilienceConfig{maxAttempts: 0}

	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), orch, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected success via fallback, got: %v", err)
	}
	if !outcome.swappedToFallback {
		t.Error("expected swappedToFallback=true after persistent blip")
	}
	if got := outcome.activeModel.Model(); got != "fallback-model" {
		t.Errorf("active model = %q, want fallback-model", got)
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 2 {
		t.Errorf("primary called %d times, want 2 (initial + in-place retry)", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
}

func TestStreamRoundSwapsToFallbackOnRetryExhaustion(t *testing.T) {
	primaryCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&primaryCalls, 1)
				return nil, &fantasy.ProviderError{
					StatusCode: http.StatusTooManyRequests,
					Title:      "too many requests",
					Message:    "rate limited",
				}
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
	e.resilience = resilienceConfig{maxAttempts: 0}

	orch := newOrchestrationState(e.logSession, 50)
	maxTokens := int64(1000)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	outcome, err := e.streamRoundWithResilience(
		context.Background(), orch, maxTokens,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err != nil {
		t.Fatalf("expected success via fallback, got: %v", err)
	}
	if !outcome.swappedToFallback {
		t.Error("expected swappedToFallback=true after retry exhaustion")
	}
	if got := outcome.activeModel.Model(); got != "fallback-model" {
		t.Errorf("active model = %q, want fallback-model", got)
	}
	if got := atomic.LoadInt32(&primaryCalls); got != 1 {
		t.Errorf("primary called %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&fallbackCalls); got != 1 {
		t.Errorf("fallback called %d times, want 1", got)
	}
}

func TestStreamRoundFatalPropagates(t *testing.T) {
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				return nil, &fantasy.ProviderError{
					StatusCode: http.StatusBadRequest,
					Message:    "your prompt is invalid",
				}
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

	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test")}

	_, err := e.streamRoundWithResilience(
		context.Background(), orch, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err == nil {
		t.Fatal("expected error for fatal 400, got nil")
	}
	if atomic.LoadInt32(&fallbackCalls) != 0 {
		t.Errorf("fallback was called %d times on fatal error; expected no swap", fallbackCalls)
	}
}

func TestStreamRoundCancelledContextShortCircuits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fallbackCalls := int32(0)
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				return nil, context.Canceled
			},
		},
		name: "primary-model",
	}
	fallback := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				atomic.AddInt32(&fallbackCalls, 1)
				return nil, nil
			},
		},
		name: "fallback-model",
	}

	e := newMockEngine(t, primary)
	e.fallbackModel = fallback

	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test")}

	_, err := e.streamRoundWithResilience(
		ctx, orch, 1000, messages, buildAgent(e.model), e.model, false, buildAgent,
	)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if atomic.LoadInt32(&fallbackCalls) != 0 {
		t.Error("fallback must not be called when context is cancelled")
	}
}

func TestRetryLoggerRecordsToSession(t *testing.T) {
	session := NewLogSession()
	logger := newRetryLogger(session)

	logger(&fantasy.ProviderError{
		StatusCode: http.StatusTooManyRequests,
		Title:      "",
		Message:    "rate limited",
	}, 500)
	logger(&fantasy.ProviderError{
		StatusCode: 529,
		Title:      "overloaded",
		Message:    "service overloaded",
	}, 1500)

	var seen int
	var mu sync.Mutex
	mu.Lock()
	for _, m := range session.Messages {
		if m.MessageType != nil && *m.MessageType == "system_retry" {
			seen++
		}
	}
	mu.Unlock()
	if seen != 2 {
		t.Errorf("expected 2 system_retry log entries, got %d", seen)
	}
}
