package redact

import (
	"encoding/json"
	"regexp"
	"strings"
)

var authorizationValue = regexp.MustCompile(`(?i)authorization:\s*(?:bearer|basic)\s+([A-Za-z0-9\-._~+/]+=*)`)

// VaultReferencePrefix opens a show_ui credential reference ("$secret:<id>").
const VaultReferencePrefix = "$secret:"

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
// the whole value matches no registered secret, credential shape or custom
// pattern, and the id itself is nothing any rule would redact. A secret
// wrapped in the reference syntax — or one whose own bytes begin "secret:" —
// is therefore still redacted.
func (r *Redactor) keepVaultReference(key, value string, credentials []string) bool {
	if !refKey.MatchString(key) || !vaultReference.MatchString(value) {
		return false
	}
	if r.matchesLiteralOrShape(value) {
		return false
	}
	id := strings.TrimPrefix(value, VaultReferencePrefix)
	if looksLikeJWT(id) {
		return false
	}
	for _, c := range credentials {
		// A Basic credential is collected with its "=" padding, which a
		// field ID cannot hold.
		if t := strings.TrimRight(c, "="); t != "" && strings.Contains(value, t) {
			return false
		}
	}
	return r.redactPlain(id) == id
}

// looksLikeJWT reports whether id has the compact JWT/JWE shape: three
// dot-separated base64url segments whose header and payload both start "eyJ"
// (base64url for `{"`), or five dot-separated segments starting "eyJ", with no
// repeater "[index]" brackets. Plain field IDs such as "eyJ_password" or
// "creds[0].eyJ_password" are preserved.
func looksLikeJWT(id string) bool {
	if strings.Contains(id, "[") {
		return false
	}
	segs := strings.Split(id, ".")
	switch len(segs) {
	case 3:
		return strings.HasPrefix(segs[0], "eyJ") && strings.HasPrefix(segs[1], "eyJ")
	case 5:
		return strings.HasPrefix(segs[0], "eyJ")
	default:
		return false
	}
}

// matchesLiteralOrShape reports whether s matches a registered literal, a
// credential-shape pattern or an operator-configured custom pattern — every
// rule except the generic marker=value rule, whose "secret:<value>" arm would
// match the "$secret:<id>" prefix itself.
func (r *Redactor) matchesLiteralOrShape(s string) bool {
	r.sweepDue()
	r.mu.RLock()
	for _, lit := range r.literals {
		if lit.value != "" && strings.Contains(s, lit.value) {
			r.mu.RUnlock()
			return true
		}
	}
	r.mu.RUnlock()
	for _, p := range r.patterns[:r.shapePatterns] {
		if p.re.MatchString(s) {
			return true
		}
	}
	for _, p := range r.customPatterns {
		if p.re.MatchString(s) {
			return true
		}
	}
	return false
}

// MarkerArgumentRefusal is the reason a tool gives when it refuses an
// argument that carries MarkerPrefix: the model copied the placeholder out of
// an earlier, redacted call, and sending or typing it would act on the
// placeholder as if it were the value. It names the placeholder in a form the
// redactor leaves alone, so the model can still read it.
const MarkerArgumentRefusal = `an argument contains "[REDACTED]", the placeholder the conversation shows in place of a secret; it is not the secret itself. If you are quoting redacted text, leave the placeholder out. If the call needs the secret, it has to be supplied again: in a chat, ask the user to submit it with show_ui and pass the reference that submission gives you`

// VaultReferenceArgumentRefusal is the reason a saved custom connector tool
// gives when an argument carries a "$secret:" vault reference: custom
// connector tools have no show_ui vault and take their credentials from the
// saved connection instead, so substituting the argument would send or type
// the reference text itself.
const VaultReferenceArgumentRefusal = `an argument contains a "$secret:..." vault reference, which saved connector tools cannot resolve; saved connectors use the credentials stored on the connection itself, or use connector_studio / playwright_browser in an interactive chat`

// SavedConnectorMarkerRefusal is the reason a saved custom connector tool
// gives when its own persisted definition or credentials carry MarkerPrefix or
// VaultReferencePrefix: an earlier save recorded a placeholder or unresolved
// vault reference in place of a secret or template value.
const SavedConnectorMarkerRefusal = `this saved connector's stored definition or credentials contain "[REDACTED]" or an unresolved "$secret:..." reference instead of the real value; update or recreate the connector with real credentials before calling it`

// ContainsMarker reports whether any string or map key in v (a decoded JSON
// value: strings, maps, slices) carries a redaction marker: a value the model
// copied out of an earlier, redacted tool call, which must not be used as
// written.
func ContainsMarker(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, MarkerPrefix)
	case map[string]any:
		for k, c := range x {
			if strings.Contains(k, MarkerPrefix) || ContainsMarker(c) {
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

// ContainsVaultReference reports whether any string or map key in v (a decoded
// JSON value: strings, maps, slices) carries a "$secret:" vault reference.
func ContainsVaultReference(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, VaultReferencePrefix)
	case map[string]any:
		for k, c := range x {
			if strings.Contains(k, VaultReferencePrefix) || ContainsVaultReference(c) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if ContainsVaultReference(c) {
				return true
			}
		}
	}
	return false
}
