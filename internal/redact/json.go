package redact

import (
	"encoding/json"
	"regexp"
	"strings"
)

var authorizationValue = regexp.MustCompile(`(?i)authorization:\s*(?:bearer|basic)\s+([A-Za-z0-9\-._~+/]+=*)`)

// vaultReference is a show_ui credential reference ("$secret:<field_id>",
// docs/CONNECTOR-BUILDER.md §1.2): what a tool call carries in its *_ref
// arguments in place of a vaulted credential, resolved host-side. A field ID
// follows the form spec's id grammar (internal/genui: a letter or underscore,
// then up to 63 letters, digits or underscores); a nested field's reference
// is its path, joined with "." and with a repeater item's index in brackets
// ("creds[0].password"). Each segment is capped, not the path, so a long
// token wrapped in the syntax is not taken for one, while the deepest path a
// card can produce is. Redacted, a reference would hide from the model, on
// later turns, which credential its own earlier call used, so
// keepVaultReference keeps one; see there for when.
var vaultReference = regexp.MustCompile(`^\$secret:` + vaultSegment + `(?:\.` + vaultSegment + `){0,7}$`)

const vaultSegment = `[A-Za-z_][A-Za-z0-9_]{0,63}(?:\[[0-9]{1,6}\])?`

// refKey is the argument a reference is passed in (password_ref, secret_ref,
// username_ref).
var refKey = regexp.MustCompile(`(?i)_ref$`)

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
			if r.keepVaultReference(key, x, credentials) {
				return x
			}
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

// keepVaultReference reports whether a JSON string value is a vault reference
// to leave as it is: the value under a *_ref key is exactly "$secret:<id>",
// and the id itself is nothing any other rule would redact (no registered
// literal, no vendor key shape, no credential seen elsewhere in this payload).
// A secret wrapped in the reference syntax is therefore still redacted.
func (r *Redactor) keepVaultReference(key, value string, credentials []string) bool {
	if !refKey.MatchString(key) || !vaultReference.MatchString(value) {
		return false
	}
	id := strings.TrimPrefix(value, "$secret:")
	// Every JWT starts "eyJ" ({"); one fits the field ID grammar but is no
	// field ID.
	for _, seg := range strings.Split(id, ".") {
		if strings.HasPrefix(seg, "eyJ") {
			return false
		}
	}
	for _, c := range credentials {
		// A Basic credential is collected with its "=" padding, which a
		// field ID cannot hold.
		if t := strings.TrimRight(c, "="); t != "" && strings.Contains(id, t) {
			return false
		}
	}
	return r.redactPlain(id) == id
}

// MarkerArgumentRefusal is the reason a tool gives when it refuses an
// argument that carries MarkerPrefix: the model copied the placeholder out of
// an earlier, redacted call, and sending or typing it would act on the
// placeholder as if it were the value. It names the placeholder in a form the
// redactor leaves alone, so the model can still read it.
const MarkerArgumentRefusal = `an argument contains "[REDACTED]", the placeholder the conversation shows in place of a secret; it is not the secret itself. If you are quoting redacted text, leave the placeholder out. If the call needs the secret, it has to be supplied again: in a chat, ask the user to submit it with show_ui and pass the reference that submission gives you`

// ContainsMarker reports whether any string in v (a decoded JSON value:
// strings, maps, slices) carries a redaction marker: a value the model copied
// out of an earlier, redacted tool call, which must not be used as written.
func ContainsMarker(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, MarkerPrefix)
	case map[string]any:
		for _, c := range x {
			if ContainsMarker(c) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if ContainsMarker(c) {
				return true
			}
		}
	}
	return false
}
