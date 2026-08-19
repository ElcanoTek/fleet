package config

// ── Typed env-knob registry (#1119) ──
//
// One table describes every numeric/bool/duration env knob the loader
// consumes: its canonical name, how it resolves (the FLEET_→CHAT_→CUTLASS_
// alias chain vs a direct key), its kind, and any range constraints. The table
// is the SINGLE parse implementation behind the three paths that read these
// knobs — boot (Load, via the loadParser collector), hot-reload (reload.go's
// reloadFleet* helpers), and the `fleet validate-config` preflight
// (ValidateEnvKnobs) — so the three can never disagree about what a given env
// accepts or rejects.
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
// Adding a knob: give Load a loadParser call AND add a registry entry here.
// The pairing cannot drift silently — a loader call without a registry entry
// (or with a mismatched kind) records a load error, and the registry-coverage
// test (knobs_coverage_test.go) cross-checks the two directions from source.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// knobKind is the value type an env knob parses to.
type knobKind int

const (
	kindInt knobKind = iota
	kindInt64
	kindFloat
	kindBool
	kindDuration
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
	default:
		return "unknown"
	}
}

// envKnob is one registered numeric/bool/duration env knob.
type envKnob struct {
	// key is the canonical env-var name, as reported in error messages
	// (e.g. "FLEET_MAX_COST_USD", "CONVERSATION_TTL_DAYS").
	key string
	// fleet marks a knob that resolves through the FLEET_→CHAT_→CUTLASS_
	// alias chain (lookupFleet on the suffix after "FLEET_"). A non-fleet
	// knob reads exactly key from the process env.
	fleet bool
	kind  knobKind
	// min/max are optional INCLUSIVE bounds (nil = unbounded), expressed as
	// float64 so one pair covers every numeric kind. They mirror the bounds
	// the hot-reload path has always enforced (plus the two validate-config
	// enforced), so boot and reload reject the same ranges.
	min, max *float64
}

// bound builds a *float64 for the registry literals below.
func bound(v float64) *float64 { return &v }

// envKnobs is the registry: every numeric/bool/duration knob Load consumes.
// Grouping and order mirror Load's struct literal for reviewability.
var envKnobs = []envKnob{
	// ── data (interactive) ──
	{key: "CONVERSATION_TTL_DAYS", kind: kindInt},
	{key: "CONVERSATION_UNPINNED_CAP", kind: kindInt},
	{key: "FLEET_INPUT_QUEUE_RETENTION_DAYS", fleet: true, kind: kindInt, min: bound(0)},
	{key: "FLEET_TURN_EVENT_RETENTION_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_UPLOAD_MAX_BYTES", fleet: true, kind: kindInt64},
	{key: "FLEET_AUTO_ARCHIVE_AFTER_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_SEARCH_ENABLED", kind: kindBool},
	{key: "FLEET_CONVERSATION_SOFT_DELETE", kind: kindBool},

	// ── DB connection pools (#276) ──
	{key: "FLEET_CHAT_DB_MAX_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_CHAT_DB_MIN_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_CHAT_DB_MAX_CONN_IDLE_TIME", fleet: true, kind: kindDuration},
	{key: "FLEET_CHAT_DB_MAX_CONN_LIFETIME", fleet: true, kind: kindDuration},
	{key: "FLEET_CHAT_DB_CONNECT_TIMEOUT", fleet: true, kind: kindDuration},
	{key: "FLEET_SCHED_DB_MAX_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_SCHED_DB_MIN_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_SCHED_DB_MAX_CONN_IDLE_TIME", fleet: true, kind: kindDuration},
	{key: "FLEET_SCHED_DB_MAX_CONN_LIFETIME", fleet: true, kind: kindDuration},
	{key: "FLEET_SCHED_DB_CONNECT_TIMEOUT", fleet: true, kind: kindDuration},

	// ── LLM (shared) ── bounds on the four hot-reloadable ceilings match
	// reload.go, so boot and reload agree (#1119).
	{key: "FLEET_MAX_ITERATIONS", fleet: true, kind: kindInt, min: bound(1), max: bound(10000)},
	{key: "FLEET_MAX_COST_USD", fleet: true, kind: kindFloat, min: bound(0)},
	{key: "FLEET_MAX_TOTAL_TOKENS", fleet: true, kind: kindInt, min: bound(0)},
	{key: "FLEET_DEFAULT_THINKING_BUDGET_TOKENS", fleet: true, kind: kindInt},
	{key: "FLEET_SHUTDOWN_GRACE_SECONDS", fleet: true, kind: kindInt},
	{key: "FLEET_TURN_TIMEOUT_SECONDS", fleet: true, kind: kindInt},
	{key: "FLEET_TEMPERATURE", fleet: true, kind: kindFloat, min: bound(0)},
	{key: "LLM_MAX_TOKENS", kind: kindInt},
	{key: "FLEET_ERROR_ANALYSIS_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SELF_IMPROVE_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_AUTO_TITLE", fleet: true, kind: kindBool},
	{key: "FLEET_MEMORY_AUTOINDEX_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_MEMORY_GRAPH_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_APPROVAL_TIMEOUT_SECONDS", fleet: true, kind: kindInt},
	{key: "FLEET_AUTO_APPROVE_IN_TEST", fleet: true, kind: kindBool},
	// min 1: `fleet serve` hands this straight to admission.New, which floors
	// a 0 to total=1 with no interactive reserve — a box-wide concurrency cap
	// of ONE, not "use a default". (Only the standalone runner pool treats
	// n<1 as "fall back to 8".) A set 0 is a misconfiguration; refuse it.
	{key: "FLEET_MAX_CONCURRENT_AGENTS", fleet: true, kind: kindInt, min: bound(1)},

	// ── phone a friend / sub-agents (#175, #264, #1043) ──
	{key: "FLEET_PHONE_A_FRIEND_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SUBAGENTS_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SUBAGENTS_MAX_DEPTH", fleet: true, kind: kindInt},
	{key: "FLEET_SUBAGENTS_MAX_CHILDREN", fleet: true, kind: kindInt},
	// Out-of-range numeric fractions are CLAMPED by normalizeBudgetFraction
	// (documented: a misconfiguration must never mean "unbounded"), so no
	// bounds here — only a non-numeric value errors.
	{key: "FLEET_SUBAGENTS_BUDGET_FRACTION", fleet: true, kind: kindFloat},

	// ── task memory (#198, #285) ──
	{key: "FLEET_TASK_MEMORY_MAX_KEYS", fleet: true, kind: kindInt},
	{key: "FLEET_TASK_MEMORY_MAX_VALUE_BYTES", fleet: true, kind: kindInt},

	// ── run-history retention (#252) + task pacing (#230, #510) ──
	{key: "FLEET_RUN_LOG_RETENTION_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_KEEP_RUNS_PER_TASK", fleet: true, kind: kindInt},
	{key: "FLEET_CLEANUP_HOUR", fleet: true, kind: kindInt},
	{key: "FLEET_TASK_STARVATION_WINDOW_MINUTES", fleet: true, kind: kindInt},
	{key: "FLEET_PAUSED_TASK_EXPIRY_MINUTES", fleet: true, kind: kindInt},

	// ── process log file sink (#298) + log archival (#272) ──
	{key: "FLEET_LOG_MAX_SIZE_MB", fleet: true, kind: kindInt},
	{key: "FLEET_LOG_MAX_AGE_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_LOG_MAX_BACKUPS", fleet: true, kind: kindInt},
	{key: "FLEET_LOG_COMPRESS", fleet: true, kind: kindBool},
	{key: "FLEET_LOG_ARCHIVE_AFTER_DAYS", fleet: true, kind: kindInt},

	// ── remote MCP OAuth (#443) ──
	{key: "FLEET_REMOTE_MCP_ALLOW_INSECURE_HTTP", fleet: true, kind: kindBool},

	// ── rate limit (interactive) ──
	{key: "CHAT_RATE_PER_MIN", kind: kindInt},
	{key: "CHAT_RATE_PER_DAY", kind: kindInt},
	{key: "FLEET_CHAT_RATE_LIMIT_ENABLED", kind: kindBool},
	{key: "FLEET_CHAT_RATE_LIMIT_CONCURRENT", kind: kindInt},

	// ── sandbox ──
	{key: "FLEET_PII_REDACTION_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_CONTEXT_HANDLES_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SANDBOX_PIDS", kind: kindInt},
	{key: "FLEET_SANDBOX_DISK_GB", kind: kindInt},
	{key: "FLEET_SANDBOX_MEMORY_MAX_MB", fleet: true, kind: kindInt},
	{key: "FLEET_SANDBOX_CPUS_MAX", fleet: true, kind: kindFloat},
	{key: "FLEET_SANDBOX_PIDS_MAX", fleet: true, kind: kindInt},
	{key: "FLEET_SANDBOX_WARM_SIZE", fleet: true, kind: kindInt},
	{key: "FLEET_SANDBOX_WARM_TTL", fleet: true, kind: kindInt},

	// ── python REPL (#213) ──
	{key: "FLEET_PYTHON_CELL_TIMEOUT", fleet: true, kind: kindInt},
	{key: "FLEET_PYTHON_REPL_IDLE_TTL", fleet: true, kind: kindInt},
	{key: "FLEET_PYTHON_REPL_MAX", fleet: true, kind: kindInt},

	// ── lockdown / test harness ── FLEET_LOCKDOWN_ONLY is the security knob
	// whose silent fail-open motivated #1119: an unrecognized token used to
	// leave lockdown OFF; now it refuses to boot.
	{key: "FLEET_LOCKDOWN_ONLY", fleet: true, kind: kindBool},
	{key: "FLEET_MOCK_MODE", fleet: true, kind: kindBool},
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
	return strings.TrimSpace(stripQuotes(strings.TrimSpace(raw)))
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
	case kindDuration:
		_, _, err = k.parseDuration(raw)
	}
	return err
}

// ValidateEnvKnobs preflights every registered numeric/bool/duration env knob:
// each knob that is SET (non-blank after quote stripping, any alias spelling)
// gets the exact parse + range check Load enforces at boot, and each problem
// string names the env var, the offending value, and the expected format.
// Unset knobs are fine — the default applies. `fleet validate-config` calls
// this so its preflight is table-driven from the same registry the boot and
// hot-reload paths parse through, and can never drift from them.
func ValidateEnvKnobs() []string {
	var problems []string
	for i := range envKnobs {
		k := &envKnobs[i]
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
			"%s: BUG — read by the loader but not in the envKnobs registry; add an entry in knobs.go so boot, hot-reload, and `fleet validate-config` all validate it (#1119)", key))
		return nil
	}
	if k.kind != kind {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — envKnobs registry kind mismatch (registry %s, loader reads %s); fix the entry in knobs.go", key, k.kind.label(), kind.label()))
		return nil
	}
	return k
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
