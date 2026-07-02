// Package piiredact is an OPTIONAL, provider-neutral PII redaction pass for
// content that flows into the model context (#450). It is DEFAULT OFF and
// deterministic (regex + validators) so it runs self-hosted with NO model
// server; an external ONNX/HTTP classifier (e.g. Rampart) is a pluggable
// follow-on that can implement the same Redactor interface.
//
// It COMPLEMENTS internal/redact (which scrubs SECRETS unconditionally): PII
// redaction is opt-in, has strictness MODES, and reports structured findings
// (types + counts, NEVER raw values) for audit. It is applied at the SAME choke
// point internal/redact uses — tool OUTPUT, where external data (connector
// records, emails, tickets) first enters the model context — over plain TEXT
// only, never the cacheable system-prompt prefix or structured tool-call
// arguments, so the prompt-cache prefix-stability contract (#507) holds.
package piiredact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Mode is the strictness of the redaction pass.
type Mode string

const (
	// ModeOff disables the pass entirely (the default). A nil/off redactor is a
	// no-op so callers can wire it unconditionally.
	ModeOff Mode = "off"
	// ModeObserve detects and REPORTS PII (for audit) but passes the text through
	// UNCHANGED — a monitoring posture with no behavior change to the run.
	ModeObserve Mode = "observe"
	// ModeRedact replaces each detected span with a [PII:<kind>] marker so the
	// model sees the structure without the sensitive value.
	ModeRedact Mode = "redact"
	// ModeBlock WITHHOLDS the content: it is replaced wholesale with a block
	// notice so the value never reaches the model (fail-closed for that text).
	ModeBlock Mode = "block"
)

// ParseMode validates a configured mode string. Empty → ModeOff.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.TrimSpace(strings.ToLower(s))) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeObserve:
		return ModeObserve, nil
	case ModeRedact:
		return ModeRedact, nil
	case ModeBlock:
		return ModeBlock, nil
	default:
		return ModeOff, fmt.Errorf("invalid PII redaction mode %q (want off|observe|redact|block)", s)
	}
}

// Kind names a class of detected PII.
type Kind string

const (
	KindEmail      Kind = "email"
	KindSSN        Kind = "ssn"
	KindCreditCard Kind = "credit_card"
	KindIP         Kind = "ip"
	KindPhone      Kind = "phone"
)

// Finding is an audit record: a PII kind and how many spans matched. It NEVER
// carries the raw matched value — surfacing the value would defeat the purpose.
type Finding struct {
	Kind  Kind `json:"kind"`
	Count int  `json:"count"`
}

// Result is the outcome of a redaction pass.
type Result struct {
	// Text is the (possibly redacted / withheld) output. In observe mode it equals
	// the input.
	Text string
	// Findings lists the detected kinds + counts (empty when nothing matched).
	Findings []Finding
	// Blocked is true only in block mode when at least one span was detected (the
	// content was withheld).
	Blocked bool
}

// Found reports whether any PII was detected.
func (r Result) Found() bool { return len(r.Findings) > 0 }

// Summary renders the findings as "email×2, ssn×1" for an audit log line (no raw
// values). Empty string when nothing was found.
func (r Result) Summary() string {
	parts := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		parts = append(parts, fmt.Sprintf("%s×%d", f.Kind, f.Count))
	}
	return strings.Join(parts, ", ")
}

// Redactor is the provider-neutral interface. The deterministic PatternRedactor
// is the built-in impl; an external ONNX/HTTP classifier (Rampart) is a
// follow-on that satisfies the same contract.
type Redactor interface {
	Redact(text string) Result
	Mode() Mode
}

// piiPattern is a detector: a regex plus an optional validator that rejects a
// raw match (e.g. Luhn for cards, octet-range for IPs) to bound false positives.
type piiPattern struct {
	kind      Kind
	re        *regexp.Regexp
	validate  func(match string) bool // nil = accept any regex match
	blockword string                  // the marker written in redact mode
}

// PatternRedactor is the deterministic default implementation. It is safe for
// concurrent Redact calls after construction.
type PatternRedactor struct {
	mode     Mode
	patterns []piiPattern
}

// New builds a PatternRedactor for the given mode. ModeOff yields a redactor
// whose Redact is a pass-through no-op.
func New(mode Mode) *PatternRedactor {
	return &PatternRedactor{mode: mode, patterns: defaultPatterns()}
}

// Mode reports the configured strictness.
func (r *PatternRedactor) Mode() Mode { return r.mode }

// Redact runs the configured mode over text. Off/empty → pass-through. It
// collects validated, non-overlapping matches across all patterns (priority by
// pattern order, so a card claimed first isn't re-matched as a phone), then
// either reports (observe), rewrites with markers (redact), or withholds (block).
func (r *PatternRedactor) Redact(text string) Result {
	if r == nil || r.mode == ModeOff || text == "" {
		return Result{Text: text}
	}

	type span struct {
		start, end int
		kind       Kind
		marker     string
	}
	var spans []span
	for _, p := range r.patterns {
		for _, idx := range p.re.FindAllStringIndex(text, -1) {
			if p.validate != nil && !p.validate(text[idx[0]:idx[1]]) {
				continue
			}
			spans = append(spans, span{start: idx[0], end: idx[1], kind: p.kind, marker: p.blockword})
		}
	}
	if len(spans) == 0 {
		return Result{Text: text}
	}

	// Resolve overlaps: sort by start (then longer first), keep a span only when
	// it doesn't overlap one already kept. Pattern order broke ties at collection
	// time via append order, but sorting is by position; a longer earlier match
	// wins its region.
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	kept := spans[:0:0]
	lastEnd := -1
	for _, s := range spans {
		if s.start < lastEnd {
			continue // overlaps a kept span
		}
		kept = append(kept, s)
		lastEnd = s.end
	}

	counts := map[Kind]int{}
	for _, s := range kept {
		counts[s.kind]++
	}
	findings := findingsFrom(counts)

	switch r.mode {
	case ModeObserve:
		return Result{Text: text, Findings: findings}
	case ModeBlock:
		return Result{
			Text:     fmt.Sprintf("[BLOCKED: content withheld — PII detected (%s)]", summarize(findings)),
			Findings: findings,
			Blocked:  true,
		}
	default: // ModeRedact
		var b strings.Builder
		prev := 0
		for _, s := range kept {
			b.WriteString(text[prev:s.start])
			b.WriteString(s.marker)
			prev = s.end
		}
		b.WriteString(text[prev:])
		return Result{Text: b.String(), Findings: findings}
	}
}

func findingsFrom(counts map[Kind]int) []Finding {
	out := make([]Finding, 0, len(counts))
	for k, c := range counts {
		out = append(out, Finding{Kind: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func summarize(fs []Finding) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, string(f.Kind))
	}
	return strings.Join(parts, ",")
}

func marker(kind Kind) string { return "[PII:" + string(kind) + "]" }

// defaultPatterns is the built-in detector set, ordered by priority (earlier =
// preferred when spans overlap). Kept deliberately conservative to bound false
// positives — this is a redaction aid, not a certified DLP engine.
func defaultPatterns() []piiPattern {
	return []piiPattern{
		{
			kind:      KindEmail,
			re:        regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
			blockword: marker(KindEmail),
		},
		{
			// US SSN, hyphenated form only (bare 9-digit runs are too ambiguous).
			kind:      KindSSN,
			re:        regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
			blockword: marker(KindSSN),
		},
		{
			// Candidate card: 13–19 digits, optionally grouped by space/hyphen.
			// Validated by Luhn to reject arbitrary long digit runs.
			kind:      KindCreditCard,
			re:        regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),
			validate:  validLuhn,
			blockword: marker(KindCreditCard),
		},
		{
			kind:      KindIP,
			re:        regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),
			validate:  validIPv4,
			blockword: marker(KindIP),
		},
		{
			// Conservative NANP phone: optional +1, then 3-3-4 with a separator
			// between groups (a separator is REQUIRED so a bare 10-digit id isn't
			// swept up). Documented as best-effort; false positives are possible.
			kind:      KindPhone,
			re:        regexp.MustCompile(`(?:\+?1[ .\-])?\(?\d{3}\)?[ .\-]\d{3}[ .\-]\d{4}\b`),
			blockword: marker(KindPhone),
		},
	}
}

// validLuhn reports whether the digits in s pass the Luhn checksum (and number
// 13–19 digits). Separators are stripped first.
func validLuhn(s string) bool {
	digits := make([]int, 0, len(s))
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits = append(digits, int(c-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// validIPv4 rejects dotted quads with an octet > 255 (so "999.1.2.3" and version
// strings like "1.2.3.4000" don't count).
func validIPv4(s string) bool {
	octets := strings.Split(s, ".")
	if len(octets) != 4 {
		return false
	}
	for _, o := range octets {
		if len(o) == 0 || len(o) > 3 {
			return false
		}
		n := 0
		for _, c := range o {
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}
