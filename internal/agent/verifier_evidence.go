package agent

import (
	"encoding/json"
	"regexp"
	"sort"
)

// Keep only bounded control fields. Never forward arbitrary tool output, code,
// report rows, credentials, download URLs, or free-text instructions to the
// verifier. Field paths preserve the difference between a requested outcome
// and a returned freshness/status envelope. This is connector-independent.
var verifierIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_./:+-]{1,128}$`)

func verifierEvidence(raw string) map[string]any {
	if len(raw) > 1<<20 {
		return nil
	}
	var value map[string]any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return nil
	}
	result := make(map[string]any)
	collectVerifierEvidence(value, "", 0, result)
	return result
}

func collectVerifierEvidence(value map[string]any, path string, depth int, result map[string]any) {
	if depth > 4 {
		return
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(result) >= 32 {
			return
		}
		v := value[key]
		switch key {
		case "status", "outcome", "last_check_outcome", "slug", "path", "source_id", "id", "version_id", "live_version_id", "expected_version",
			"source_as_of", "source_as_of_seen", "last_check_source_as_of", "dataThrough", "schema_sha256", "template_sha256":
			if text, ok := v.(string); ok && verifierIdentifier.MatchString(text) {
				result[path+key] = text
			}
		case "success", "ok", "complete", "isError", "version_is_live", "page_is_live", "disabled", "require_approval", "publish":
			if flag, ok := v.(bool); ok {
				result[path+key] = flag
			}
		case "freshness", "envelope", "page", "version", "result", "structuredContent":
			if nested, ok := v.(map[string]any); ok {
				collectVerifierEvidence(nested, path+key+".", depth+1, result)
			}
		case "content":
			// MCP's text-content wrapper may contain the JSON response. Do not
			// interpret arbitrary plain text or recursively follow unknown fields.
			if blocks, ok := v.([]any); ok && len(blocks) == 1 {
				if block, ok := blocks[0].(map[string]any); ok && block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						var nested map[string]any
						if json.Unmarshal([]byte(text), &nested) == nil {
							collectVerifierEvidence(nested, path+"content.", depth+1, result)
						}
					}
				}
			}
		}
	}
}
