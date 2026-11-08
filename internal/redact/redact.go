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
//
// Literals come in two flavors. A PERMANENT literal (AddLiteral) lives for the
// process lifetime: a boot-time env secret, a static connector API key. A
// SCOPED literal (RegisterSecrets with a scope) belongs to a named
// rotating credential set — one hosted-MCP server row, whose OAuth access +
// refresh pair is replaced wholesale on every refresh. Each rotation opens a new
// generation for that scope and retires the previous one after a grace window,
// so a long-lived process scanning for hourly-rotating tokens plateaus instead
// of accumulating every token it has ever seen (#1274).
package redact

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// placeholder is what every matched secret is replaced with.
const placeholder = "[REDACTED]"

// MarkerPrefix opens every replacement the redactor writes ("[REDACTED]",
// "[REDACTED PRIVATE KEY]"), so a reader of already-redacted text can tell
// that a secret was scrubbed out of it (an approval's result card,
// docs/APPROVAL-RESULT-CARDS.md).
const MarkerPrefix = "[REDACTED"

// minLiteralLen guards literal redaction against scrubbing short, common env
// values (e.g. "true", a port number) that happen to be registered.
const minLiteralLen = 8

// literalRetireGrace is how long a SUPERSEDED scoped literal — the access token
// an OAuth refresh just replaced, the refresh token that rotation consumed, the
// authorization code an exchange just spent — stays in the scan set after its
// replacement is registered.
//
// THE SAFETY TRADEOFF RUNS ONE WAY ONLY, and this constant is sized for it.
// Retiring too EARLY is an under-redaction bug: a secret that is still quoted
// in an in-flight request's error text would reach a log, an HTTP response or
// the model context in the clear, which is the same blast radius as a plaintext
// leak. Retiring too LATE costs one strings.ReplaceAll pass per Redact and some
// retained memory. So the window is sized for the longest plausible ECHO of a
// just-rotated secret rather than the shortest: a request that was already
// using the old token can still be in flight when the rotation lands, and its
// failure text then travels through retries, backoff, masked-error logging and
// an audit write before anyone reads it.
//
// Fifteen minutes is ~2 orders of magnitude above every request timeout on the
// paths that carry these secrets (remotemcp Config.HTTPTimeout 30s,
// RefreshTimeout 10s) and still bounds retention to about two generations for
// the hourly-expiry tokens that motivated #1274. Never shorten it to save
// memory: the memory is bounded by the generation swap, not by this number.
const literalRetireGrace = 15 * time.Minute

// There is deliberately NO count cap on a scope's generations. Every
// superseded generation stays scanned for the full literalRetireGrace, however
// fast the scope rotates: a refresh storm is exactly when an in-flight request
// still carrying an older token is likeliest to fail and echo it, and a cap
// that retired the oldest early (an earlier backstop of 4 generations did)
// turned the storm into a leak. The scan set is bounded by the rotation rate
// times the grace window — a scope refreshing every second for 15 minutes holds
// about 900 generations — and by the sweep, which drops each generation the
// moment its own window ends.

// pattern pairs a compiled regex with its replacement. Replacements that keep a
// captured group (e.g. "${1}[REDACTED]") preserve a leading marker so the output
// stays readable ("api_key=[REDACTED]").
type pattern struct {
	re   *regexp.Regexp
	repl string
	// hint is a substring every match contains (lower-cased; matched
	// case-insensitively when fold is set). ScrubLine skips the regex on a
	// line without it — the backstop runs on every log line, and most lines
	// carry no credential shape at all.
	hint string
	fold bool
}

// mayMatch reports whether p could match s (its hint is present).
func (p pattern) mayMatch(s, lower string) bool {
	switch {
	case p.hint == "":
		return true
	case p.fold:
		return strings.Contains(lower, p.hint)
	default:
		return strings.Contains(s, p.hint)
	}
}

// literal is one registered secret VALUE in the scan set.
type literal struct {
	value string
	// jsonForm and quotedForm are how value reads inside an encoded log line:
	// a JSON string (encoding/json, HTML escaping off, as log/slog writes it)
	// and a Go-quoted logfmt value (strconv.Quote). They equal value unless it
	// holds a quote, a backslash or a control character. ScrubLine matches
	// only the form of the line's own encoding, so a literal's raw bytes can
	// never match the encoding's syntax (a literal `level":"INFO` would
	// otherwise hit every JSON line).
	jsonForm, quotedForm string
	// permanent marks a literal registered without a scope (a boot-time env
	// secret); it is never retired.
	permanent bool
	// scopes are the rotating credential sets currently holding this value,
	// each with the generation it was last registered under. One value can be
	// live in several (a client secret shared by two hosted-MCP rows): it is
	// retired only once EVERY scope holding it has superseded it.
	scopes map[string]uint64
	// retireAt is the unix-nano deadline after which this value may be dropped;
	// 0 means "live, never drop". Only a value no scope holds any more is ever
	// given one, so a still-current secret cannot be evicted.
	retireAt int64
}

func newLiteral(value string) *literal {
	l := &literal{value: value, jsonForm: value, quotedForm: value}
	if strings.ContainsFunc(value, func(r rune) bool { return r == '"' || r == '\\' || r < 0x20 || r == 0x7f }) {
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(value) == nil {
			if js := strings.TrimSuffix(buf.String(), "\n"); len(js) >= 2 {
				l.jsonForm = js[1 : len(js)-1]
			}
		}
		if q := strconv.Quote(value); len(q) >= 2 {
			l.quotedForm = q[1 : len(q)-1]
		}
	}
	return l
}

// Redactor applies a prioritized list of patterns + registered literals to a
// string. Safe for concurrent use after construction: the patterns are fixed
// at NewRedactor, and the literal set is guarded by an RWMutex so AddLiteral
// and RegisterSecrets may be called at any time —
// including for secrets acquired at RUNTIME (e.g. a refreshed OAuth bearer,
// #1124) — while Redact runs on other goroutines.
type Redactor struct {
	patterns       []pattern
	customPatterns []pattern
	// shapePatterns is how many leading patterns are credential shapes
	// (ScrubLine applies only those).
	shapePatterns int

	// mu guards literals, index and scopeGen. index mirrors literals for O(1)
	// dedupe: runtime registration re-offers the same token on every
	// acquisition, and appending it each time would grow the scan list
	// unboundedly over the process lifetime.
	mu       sync.RWMutex
	literals []*literal
	index    map[string]*literal
	scopeGen map[string]uint64

	// nextRetire is the earliest pending retireAt (0 = nothing pending), read
	// WITHOUT the lock so the common Redact path stays a single RLock and only
	// pays for the write lock when a retirement is actually due.
	nextRetire atomic.Int64

	// now is the clock. nil means time.Now; tests substitute it to drive the
	// grace window deterministically.
	now func() time.Time
}

// canonicalPatterns are ordered most-specific-first so a vendor-prefixed key is
// replaced whole before the generic marker rule could capture only its value.
func canonicalPatterns() []pattern {
	return []pattern{
		// Entire PEM private-key blocks.
		{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]", "PRIVATE KEY-----", false},
		// Vendor API-key prefixes (specific → generic). Every prefix rule is
		// anchored with \b so a hyphenated prose word that happens to END in a
		// prefix ("desk-mounted-display-arm-…") is not eaten: the generic sk-
		// rule below admits `-` and `_` because current OpenAI keys carry
		// hyphenated sub-prefixes (sk-proj-…, sk-svcacct-…, sk-admin-…) that the
		// old alnum-only tail never matched past the first hyphen.
		{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{20,}`), placeholder, "sk-ant-", false},       // Anthropic
		{regexp.MustCompile(`\bsk-or-v1-[A-Za-z0-9\-_]{20,}`), placeholder, "sk-or-v1-", false},   // OpenRouter
		{regexp.MustCompile(`\bsk-[A-Za-z0-9\-_]{20,}`), placeholder, "sk-", false},               // OpenAI (incl. sk-proj-/sk-svcacct-) + generic sk-
		{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`), placeholder, "github_pat_", false}, // GitHub fine-grained PAT
		{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`), placeholder, "gh", false},            // GitHub classic PAT/OAuth/refresh
		{regexp.MustCompile(`\bglpat-[A-Za-z0-9\-_]{20,}`), placeholder, "glpat-", false},         // GitLab PAT
		{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}`), placeholder, "xox", false},        // Slack bot/user/app/refresh/config tokens
		{regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{10,}`), placeholder, "k_", false},  // Stripe secret / restricted keys
		{regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}`), placeholder, "AIza", false},              // Google API key
		{regexp.MustCompile(`\bAKIA[A-Z0-9]{16}`), placeholder, "AKIA", false},                    // AWS access key ID
		// HTTP Authorization: Bearer <token> (e.g. in captured curl/wget output).
		{regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)([A-Za-z0-9\-._~+/]+=*)`), "${1}" + placeholder, "authorization", true},
		// HTTP Authorization: Basic <base64(user:pass)>. A dedicated pattern
		// because the scheme word ("Basic", 5 chars) sits where the generic
		// marker rule expects the 8+-char value, so Basic credentials — which
		// decode to the plaintext user:password — used to pass through.
		{regexp.MustCompile(`(?i)(authorization:\s*basic\s+)([A-Za-z0-9+/\-._~]+=*)`), "${1}" + placeholder, "authorization", true},
		// Marker = value, including the JSON-quoted form {"api_key":"..."}: the
		// separator class includes : = whitespace and quotes so the value after a
		// recognized marker is scrubbed even with no spaces. Value is 8+ chars up
		// to the next delimiter. This closes the markerless-JSON gap.
		//
		// The keyword may also be an interior token of a longer key name
		// (aws_secret_access_key, secret_access_key, gcp_refresh_token, …): after
		// the keyword, `_`/`-`-led name characters are allowed before the
		// separator. Requiring that boundary keeps prose words that merely embed
		// a keyword (secretary, tokenizer) from matching.
		{regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|authorization)(?:[_-][A-Za-z0-9_-]*)?["']?\s*[:=]["'\s]*(?:(?:bearer|basic)\s+)?)([^\s"',}{]{8,})`), "${1}" + placeholder, "", false},
	}
}

// ScrubLine is the line-level backstop for an already-encoded log record (a
// JSON object or a logfmt line): the registered literals — in their raw and
// the form the line's encoding gives them — and the vendor credential
// shapes (PEM blocks, vendor key prefixes, Authorization: Bearer/Basic), but
// not the generic marker=value rule or operator patterns, which decide by a
// key name and could span the encoding's structure. Every placeholder is
// free of quotes and backslashes, so a replacement inside a quoted value
// keeps the line well-formed. Unlike Redact it never re-encodes: bytes it
// does not replace are left exactly as they were.
//
// jsonEncoding says how the line is encoded: true for a JSON record, false
// for a logfmt line. Each literal is matched in that encoding's form only.
func (r *Redactor) ScrubLine(line string, jsonEncoding bool) string {
	if line == "" || r == nil {
		return line
	}
	r.sweepDue()
	out := line
	r.mu.RLock()
	for _, lit := range r.literals {
		if lit.value == "" {
			continue
		}
		form := lit.quotedForm
		if jsonEncoding {
			form = lit.jsonForm
		}
		out = strings.ReplaceAll(out, form, placeholder)
	}
	r.mu.RUnlock()
	lower := strings.ToLower(out)
	for _, p := range r.patterns[:r.shapePatterns] {
		if p.mayMatch(out, lower) {
			out = p.re.ReplaceAllString(out, p.repl)
		}
	}
	return out
}

// NewRedactor returns a Redactor with the canonical patterns plus any extra
// caller-supplied regexes (invalid ones are skipped).
func NewRedactor(extraPatterns []string) *Redactor {
	pats := canonicalPatterns()
	canonicalCount := len(pats)
	for _, p := range extraPatterns {
		if re, err := regexp.Compile(p); err == nil {
			pats = append(pats, pattern{re: re, repl: placeholder})
		}
	}
	// Every canonical pattern but the last (the generic marker=value rule) is
	// a credential SHAPE, safe for ScrubLine.
	return &Redactor{patterns: pats, customPatterns: pats[canonicalCount:], shapePatterns: canonicalCount - 1}
}

// AddLiteral registers a raw value for PERMANENT literal redaction — a
// high-entropy secret discovered at startup, or a runtime-acquired credential
// that does not rotate on a clock (an unsealed connector API key). Values
// shorter than minLiteralLen are ignored to avoid scrubbing common short
// strings; duplicates are ignored so re-registering the same value on every
// acquisition cannot grow the scan list. Safe to call concurrently with Redact.
//
// Use RegisterSecrets with a scope instead for a credential set that rotates
// (an OAuth access+refresh pair): a scoped rotation retires superseded
// generations, which a permanent literal by definition never does.
func (r *Redactor) AddLiteral(value string) {
	r.addLiterals("", false, value)
}

// RegisterSecrets is the single entry point the host processes wire their
// runtime secret-acquisition observer to (see agentcore.RegisterSecretLiterals
// and mcpbroker.RegisterSecretLiterals): it maps the observer's (scope,
// rotated) shape onto the three registration modes so the rotation semantics
// live here and not in each caller:
//
//   - scope == "": a PERMANENT literal, exactly AddLiteral.
//   - rotated == false: values join scope's CURRENT generation without
//     superseding anything — a credential the scope is using right now (the
//     stored refresh token about to ride a token request, the client secret
//     authenticating it, a bearer an acquisition returned unchanged). Nothing
//     is retired, so this can never shorten a live secret's coverage.
//   - rotated == true: a NEW generation for scope with values as its complete
//     live set; the previous generations retire literalRetireGrace from now.
//     Call it exactly when a rotation has SUCCEEDED and pass every secret that
//     is live afterwards (the fresh access token, the fresh refresh token, the
//     client secret that still authenticates the connection) — a value
//     re-offered here is revived into the new generation, so listing a
//     still-live secret keeps it, while omitting one starts its grace clock.
func (r *Redactor) RegisterSecrets(scope string, rotated bool, values ...string) {
	r.addLiterals(scope, rotated, values...)
}

// addLiterals is the one registration path. rotated=true opens a new generation
// for scope (retiring the older ones); rotated=false joins the current one.
func (r *Redactor) addLiterals(scope string, rotated bool, values ...string) {
	if r == nil {
		return
	}
	scope = strings.TrimSpace(scope)
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= minLiteralLen {
			kept = append(kept, v)
		}
		// A multi-line secret (a PEM key, a JSON service-account file) is also
		// registered line by line: text that is scrubbed one line at a time
		// (a subprocess's stderr) never contains the whole value, so only its
		// lines can be matched there.
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
				if line = strings.TrimSpace(line); len(line) >= minLiteralLen && line != v {
					kept = append(kept, line)
				}
			}
		}
	}
	// A rotation with no usable value still has to run: it is what starts the
	// previous generation's grace clock. A non-rotating call with nothing to
	// register is a no-op.
	if len(kept) == 0 && (!rotated || scope == "") {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.index == nil {
		r.index = make(map[string]*literal, len(kept))
	}
	nowNano := r.nowNano()
	gen := uint64(0)
	if scope != "" {
		if r.scopeGen == nil {
			r.scopeGen = make(map[string]uint64)
		}
		gen = r.scopeGen[scope]
		if rotated || gen == 0 {
			gen++
			r.scopeGen[scope] = gen
		}
		if rotated {
			r.retireOlderGenerationsLocked(scope, gen, nowNano)
		}
	}
	for _, v := range kept {
		r.upsertLocked(v, scope, gen)
	}
	r.sweepLocked(nowNano)
}

// retireOlderGenerationsLocked starts the grace clock on every literal of scope
// older than gen. Each gets the full window from the moment it is superseded;
// one already counting down keeps its earlier deadline (it was superseded
// first), and nothing is made due early.
//
// The scope lets go of each such value; a value no scope holds any more (and
// that is not permanent) starts its grace clock. A value another scope still
// holds stays live.
func (r *Redactor) retireOlderGenerationsLocked(scope string, gen uint64, nowNano int64) {
	graceDeadline := nowNano + literalRetireGrace.Nanoseconds()
	for _, l := range r.literals {
		held, ok := l.scopes[scope]
		if !ok || held >= gen {
			continue
		}
		delete(l.scopes, scope)
		if !l.permanent && len(l.scopes) == 0 && l.retireAt == 0 {
			l.retireAt = graceDeadline
		}
	}
}

// upsertLocked adds value, or REVIVES an existing entry into (scope, gen).
// Reviving only ever lengthens a value's coverage:
//   - a permanent literal is never demoted into a retirable scope, because the
//     same bytes may also be a boot-time env secret that stays live for the
//     process lifetime;
//   - a scoped literal re-offered without a scope is promoted to permanent;
//   - a value offered by a second scope is held by both (scopes), so either
//     rotating away from it leaves it live for the other;
//   - a stale offer (an older generation of the same scope) does not pull a
//     value back into that older generation.
//
// Over-retention is acceptable here; under-redaction is not.
func (r *Redactor) upsertLocked(value, scope string, gen uint64) {
	l, ok := r.index[value]
	if !ok {
		l = newLiteral(value)
		r.literals = append(r.literals, l)
		r.index[value] = l
	}
	switch {
	case l.permanent:
		return // already permanent
	case scope == "":
		l.permanent, l.scopes = true, nil
	default:
		if held, holds := l.scopes[scope]; holds && gen < held {
			return // stale offer; the newer generation already covers it
		}
		if l.scopes == nil {
			l.scopes = make(map[string]uint64, 1)
		}
		l.scopes[scope] = gen
	}
	l.retireAt = 0
}

// sweepLocked drops every literal whose retirement deadline has passed and
// recomputes nextRetire. Caller holds the write lock.
func (r *Redactor) sweepLocked(nowNano int64) {
	next := int64(0)
	kept := r.literals[:0]
	for _, l := range r.literals {
		if l.retireAt != 0 && l.retireAt <= nowNano {
			delete(r.index, l.value)
			// Go strings are immutable, so a retired secret's bytes cannot be
			// wiped in place without unsafe — dropping the last reference to
			// them is what lets the GC reclaim them, and is the reason
			// retirement exists at all beyond bounding the scan cost.
			l.value = ""
			continue
		}
		if l.retireAt != 0 && (next == 0 || l.retireAt < next) {
			next = l.retireAt
		}
		kept = append(kept, l)
	}
	for i := len(kept); i < len(r.literals); i++ {
		r.literals[i] = nil // don't let the backing array pin retired entries
	}
	r.literals = kept
	r.nextRetire.Store(next)
}

// sweepDue retires what is due, taking the write lock ONLY when the lock-free
// pre-check says a deadline has passed.
func (r *Redactor) sweepDue() {
	next := r.nextRetire.Load()
	if next == 0 {
		return
	}
	if r.nowNano() < next {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(r.nowNano())
}

// nowNano reads the (optionally substituted) clock.
func (r *Redactor) nowNano() int64 {
	if r.now != nil {
		return r.now().UnixNano()
	}
	return time.Now().UnixNano()
}

// MaxLiteralLen is the byte length of the longest registered literal (0 for
// none). A caller that cuts text at a fixed point sizes its safety margin
// from it: a literal straddling the cut is never matched, so the bytes
// before the cut that could be its prefix must be withheld too.
func (r *Redactor) MaxLiteralLen() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, l := range r.literals {
		n = max(n, len(l.value))
	}
	return n
}

// Redact returns input with every matched secret replaced. Literals run first
// (exact, novel formats), then the shape patterns.
func (r *Redactor) Redact(input string) string {
	if input == "" || r == nil {
		return input
	}
	if json.Valid([]byte(input)) {
		// Operator patterns may span keys and values. Run them on the original
		// serialization too; an invalid structural replacement fails closed.
		for _, p := range r.customPatterns {
			input = p.re.ReplaceAllString(input, p.repl)
		}
		if !json.Valid([]byte(input)) {
			return `"[REDACTED]"`
		}
		return r.redactJSON(input)
	}
	return r.redactPlain(input)
}

func (r *Redactor) redactPlain(input string) string {
	r.sweepDue()
	out := input
	r.mu.RLock()
	for _, lit := range r.literals {
		if lit.value == "" {
			continue // retired mid-sweep; an empty needle would match everywhere
		}
		out = strings.ReplaceAll(out, lit.value, placeholder)
	}
	r.mu.RUnlock()
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

// IsSecretEnvName reports whether an env-var NAME denotes a credential whose
// value must never be echoed. It is the single name-based heuristic — the same
// one RegisterEnvLiterals uses to seed the scrubber — exported so CLI surfaces
// that render env files (`fleet env show`) mask by the identical rule instead
// of growing a second, drifting copy.
func IsSecretEnvName(name string) bool {
	return secretEnvNamePattern.MatchString(name)
}

// pathElementPattern matches one absolute filesystem path built only from the
// characters deployment paths use. "+" and "=" are left out on purpose: they
// are what standard base64 adds to this alphabet, so a token that merely
// happens to start with "/" rarely passes.
var pathElementPattern = regexp.MustCompile(`^/[A-Za-z0-9._@~-]+(/[A-Za-z0-9._@~-]*)*$`)

// IsPathListValue reports whether value is an absolute filesystem path or a
// colon-separated list of them (CUTLASS_ALLOWED_DIRS, a PATH-style setting).
// Such a value is configuration, not a credential, and as a literal it would
// blank every output that names a file under it — the run workspace included.
// Callers that register env values wholesale use it, together with
// IsSecretEnvName, to leave paths out.
func IsPathListValue(value string) bool {
	found := false
	for _, part := range strings.Split(value, ":") {
		if part == "" {
			continue
		}
		if !pathElementPattern.MatchString(part) {
			return false
		}
		found = true
	}
	return found
}

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
