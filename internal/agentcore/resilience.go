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

	"github.com/ElcanoTek/fleet/internal/metrics"
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
	ErrFirstChunkTimeout   = errors.New("provider first-chunk timeout")
	// ErrCommittedSideEffects: the provider failed mid-round after the attempt
	// had already executed at least one tool. In-run recovery (in-place retry or
	// fallback swap) is deliberately suppressed — a re-driven round restarts
	// from its input messages, so the model could re-issue the executed tool
	// calls and repeat their side effects (ADR-0035). The provider failure
	// itself is still transient infra, so the scheduler's whole-task RetryPolicy
	// (an explicit operator opt-in to re-running a task's side effects) decides
	// whether the task re-runs; it must NOT be classed as deterministic.
	ErrCommittedSideEffects = errors.New("provider failed after tool execution began; failover suppressed")
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

	// First-chunk watchdog (#1537). A provider that accepts the request but
	// produces no semantic event within the timeout is treated as a stream
	// blip. The base was a flat 30 s; a ~115K-token prompt on a slower
	// provider legitimately takes longer to start streaming, and two such
	// timeouts in a row swapped a run to its fallback model in the audit
	// tail. The timeout now grows with the prompt: base + 2 s per 10K prompt
	// tokens (the previous step's input size), capped. Both ends are knobs:
	// FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS (default 30, floor 5) and
	// FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS (default 180, never
	// below the base).
	defaultFirstChunkTimeout      = 30 * time.Second
	minFirstChunkTimeout          = 5 * time.Second
	firstChunkTimeoutPer10KTokens = 2 * time.Second
	defaultFirstChunkTimeoutMax   = 180 * time.Second
	firstChunkTimeoutBaseEnv      = "PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS"
	firstChunkTimeoutMaxEnv       = "PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS"
)

// firstChunkTimeoutFor resolves the watchdog deadline for a provider call
// whose prompt is roughly promptTokens long (the previous step's input token
// count; 0 when unknown, which yields the base).
func firstChunkTimeoutFor(p EnvPrefix, promptTokens int) time.Duration {
	base := secondsKnob(p, firstChunkTimeoutBaseEnv, defaultFirstChunkTimeout)
	if base < minFirstChunkTimeout {
		base = minFirstChunkTimeout
	}
	maxTimeout := secondsKnob(p, firstChunkTimeoutMaxEnv, defaultFirstChunkTimeoutMax)
	if maxTimeout < base {
		maxTimeout = base
	}
	if promptTokens < 0 {
		promptTokens = 0
	}
	timeout := base + time.Duration(promptTokens/10_000)*firstChunkTimeoutPer10KTokens
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	return timeout
}

// secondsKnob reads a whole-or-fractional-seconds env knob through the
// prefix aliases; unset or unparseable yields def (the registry in
// internal/config refuses a malformed value at boot, this is the lenient
// embedder fallback).
func secondsKnob(p EnvPrefix, suffix string, def time.Duration) time.Duration {
	secs := p.lookupFloatDefault(suffix, def.Seconds())
	if secs <= 0 {
		return def
	}
	return time.Duration(secs * float64(time.Second))
}

// firstChunkTimeoutError is the watchdog's error: it satisfies
// errors.Is(err, ErrFirstChunkTimeout) (so classifyStreamError still files it
// as a stream blip) and carries the deadline and prompt size the retry
// log/event report, so the correlation between prompt size and a timeout is
// visible in exported logs (#1537).
type firstChunkTimeoutError struct {
	timeout      time.Duration
	promptTokens int
	cause        error
}

func (e *firstChunkTimeoutError) Error() string {
	return fmt.Sprintf("%s after %s (prompt ≈ %d tokens): %v", ErrFirstChunkTimeout, e.timeout, e.promptTokens, e.cause)
}

func (e *firstChunkTimeoutError) Unwrap() []error { return []error{ErrFirstChunkTimeout, e.cause} }

// firstChunkTimeoutDetail extracts the watchdog detail from a stream error,
// when it was one.
func firstChunkTimeoutDetail(err error) (timeout time.Duration, promptTokens int, ok bool) {
	var fc *firstChunkTimeoutError
	if errors.As(err, &fc) {
		return fc.timeout, fc.promptTokens, true
	}
	return 0, 0, false
}

// streamBlipRetryDelay is the wait before retrying the same model after a
// transient mid-stream error. A var (not a const) only so the package's tests
// can shorten it: six of them drive a blip through this path, and at 3s each
// the waits were most of the package's wall-clock.
var streamBlipRetryDelay = 3 * time.Second

// resilienceConfig is resolved once at engine construction.
type resilienceConfig struct {
	maxAttempts int
}

// loadResilienceConfig reads the retry budget from the environment. It accepts
// the legacy CUTLASS_RETRY_MAX_ATTEMPTS name directly (so the lifted test's
// t.Setenv(retryMaxAttemptsEnv, …) works unchanged) and also the canonical
// FLEET_RETRY_MAX_ATTEMPTS via EnvPrefix.
//
// The knob is a scopeExternal row in the config package's env-knob registry
// (#1273), so a malformed value refuses the boot; the warn-and-default branch
// below remains for the engine-construction paths that run without a
// config.Load (tests, embedders) rather than as the operator-facing behavior.
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
	// streamErrorProviderRejected is a non-retryable 4xx the provider returned
	// for THIS request/model pair (ADR-0067): not credentials or billing, so a
	// configured fallback model may still accept the same work.
	streamErrorProviderRejected
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
	case streamErrorProviderRejected:
		return "provider_rejected"
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
	if errors.Is(err, ErrFirstChunkTimeout) {
		return streamErrorStreamBlip, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return streamErrorCancelled, nil
	}

	var retryErr *fantasy.RetryError
	var providerErr *fantasy.ProviderError
	errors.As(err, &providerErr)
	providerErr = normalizeStreamProviderError(providerErr)
	if providerErr != nil {
		if providerErr.IsContextTooLarge() {
			return streamErrorContextTooLarge, providerErr
		}
		if providerErr.IsRetryable() {
			return streamErrorRetryExhausted, providerErr
		}
		if isProviderRejection(providerErr) {
			return streamErrorProviderRejected, providerErr
		}
		return streamErrorFatal, providerErr
	}
	if errors.As(err, &retryErr) {
		return streamErrorRetryExhausted, nil
	}
	if sse := parseSSEStreamError(err); sse != nil {
		if sse.IsContextTooLarge() {
			return streamErrorContextTooLarge, sse
		}
		if sse.IsRetryable() {
			return streamErrorStreamBlip, sse
		}
		if isProviderRejection(sse) {
			return streamErrorProviderRejected, sse
		}
		return streamErrorFatal, sse
	}
	if h2 := parseHTTP2StreamError(err); h2 != nil {
		return streamErrorStreamBlip, h2
	}
	// Older adapters exposed only this exact in-band provider failure. Bound
	// recovery as an opaque stream failure; never assign an invented HTTP code.
	if strings.TrimSpace(err.Error()) == "stream error: Provider returned error" {
		return streamErrorStreamBlip, &fantasy.ProviderError{Title: "stream error", Message: "Provider returned error", Cause: err, TransientError: true}
	}
	return streamErrorFatal, nil
}

// isProviderRejection reports whether a non-retryable provider error is a
// per-request rejection (ADR-0067): an explicit 4xx that is NOT a credential
// (401, or the adapter's AuthError flag) or billing (402) failure. A gateway
// such as OpenRouter relays the upstream provider's 4xx verbatim as
// "Provider returned error", so the same request may well succeed on the
// configured fallback model; a key- or credit-level failure would not.
func isProviderRejection(providerErr *fantasy.ProviderError) bool {
	if providerErr == nil || providerErr.AuthError {
		return false
	}
	switch providerErr.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired:
		return false
	}
	return providerErr.StatusCode >= http.StatusBadRequest && providerErr.StatusCode < http.StatusInternalServerError
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
// observed-context cache (ground truth for this exact provider+slug pair).
func recordContextFromError(model fantasy.LanguageModel, providerErr *fantasy.ProviderError) {
	if providerErr == nil || model == nil || strings.TrimSpace(model.Model()) == "" {
		return
	}
	if providerErr.ContextMaxTokens > 0 {
		recordContextMaxForModel(model, providerErr.ContextMaxTokens)
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
			status, title, rounded, summarizeForConsole(err.Message))
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
func (e *engine) logStreamBlipRetry(providerErr *fantasy.ProviderError, cause error) {
	if e == nil || e.logSession == nil {
		return
	}
	status := 0
	body := ""
	if providerErr != nil {
		status = providerErr.StatusCode
		body = summarizeForConsole(providerErr.Message)
	}
	msgType := messageTypeSystemRetry
	note := fmt.Sprintf("[stream-blip-retry] status=%d delay=%s msg=%q",
		status, streamBlipRetryDelay, body)
	if timeout, promptTokens, ok := firstChunkTimeoutDetail(cause); ok {
		note += fmt.Sprintf(" first_chunk_timeout=%s prompt_tokens=%d", timeout, promptTokens)
	}
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
		body = summarizeForConsole(providerErr.Message)
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

func emitProviderFailover(sink *streamSink, from, to fantasy.LanguageModel, reason streamErrorClass, providerErr *fantasy.ProviderError) {
	metrics.RecordProviderFailover(fleetProviderName(from), fleetProviderName(to), reason.String())
	if sink == nil {
		return
	}
	sink.emit("fleet.provider_failover", map[string]any{
		"from_provider": fleetProviderName(from),
		"to_provider":   fleetProviderName(to),
		"from_model":    from.Model(),
		"to_model":      to.Model(),
		"reason":        reason.String(),
		"status":        providerErrStatus(providerErr),
	})
}

// emitTurnRetry streams the non-terminal "retrying" signal (#833). Consumers
// existed before any producer did: the web client renders an inline badge
// (RetryEventPayload — status_code/title/message/delay_ms), and journal
// recovery (store.buildRecoveredEntries) resets accumulated text on it so a
// rolled-back attempt's discarded partial output is not projected into the
// recovered history. Emitted from fantasy's inner-retry backoff (engine.go
// stream OnRetry) and from every rollbackAttempt re-drive.
//
// cause is the stream error that triggered the re-drive (nil from fantasy's
// inner retry); when it is the first-chunk watchdog, the payload also carries
// first_chunk_timeout_ms and prompt_tokens so a timeout can be correlated with
// prompt size in exported logs (#1537).
func emitTurnRetry(sink *streamSink, providerErr *fantasy.ProviderError, delay time.Duration, cause error) {
	if sink == nil {
		return
	}
	payload := map[string]any{"delay_ms": delay.Milliseconds()}
	if providerErr != nil {
		title := providerErr.Title
		if title == "" {
			title = fantasy.ErrorTitleForStatusCode(providerErr.StatusCode)
		}
		payload["status_code"] = providerErr.StatusCode
		payload["title"] = title
		payload["message"] = summarizeForConsole(providerErr.Message)
	}
	if timeout, promptTokens, ok := firstChunkTimeoutDetail(cause); ok {
		payload["title"] = "Provider slow to start streaming"
		payload["message"] = fmt.Sprintf("no first chunk within %s for a prompt of about %d tokens; retrying", timeout, promptTokens)
		payload["first_chunk_timeout_ms"] = timeout.Milliseconds()
		payload["prompt_tokens"] = promptTokens
	}
	sink.emit("turn.retry", payload)
}

func fleetProviderName(model fantasy.LanguageModel) string {
	if named, ok := model.(interface{ fleetProviderName() string }); ok {
		return named.fleetProviderName()
	}
	return model.Provider()
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
	completedSteps := 0

	// Runaway-compaction backstop (#598): when the last maxConsecutiveCompactions
	// rounds EACH needed a force-compaction (with no compaction-free round in
	// between — see the conditional reset below), the history is re-overflowing
	// every round no matter how much is summarized away, so burning another paid
	// round on it is pointless. Surface the terminal error instead.
	if e != nil && e.consecutiveCompactions >= maxConsecutiveCompactions {
		return streamRoundOutcome{}, fmt.Errorf(
			"%w: %d consecutive rounds required compaction (model=%s, context_window=%d tokens). "+
				"Split this task into smaller pieces, move heavy context into a file the agent can "+
				"view_file on demand, or switch to a model with a larger context window",
			ErrContextBudgetExhausted, e.consecutiveCompactions, activeModel.Model(),
			contextWindowForActiveModel(activeModel),
		)
	}

	// Circuit-breaker fast path: if the primary model's circuit is open (sustained
	// recent failures accumulated across runs), skip straight to the fallback
	// instead of burning this round's attempts on a known-bad model. Half-open is
	// deliberately NOT skipped — that state exists to send exactly one probe.
	if e != nil && !swappedToFallback && e.healthRegistry.State(activeModel.Model()) == CircuitOpen && canSwapFallback(e, activeModel, swappedToFallback) {
		log.Printf("⚡ circuit open for %s; routing to fallback %s without a primary attempt", activeModel.Model(), e.fallbackModel.Model())
		emitProviderFailover(sink, activeModel, e.fallbackModel, streamErrorRetryExhausted, nil)
		activeModel = e.takeFallback()
		currentAgent = buildAgent(activeModel)
		swappedToFallback = true
	}

	recoveryLimit := maxInnerEscalations
	if e != nil {
		recoveryLimit += len(e.fallbackModels)
	}
	for attempt := 0; attempt < recoveryLimit; attempt++ {
		attemptMark := sink.mark()
		rs := newRoundState(e, orch, maxTokens)
		rs.sink = sink
		rs.priorSteps = completedSteps
		result, err := rs.stream(ctx, currentAgent, activeModel, messages)

		if err == nil {
			if e != nil {
				e.healthRegistry.RecordSuccess(activeModel.Model())
			}
			// A round that completed WITHOUT needing a force-compaction is the
			// "compaction-free round" the consecutive-compaction contract counts
			// against: reset the counter so the cap (maxConsecutiveCompactions)
			// only trips on force-compactions in CONSECUTIVE rounds, not
			// well-spaced compactions that each bought many clean rounds over a
			// long run. A round that recovered VIA a force-compaction keeps its
			// increment even though its stream ultimately succeeded — the
			// compaction is what made the stream succeed, so it must still count
			// toward the runaway cap. (Resetting on ANY clean stream, the
			// pre-#598 behavior, made the cap unreachable: every successful round
			// return passed through this reset first, and every error return
			// halted the whole run, so the >= cap gate above always saw 0.)
			if e != nil && !forceCompactedThisRound {
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
		e.logProviderFailure(activeModel, class, providerErr)
		// Resume from a completed step, not from the round's original input.
		// A call/result observed after the checkpoint makes the failed step
		// unsafe to replay, even if a matching result happened to arrive.
		if rs.canResume(class, attemptMark) {
			messages = rs.recoveryMessages
			attemptMark = rs.recoveryMark
			completedSteps += rs.recoverySteps
		}
		// Never replay a tool step lacking a safe completed-step checkpoint.
		// This also gates context compaction: restarting from older input would
		// lose the completed results and invite duplicate writes (ADR-0065).
		if committedSideEffectsBlockRecovery(sink, attemptMark, class) {
			return streamRoundOutcome{}, fmt.Errorf("%w: %w", ErrCommittedSideEffects, err)
		}
		// Safe to re-drive (no tool side effects past the mark): drop the failed
		// attempt's partial text/reasoning accumulation and its trailing
		// assistant message so the regeneration replaces — not duplicates — the
		// abandoned partial output.
		rollbackAttempt := func() {
			// The rollback only unwinds the in-memory sink; deltas already
			// journaled/streamed can't be unsent, so mark the discard point for
			// journal recovery and the live client (#833).
			emitTurnRetry(sink, providerErr, 0, err)
			sink.rollbackTo(attemptMark)
			messages = dropTrailingAssistant(messages)
		}
		e.recordProviderHealthFailure(activeModel, class, providerErr)
		switch class {
		case streamErrorNone:
			return streamRoundOutcome{}, fmt.Errorf("unexpected stream state (class=none): %w", err)
		case streamErrorCancelled:
			return streamRoundOutcome{}, fmt.Errorf("context cancelled: %w", err)
		case streamErrorContextTooLarge:
			if activeModel != nil {
				recordContextFromError(activeModel, providerErr)
			}
			if !forceCompactedThisRound {
				log.Printf("⚠️  Provider rejected prompt as too large (status=%d); forcing compaction and retrying",
					providerErrStatus(providerErr))
				// Discard the failed attempt's partial output BEFORE compacting,
				// so the re-driven round replaces — not duplicates — it in the
				// sink and the message history (same contract as the blip/
				// exhausted re-drives below).
				rollbackAttempt()
				messages = e.forceCompactMessageHistory(ctx, messages)
				forceCompactedThisRound = true
				continue
			}
			if canSwapFallback(e, activeModel, swappedToFallback) {
				e.logFallbackSwap(class, providerErr)
				emitProviderFailover(sink, activeModel, e.fallbackModel, class, providerErr)
				activeModel = e.takeFallback()
				currentAgent = buildAgent(activeModel)
				swappedToFallback = true
				rollbackAttempt()
				continue
			}
			return streamRoundOutcome{}, fmt.Errorf("fantasy agent error (context still too large after forced compaction): %w", err)
		case streamErrorRetryExhausted:
			if !canSwapFallback(e, activeModel, swappedToFallback) {
				return streamRoundOutcome{}, fmt.Errorf("fantasy agent error (retry budget exhausted): %w: %w", ErrRetryBudgetExhausted, err)
			}
			e.logFallbackSwap(class, providerErr)
			emitProviderFailover(sink, activeModel, e.fallbackModel, class, providerErr)
			activeModel = e.takeFallback()
			currentAgent = buildAgent(activeModel)
			swappedToFallback = true
			rollbackAttempt()
			continue
		case streamErrorStreamBlip:
			if !streamBlipRetryUsed {
				streamBlipRetryUsed = true
				log.Printf("🔁 Mid-stream provider error (status=%d); retrying same model once after %s",
					providerErrStatus(providerErr), streamBlipRetryDelay)
				e.logStreamBlipRetry(providerErr, err)
				select {
				case <-time.After(streamBlipRetryDelay):
				case <-ctx.Done():
					return streamRoundOutcome{}, fmt.Errorf("context cancelled during stream-blip retry: %w", ctx.Err())
				}
				rollbackAttempt()
				continue
			}
			if !canSwapFallback(e, activeModel, swappedToFallback) {
				return streamRoundOutcome{}, fmt.Errorf("fantasy agent error (stream blip persisted, no fallback available): %w: %w", ErrStreamBlipPersisted, err)
			}
			e.logFallbackSwap(class, providerErr)
			emitProviderFailover(sink, activeModel, e.fallbackModel, class, providerErr)
			activeModel = e.takeFallback()
			currentAgent = buildAgent(activeModel)
			swappedToFallback = true
			rollbackAttempt()
			continue
		case streamErrorProviderRejected:
			// A 4xx the provider returned for this model may not bind the
			// fallback (ADR-0067). Promote it once, only from a safe point:
			// no tool event since the (possibly resumed) checkpoint, so the
			// fallback re-drives without replaying executed calls. Otherwise
			// the rejection stays terminal exactly as before.
			if !e.canPromoteOnRejection(sink, attemptMark, activeModel, swappedToFallback) {
				return streamRoundOutcome{}, fatalProviderError(err, providerErr)
			}
			e.logFallbackSwap(class, providerErr)
			emitProviderFailover(sink, activeModel, e.fallbackModel, class, providerErr)
			activeModel = e.takeFallback()
			currentAgent = buildAgent(activeModel)
			swappedToFallback = true
			rollbackAttempt()
			continue
		case streamErrorFatal:
			return streamRoundOutcome{}, fatalProviderError(err, providerErr)
		}
	}
	return streamRoundOutcome{}, fmt.Errorf("fantasy agent error after %d recovery attempts: %w", recoveryLimit, lastErr)
}

func fatalProviderError(err error, providerErr *fantasy.ProviderError) error {
	if providerErr == nil {
		return fmt.Errorf("fantasy agent error: %w", err)
	}
	// Name the upstream cause when the gateway relayed one (OpenRouter's
	// "Provider returned error" is otherwise opaque in the dead-letter reason).
	if detail := providerErrorUpstreamDetail(providerErr); detail != "" {
		return fmt.Errorf("fantasy agent error (provider_status=%d, retryable=false, %s): %w", providerErr.StatusCode, detail, err)
	}
	return fmt.Errorf("fantasy agent error (provider_status=%d, retryable=false): %w", providerErr.StatusCode, err)
}

// committedSideEffectsBlockRecovery reports whether a transient-class failure
// must NOT be re-driven: a tool event landed after the (possibly resumed)
// attempt mark, so restarting from the round's input could replay an executed
// call (ADR-0035 / ADR-0065). A provider rejection is gated separately in
// canPromoteOnRejection so that its unsafe case stays terminal, not transient.
func committedSideEffectsBlockRecovery(sink *streamSink, attemptMark sinkMark, class streamErrorClass) bool {
	if sink.toolEventCount() <= attemptMark.toolEvents {
		return false
	}
	return class == streamErrorRetryExhausted || class == streamErrorStreamBlip || class == streamErrorContextTooLarge
}

// recordProviderHealthFailure feeds genuine provider failures into the circuit
// breaker (#267) so error frequency accumulates across runs. Cancellation, the
// cost ceiling, and prompt-too-large are not provider-health signals, so they
// don't count.
func (e *engine) recordProviderHealthFailure(activeModel fantasy.LanguageModel, class streamErrorClass, providerErr *fantasy.ProviderError) {
	if e == nil {
		return
	}
	switch class {
	case streamErrorRetryExhausted, streamErrorStreamBlip, streamErrorProviderRejected, streamErrorFatal:
		e.healthRegistry.RecordError(activeModel.Model(), streamErrorDesc(providerErr))
	default:
	}
}

// canPromoteOnRejection decides whether a provider rejection may swap to the
// configured fallback (ADR-0067): a fallback must exist and differ from the
// active model, and no tool event may have landed since attemptMark (which
// the caller has already advanced to a completed-step checkpoint when one
// applied), so the fallback's re-drive cannot replay an executed call.
func (e *engine) canPromoteOnRejection(sink *streamSink, attemptMark sinkMark, activeModel fantasy.LanguageModel, swappedToFallback bool) bool {
	if sink.toolEventCount() > attemptMark.toolEvents {
		return false
	}
	return canSwapFallback(e, activeModel, swappedToFallback)
}

func (e *engine) logProviderFailure(model fantasy.LanguageModel, class streamErrorClass, providerErr *fantasy.ProviderError) {
	if e == nil || e.logSession == nil || providerErr == nil {
		return
	}
	// Keep classification in exported logs even when recovery is suppressed.
	// Raw response bodies/headers can contain credentials and are never emitted.
	note := fmt.Sprintf("[provider-failure] model=%s status=%d class=%s msg=%q", model.Model(), providerErr.StatusCode, class,
		summarizeForLog(toolRedactor().Redact(providerErr.Message), 300))
	if detail := providerErrorUpstreamDetail(providerErr); detail != "" {
		note += " " + detail
	}
	msgType := messageTypeSystemRetry
	e.logSession.AddMessageWithMetadata(roleUser, note, nil, nil, &msgType, nil, nil, "")
}
