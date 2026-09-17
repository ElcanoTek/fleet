package redact

import (
	"encoding/json"
	"regexp"
	"strings"
)

var authorizationValue = regexp.MustCompile(`(?i)authorization:\s*(?:bearer|basic)\s+([A-Za-z0-9\-._~+/]+=*)`)
var secretJSONKey = regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd|authorization)(?:[_-][A-Za-z0-9_-]*)?$`)

// Redact decoded JSON strings, never the escapes framing them. In particular,
// a second marker pass over an escaped curl example must not eat its closing
// quote's backslash. Keep number lexemes and untouched documents byte-exact.
func (r *Redactor) redactJSON(input string) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return r.redactPlain(input)
	}
	// A returned bearer can occur twice: in an Authorization example and under
	// an unfamiliar field name. Scrub all copies in this response without adding
	// short-lived credentials to the process's permanent literal registry.
	var credentials []string
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case string:
			for _, match := range authorizationValue.FindAllStringSubmatch(x, -1) {
				if len(match[1]) >= minLiteralLen && match[1] != placeholder {
					credentials = append(credentials, match[1])
				}
			}
		case map[string]any:
			for key, child := range x {
				if value, ok := child.(string); ok {
					collect(key + ": " + value)
				}
				collect(child)
			}
		case []any:
			for _, child := range x {
				collect(child)
			}
		}
	}
	collect(value)
	changed := false
	var scrub func(any, string) any
	scrub = func(v any, key string) any {
		switch x := v.(type) {
		case string:
			clean := x
			for _, credential := range credentials {
				clean = strings.ReplaceAll(clean, credential, placeholder)
			}
			if secretJSONKey.MatchString(key) && len(clean) >= minLiteralLen {
				clean = placeholder
			} else {
				clean = r.redactPlain(clean)
			}
			changed = changed || clean != x
			return clean
		case json.Number:
			clean := r.redactPlain(x.String())
			if secretJSONKey.MatchString(key) && len(clean) >= minLiteralLen {
				clean = placeholder
			}
			if clean != x.String() {
				changed = true
				return clean
			}
			return x
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, child := range x {
				cleanKey := k
				for _, credential := range credentials {
					cleanKey = strings.ReplaceAll(cleanKey, credential, placeholder)
				}
				cleanKey = r.redactPlain(cleanKey)
				changed = changed || cleanKey != k
				out[cleanKey] = scrub(child, k)
			}
			return out
		case []any:
			for i, child := range x {
				x[i] = scrub(child, key)
			}
		}
		return v
	}
	value = scrub(value, "")
	if !changed {
		return input
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return placeholder
	} // never fall back to unsanitized bytes
	return string(encoded)
}
