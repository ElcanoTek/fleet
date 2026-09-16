package agentcore

import (
	"encoding/json"
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
