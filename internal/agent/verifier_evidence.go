package agent

import (
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
)

const (
	verifierEvidenceInputCap = 1 << 20
	verifierEvidenceByteCap  = 4096
	verifierEvidenceFields   = 32
	verifierEvidenceVisits   = 256
	verifierEvidenceDepth    = 4
)

// A bounded structural projection, not a connector contract. Field names and
// values remain data: the task determines their meaning and completion rules.
// The shared scrubber runs before parsing, including on decoded MCP text. Drop
// credential-bearing subtrees as well, and omit prose, URLs and data arrays.
var (
	verifierFieldKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`)
	verifierValue    = regexp.MustCompile(`^[a-zA-Z0-9_./:+-]{1,128}$`)
	verifierPrivate  = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|authorization|cookie|ticket|private.?key|api.?key|access.?key)|^(headers?|env|environment)$`)
)

type verifierProjection struct {
	fields map[string]any
	visits int
	bytes  int
}

func verifierEvidence(raw string) map[string]any {
	value := decodeVerifierJSON(raw)
	if value == nil {
		return nil
	}
	projection := verifierProjection{fields: make(map[string]any)}
	projection.collect(value, "", 0)
	return projection.fields
}

func decodeVerifierJSON(raw string) map[string]any {
	if len(raw) > verifierEvidenceInputCap {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(redactSecrets(raw)))
	decoder.UseNumber() // Keep large identifiers exact instead of rounding to float64.
	var value map[string]any
	if decoder.Decode(&value) != nil {
		return nil
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil
	}
	return value
}

func (p *verifierProjection) collect(value map[string]any, path string, depth int) {
	if depth > verifierEvidenceDepth {
		return
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if p.visits >= verifierEvidenceVisits || len(p.fields) >= verifierEvidenceFields {
			return
		}
		p.visits++
		if !verifierFieldKey.MatchString(key) || verifierPrivate.MatchString(key) {
			continue
		}
		v := value[key]
		switch nested := v.(type) {
		case map[string]any:
			p.collect(nested, path+key+".", depth+1)
		case []any:
			// MCP has a standard text-content wrapper. Other arrays are data
			// payloads; they never become a second bulk channel to the verifier.
			if key == "content" && len(nested) == 1 {
				if block, ok := nested[0].(map[string]any); ok && block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						p.collect(decodeVerifierJSON(text), path+key+".", depth+1)
					}
				}
			}
		default:
			p.add(path+key, v)
		}
	}
}

func (p *verifierProjection) add(path string, value any) {
	switch scalar := value.(type) {
	case string:
		if !verifierValue.MatchString(scalar) || strings.Contains(scalar, "://") {
			return
		}
	case json.Number:
		if len(scalar) > 32 {
			return
		}
	case bool, nil:
	default:
		return
	}
	encoded, err := json.Marshal(map[string]any{path: value})
	if err != nil || p.bytes+len(encoded) > verifierEvidenceByteCap {
		return
	}
	p.bytes += len(encoded) // Per-field braces conservatively bound the final JSON.
	p.fields[path] = value
}
