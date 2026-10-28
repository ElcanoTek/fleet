package agentcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

// isUnretriedMidStreamError reports whether err is an in-band SSE stream error
// (Title=="stream error" and StatusCode==0 before normalization) that Fantasy's
// pre-stream retry loop never retried (no *fantasy.RetryError wrapper). When no
// fallback model is configured (e.g., interactive chat), streamErrorRetryExhausted
// grants the same single bounded same-model retry that streamErrorStreamBlip gets.
func isUnretriedMidStreamError(err error) bool {
	if err == nil {
		return false
	}
	var retryErr *fantasy.RetryError
	if errors.As(err, &retryErr) {
		return false
	}
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == 0 && providerErr.Title == "stream error"
}

// Some OpenAI-compatible providers send numeric HTTP codes inside an SSE
// error envelope. Fantasy's adapter decodes code as a string and can lose the
// retry classification while preserving ResponseBody. Recover only structured
// status/type information; never infer an HTTP status from prose or log the body.
func normalizeStreamProviderError(original *fantasy.ProviderError) *fantasy.ProviderError {
	if original == nil || original.StatusCode != 0 || original.Title != "stream error" {
		return original
	}
	normalized := *original
	var envelope struct {
		Error struct {
			Code json.RawMessage `json:"code"`
			Type string          `json:"type"`
		} `json:"error"`
	}
	if len(original.ResponseBody) <= 64*1024 {
		_ = json.Unmarshal(original.ResponseBody, &envelope)
	}
	code := strings.Trim(string(envelope.Error.Code), `"`)
	if status, err := strconv.Atoi(code); err == nil && status >= 400 && status <= 599 {
		normalized.StatusCode = status
		normalized.TransientError = false
		return &normalized
	}
	if fantasy.TransientStreamErrorTypes[envelope.Error.Type] || fantasy.TransientStreamErrorTypes[code] {
		normalized.TransientError = true
	} else if code == "" && envelope.Error.Type == "" && original.Message == "Provider returned error" {
		// Opaque provider failures may recover within the bounded in-run ladder.
		// Explicit validation/auth codes above never take this path.
		normalized.TransientError = true
	}
	return &normalized
}

// upstreamErrorDetail is the structured `error.metadata` a relaying gateway
// (OpenRouter) attaches when the upstream provider failed: the provider that
// actually rejected the call, its typed error class, its own code, and its
// raw message. Only these named fields are ever read; the rest of the
// response body (headers, request echo) stays unlogged.
type upstreamErrorDetail struct {
	ProviderName string
	ErrorType    string
	ProviderCode string
	Raw          string
	// Code and Message are the gateway's own top-level error.code and
	// error.message, read only to classify the failure (never logged).
	Code    string
	Message string
}

// upstreamDetailMaxBody bounds how much response body the parser will scan.
const upstreamDetailMaxBody = 64 * 1024

// upstreamRawMaxLen bounds the upstream message carried into log notes and
// dead-letter reasons.
const upstreamRawMaxLen = 300

// parseUpstreamErrorDetail extracts OpenRouter's error.metadata from a
// provider error body. The body may be a bare JSON envelope (an SSE error
// event) or an httputil response dump (headers, blank line, JSON body).
func parseUpstreamErrorDetail(body []byte) upstreamErrorDetail {
	var out upstreamErrorDetail
	if len(body) == 0 || len(body) > upstreamDetailMaxBody {
		return out
	}
	text := string(body)
	if strings.HasPrefix(text, "HTTP/") {
		if idx := strings.Index(text, "\r\n\r\n"); idx >= 0 {
			text = text[idx+4:]
		} else if idx := strings.Index(text, "\n\n"); idx >= 0 {
			text = text[idx+2:]
		}
	}
	text = trimToJSONObject(text)
	if text == "" {
		return out
	}
	var envelope struct {
		Error struct {
			Code     json.RawMessage            `json:"code"`
			Message  string                     `json:"message"`
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return out
	}
	out.ProviderName = rawMessageText(envelope.Error.Metadata["provider_name"])
	out.ErrorType = rawMessageText(envelope.Error.Metadata["error_type"])
	out.ProviderCode = rawMessageText(envelope.Error.Metadata["provider_code"])
	out.Raw = rawMessageText(envelope.Error.Metadata["raw"])
	out.Code = rawMessageText(envelope.Error.Code)
	out.Message = strings.TrimSpace(envelope.Error.Message)
	return out
}

// rawMessageText renders a metadata value: JSON strings are unquoted, any
// other value (an object, a number) is kept as compact JSON.
func rawMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

// providerErrorUpstreamDetail returns a single-line, redacted, bounded
// description of the upstream cause behind a relayed provider error, or ""
// when the body carries none. It feeds the [provider-failure] log note and
// the terminal error text so an operator can tell a Google 400 from an
// OpenRouter one without the raw body ever being logged.
func providerErrorUpstreamDetail(providerErr *fantasy.ProviderError) string {
	if providerErr == nil {
		return ""
	}
	detail := parseUpstreamErrorDetail(providerErr.ResponseBody)
	var parts []string
	if detail.ProviderName != "" {
		parts = append(parts, "provider="+detail.ProviderName)
	}
	if detail.ErrorType != "" {
		parts = append(parts, "error_type="+detail.ErrorType)
	}
	if detail.ProviderCode != "" {
		parts = append(parts, "provider_code="+detail.ProviderCode)
	}
	if detail.Raw != "" {
		raw := strings.Join(strings.Fields(toolRedactor().Redact(detail.Raw)), " ")
		parts = append(parts, fmt.Sprintf("raw=%q", truncate(raw, upstreamRawMaxLen)))
	}
	return strings.Join(parts, " ")
}

// contextLengthCode is the error code OpenAI-compatible providers use for an
// over-window prompt. OpenRouter relays it either as the top-level error.code
// or as error.metadata.provider_code.
const contextLengthCode = "context_length_exceeded"

// Over-window prompt messages that fantasy's OpenAI-compatible error parser
// does not recognize, so a relayed rejection would otherwise be classed as an
// ordinary 4xx and end the turn instead of compacting:
//   - OpenAI:    "Your input exceeds the context window of this model."
//     and "This model's maximum context length is N tokens. However, your
//     messages resulted in M tokens."
//   - Anthropic: "prompt is too long: M tokens > N maximum" (recognized by
//     fantasy's native Anthropic provider, not when relayed by OpenRouter
//     inside error.metadata.raw).
//   - Google:    "The input token count exceeds the maximum number of tokens
//     allowed N." (Gemini 4, observed live through OpenRouter 2026-10-14),
//     and the older "The input token count (M) exceeds the maximum number of
//     tokens allowed (N)."
var (
	contextWindowExceededPattern = regexp.MustCompile(`(?i)\bexceeds the context window\b|\bcontext[_ ]length[_ ]exceeded\b`)
	promptTooLongPattern         = regexp.MustCompile(`(?i)\bprompt is too long:\s*(\d+)\s*tokens?\s*>\s*(\d+)\s*maximum\b`)
	maxContextLengthPattern      = regexp.MustCompile(`(?i)\bmaximum context length is (\d+) tokens\b(?:[^"}]{0,120}?\bresulted in (\d+) tokens\b)?`)
	inputTokenCountPattern       = regexp.MustCompile(`(?i)\binput token count\b(?:\s*\(?(\d+)\)?)?\s+exceeds the maximum number of tokens allowed\b(?:\s*\(?(\d+)\)?)?`)
)

// contextLengthRejection reports whether a provider error is really an
// over-window prompt that fantasy left unflagged, and returns a copy marked
// ContextTooLargeErr (with the used/max token counts when the message carries
// them) so the resilience loop takes the forced-compaction path and records
// the observed window. Credential and billing failures never match: a
// compacted prompt would not get past them.
//
// Only the adapter's message, the gateway's error.code/error.message and the
// structured upstream detail (error.metadata) are consulted.
func contextLengthRejection(providerErr *fantasy.ProviderError) (*fantasy.ProviderError, bool) {
	if providerErr == nil || providerErr.AuthError {
		return nil, false
	}
	switch providerErr.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return nil, false
	}
	if providerErr.StatusCode >= http.StatusInternalServerError {
		return nil, false
	}
	detail := parseUpstreamErrorDetail(providerErr.ResponseBody)
	texts := []string{providerErr.Message, detail.Message, detail.Raw}
	matched := strings.EqualFold(detail.Code, contextLengthCode) || strings.EqualFold(detail.ProviderCode, contextLengthCode)
	used, maxTokens := 0, 0
	for _, text := range texts {
		if text == "" {
			continue
		}
		if contextWindowExceededPattern.MatchString(text) {
			matched = true
		}
		if m := promptTooLongPattern.FindStringSubmatch(text); m != nil {
			matched = true
			used, maxTokens = atoiOrZero(m[1]), atoiOrZero(m[2])
		} else if m := inputTokenCountPattern.FindStringSubmatch(text); m != nil {
			matched = true
			used, maxTokens = atoiOrZero(m[1]), atoiOrZero(m[2])
		} else if m := maxContextLengthPattern.FindStringSubmatch(text); m != nil {
			matched = true
			maxTokens, used = atoiOrZero(m[1]), atoiOrZero(m[2])
		}
	}
	if !matched {
		return nil, false
	}
	flagged := *providerErr
	flagged.ContextTooLargeErr = true
	if flagged.ContextUsedTokens == 0 {
		flagged.ContextUsedTokens = used
	}
	if flagged.ContextMaxTokens == 0 {
		flagged.ContextMaxTokens = maxTokens
	}
	return &flagged, true
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// expiredPromptCachePattern matches the upstream message Google returns when
// the implicit prompt cache a long conversation was riding on has been
// evicted between two steps: `Cache content <id> is expired.` (status
// INVALID_ARGUMENT, relayed by OpenRouter as a 400 "Provider returned error").
var expiredPromptCachePattern = regexp.MustCompile(`(?i)\bcache(?:d)? content\b[^"}]{0,80}\bis expired\b`)

// isExpiredPromptCacheRejection reports whether a 4xx provider error is really
// an expired server-side prompt cache. The request itself is well-formed — the
// same messages re-sent a moment later build a fresh cache and succeed — so the
// error is transient for THIS request, not a per-request rejection (ADR-0067)
// that should promote the fallback model, and certainly not terminal. A
// scheduled page refresh dead-lettered on exactly this after 10 minutes of
// work because the 400 was classed as a rejection past a tool step, where the
// rejection path stays terminal by design.
//
// Only the structured upstream detail (OpenRouter's error.metadata.raw) and
// the adapter's own message are consulted; nothing else in the body is read.
func isExpiredPromptCacheRejection(providerErr *fantasy.ProviderError) bool {
	if providerErr == nil || providerErr.AuthError || providerErr.StatusCode != 400 {
		return false
	}
	if expiredPromptCachePattern.MatchString(providerErr.Message) {
		return true
	}
	detail := parseUpstreamErrorDetail(providerErr.ResponseBody)
	return expiredPromptCachePattern.MatchString(detail.Raw)
}
