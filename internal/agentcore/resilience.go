package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"golang.org/x/net/http2"
)

// Stream resilience (cutlass resilience.go is the richer superset; taken whole).
//
// Fantasy's Agent.Stream already does per-step retry-with-backoff and honours
// retry-after headers. We override MaxRetries and layer recoveries on top that
// fantasy can't do alone:
//
//  1. context-too-large → force-compact + retry same model, then escalate;
//  2. retry-budget exhaustion → swap to the fallback model + retry;
//  3. mid-stream SSE / HTTP2 stream blip → one in-place retry, then fallback.
//
// chat's resilience layer is a strict subset (no fallback model, no SSE/HTTP2
// parsing) plus a chat-specific model-reselection UX; those interactive
// concerns belong to the interactive Observer/Policy (P3), not this shared core.
// chat's cases (cancel, context-too-large, retry-exhausted, fatal) are all
// covered by the cutlass classifier below.
//
// The env var is parameterized: <PREFIX>_RETRY_MAX_ATTEMPTS, with CHAT_/CUTLASS_
// back-compat aliases (see env.go); the lifted cutlass test sets
// CUTLASS_RETRY_MAX_ATTEMPTS via the retryMaxAttemptsEnv constant.

// Transient-failure sentinels. When a run exhausts the in-run recovery budget
// (the per-step retries + fallback swap above) on a TRANSIENT failure, the error
// is tagged with one of these so a higher layer (e.g. the scheduler's whole-task
// retry, internal/runner) can distinguish a recoverable infra blip from a
// deterministic failure via errors.Is. The original provider error stays in the
// chain (wrapped alongside the sentinel) for logging.
var (
	// ErrRetryBudgetExhausted: the model kept failing after every in-run retry +
	// fallback swap — a transient class worth a whole-task retry.
	ErrRetryBudgetExhausted = errors.New("retry budget exhausted")
	// ErrStreamBlipPersisted: a mid-stream transport blip survived the in-place
	// retry and there was no fallback to swap to — also transient.
	ErrStreamBlipPersisted = errors.New("stream blip persisted")
)

const (
	// defaultRetryMaxAttempts is the retry count (not counting the original
	// attempt) passed to fantasy's inner retry loop.
	defaultRetryMaxAttempts = 5
	// retryMaxAttemptsEnv is the env-var SUFFIX (read via EnvPrefix). The legacy
	// full name CUTLASS_RETRY_MAX_ATTEMPTS resolves through the back-compat
	// aliases, so the lifted test's t.Setenv keeps working.
	retryMaxAttemptsEnv = "CUTLASS_RETRY_MAX_ATTEMPTS"
	// maxInnerEscalations caps how many outer-loop recoveries per round.
	maxInnerEscalations = 3
	// streamBlipRetryDelay is the wait before retrying the same model after a
	// transient mid-stream error.
	streamBlipRetryDelay = 3 * time.Second
)

// resilienceConfig is resolved once at engine construction.
type resilienceConfig struct {
	maxAttempts int
}

// loadResilienceConfig reads the retry budget from the environment. It accepts
// the legacy CUTLASS_RETRY_MAX_ATTEMPTS name directly (so the lifted test's
// t.Setenv(retryMaxAttemptsEnv, …) works unchanged) and also the canonical
// FLEET_RETRY_MAX_ATTEMPTS via EnvPrefix.
func loadResilienceConfig() resilienceConfig {
	return loadResilienceConfigFor("")
}

func loadResilienceConfigFor(prefix EnvPrefix) resilienceConfig {
	attempts := defaultRetryMaxAttempts
	raw := strings.TrimSpace(prefix.lookup("RETRY_MAX_ATTEMPTS"))
	if raw == "" {
		// retryMaxAttemptsEnv is the legacy full name; lookup the suffix too.
		raw = strings.TrimSpace(prefix.lookup(strings.TrimPrefix(retryMaxAttemptsEnv, "CUTLASS_")))
	}
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			attempts = n
		} else {
			log.Printf("Warning: ignoring invalid %s=%q (using default %d)", retryMaxAttemptsEnv, raw, defaultRetryMaxAttempts)
		}
	}
	return resilienceConfig{maxAttempts: attempts}
}

// streamErrorClass groups the failure modes of an Agent.Stream call so the loop
// can branch without inspecting provider-specific types.
type streamErrorClass int

const (
	streamErrorNone streamErrorClass = iota
	streamErrorCancelled
	streamErrorContextTooLarge
	streamErrorRetryExhausted
	streamErrorStreamBlip
	streamErrorFatal
)

func (c streamErrorClass) String() string {
	switch c {
	case streamErrorNone:
		return "ok"
	case streamErrorCancelled:
		return "cancelled"
	case streamErrorContextTooLarge:
		return "context_too_large"
	case streamErrorRetryExhausted:
		return "retry_exhausted"
	case streamErrorStreamBlip:
		return "stream_blip"
	case streamErrorFatal:
		return "fatal"
	default:
		return statusUnknown
	}
}

// classifyStreamError maps a raw Agent.Stream error into a streamErrorClass plus
// the underlying *fantasy.ProviderError when one was wrapped.
func classifyStreamError(err error) (streamErrorClass, *fantasy.ProviderError) {
	if err == nil {
		return streamErrorNone, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return streamErrorCancelled, nil
	}

	var retryErr *fantasy.RetryError
	var providerErr *fantasy.ProviderError
	if errors.As(err, &retryErr) {
		errors.As(err, &providerErr)
		if providerErr != nil && providerErr.IsContextTooLarge() {
			return streamErrorContextTooLarge, providerErr
		}
		return streamErrorRetryExhausted, providerErr
	}
	if errors.As(err, &providerErr) {
		if providerErr.IsContextTooLarge() {
			return streamErrorContextTooLarge, providerErr
		}
		if providerErr.IsRetryable() {
			return streamErrorRetryExhausted, providerErr
		}
		return streamErrorFatal, providerErr
	}
	if sse := parseSSEStreamError(err); sse != nil {
		if sse.IsContextTooLarge() {
			return streamErrorContextTooLarge, sse
		}
		if sse.IsRetryable() {
			return streamErrorStreamBlip, sse
		}
		return streamErrorFatal, sse
	}
	if h2 := parseHTTP2StreamError(err); h2 != nil {
		return streamErrorStreamBlip, h2
	}
	return streamErrorFatal, nil
}

// sseStreamErrorPrefix is the fragment the charm openai-go SSE decoder produces
// for a mid-stream error event.
const sseStreamErrorPrefix = "received error while streaming:"

type sseErrorPayload struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
}

// parseSSEStreamError returns a synthetic *fantasy.ProviderError built from a
// mid-stream SSE error event, or nil when err doesn't look like one.
func parseSSEStreamError(err error) *fantasy.ProviderError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	idx := strings.Index(msg, sseStreamErrorPrefix)
	if idx < 0 {
		return nil
	}
	body := trimToJSONObject(msg[idx+len(sseStreamErrorPrefix):])
	unparseableFallback := &fantasy.ProviderError{
		StatusCode: http.StatusBadGateway,
		Title:      "stream error (unparseable)",
		Message:    msg,
	}
	if body == "" {
		return unparseableFallback
	}
	var payload sseErrorPayload
	if jerr := json.Unmarshal([]byte(body), &payload); jerr != nil {
		return unparseableFallback
	}
	status := parseSSEStatusCode(payload.Code)
	title := payload.Type
	if title == "" {
		title = fantasy.ErrorTitleForStatusCode(status)
	}
	detail := payload.Message
	if detail == "" {
		detail = msg
	}
	return &fantasy.ProviderError{
		StatusCode: status,
		Title:      title,
		Message:    detail,
	}
}

// http2StreamErrorPrefix is the fragment Go's net/http2 produces for a
// stream-level error.
const http2StreamErrorPrefix = "stream error: stream ID "

// parseHTTP2StreamError returns a synthetic *fantasy.ProviderError built from a
// raw HTTP/2 stream error (transient RST_STREAM frames), or nil.
func parseHTTP2StreamError(err error) *fantasy.ProviderError {
	if err == nil {
		return nil
	}
	var streamErr http2.StreamError
	if errors.As(err, &streamErr) {
		return &fantasy.ProviderError{
			StatusCode: http.StatusBadGateway,
			Title:      "http2 stream error",
			Message:    err.Error(),
		}
	}
	if strings.Contains(err.Error(), http2StreamErrorPrefix) {
		return &fantasy.ProviderError{
			StatusCode: http.StatusBadGateway,
			Title:      "http2 stream error",
			Message:    err.Error(),
		}
	}
	return nil
}

// trimToJSONObject returns the substring covering the outermost balanced {…}.
func trimToJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inStr {
			escaped = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// parseSSEStatusCode extracts an HTTP status code from the `code` field of an
// SSE error payload; unparseable becomes 502 (retryable default).
func parseSSEStatusCode(raw json.RawMessage) int {
	if len(raw) == 0 {
		return http.StatusBadGateway
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if parsed, perr := strconv.Atoi(strings.TrimSpace(s)); perr == nil && parsed > 0 {
			return parsed
		}
	}
	return http.StatusBadGateway
}

// recordContextFromError writes a provider-reported window size into the
// observed-context cache (ground truth for the active slug).
func recordContextFromError(slug string, providerErr *fantasy.ProviderError) {
	if providerErr == nil || slug == "" {
		return
	}
	if providerErr.ContextMaxTokens > 0 {
		recordContextMax(slug, providerErr.ContextMaxTokens)
	}
}

// newRetryLogger returns a fantasy.OnRetryCallback that mirrors each retry into
// the session log as a system_retry entry tagged with status + delay.
func newRetryLogger(session *LogSession) fantasy.OnRetryCallback {
	return func(err *fantasy.ProviderError, delay time.Duration) {
		if err == nil {
			return
		}
		status := err.StatusCode
		title := err.Title
		if title == "" {
			title = fantasy.ErrorTitleForStatusCode(status)
		}
		rounded := delay.Round(10 * time.Millisecond)
		log.Printf("🔁 LLM retry: status=%d (%s) delay=%s msg=%q",
			status, title, rounded, summarizeForConsole(err.Message, 200))
		if session == nil {
			return
		}
		msgType := messageTypeSystemRetry
		note := fmt.Sprintf("[retry] status=%d title=%q delay=%s msg=%q",
			status, title, rounded, summarizeForLog(err.Message, 500))
		session.AddMessageWithMetadata(roleUser, note, nil, nil, &msgType, nil, nil, "")
	}
}

// logStreamBlipRetry records a same-model in-place retry triggered by a
// mid-stream error.
func (e *engine) logStreamBlipRetry(providerErr *fantasy.ProviderError) {
	if e == nil || e.logSession == nil {
		return
	}
	status := 0
	body := ""
	if providerErr != nil {
		status = providerErr.StatusCode
		body = summarizeForConsole(providerErr.Message, 200)
	}
	msgType := messageTypeSystemRetry
	note := fmt.Sprintf("[stream-blip-retry] status=%d delay=%s msg=%q",
		status, streamBlipRetryDelay, body)
	e.logSession.AddMessageWithMetadata(roleUser, note, nil, nil, &msgType, nil, nil, "")
}

// logFallbackSwap records a fallback-model promotion triggered by a stream
// failure.
func (e *engine) logFallbackSwap(reason streamErrorClass, providerErr *fantasy.ProviderError) {
	if e == nil || e.fallbackModel == nil {
		return
	}
	status := 0
	body := ""
	if providerErr != nil {
		status = providerErr.StatusCode
		body = summarizeForConsole(providerErr.Message, 200)
	}
	log.Printf("⚠️  Primary model failed (%s, status=%d); swapping to fallback %s",
		reason, status, e.fallbackModel.Model())
	if e.logSession == nil {
		return
	}
	msgType := messageTypeSystemEnforcement
	note := fmt.Sprintf("[system] primary model failure (%s status=%d msg=%q); swapped to fallback model %s",
		reason, status, body, e.fallbackModel.Model())
	e.logSession.AddMessageWithMetadata(roleUser, note, nil, nil, &msgType, nil, nil, "")
}

// providerErrStatus returns the HTTP status from a ProviderError or 0.
func providerErrStatus(err *fantasy.ProviderError) int {
	if err == nil {
		return 0
	}
	return err.StatusCode
}

// streamErrorDesc is a short operator-facing description of a provider failure
// for the circuit-breaker snapshot (the last_error field).
func streamErrorDesc(providerErr *fantasy.ProviderError) string {
	if status := providerErrStatus(providerErr); status != 0 {
		return fmt.Sprintf("HTTP %d", status)
	}
	return "provider error"
}

// streamRoundOutcome bundles everything the loop needs back after a resilient
// stream attempt, including state mutated during recovery.
type streamRoundOutcome struct {
	result            *fantasy.AgentResult
	messages          []fantasy.Message
	agent             fantasy.Agent
	activeModel       fantasy.LanguageModel
	swappedToFallback bool
}

// streamRoundWithResilience drives a single enforcement round through the
// fantasy stream call, applying up to maxInnerEscalations recoveries on failure.
func (e *engine) streamRoundWithResilience(
	ctx context.Context,
	orch *orchestrationState,
	sink *streamSink,
	maxTokens int64,
	messages []fantasy.Message,
	currentAgent fantasy.Agent,
	activeModel fantasy.LanguageModel,
	swappedToFallback bool,
	buildAgent func(fantasy.LanguageModel) fantasy.Agent,
) (streamRoundOutcome, error) {
	forceCompactedThisRound := false
	streamBlipRetryUsed := false
	var lastErr error

	if e != nil && e.consecutiveCompactions >= maxConsecutiveCompactions {
		return streamRoundOutcome{}, fmt.Errorf(
			"%w: %d consecutive steps required compaction (model=%s, context_window=%d tokens). "+
				"Split this task into smaller pieces, move heavy context into a file the agent can "+
				"view_file on demand, or switch to a model with a larger context window",
			ErrContextBudgetExhausted, e.consecutiveCompactions, activeModel.Model(),
			contextWindowForModel(activeModel.Model()),
		)
	}

	// Circuit-breaker fast path: if the primary model's circuit is open (sustained
	// recent failures accumulated across runs), skip straight to the fallback
	// instead of burning this round's attempts on a known-bad model. Half-open is
	// deliberately NOT skipped — that state exists to send exactly one probe.
	if e != nil && !swappedToFallback && e.healthRegistry.State(activeModel.Model()) == CircuitOpen && canSwapFallback(e, activeModel, swappedToFallback) {
		log.Printf("⚡ circuit open for %s; routing to fallback %s without a primary attempt", activeModel.Model(), e.fallbackModel.Model())
		activeModel = e.fallbackModel
		currentAgent = buildAgent(activeModel)
		swappedToFallback = true
	}

	for attempt := 0; attempt < maxInnerEscalations; attempt++ {
		rs := newRoundState(e, orch, maxTokens)
		rs.sink = sink
		result, err := rs.stream(ctx, currentAgent, activeModel, messages)

		if err == nil {
			if e != nil {
				e.healthRegistry.RecordSuccess(activeModel.Model())
			}
			// A clean stream round is the "clean step in between" the
			// consecutive-compaction contract is written against: reset the
			// counter so the cap (maxConsecutiveCompactions) only trips on
			// compactions in consecutive FAILING rounds, not well-spaced
			// compactions that each recovered cleanly over a long run.
			if e != nil {
				e.consecutiveCompactions = 0
			}
			return streamRoundOutcome{
				result:            result,
				messages:          messages,
				agent:             currentAgent,
				activeModel:       activeModel,
				swappedToFallback: swappedToFallback,
			}, nil
		}
		// A cost/token ceiling abort (budget-guarded PrepareStep) is a clean stop,
		// not a retryable provider blip — surface it verbatim so the run loop can
		// finish gracefully with the partial transcript.
		if errors.Is(err, ErrCostCeilingExceeded) {
			return streamRoundOutcome{}, err
		}
		lastErr = err

		class, providerErr := classifyStreamError(err)
		// Feed genuine provider failures into the circuit breaker (#267) so error
		// frequency accumulates across runs. Cancellation, the cost ceiling, and
		// prompt-too-large are not provider-health signals, so they don't count.
		if e != nil {
			switch class {
			case streamErrorRetryExhausted, streamErrorStreamBlip, streamErrorFatal:
				e.healthRegistry.RecordError(activeModel.Model(), streamErrorDesc(providerErr))
			default:
			}
		}
		switch class {
		case streamErrorNone:
			return streamRoundOutcome{}, fmt.Errorf("unexpected stream state (class=none): %w", err)
		case streamErrorCancelled:
			return streamRoundOutcome{}, fmt.Errorf("context cancelled: %w", err)
		case streamErrorContextTooLarge:
			if activeModel != nil {
				recordContextFromError(activeModel.Model(), providerErr)
			}
			if !forceCompactedThisRound {
				log.Printf("⚠️  Provider rejected prompt as too large (status=%d); forcing compaction and retrying",
					providerErrStatus(providerErr))
				messages = e.forceCompactMessageHistory(ctx, messages)
				forceCompactedThisRound = true
				continue
			}
			if canSwapFallback(e, activeModel, swappedToFallback) {
				e.logFallbackSwap(class, providerErr)
				activeModel = e.fallbackModel
				currentAgent = buildAgent(activeModel)
				swappedToFallback = true
				messages = dropTrailingAssistant(messages)
				continue
			}
			return streamRoundOutcome{}, fmt.Errorf("fantasy agent error (context still too large after forced compaction): %w", err)
		case streamErrorRetryExhausted:
			if !canSwapFallback(e, activeModel, swappedToFallback) {
				return streamRoundOutcome{}, fmt.Errorf("fantasy agent error (retry budget exhausted): %w: %w", ErrRetryBudgetExhausted, err)
			}
			e.logFallbackSwap(class, providerErr)
			activeModel = e.fallbackModel
			currentAgent = buildAgent(activeModel)
			swappedToFallback = true
			messages = dropTrailingAssistant(messages)
			continue
		case streamErrorStreamBlip:
			if !streamBlipRetryUsed {
				streamBlipRetryUsed = true
				log.Printf("🔁 Mid-stream provider error (status=%d); retrying same model once after %s",
					providerErrStatus(providerErr), streamBlipRetryDelay)
				e.logStreamBlipRetry(providerErr)
				select {
				case <-time.After(streamBlipRetryDelay):
				case <-ctx.Done():
					return streamRoundOutcome{}, fmt.Errorf("context cancelled during stream-blip retry: %w", ctx.Err())
				}
				messages = dropTrailingAssistant(messages)
				continue
			}
			if !canSwapFallback(e, activeModel, swappedToFallback) {
				return streamRoundOutcome{}, fmt.Errorf("fantasy agent error (stream blip persisted, no fallback available): %w: %w", ErrStreamBlipPersisted, err)
			}
			e.logFallbackSwap(class, providerErr)
			activeModel = e.fallbackModel
			currentAgent = buildAgent(activeModel)
			swappedToFallback = true
			messages = dropTrailingAssistant(messages)
			continue
		case streamErrorFatal:
			return streamRoundOutcome{}, fmt.Errorf("fantasy agent error: %w", err)
		}
	}
	return streamRoundOutcome{}, fmt.Errorf("fantasy agent error after %d recovery attempts: %w", maxInnerEscalations, lastErr)
}
