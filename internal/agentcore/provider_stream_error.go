package agentcore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

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
