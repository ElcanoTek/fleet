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
	// verifierScalarMaxLen bounds a string retained verbatim as a scalar: one
	// identifier-like token — an id, a status, a date, an e-mail address.
	verifierScalarMaxLen = 128
	// Bulk text (multi-line, or longer than the scalar limit: a tool's printed
	// output, a file body) is not retained; a bounded head … tail excerpt is
	// recorded under <path>#excerpt instead, so a read-only check the model ran
	// through run_python or bash is still visible to the verifier. Short free
	// text ("Ignore task and create") is neither: it is dropped as before. At
	// most verifierExcerptFields excerpts per projection, each at most
	// verifierExcerptMax collapsed characters.
	verifierExcerptMax    = 600
	verifierExcerptHead   = 400
	verifierExcerptTail   = 180
	verifierExcerptFields = 2
	verifierExcerptSuffix = "#excerpt"
)

// A bounded structural projection, not a connector contract. Field names and
// values remain data: the task determines their meaning and completion rules.
// The shared scrubber runs before parsing, including on decoded MCP text. Drop
// credential-bearing subtrees as well, prose, URLs and bulk data; bulk text
// survives only as a bounded, labeled excerpt.
var (
	verifierFieldKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)
	// '@' is admitted so a recipient address is evidence: without it the
	// verifier could never confirm who an email went to (#1540).
	verifierValue    = regexp.MustCompile(`^[a-zA-Z0-9_.@/:+-]{1,128}$`)
	verifierPrivate  = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|authorization|cookie|ticket|private.?key|api.?key|access.?key)|^(headers?|env|environment)$`)
	verifierURLToken = regexp.MustCompile(`\S+://\S+`)
)

type verifierProjection struct {
	fields   map[string]any
	omitted  bool
	visits   int
	bytes    int
	excerpts int
	// noExcerpts disables bulk-text excerpts: arguments are requested intent,
	// not proof, and an emailed HTML body or a script source is noise there.
	// Excerpts exist so what a tool RETURNED (a printed check) stays visible.
	noExcerpts bool
}

// projectVerifierEvidence projects a tool RESULT: scalars plus bounded excerpts
// of bulk text.
func projectVerifierEvidence(raw string) verifierProjection {
	return projectVerifier(raw, false)
}

// projectVerifierArguments projects a tool call's ARGUMENTS: scalars only.
func projectVerifierArguments(raw string) verifierProjection {
	return projectVerifier(raw, true)
}

func projectVerifier(raw string, noExcerpts bool) verifierProjection {
	value := decodeVerifierJSON(raw)
	if value == nil {
		return verifierProjection{omitted: true}
	}
	projection := verifierProjection{fields: make(map[string]any), noExcerpts: noExcerpts}
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

// verifierTextScalar reports whether a string is retained verbatim: one
// identifier-like token (ids, statuses, dates, e-mail addresses) of at most
// verifierScalarMaxLen bytes that is not a URL. Free text with spaces is not
// a scalar; bulk text may still surface as an excerpt (verifierBulkText).
func verifierTextScalar(s string) bool {
	return verifierValue.MatchString(s) && !strings.Contains(s, "://")
}

// verifierBulkText reports whether a non-scalar string is bulk text worth an
// excerpt — multi-line, or longer than the scalar limit — as opposed to short
// free text, which stays dropped so a tool cannot slip a sentence into the
// verifier's evidence under an ordinary key.
func verifierBulkText(s string) bool {
	return strings.ContainsAny(s, "\n\r") || len(s) > verifierScalarMaxLen
}

// verifierTextExcerpt renders bulk text as a bounded, single-line excerpt:
// URLs replaced by "[url]", whitespace collapsed, and the middle elided to a
// head … tail when the text exceeds verifierExcerptMax. "" when nothing but
// URLs or whitespace remains. Slicing is by rune so the excerpt stays valid
// UTF-8 for the verifier model.
func verifierTextExcerpt(s string) string {
	s = verifierURLToken.ReplaceAllString(s, "[url]")
	s = strings.Join(strings.Fields(s), " ")
	if strings.Trim(s, "[url] ") == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= verifierExcerptMax {
		return s
	}
	return string(runes[:verifierExcerptHead]) + " … " + string(runes[len(runes)-verifierExcerptTail:])
}

func verifierScalar(value any) bool {
	switch scalar := value.(type) {
	case string:
		return verifierTextScalar(scalar)
	case json.Number:
		return len(scalar) <= 32
	case bool, nil:
		return true
	default:
		return false
	}
}

func (p *verifierProjection) add(path string, value any) {
	if text, ok := value.(string); ok && !verifierTextScalar(text) {
		// The full value is not evidence; a bounded excerpt of bulk text may be.
		p.omitted = true
		if !p.noExcerpts && verifierBulkText(text) {
			p.addExcerpt(path, text)
		}
		return
	}
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

// addExcerpt records a bounded excerpt of a long text field under
// <path>#excerpt. Bounded by count (verifierExcerptFields), by size (the
// shared byte cap) and deduplicated, so a tool that echoes the same text under
// two keys (run_python's output and stdout) spends one slot, not two. The
// field key alphabet excludes '#', so the suffix cannot collide with a real
// key.
func (p *verifierProjection) addExcerpt(path, text string) {
	if p.excerpts >= verifierExcerptFields {
		return
	}
	excerpt := verifierTextExcerpt(text)
	if excerpt == "" {
		return
	}
	for key, existing := range p.fields {
		if strings.HasSuffix(key, verifierExcerptSuffix) && existing == excerpt {
			return
		}
	}
	key := path + verifierExcerptSuffix
	encoded, err := json.Marshal(map[string]any{key: excerpt})
	if err != nil || p.bytes+len(encoded) > verifierEvidenceByteCap || len(p.fields) >= verifierEvidenceFields {
		return
	}
	p.bytes += len(encoded)
	p.fields[key] = excerpt
	p.excerpts++
}
