package agentcore

import (
	"os"
	"strconv"
	"strings"
)

// Env-var prefix handling.
//
// chat and cutlass each baked a compile-time prefix into their env-var
// names (CHAT_* and CUTLASS_*). The unified runtime is configured once with
// a canonical prefix — FLEET_ — but keeps the two legacy prefixes alive as
// back-compat aliases so a deployment that still sets CHAT_DISABLE_PROMPT_CACHE
// or CUTLASS_RETRY_MAX_ATTEMPTS keeps working, and so the lifted parity tests
// (which set the legacy names) stay green.
//
// EnvPrefix is the single divergence axis for env naming: a constructor takes
// a prefix, and every env lookup goes through lookupEnv which tries the
// configured prefix first and then the legacy aliases.

// CanonicalEnvPrefix is the default env-var prefix for the unified runtime.
const CanonicalEnvPrefix = "FLEET"

// legacyEnvPrefixes are the back-compat aliases tried after the configured
// prefix. Order is irrelevant: the first non-empty match wins, and the two
// front-ends never set the same suffix to conflicting values.
var legacyEnvPrefixes = []string{"CHAT", "CUTLASS"}

// EnvPrefix names the env-var family a run reads. The zero value behaves like
// CanonicalEnvPrefix. Constructed once at Manager/engine build time and carried
// on RunConfig so the loop's env lookups are deterministic per run.
type EnvPrefix string

// normalize returns the effective prefix, defaulting to the canonical one and
// stripping any trailing underscore the caller may have included.
func (p EnvPrefix) normalize() string {
	s := strings.TrimRight(strings.ToUpper(strings.TrimSpace(string(p))), "_")
	if s == "" {
		return CanonicalEnvPrefix
	}
	return s
}

// lookup returns the value of <PREFIX>_<suffix>, falling back to the legacy
// CHAT_/CUTLASS_ aliases when the configured prefix is unset. The first
// non-empty value wins.
func (p EnvPrefix) lookup(suffix string) string {
	suffix = strings.TrimLeft(suffix, "_")
	if v := strings.TrimSpace(os.Getenv(p.normalize() + "_" + suffix)); v != "" {
		return v
	}
	for _, legacy := range legacyEnvPrefixes {
		if legacy == p.normalize() {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(legacy + "_" + suffix)); v != "" {
			return v
		}
	}
	return ""
}

// lookupBool parses the resolved value as a bool (strconv.ParseBool rules).
// Unset / unparseable both report false, matching the original kill-switch
// semantics in both repos.
func (p EnvPrefix) lookupBool(suffix string) bool {
	v, _ := strconv.ParseBool(p.lookup(suffix))
	return v
}

// lookupFloatDefault parses the resolved value as a float64, returning def when
// the var is unset or unparseable. Used for fractional thresholds (e.g. the
// context-pressure ratios) where a bad value must fall back to a safe default
// rather than collapse to zero.
func (p EnvPrefix) lookupFloatDefault(suffix string, def float64) float64 {
	raw := p.lookup(suffix)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}
