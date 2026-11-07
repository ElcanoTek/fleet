package config

// ── Typed env-knob registry (#1119, extended by #1273) ──
//
// One table describes every numeric/bool/duration env knob the BINARY parses,
// wherever it parses it: the knob's canonical name, how it resolves (the
// FLEET_→CHAT_→CUTLASS_ alias chain vs a direct key), its kind, and any range
// constraints. The table is the SINGLE parse implementation behind the three
// paths that read these knobs — boot (Load, via the loadParser collector),
// hot-reload (reload.go's reloadFleet* helpers), and the `fleet
// validate-config` preflight (ValidateEnvKnobs) — so the three can never
// disagree about what a given env accepts or rejects.
//
// Semantics, shared by all three paths:
//
//   - An UNSET knob (or one that is blank after whitespace/quote stripping)
//     gets its default — never an error.
//   - A SET-but-malformed or out-of-range value is an ERROR: boot refuses to
//     start, reload keeps the running value and reports a ReloadError, and
//     validate-config reports a blocking env_vars failure. This matches the
//     loader's IP-list/TLS/network-mode posture; before #1119 these knobs
//     silently fell back to their defaults, which was fail-OPEN for security
//     knobs (FLEET_LOCKDOWN_ONLY=enabled left lockdown off).
//   - Values are cleaned before parsing: surrounding whitespace trimmed, ONE
//     layer of matching quotes stripped (podman/docker --env-file keep them),
//     then trimmed again.
//
// ── The three knob classes (#1273) ──
//
// #1119 covered only what Load itself consumes; a knob parsed at its point of
// use somewhere else in the tree stayed silently (or warn-)defaulting, and
// `fleet validate-config` could not preflight it at all. Every knob now
// carries a scope and a strictness, so there is no silent class left:
//
//  1. scopeLoader (the #1119 population) — Load consumes the value through a
//     loadParser call. Registry↔loader agreement is asserted BOTH ways from
//     source by knobs_coverage_test.go.
//  2. scopeExternal — the value is consumed OUTSIDE Load (deep in
//     internal/sandbox, internal/agentcore, internal/httpapi, cmd/fleet …),
//     usually because it is needed where no *Config is in reach. Load does
//     not consume these, but it VALIDATES them (externalKnobProblems, called
//     from Load) — so a malformed value still refuses to boot, in the same
//     one-pass error as every loader knob. Where a read happens in a verb
//     that never calls Load (`fleet backup`), the point of use resolves the
//     value through the exported EnvKnob* helpers below, which parse through
//     this same table.
//  3. lenient — a documented-lenient knob (today only the OTEL sample ratio):
//     the consumer deliberately absorbs a bad value rather than failing, so
//     boot does NOT refuse. It is still registered, so validate-config
//     preflights the syntax and reports it as an ADVISORY. Every lenient row
//     must carry the rationale in why (a registry test enforces that).
//
// Adding a knob: give its entry in the configuration registry
// (internal/config/envreg/table.go) a Knob spec — the rows below are derived
// from it (#2053). For a loader knob, give Load a loadParser call too — the
// pairing cannot drift silently, since a loader call without a knob row (or
// with a mismatched kind) records a load error, and knobs_coverage_test.go
// cross-checks both directions from source. For an out-of-loader knob,
// knobs_sweep_test.go walks the whole repo for ad-hoc env read+parse pairs
// (os.Getenv or envreg.Get) and fails until the key has a Knob spec.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// knobKind is the value type an env knob parses to.
type knobKind int

const (
	kindInt knobKind = iota
	kindInt64
	kindFloat
	kindBool
	kindDuration
	// kindStrconvBool is a bool whose CONSUMER parses it with
	// strconv.ParseBool, which accepts a NARROWER token set than kindBool
	// (1/0/t/f/true/false and the TRUE/True case variants — but not
	// yes/no/on/off). Registered separately so the registry describes what
	// the reader actually honors: validating `=yes` as fine while the reader
	// silently resolves it to false would be a worse assurance than none
	// (#1273).
	kindStrconvBool
)

// label names the kind in registry-bug messages.
func (k knobKind) label() string {
	switch k {
	case kindInt:
		return "int"
	case kindInt64:
		return "int64"
	case kindFloat:
		return "float"
	case kindBool:
		return "bool"
	case kindDuration:
		return "duration"
	case kindStrconvBool:
		return "strconv-bool"
	default:
		return "unknown"
	}
}

// knobScope says which code path consumes a knob's value — see the three
// classes in the package comment above.
type knobScope int

const (
	// scopeLoader: consumed by Load through a loadParser call (the #1119 set).
	scopeLoader knobScope = iota
	// scopeExternal: consumed outside Load, at its point of use (#1273).
	scopeExternal
)

// envKnob is one registered numeric/bool/duration env knob.
type envKnob struct {
	// key is the canonical env-var name, as reported in error messages
	// (e.g. "FLEET_MAX_COST_USD", "CONVERSATION_TTL_DAYS").
	key string
	// fleet marks a knob that resolves through the FLEET_→CHAT_→CUTLASS_
	// alias chain (lookupFleet on the suffix after "FLEET_"). A non-fleet
	// knob reads exactly key from the process env. Set it ONLY when the
	// consumer honors the aliases too (the loader always does; an external
	// consumer does when it reads through agentcore's fleetEnv reader,
	// and does NOT when it calls os.Getenv("FLEET_…") directly) — otherwise
	// boot would refuse over a spelling nothing ever reads.
	fleet bool
	kind  knobKind
	// min/max are optional INCLUSIVE bounds (nil = unbounded), expressed as
	// float64 so one pair covers every numeric kind. They mirror the bounds
	// the hot-reload path has always enforced (plus the two validate-config
	// enforced), so boot and reload reject the same ranges.
	min, max *float64

	// scope says who consumes the value (see knobScope).
	scope knobScope
	// readBy names the package/file that consumes an external knob, so a boot
	// failure points at the code that reads it. Required on scopeExternal rows.
	readBy string
	// lenient marks a knob whose consumer DELIBERATELY absorbs a malformed
	// value. Boot does not refuse; validate-config still preflights the syntax
	// and reports an advisory. why is the rationale and is REQUIRED here.
	lenient bool
	why     string
}

// envKnobs is the knob table: every numeric/bool/duration knob the binary
// parses. Since #2053 it is DERIVED from the configuration registry
// (internal/config/envreg/table.go) — each entry carrying a Knob spec becomes
// one row, in registry order — so a knob's kind, bounds, consumer and alias
// behaviour are declared once, next to its default and description.
// TestKnobsMatchPreRegistrySnapshot pins the derived rows to the table this
// replaced, field for field.
var envKnobs = func() []envKnob {
	var out []envKnob
	for _, e := range envreg.All() {
		if e.Knob == nil {
			continue
		}
		k := envKnob{
			key:     e.Name,
			fleet:   e.Chain, // LEGACY-ENV-ALIAS: remove after migrate-env
			kind:    knobKindOf(e.Type),
			min:     e.Knob.Min,
			max:     e.Knob.Max,
			readBy:  e.Knob.ReadBy,
			lenient: e.Knob.Lenient,
			why:     e.Knob.Why,
		}
		if e.Knob.ReadBy != "" {
			k.scope = scopeExternal
		}
		out = append(out, k)
	}
	return out
}()

// knobKindOf maps a registry type onto the parser's kind. envreg refuses a
// Knob on any other type at init, so the default is unreachable.
func knobKindOf(t envreg.Type) knobKind {
	switch t {
	case envreg.TypeInt:
		return kindInt
	case envreg.TypeInt64:
		return kindInt64
	case envreg.TypeFloat:
		return kindFloat
	case envreg.TypeBool:
		return kindBool
	case envreg.TypeDuration:
		return kindDuration
	case envreg.TypeStrictBool:
		return kindStrconvBool
	default:
		panic("config: registry knob of non-knob type " + string(t))
	}
}

// envKnobByKey indexes the registry by canonical name for the loader and the
// reload path.
var envKnobByKey = func() map[string]*envKnob {
	m := make(map[string]*envKnob, len(envKnobs))
	for i := range envKnobs {
		k := &envKnobs[i]
		if _, dup := m[k.key]; dup {
			panic("config: duplicate envKnobs entry " + k.key)
		}
		m[k.key] = k
	}
	return m
}()

// lookup resolves the knob's raw value from the environment; ok is false when
// no spelling is set (non-empty).
func (k *envKnob) lookup() (string, bool) {
	if k.fleet {
		return lookupFleet(strings.TrimPrefix(k.key, canonicalPrefix))
	}
	v := os.Getenv(k.key)
	return v, v != ""
}

// cleanEnvValue normalizes a raw env value before typed parsing: surrounding
// whitespace is trimmed, ONE layer of matching quotes is stripped (podman/
// docker --env-file keep them in place), then the result is trimmed again. A
// blank result is treated as UNSET (the default applies) rather than
// malformed.
func cleanEnvValue(raw string) string {
	return strings.TrimSpace(creds.StripEnvQuotes(strings.TrimSpace(raw)))
}

// checkBounds enforces the knob's optional inclusive bounds. The message names
// the offending value and the full expected range; the caller prefixes the env
// var name.
func (k *envKnob) checkBounds(v float64) error {
	if (k.min != nil && v < *k.min) || (k.max != nil && v > *k.max) {
		val := strconv.FormatFloat(v, 'f', -1, 64)
		switch {
		case k.min != nil && k.max != nil:
			return fmt.Errorf("%s is out of range (must be between %s and %s)",
				val, formatBound(*k.min), formatBound(*k.max))
		case k.min != nil:
			return fmt.Errorf("%s is out of range (must be >= %s)", val, formatBound(*k.min))
		default:
			return fmt.Errorf("%s is out of range (must be <= %s)", val, formatBound(*k.max))
		}
	}
	return nil
}

func formatBound(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// The typed parsers below share one contract: (value, set, err). set is false
// when the cleaned value is blank (treat as unset → default); err is non-nil
// for a non-blank value that fails to parse or falls outside the bounds. Error
// messages name the offending value and the expected format; callers prefix
// the env-var name (Load's collector and validate-config inline it, the
// reload path carries it in ReloadError.Key).

func (k *envKnob) parseInt(raw string) (int, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0, false, fmt.Errorf("invalid integer %q (expected a whole number like \"30\")", v)
	}
	if err := k.checkBounds(float64(i)); err != nil {
		return 0, false, err
	}
	return i, true, nil
}

func (k *envKnob) parseInt64(raw string) (int64, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid integer %q (expected a whole number like \"1073741824\")", v)
	}
	if err := k.checkBounds(float64(i)); err != nil {
		return 0, false, err
	}
	return i, true, nil
}

func (k *envKnob) parseFloat(raw string) (float64, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid number %q (expected a decimal number like \"12.5\")", v)
	}
	if err := k.checkBounds(f); err != nil {
		return 0, false, err
	}
	return f, true, nil
}

func (k *envKnob) parseBool(raw string) (bool, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return false, false, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	}
	return false, false, fmt.Errorf("invalid boolean %q (expected one of: 1, 0, true, false, yes, no, on, off)", v)
}

// parseStrconvBool mirrors a consumer that calls strconv.ParseBool directly
// (#1273): the accepted tokens are exactly ParseBool's, so the registry never
// certifies a spelling the reader would resolve to the zero value.
func (k *envKnob) parseStrconvBool(raw string) (bool, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return false, false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false, fmt.Errorf("invalid boolean %q (this knob is read with Go's strconv.ParseBool, which accepts only: 1, 0, t, f, true, false, TRUE, FALSE, True, False — not yes/no/on/off)", v)
	}
	return b, true, nil
}

func (k *envKnob) parseDuration(raw string) (time.Duration, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("invalid duration %q (expected a Go duration like \"30s\", \"5m\", or \"1h30m\")", v)
	}
	if err := k.checkBounds(float64(d)); err != nil {
		return 0, false, err
	}
	return d, true, nil
}

// check parses raw for the knob's kind and reports the first problem, or nil
// when the value is well-formed (or blank = unset). validate-config's
// registry walk uses it; the typed callers use the parse* funcs directly.
func (k *envKnob) check(raw string) error {
	var err error
	switch k.kind {
	case kindInt:
		_, _, err = k.parseInt(raw)
	case kindInt64:
		_, _, err = k.parseInt64(raw)
	case kindFloat:
		_, _, err = k.parseFloat(raw)
	case kindBool:
		_, _, err = k.parseBool(raw)
	case kindStrconvBool:
		_, _, err = k.parseStrconvBool(raw)
	case kindDuration:
		_, _, err = k.parseDuration(raw)
	}
	return err
}

// ValidateEnvKnobs preflights every registered STRICT numeric/bool/duration
// env knob — the loader's own (#1119) and, since #1273, the out-of-loader ones
// too: each knob that is SET (non-blank after quote stripping, any alias
// spelling it is actually read under) gets the exact parse + range check boot
// enforces, and each problem string names the env var, the offending value,
// and the expected format. Unset knobs are fine — the default applies.
// `fleet validate-config` calls this so its preflight is table-driven from the
// same registry the boot and hot-reload paths parse through, and can never
// drift from them.
//
// Documented-lenient knobs are excluded here and reported by
// ValidateLenientEnvKnobs instead, so a blocking failure means "this refuses
// to boot" and nothing weaker.
func ValidateEnvKnobs() []string {
	return validateKnobs(func(k *envKnob) bool { return !k.lenient })
}

// ValidateLenientEnvKnobs preflights the documented-lenient knobs (#1273):
// their consumer deliberately absorbs a malformed value, so boot does not
// refuse — but an operator who typo'd one still deserves to be told, so
// `fleet validate-config` reports these as ADVISORIES (warn, never blocking).
// Each problem names the variable, the offending value, and what the consumer
// will do with it instead.
func ValidateLenientEnvKnobs() []string {
	problems := validateKnobs(func(k *envKnob) bool { return k.lenient })
	for i, p := range problems {
		problems[i] = p + " — not a boot failure: " + lenientWhyByProblem(p)
	}
	return problems
}

// lenientWhyByProblem recovers the rationale for the knob a problem string
// names (problems are formatted "KEY: …"), so the advisory carries the WHY
// from the registry row rather than a second copy of it here.
func lenientWhyByProblem(problem string) string {
	key, _, _ := strings.Cut(problem, ":")
	if k := envKnobByKey[key]; k != nil && k.why != "" {
		return k.why
	}
	return "documented-lenient knob"
}

// validateKnobs walks the registry, keeping the rows want selects.
func validateKnobs(want func(*envKnob) bool) []string {
	var problems []string
	for i := range envKnobs {
		k := &envKnobs[i]
		if !want(k) {
			continue
		}
		raw, ok := k.lookup()
		if !ok {
			continue
		}
		if err := k.check(raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", k.key, err))
		}
	}
	return problems
}

// externalKnobProblems is the boot gate for scopeExternal knobs (#1273). Load
// calls it and folds the result into the SAME one-pass error the loader knobs
// produce, so a malformed knob refuses to boot whether Load consumes the value
// or some package deep in the tree does. Lenient rows are skipped by design.
//
// Each message names the consumer, because the operator cannot infer it from a
// config-load failure the way they can for a loader knob.
func externalKnobProblems() []string {
	var problems []string
	for i := range envKnobs {
		k := &envKnobs[i]
		if k.scope != scopeExternal || k.lenient {
			continue
		}
		raw, ok := k.lookup()
		if !ok {
			continue
		}
		if err := k.check(raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v (read by %s)", k.key, err, k.readBy))
		}
	}
	return problems
}

// ── the exported point-of-use readers (#1273) ──
//
// A `fleet` verb that never calls Load (`fleet backup`) gets no boot gate, so
// it resolves its knob through these helpers instead: same registry row, same
// parser, same bounds, and a real error the verb can refuse on. They are
// deliberately narrow — one lookup, no collection — since a CLI verb wants to
// fail on the knob it actually needs, not on an unrelated malformed one.
//
// Calling one with a key that is not a scopeExternal row of the matching kind
// is a programming error and is reported as such (the sweep test and the
// registry tests both catch it, but a runtime error beats a silent default).

// EnvKnobInt resolves a registered external int knob, returning def when it is
// unset (or blank) and an error when it is set but malformed/out of range.
func EnvKnobInt(key string, def int) (int, error) {
	k, err := externalKnob(key, kindInt)
	if err != nil {
		return def, err
	}
	raw, ok := k.lookup()
	if !ok {
		return def, nil
	}
	v, set, err := k.parseInt(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %w", k.key, err)
	}
	if !set {
		return def, nil
	}
	return v, nil
}

// EnvKnobBool resolves a registered external bool knob, returning def when it
// is unset (or blank) and an error when it is set but not a recognized
// boolean token.
func EnvKnobBool(key string, def bool) (bool, error) {
	k, err := externalKnob(key, kindBool)
	if err != nil {
		return def, err
	}
	raw, ok := k.lookup()
	if !ok {
		return def, nil
	}
	v, set, err := k.parseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %w", k.key, err)
	}
	if !set {
		return def, nil
	}
	return v, nil
}

// externalKnob resolves the registry row a point-of-use reader expects.
func externalKnob(key string, kind knobKind) (*envKnob, error) {
	k := envKnobByKey[key]
	if k == nil {
		return nil, fmt.Errorf("%s: BUG — read at its point of use but not in the envKnobs registry; add a scopeExternal entry in internal/config/envreg/table.go (its Knob spec) so boot and `fleet validate-config` validate it (#1273)", key)
	}
	if k.scope != scopeExternal {
		return nil, fmt.Errorf("%s: BUG — envKnobs registry has it as a loader knob; read it off the *Config instead", key)
	}
	if k.kind != kind {
		return nil, fmt.Errorf("%s: BUG — envKnobs registry kind mismatch (registry %s, caller reads %s); fix the entry in internal/config/envreg/table.go (its Knob spec)", key, k.kind.label(), kind.label())
	}
	return k, nil
}

// ── the Load-side collector ──

// loadParser resolves typed env knobs for Load, collecting every malformed
// value so boot can refuse with ONE error naming all of them (instead of
// failing one var at a time). Its methods keep the historical helper names so
// Load's call sites read unchanged; each returns the default for an
// unset/blank knob and records an error (still returning the default, so the
// struct literal stays total) for a set-but-malformed one.
type loadParser struct {
	problems []string
}

// knob resolves the registry entry the caller expects, recording a loud
// internal error when it is missing or mismatched: this is what makes the
// loader↔registry pairing impossible to drift silently — a new getenv* call
// without a registry entry fails every Load (and therefore every test that
// boots a config).
func (p *loadParser) knob(key string, kind knobKind) *envKnob {
	k := envKnobByKey[key]
	if k == nil {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — read by the loader but not in the envKnobs registry; add an entry in internal/config/envreg/table.go (its Knob spec) so boot, hot-reload, and `fleet validate-config` all validate it (#1119)", key))
		return nil
	}
	if k.kind != kind {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — envKnobs registry kind mismatch (registry %s, loader reads %s); fix the entry in internal/config/envreg/table.go (its Knob spec)", key, k.kind.label(), kind.label()))
		return nil
	}
	if k.scope != scopeLoader {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — envKnobs registry marks it scopeExternal but the loader consumes it; drop the scope/readBy fields from its entry in internal/config/envreg/table.go (its Knob spec) (#1273)", key))
		return nil
	}
	return k
}

// collectExternalKnobs folds the scopeExternal boot gate (#1273) into the same
// one-pass error the loader knobs produce, so `fleet serve` refuses on every
// malformed knob at once — whoever ends up consuming the value.
func (p *loadParser) collectExternalKnobs() {
	p.problems = append(p.problems, externalKnobProblems()...)
}

// fail records one malformed knob, prefixed with its canonical env-var name.
func (p *loadParser) fail(k *envKnob, err error) {
	p.problems = append(p.problems, fmt.Sprintf("%s: %v", k.key, err))
}

// err reports every problem collected during Load, or nil when the
// environment parsed clean. The message enumerates each offending variable so
// an operator fixes the whole file in one pass.
func (p *loadParser) err() error {
	if len(p.problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid environment configuration (unset variables get defaults; set variables must parse):\n  - %s",
		strings.Join(p.problems, "\n  - "))
}

func (p *loadParser) getenvInt(key string, def int) int {
	k := p.knob(key, kindInt)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseInt(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvBool(key string, def bool) bool {
	k := p.knob(key, kindBool)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseBool(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvFleetInt(suffix string, def int) int {
	return p.getenvInt(canonicalPrefix+strings.TrimLeft(suffix, "_"), def)
}

func (p *loadParser) getenvFleetBool(suffix string, def bool) bool {
	return p.getenvBool(canonicalPrefix+strings.TrimLeft(suffix, "_"), def)
}

func (p *loadParser) getenvFleetInt64(suffix string, def int64) int64 {
	k := p.knob(canonicalPrefix+strings.TrimLeft(suffix, "_"), kindInt64)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseInt64(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvFleetFloat(suffix string, def float64) float64 {
	k := p.knob(canonicalPrefix+strings.TrimLeft(suffix, "_"), kindFloat)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseFloat(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvFleetDuration(suffix string, def time.Duration) time.Duration {
	k := p.knob(canonicalPrefix+strings.TrimLeft(suffix, "_"), kindDuration)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseDuration(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}
