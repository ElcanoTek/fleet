// Package redact is the centralized secret scrubber for fleet. It replaces the
// single marker-only regex that used to live in agentcore with a prioritized set
// of patterns (vendor key prefixes, PEM blocks, marker=value pairs including the
// JSON-quoted form) plus optional literal redaction of known high-entropy values
// discovered at startup (e.g. env-var secrets), so a novel key format is still
// scrubbed by value even when its shape isn't recognized.
//
// It is applied to tool OUTPUT before that text re-enters the model context, the
// SSE stream, the session log, or the turn-event DB — the blast radius of a
// leaked credential is the same as a plaintext leak, so redaction happens at the
// choke point where external data first enters fleet.
package redact

import (
	"regexp"
	"strings"
)

// placeholder is what every matched secret is replaced with.
const placeholder = "[REDACTED]"

// minLiteralLen guards literal redaction against scrubbing short, common env
// values (e.g. "true", a port number) that happen to be registered.
const minLiteralLen = 8

// pattern pairs a compiled regex with its replacement. Replacements that keep a
// captured group (e.g. "${1}[REDACTED]") preserve a leading marker so the output
// stays readable ("api_key=[REDACTED]").
type pattern struct {
	re   *regexp.Regexp
	repl string
}

// Redactor applies a prioritized list of patterns + registered literals to a
// string. Safe for concurrent Redact calls after construction; AddLiteral must
// not race with Redact (call it during setup, before first use).
type Redactor struct {
	patterns []pattern
	literals []string
}

// canonicalPatterns are ordered most-specific-first so a vendor-prefixed key is
// replaced whole before the generic marker rule could capture only its value.
func canonicalPatterns() []pattern {
	return []pattern{
		// Entire PEM private-key blocks.
		{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
		// Vendor API-key prefixes (specific → generic).
		{regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{20,}`), placeholder},   // Anthropic
		{regexp.MustCompile(`sk-or-v1-[A-Za-z0-9\-_]{20,}`), placeholder}, // OpenRouter
		{regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`), placeholder},          // OpenAI + generic sk-
		{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), placeholder},   // GitHub PAT/OAuth/refresh
		{regexp.MustCompile(`glpat-[A-Za-z0-9\-_]{20,}`), placeholder},    // GitLab PAT
		{regexp.MustCompile(`AKIA[A-Z0-9]{16}`), placeholder},             // AWS access key ID
		// HTTP Authorization: Bearer <token> (e.g. in captured curl/wget output).
		{regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)([A-Za-z0-9\-._~+/]+=*)`), "${1}" + placeholder},
		// Marker = value, including the JSON-quoted form {"api_key":"..."}: the
		// separator class includes : = whitespace and quotes so the value after a
		// recognized marker is scrubbed even with no spaces. Value is 8+ chars up
		// to the next delimiter. This closes the markerless-JSON gap.
		{regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|authorization)["']?\s*[:=]["'\s]*(?:bearer\s+)?)([^\s"',}{]{8,})`), "${1}" + placeholder},
	}
}

// NewRedactor returns a Redactor with the canonical patterns plus any extra
// caller-supplied regexes (invalid ones are skipped).
func NewRedactor(extraPatterns []string) *Redactor {
	pats := canonicalPatterns()
	for _, p := range extraPatterns {
		if re, err := regexp.Compile(p); err == nil {
			pats = append(pats, pattern{re, placeholder})
		}
	}
	return &Redactor{patterns: pats}
}

// AddLiteral registers a raw value for literal redaction (a high-entropy secret
// discovered at startup). Values shorter than minLiteralLen are ignored to avoid
// scrubbing common short strings. Call during setup, before the first Redact.
func (r *Redactor) AddLiteral(value string) {
	if len(value) >= minLiteralLen {
		r.literals = append(r.literals, value)
	}
}

// Redact returns input with every matched secret replaced. Literals run first
// (exact, novel formats), then the shape patterns.
func (r *Redactor) Redact(input string) string {
	if input == "" || r == nil {
		return input
	}
	out := input
	for _, lit := range r.literals {
		if lit != "" {
			out = strings.ReplaceAll(out, lit, placeholder)
		}
	}
	for _, p := range r.patterns {
		out = p.re.ReplaceAllString(out, p.repl)
	}
	return out
}

// secretEnvNamePattern recognizes env-var NAMES whose values should be
// registered as literals (so a connector secret of any shape is scrubbed by
// value). Conservative on purpose: only names that clearly denote a credential,
// so ordinary long values (PATH, URLs) are not blanket-redacted.
var secretEnvNamePattern = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|API_?KEY)`)

// RegisterEnvLiterals adds the values of secret-looking env vars (by name) to r
// as literals. environ is in os.Environ() form ("NAME=value").
func (r *Redactor) RegisterEnvLiterals(environ []string) {
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name, val := kv[:eq], kv[eq+1:]
		if secretEnvNamePattern.MatchString(name) {
			r.AddLiteral(val)
		}
	}
}
