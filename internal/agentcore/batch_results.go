package agentcore

import (
	"bytes"
	"encoding/json"
	"strings"
)

// maxDealOutcomesBytes is the largest result parseDealOutcomes inspects; a
// larger per-record envelope is uninspectable and fails closed
// (malformedDealOutcomes). It also bounds the raw result text handed to the
// policy (policyResultText).
const maxDealOutcomesBytes = 8 * 1024 * 1024

// malformedDealOutcomes reports a response that carries (or may carry) a
// per-record results envelope which parseDealOutcomes did NOT accept. Such a
// payload must fail closed — it is NOT a single-call success, so the
// input-derived record ids must never be credited as done (ported from cutlass
// malformedDealOutcomes, Codex review on cutlass#1067). True when:
//   - the object is too large to inspect (maxDealOutcomesBytes) or is
//     object-prefixed but does not decode;
//   - a top-level "results" key (matched case-insensitively, as
//     parseDealOutcomes' struct decode matches it) is present and non-null but
//     is not an array, is an empty array, or has any row that is not an object
//     or that carries a deal_id/success key (parseDealOutcomes refused it, so
//     one of the two is missing or mistyped).
//
// Only a non-empty results array whose rows are all objects with neither
// deal_id nor success (an unrelated list shape) is left to the single-call
// path.
func malformedDealOutcomes(resultText string) bool {
	s := strings.TrimSpace(resultText)
	if s == "" || s[0] != '{' {
		return false
	}
	if len(s) > maxDealOutcomesBytes {
		return true
	}
	// Decode the FIRST JSON value, as parseDealOutcomes does, so trailing text
	// (a post_tool_use fragment, concatenated MCP text blocks) cannot suppress
	// validation. An object-prefixed body that will not decode is
	// uninspectable — its keys may be escaped or truncated — so it fails
	// closed.
	var top map[string]json.RawMessage
	if json.NewDecoder(strings.NewReader(s)).Decode(&top) != nil {
		return true
	}
	for k, v := range top {
		if !strings.EqualFold(k, "results") {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			continue
		}
		if malformedResultRows(v) {
			return true
		}
	}
	return false
}

// malformedResultRows reports a non-null results value that is not a non-empty
// array of objects free of deal_id/success keys.
func malformedResultRows(v json.RawMessage) bool {
	var rows []json.RawMessage
	if json.Unmarshal(v, &rows) != nil || len(rows) == 0 {
		return true
	}
	for _, raw := range rows {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil || row == nil {
			return true
		}
		for rk := range row {
			if strings.EqualFold(rk, "deal_id") || strings.EqualFold(rk, "success") {
				return true
			}
		}
	}
	return false
}
