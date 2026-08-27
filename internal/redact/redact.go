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
// SCOPED literal (AddScopedLiterals / RotateScopedLiterals) belongs to a named
// rotating credential set — one hosted-MCP server row, whose OAuth access +
// refresh pair is replaced wholesale on every refresh. Each rotation opens a new
// generation for that scope and retires the previous one after a grace window,
// so a long-lived process scanning for hourly-rotating tokens plateaus instead
// of accumulating every token it has ever seen (#1274).
package redact

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// placeholder is what every matched secret is replaced with.
const placeholder = "[REDACTED]"

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

// maxScopeGenerations bounds how many generations of ONE scope may sit in the
// scan set at once (the live generation plus superseded ones still inside their
// grace window). It is a backstop, not the primary mechanism: it engages only
// when a scope rotates FASTER than literalRetireGrace, where the grace window
// alone would let generations pile up (a refresh storm, a misconfigured
// zero-lifetime token). It can never drop the live generation — a value it
// drops has been superseded maxScopeGenerations-1 times over, and for a rotated
// OAuth pair that means the authorization server itself has invalidated it.
const maxScopeGenerations = 4

// pattern pairs a compiled regex with its replacement. Replacements that keep a
// captured group (e.g. "${1}[REDACTED]") preserve a leading marker so the output
// stays readable ("api_key=[REDACTED]").
type pattern struct {
	re   *regexp.Regexp
	repl string
}

// literal is one registered secret VALUE in the scan set.
type literal struct {
	value string
	// scope names the rotating credential set this value belongs to, or "" for
	// a permanent literal (a boot-time env secret), which is never retired.
	scope string
	// gen is the scope's generation this value was last registered under.
	gen uint64
	// retireAt is the unix-nano deadline after which this value may be dropped;
	// 0 means "live, never drop". Only a SUPERSEDED generation is ever given
	// one, so a still-current secret cannot be evicted.
	retireAt int64
}

// Redactor applies a prioritized list of patterns + registered literals to a
// string. Safe for concurrent use after construction: the patterns are fixed
// at NewRedactor, and the literal set is guarded by an RWMutex so AddLiteral,
// AddScopedLiterals and RotateScopedLiterals may be called at any time —
// including for secrets acquired at RUNTIME (e.g. a refreshed OAuth bearer,
// #1124) — while Redact runs on other goroutines.
type Redactor struct {
	patterns []pattern

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
		// HTTP Authorization: Basic <base64(user:pass)>. A dedicated pattern
		// because the scheme word ("Basic", 5 chars) sits where the generic
		// marker rule expects the 8+-char value, so Basic credentials — which
		// decode to the plaintext user:password — used to pass through.
		{regexp.MustCompile(`(?i)(authorization:\s*basic\s+)([A-Za-z0-9+/\-._~]+=*)`), "${1}" + placeholder},
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
		{regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|authorization)(?:[_-][A-Za-z0-9_-]*)?["']?\s*[:=]["'\s]*(?:(?:bearer|basic)\s+)?)([^\s"',}{]{8,})`), "${1}" + placeholder},
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

// AddLiteral registers a raw value for PERMANENT literal redaction — a
// high-entropy secret discovered at startup, or a runtime-acquired credential
// that does not rotate on a clock (an unsealed connector API key). Values
// shorter than minLiteralLen are ignored to avoid scrubbing common short
// strings; duplicates are ignored so re-registering the same value on every
// acquisition cannot grow the scan list. Safe to call concurrently with Redact.
//
// Use AddScopedLiterals/RotateScopedLiterals instead for a credential set that
// rotates (an OAuth access+refresh pair): those retire superseded generations,
// which a permanent literal by definition never does.
func (r *Redactor) AddLiteral(value string) {
	r.addLiterals("", false, value)
}

// AddScopedLiterals registers values as part of scope's CURRENT generation
// without superseding anything: the credential is one the scope is using right
// now (the stored refresh token about to ride a token request, the client
// secret authenticating it, the bearer an acquisition returned unchanged).
// Nothing is retired, so this can never shorten a live secret's coverage. An
// empty scope degrades to AddLiteral.
func (r *Redactor) AddScopedLiterals(scope string, values ...string) {
	r.addLiterals(scope, false, values...)
}

// RotateScopedLiterals opens a NEW generation for scope with values as its
// complete live set, and marks the scope's previous generations for retirement
// literalRetireGrace from now. Call it exactly when a rotation has SUCCEEDED
// and pass every secret that is live afterwards (the fresh access token, the
// fresh refresh token, and the client secret that still authenticates the
// connection) — a value re-offered here is revived into the new generation, so
// listing a still-live secret keeps it, while omitting one starts its grace
// clock. An empty scope degrades to AddLiteral (nothing is retired).
func (r *Redactor) RotateScopedLiterals(scope string, values ...string) {
	r.addLiterals(scope, true, values...)
}

// RegisterSecrets is the single entry point the host processes wire their
// runtime secret-acquisition observer to (see agentcore.RegisterSecretLiterals
// and mcpbroker.RegisterSecretLiterals): it maps the observer's (scope,
// rotated) shape onto the three registration modes so the rotation semantics
// live here and not in each caller.
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
// older than gen. The cap backstop makes far-superseded generations due
// immediately; everything else gets the full window.
func (r *Redactor) retireOlderGenerationsLocked(scope string, gen uint64, nowNano int64) {
	graceDeadline := nowNano + literalRetireGrace.Nanoseconds()
	var capFloor uint64 // generations at or below this are due now
	if gen > maxScopeGenerations {
		capFloor = gen - maxScopeGenerations
	}
	for _, l := range r.literals {
		if l.scope != scope || l.gen >= gen {
			continue
		}
		if l.gen <= capFloor {
			l.retireAt = nowNano
			continue
		}
		if l.retireAt == 0 {
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
//   - a stale offer (an older generation of the same scope) does not pull a
//     value back into that older generation.
//
// Over-retention is acceptable here; under-redaction is not.
func (r *Redactor) upsertLocked(value, scope string, gen uint64) {
	if existing, ok := r.index[value]; ok {
		switch {
		case existing.scope == "":
			return // already permanent
		case scope == "":
			existing.scope, existing.gen = "", 0
		case scope == existing.scope && gen < existing.gen:
			return // stale offer; the newer generation already covers it
		default:
			existing.scope, existing.gen = scope, gen
		}
		existing.retireAt = 0
		return
	}
	l := &literal{value: value, scope: scope, gen: gen}
	r.literals = append(r.literals, l)
	r.index[value] = l
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

// LiteralCount reports how many literal VALUES are currently scanned for. It
// exposes the count only, never a value — it is the diagnostic that lets the
// rotation tests assert the scan set plateaus (#1274).
func (r *Redactor) LiteralCount() int {
	if r == nil {
		return 0
	}
	r.sweepDue()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.literals)
}

// Redact returns input with every matched secret replaced. Literals run first
// (exact, novel formats), then the shape patterns.
func (r *Redactor) Redact(input string) string {
	if input == "" || r == nil {
		return input
	}
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
