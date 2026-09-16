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
	verifierEvidenceFields   = 64
	verifierEvidenceVisits   = 256
	verifierEvidenceDepth    = 8
	verifierEvidenceArrayCap = 32
)

// A bounded structural projection, not a connector contract. Field names and
// values remain data: the task determines their meaning and completion rules.
// The shared scrubber runs before parsing, including on decoded MCP text. Drop
// credential-bearing subtrees as well, and omit prose, URLs and bulk data.
var (
	verifierFieldKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)
	verifierValue    = regexp.MustCompile(`^[a-zA-Z0-9_./:+-]{1,128}$`)
	verifierPrivate  = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|authorization|cookie|ticket|private.?key|api.?key|access.?key)|^(headers?|env|environment)$`)
)

type verifierProjection struct {
	fields  map[string]any
	omitted bool
	visits  int
	bytes   int
}

func projectVerifierEvidence(raw string) verifierProjection {
	value := decodeVerifierJSON(raw)
	if value == nil {
		return verifierProjection{omitted: true}
	}
	projection := verifierProjection{fields: make(map[string]any)}
	projection.collect(value, "", 0)
	return projection
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

func privateVerifierKey(key string) bool {
	if verifierPrivate.MatchString(key) {
		return true
	}
	// A dotted key can itself name a credential subtree, e.g. env.HOME.
	for _, segment := range strings.Split(key, ".") {
		if verifierPrivate.MatchString(segment) {
			return true
		}
	}
	return false
}

func (p *verifierProjection) collect(value map[string]any, path string, depth int) {
	// Breadth-first keeps enclosing status/version fields ahead of deep profiles.
	// JSON Pointer paths distinguish literal "rows.revenue" from nested fields.
	type node struct {
		value map[string]any
		path  string
		depth int
	}
	queue := []node{{value, path, depth}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.value == nil || current.depth > verifierEvidenceDepth {
			p.omitted = true
			continue
		}
		keys := make([]string, 0, len(current.value))
		for key := range current.value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if p.visits >= verifierEvidenceVisits || len(p.fields) >= verifierEvidenceFields {
				p.omitted = true
				return
			}
			p.visits++
			if !verifierFieldKey.MatchString(key) || privateVerifierKey(key) {
				p.omitted = true
				continue
			}
			fieldPath := current.path + "/" + key // Allowed keys contain neither '/' nor '~'.
			v := current.value[key]
			switch nested := v.(type) {
			case map[string]any:
				if len(nested) == 0 {
					p.omitted = true
					continue
				}
				queue = append(queue, node{nested, fieldPath, current.depth + 1})
			case []any:
				if key == "content" && len(nested) == 1 {
					if block, ok := nested[0].(map[string]any); ok && block["type"] == "text" {
						if text, ok := block["text"].(string); ok {
							queue = append(queue, node{decodeVerifierJSON(text), fieldPath, current.depth + 1})
							p.omitted = true // The MCP wrapper itself is not retained.
							continue
						}
					}
				}
				p.add(fieldPath, nested)
			default:
				p.add(fieldPath, v)
			}
		}
	}
}

func verifierScalar(value any) bool {
	switch scalar := value.(type) {
	case string:
		return verifierValue.MatchString(scalar) && !strings.Contains(scalar, "://")
	case json.Number:
		return len(scalar) <= 32
	case bool, nil:
		return true
	default:
		return false
	}
}

func (p *verifierProjection) add(path string, value any) {
	valid := verifierScalar(value)
	if values, ok := value.([]any); ok {
		valid = len(values) <= verifierEvidenceArrayCap
		if valid {
			for _, v := range values {
				if !verifierScalar(v) {
					valid = false
					break
				}
			}
		}
	}
	if !valid {
		p.omitted = true
		return
	}
	encoded, err := json.Marshal(map[string]any{path: value})
	if err != nil || p.bytes+len(encoded) > verifierEvidenceByteCap {
		p.omitted = true
		return
	}
	p.bytes += len(encoded) // Per-field braces conservatively bound the final JSON.
	p.fields[path] = value
}
